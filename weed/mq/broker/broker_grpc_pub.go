package broker

import (
	"context"
	"fmt"
	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/mq/topic"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/mq_pb"
	"google.golang.org/grpc/peer"
	jsonpb "google.golang.org/protobuf/encoding/protojson"
	"math/rand"
	"net"
	"sync/atomic"
	"time"
)

// PUB
// 1. gRPC API to configure a topic
//    1.1 create a topic with existing partition count
//    1.2 assign partitions to brokers
// 2. gRPC API to lookup topic partitions
// 3. gRPC API to publish by topic partitions

// SUB
// 1. gRPC API to lookup a topic partitions

// Re-balance topic partitions for publishing
//   1. collect stats from all the brokers
//   2. Rebalance and configure new generation of partitions on brokers
//   3. Tell brokers to close current gneration of publishing.
// Publishers needs to lookup again and publish to the new generation of partitions.

// Re-balance topic partitions for subscribing
//   1. collect stats from all the brokers
// Subscribers needs to listen for new partitions and connect to the brokers.
// Each subscription may not get data. It can act as a backup.

func (b *MessageQueueBroker) PublishMessage(stream mq_pb.SeaweedMessaging_PublishMessageServer) error {
	// 1. write to the volume server
	// 2. find the topic metadata owning filer
	// 3. write to the filer

	var localTopicPartition *topic.LocalPartition
	req, err := stream.Recv()
	if err != nil {
		return err
	}
	response := &mq_pb.PublishMessageResponse{}
	// TODO check whether current broker should be the leader for the topic partition
	ackInterval := 1
	initMessage := req.GetInit()
	var t topic.Topic
	var p topic.Partition
	if initMessage != nil {
		t, p = topic.FromPbTopic(initMessage.Topic), topic.FromPbPartition(initMessage.Partition)
		localTopicPartition = b.localTopicManager.GetTopicPartition(t, p)
		if localTopicPartition == nil {
			localTopicPartition, err = b.loadLocalTopicPartitionFromFiler(t, p)
			// if not created, return error
			if err != nil {
				response.Error = fmt.Sprintf("topic %v partition %v not setup: %v", initMessage.Topic, initMessage.Partition, err)
				glog.Errorf("topic %v partition %v not setup: %v", initMessage.Topic, initMessage.Partition, err)
				return stream.Send(response)
			}
		}
		ackInterval = int(initMessage.AckInterval)
		stream.Send(response)
	} else {
		response.Error = fmt.Sprintf("missing init message")
		glog.Errorf("missing init message")
		return stream.Send(response)
	}

	clientName := fmt.Sprintf("%v-%4d/%s/%v", findClientAddress(stream.Context()), rand.Intn(10000), initMessage.Topic, initMessage.Partition)
	localTopicPartition.Publishers.AddPublisher(clientName, topic.NewLocalPublisher())

	ackCounter := 0
	var ackSequence int64
	var isStopping int32
	respChan := make(chan *mq_pb.PublishMessageResponse, 128)
	defer func() {
		atomic.StoreInt32(&isStopping, 1)
		close(respChan)
		localTopicPartition.Publishers.RemovePublisher(clientName)
		if localTopicPartition.MaybeShutdownLocalPartition() {
			b.localTopicManager.RemoveTopicPartition(t, p)
		}
	}()
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		for {
			select {
			case resp := <-respChan:
				if resp != nil {
					if err := stream.Send(resp); err != nil {
						glog.Errorf("Error sending response %v: %v", resp, err)
					}
				} else {
					return
				}
			case <-ticker.C:
				if atomic.LoadInt32(&isStopping) == 0 {
					response := &mq_pb.PublishMessageResponse{
						AckSequence: ackSequence,
					}
					respChan <- response
				} else {
					return
				}
			case <-localTopicPartition.StopPublishersCh:
				respChan <- &mq_pb.PublishMessageResponse{
					AckSequence: ackSequence,
					ShouldClose: true,
				}
			}
		}
	}()

	// process each published messages
	for {
		// receive a message
		req, err := stream.Recv()
		if err != nil {
			return err
		}

		// Process the received message
		if dataMessage := req.GetData(); dataMessage != nil {
			localTopicPartition.Publish(dataMessage)
		}

		ackCounter++
		ackSequence++
		if ackCounter >= ackInterval {
			ackCounter = 0
			// send back the ack
			response := &mq_pb.PublishMessageResponse{
				AckSequence: ackSequence,
			}
			respChan <- response
		}
	}

	glog.V(0).Infof("topic %v partition %v publish stream closed.", initMessage.Topic, initMessage.Partition)

	return nil
}

func (b *MessageQueueBroker) loadLocalTopicPartitionFromFiler(t topic.Topic, p topic.Partition) (localTopicPartition *topic.LocalPartition, err error) {
	self := b.option.BrokerAddress()
	glog.V(0).Infof("broker %s load topic %v partition %v", self, t, p)

	// load local topic partition from configuration on filer if not found
	var conf *mq_pb.ConfigureTopicResponse
	conf, err = b.readTopicConfFromFiler(t, p)
	if err != nil {
		return nil, err
	}

	// create local topic partition
	var hasCreated bool
	for _, assignment := range conf.BrokerPartitionAssignments {
		if assignment.LeaderBroker == string(self) && p.Equals(topic.FromPbPartition(assignment.Partition)) {
			localTopicPartition = topic.FromPbBrokerPartitionAssignment(self, p, assignment, b.genLogFlushFunc(t, assignment.Partition), b.genLogOnDiskReadFunc(t, assignment.Partition))
			b.localTopicManager.AddTopicPartition(t, localTopicPartition)
			hasCreated = true
			break
		}
	}

	if !hasCreated {
		return nil, fmt.Errorf("topic %v partition %v not assigned to broker %v", t, p, self)
	}

	return localTopicPartition, nil
}

func (b *MessageQueueBroker) readTopicConfFromFiler(t topic.Topic, p topic.Partition) (conf *mq_pb.ConfigureTopicResponse, err error) {
	topicDir := fmt.Sprintf("%s/%s/%s", filer.TopicsDir, t.Namespace, t.Name)
	if err = b.WithFilerClient(false, func(client filer_pb.SeaweedFilerClient) error {
		data, err := filer.ReadInsideFiler(client, topicDir, "topic.conf")
		if err != nil {
			return fmt.Errorf("read topic %v partition %v conf: %v", t, p, err)
		}
		// parse into filer conf object
		conf = &mq_pb.ConfigureTopicResponse{}
		if err = jsonpb.Unmarshal(data, conf); err != nil {
			return fmt.Errorf("unmarshal topic %v partition %v conf: %v", t, p, err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return conf, err
}

// duplicated from master_grpc_server.go
func findClientAddress(ctx context.Context) string {
	// fmt.Printf("FromContext %+v\n", ctx)
	pr, ok := peer.FromContext(ctx)
	if !ok {
		glog.Error("failed to get peer from ctx")
		return ""
	}
	if pr.Addr == net.Addr(nil) {
		glog.Error("failed to get peer address")
		return ""
	}
	return pr.Addr.String()
}
