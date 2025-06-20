// Copyright 2014 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package kvserver_test

import (
	"context"
	"math/rand"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/gossip"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvflowcontrol/node_rac2"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvserverpb"
	"github.com/cockroachdb/cockroach/pkg/raft/raftpb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/rpc"
	"github.com/cockroachdb/cockroach/pkg/rpc/nodedialer"
	"github.com/cockroachdb/cockroach/pkg/rpc/rpcbase"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/skip"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/metric"
	"github.com/cockroachdb/cockroach/pkg/util/netutil"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

const channelServerBrokenRangeMessage = "channelServer broken range"

type channelServer struct {
	ch       chan *kvserverpb.RaftMessageRequest
	maxSleep time.Duration

	// If non-zero, all messages to this range will return errors
	brokenRange roachpb.RangeID
}

func newChannelServer(bufSize int, maxSleep time.Duration) channelServer {
	return channelServer{
		ch:       make(chan *kvserverpb.RaftMessageRequest, bufSize),
		maxSleep: maxSleep,
	}
}

func (s channelServer) HandleRaftRequest(
	ctx context.Context, req *kvserverpb.RaftMessageRequest, _ kvserver.RaftMessageResponseStream,
) *kvpb.Error {
	if s.maxSleep != 0 {
		// maxSleep simulates goroutine scheduling delays that could
		// result in messages being processed out of order (in previous
		// transport implementations).
		time.Sleep(time.Duration(rand.Int63n(int64(s.maxSleep))))
	}
	if s.brokenRange != 0 && s.brokenRange == req.RangeID {
		return kvpb.NewErrorf(channelServerBrokenRangeMessage)
	}
	s.ch <- req
	return nil
}

func (s channelServer) HandleRaftResponse(
	ctx context.Context, resp *kvserverpb.RaftMessageResponse,
) error {
	// Mimic the logic in (*Store).HandleRaftResponse without requiring an
	// entire Store object to be pulled into these tests.
	if val, ok := resp.Union.GetValue().(*kvpb.Error); ok {
		if err, ok := val.GetDetail().(*kvpb.StoreNotFoundError); ok {
			return err
		}
	}
	log.Fatalf(ctx, "unexpected raft response: %s", resp)
	return nil
}

func (s channelServer) HandleSnapshot(
	_ context.Context,
	header *kvserverpb.SnapshotRequest_Header,
	stream kvserver.SnapshotResponseStream,
) error {
	panic("unexpected HandleSnapshot")
}

func (s channelServer) HandleDelegatedSnapshot(
	ctx context.Context, req *kvserverpb.DelegateSendSnapshotRequest,
) *kvserverpb.DelegateSnapshotResponse {
	panic("unexpected HandleDelegatedSnapshot")
}

// raftTransportTestContext contains objects needed to test RaftTransport.
// Typical usage will add multiple nodes with AddNode, attach channels
// to at least one store with ListenStore, and send messages with Send.
type raftTransportTestContext struct {
	t              testing.TB
	stopper        *stop.Stopper
	clocks         map[roachpb.NodeID]clockWithManualSource
	transports     map[roachpb.NodeID]*kvserver.RaftTransport
	nodeRPCContext *rpc.Context
	gossip         *gossip.Gossip
	st             *cluster.Settings

	skipOnListenErr bool // if true, calls Skip on error from net.Listen
}

// clockWithManualSource is a pair of clocks: a manual clock and a clock that
// uses the manual clock as a source.
type clockWithManualSource struct {
	manual *hlc.HybridManualClock
	clock  *hlc.Clock
}

func newRaftTransportTestContext(t testing.TB, st *cluster.Settings) *raftTransportTestContext {
	ctx := context.Background()
	tr := tracing.NewTracer()
	rttc := &raftTransportTestContext{
		t:          t,
		stopper:    stop.NewStopper(stop.WithTracer(tr)),
		clocks:     map[roachpb.NodeID]clockWithManualSource{},
		transports: map[roachpb.NodeID]*kvserver.RaftTransport{},
		st:         st,
	}
	opts := rpc.DefaultContextOptions()
	opts.Stopper = rttc.stopper
	opts.Settings = st
	rttc.nodeRPCContext = rpc.NewContext(ctx, opts)
	// Ensure that tests using this test context and restart/shut down
	// their servers do not inadvertently start talking to servers from
	// unrelated concurrent tests.
	rttc.nodeRPCContext.StorageClusterID.Set(ctx, uuid.MakeV4())

	// We are sharing the same RPC context for all simulated nodes, so
	// we can't enforce some of the RPC check validation.
	rttc.nodeRPCContext.TestingAllowNamedRPCToAnonymousServer = true

	rttc.gossip = gossip.NewTest(1, rttc.stopper, metric.NewRegistry())

	return rttc
}

func (rttc *raftTransportTestContext) Stop() {
	rttc.stopper.Stop(context.Background())
}

// AddNode registers a node with the cluster. Nodes must be added
// before they can be used in other methods of
// raftTransportTestContext. The node will be gossiped immediately.
func (rttc *raftTransportTestContext) AddNode(nodeID roachpb.NodeID) *kvserver.RaftTransport {
	transport, addr := rttc.AddNodeWithoutGossip(
		nodeID, util.TestAddr, rttc.stopper,
		(*node_rac2.AdmittedPiggybacker)(nil),
		nil, nil,
	)
	rttc.GossipNode(nodeID, addr)
	return transport
}

// AddNodeWithoutGossip registers a node with the cluster. Nodes must
// be added before they can be used in other methods of
// raftTransportTestContext. Unless you are testing the effects of
// delaying gossip, use AddNode instead.
func (rttc *raftTransportTestContext) AddNodeWithoutGossip(
	nodeID roachpb.NodeID,
	addr net.Addr,
	stopper *stop.Stopper,
	piggybacker node_rac2.PiggybackMsgReader,
	piggybackedResponseScheduler kvserver.PiggybackedAdmittedResponseScheduler,
	knobs *kvserver.RaftTransportTestingKnobs,
) (*kvserver.RaftTransport, net.Addr) {
	manual := hlc.NewHybridManualClock()
	clock := hlc.NewClockForTesting(manual)
	rttc.clocks[nodeID] = clockWithManualSource{manual: manual, clock: clock}
	ctx := context.Background()
	grpcServer, err := rpc.NewServer(ctx, rttc.nodeRPCContext)
	require.NoError(rttc.t, err)
	drpcServer, err := rpc.NewDRPCServer(ctx, rttc.nodeRPCContext)
	require.NoError(rttc.t, err)
	transport := kvserver.NewRaftTransport(
		log.MakeTestingAmbientCtxWithNewTracer(),
		rttc.st,
		rttc.stopper,
		clock,
		nodedialer.New(rttc.nodeRPCContext, gossip.AddressResolver(rttc.gossip)),
		grpcServer,
		drpcServer,
		piggybacker,
		piggybackedResponseScheduler,
		knobs,
	)
	rttc.transports[nodeID] = transport
	ln, err := net.Listen(addr.Network(), addr.String())
	if err != nil && rttc.skipOnListenErr {
		skip.IgnoreLintf(rttc.t, "skipping test due to listen error: %s", err)
	}
	require.NoError(rttc.t, err)
	require.NoError(rttc.t, netutil.ServeGRPC(stopper, grpcServer, ln))
	return transport, ln.Addr()
}

// GossipNode gossips the node's address, which is necessary before
// any messages can be sent to it. Normally done automatically by
// AddNode.
func (rttc *raftTransportTestContext) GossipNode(nodeID roachpb.NodeID, addr net.Addr) {
	if err := rttc.gossip.AddInfoProto(gossip.MakeNodeIDKey(nodeID),
		&roachpb.NodeDescriptor{
			NodeID:  nodeID,
			Address: util.MakeUnresolvedAddr(addr.Network(), addr.String()),
		},
		time.Hour); err != nil {
		rttc.t.Fatal(err)
	}
}

// ListenStore registers a store on a node and returns a channel for
// messages sent to that store.
func (rttc *raftTransportTestContext) ListenStore(
	nodeID roachpb.NodeID, storeID roachpb.StoreID,
) channelServer {
	ch := newChannelServer(100, 10*time.Millisecond)
	rttc.transports[nodeID].ListenIncomingRaftMessages(storeID, ch)
	return ch
}

// Send a message. Returns false if the message was dropped.
func (rttc *raftTransportTestContext) Send(
	from, to roachpb.ReplicaDescriptor, rangeID roachpb.RangeID, msg raftpb.Message,
) bool {
	msg.To = raftpb.PeerID(to.ReplicaID)
	msg.From = raftpb.PeerID(from.ReplicaID)
	req := &kvserverpb.RaftMessageRequest{
		RangeID:     rangeID,
		Message:     msg,
		ToReplica:   to,
		FromReplica: from,
	}
	return rttc.transports[from.NodeID].SendAsync(req, rpcbase.DefaultClass)
}

func TestSendAndReceive(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	rttc := newRaftTransportTestContext(t, cluster.MakeTestingClusterSettings())
	defer rttc.Stop()

	// Create several servers, each of which has two stores (A raft
	// node ID addresses a store). Node 1 has stores 1 and 2, node 2 has
	// stores 3 and 4, etc.
	//
	// We suppose that range 1 is replicated across the odd-numbered
	// stores in reverse order to ensure that the various IDs are not
	// equal: replica 1 is store 5, replica 2 is store 3, and replica 3
	// is store 1.
	const numNodes = 3
	const storesPerNode = 2
	nextNodeID := roachpb.NodeID(2)
	nextStoreID := roachpb.StoreID(2)

	// Per-node state.
	transports := map[roachpb.NodeID]*kvserver.RaftTransport{}

	// Per-store state.
	storeNodes := map[roachpb.StoreID]roachpb.NodeID{}
	channels := map[roachpb.StoreID]channelServer{}
	replicaIDs := map[roachpb.StoreID]roachpb.ReplicaID{
		1: 3,
		3: 2,
		5: 1,
	}

	messageTypes := map[raftpb.MessageType]struct{}{
		raftpb.MsgHeartbeat: {},
	}

	for nodeIndex := 0; nodeIndex < numNodes; nodeIndex++ {
		nodeID := nextNodeID
		nextNodeID++
		transports[nodeID] = rttc.AddNode(nodeID)

		for storeIndex := 0; storeIndex < storesPerNode; storeIndex++ {
			storeID := nextStoreID
			nextStoreID++

			storeNodes[storeID] = nodeID

			channels[storeID] = rttc.ListenStore(nodeID, storeID)
		}
	}

	messageTypeCounts := make(map[roachpb.StoreID]map[raftpb.MessageType]int)

	// Each store sends one snapshot and one heartbeat to each store, including
	// itself.
	for toStoreID, toNodeID := range storeNodes {
		if _, ok := messageTypeCounts[toStoreID]; !ok {
			messageTypeCounts[toStoreID] = make(map[raftpb.MessageType]int)
		}

		for fromStoreID, fromNodeID := range storeNodes {
			baseReq := kvserverpb.RaftMessageRequest{
				RangeID: 1,
				Message: raftpb.Message{
					From: raftpb.PeerID(fromStoreID),
					To:   raftpb.PeerID(toStoreID),
				},
				FromReplica: roachpb.ReplicaDescriptor{
					NodeID:  fromNodeID,
					StoreID: fromStoreID,
				},
				ToReplica: roachpb.ReplicaDescriptor{
					NodeID:  toNodeID,
					StoreID: toStoreID,
				},
			}

			for messageType := range messageTypes {
				req := baseReq
				req.Message.Type = messageType

				if !transports[fromNodeID].SendAsync(&req, rpcbase.DefaultClass) {
					t.Errorf("unable to send %s from %d to %d", messageType, fromNodeID, toNodeID)
				}
				messageTypeCounts[toStoreID][messageType]++
			}
		}
	}

	// Read all the messages from the channels. Note that the transport
	// does not guarantee in-order delivery between independent
	// transports, so we just verify that the right number of messages
	// end up in each channel.
	for toStoreID := range storeNodes {
		for len(messageTypeCounts[toStoreID]) > 0 {
			req := <-channels[toStoreID].ch
			if req.Message.To != raftpb.PeerID(toStoreID) {
				t.Errorf("got unexpected message %v on channel %d", req, toStoreID)
			}

			if typeCounts, ok := messageTypeCounts[toStoreID]; ok {
				if _, ok := typeCounts[req.Message.Type]; ok {
					typeCounts[req.Message.Type]--
					if typeCounts[req.Message.Type] == 0 {
						delete(typeCounts, req.Message.Type)
					}
				} else {
					t.Errorf("expected %v to have key %v, but it did not", typeCounts, req.Message.Type)
				}
			} else {
				t.Errorf("expected %v to have key %v, but it did not", messageTypeCounts, toStoreID)
			}
		}

		delete(messageTypeCounts, toStoreID)

		select {
		case req := <-channels[toStoreID].ch:
			t.Errorf("got unexpected message %v on channel %d", req, toStoreID)
		case <-time.After(100 * time.Millisecond):
		}
	}

	if len(messageTypeCounts) > 0 {
		t.Errorf("remaining messages expected: %v", messageTypeCounts)
	}

	// Real raft messages have different node/store/replica IDs.
	// Send a message from replica 2 (on store 3, node 2) to replica 1 (on store 5, node 3)
	fromStoreID := roachpb.StoreID(3)
	toStoreID := roachpb.StoreID(5)
	expReq := &kvserverpb.RaftMessageRequest{
		RangeID: 1,
		Message: raftpb.Message{
			Type: raftpb.MsgApp,
			From: raftpb.PeerID(replicaIDs[fromStoreID]),
			To:   raftpb.PeerID(replicaIDs[toStoreID]),
		},
		FromReplica: roachpb.ReplicaDescriptor{
			NodeID:    storeNodes[fromStoreID],
			StoreID:   fromStoreID,
			ReplicaID: replicaIDs[fromStoreID],
		},
		ToReplica: roachpb.ReplicaDescriptor{
			NodeID:    storeNodes[toStoreID],
			StoreID:   toStoreID,
			ReplicaID: replicaIDs[toStoreID],
		},
	}
	// NB: argument passed to SendAsync is not safe to use after; make a copy.
	expReqCopy := *expReq
	if !transports[storeNodes[fromStoreID]].SendAsync(&expReqCopy, rpcbase.DefaultClass) {
		t.Errorf("unable to send message from %d to %d", fromStoreID, toStoreID)
	}
	// NB: we can't use gogoproto's Equal() function here: it will panic
	// here since it doesn't know about `gogoproto.casttype`.
	if req := <-channels[toStoreID].ch; !reflect.DeepEqual(req, expReq) {
		t.Errorf("got unexpected message %v on channel %d", req, toStoreID)
	}

	select {
	case req := <-channels[toStoreID].ch:
		t.Errorf("got unexpected message %v on channel %d", req, toStoreID)
	default:
	}
}

// TestInOrderDelivery verifies that for a given pair of nodes, raft
// messages are delivered in order.
func TestInOrderDelivery(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	rttc := newRaftTransportTestContext(t, cluster.MakeTestingClusterSettings())
	defer rttc.Stop()

	const numMessages = 100
	serverReplica := roachpb.ReplicaDescriptor{
		NodeID:    2,
		StoreID:   2,
		ReplicaID: 2,
	}
	rttc.AddNode(serverReplica.NodeID)
	serverChannel := rttc.ListenStore(serverReplica.NodeID, serverReplica.StoreID)

	clientReplica := roachpb.ReplicaDescriptor{
		NodeID:    1,
		StoreID:   1,
		ReplicaID: 1,
	}
	rttc.AddNode(clientReplica.NodeID)

	for i := 0; i < numMessages; i++ {
		if !rttc.Send(clientReplica, serverReplica, 1, raftpb.Message{Commit: uint64(i)}) {
			t.Errorf("failed to send message %d", i)
		}
	}

	for i := 0; i < numMessages; i++ {
		req := <-serverChannel.ch
		if req.Message.Commit != uint64(i) {
			t.Errorf("messages out of order: got %d while expecting %d", req.Message.Commit, i)
		}
	}
}

// TestRaftTransportCircuitBreaker verifies that messages will be
// dropped waiting for raft node connection to be established.
func TestRaftTransportCircuitBreaker(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	rttc := newRaftTransportTestContext(t, cluster.MakeTestingClusterSettings())
	defer rttc.Stop()

	serverReplica := roachpb.ReplicaDescriptor{
		NodeID:    2,
		StoreID:   2,
		ReplicaID: 2,
	}
	_, serverAddr := rttc.AddNodeWithoutGossip(
		serverReplica.NodeID,
		util.TestAddr,
		rttc.stopper,
		(*node_rac2.AdmittedPiggybacker)(nil),
		nil, nil,
	)
	serverChannel := rttc.ListenStore(serverReplica.NodeID, serverReplica.StoreID)

	clientReplica := roachpb.ReplicaDescriptor{
		NodeID:    1,
		StoreID:   1,
		ReplicaID: 1,
	}
	_ = rttc.AddNode(clientReplica.NodeID)
	rttc.GossipNode(serverReplica.NodeID, &util.UnresolvedAddr{NetworkField: "invalid", AddressField: "127.0.0.1:999999999"})

	// Sending repeated messages should begin dropping once the circuit breaker
	// does trip.
	testutils.SucceedsSoon(t, func() error {
		if rttc.Send(clientReplica, serverReplica, 1, raftpb.Message{Commit: 1}) {
			return errors.Errorf("expected circuit breaker to trip")
		}
		return nil
	})

	// Now, gossip address of server.
	rttc.GossipNode(serverReplica.NodeID, serverAddr)

	// Keep sending commit=2 until breaker resets and we receive the
	// first instance. It's possible an earlier message for commit=1
	// snuck in.
	testutils.SucceedsSoon(t, func() error {
		if !rttc.Send(clientReplica, serverReplica, 1, raftpb.Message{Commit: 2}) {
			return errors.New("messages still dropped")
		}
		select {
		case req := <-serverChannel.ch:
			if req.Message.Commit == 2 {
				return nil
			}
		default:
		}
		return errors.Errorf("expected message commit=2")
	})
}

// TestRaftTransportIndependentRanges ensures that errors from one
// range do not interfere with messages to another range on the same
// store.
func TestRaftTransportIndependentRanges(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	rttc := newRaftTransportTestContext(t, cluster.MakeTestingClusterSettings())
	defer rttc.Stop()

	server := roachpb.ReplicaDescriptor{
		NodeID:    1,
		StoreID:   1,
		ReplicaID: 1,
	}
	serverTransport := rttc.AddNode(server.NodeID)
	client := roachpb.ReplicaDescriptor{
		NodeID:    2,
		StoreID:   2,
		ReplicaID: 2,
	}
	rttc.AddNode(client.NodeID)

	const numMessages = 50
	channelServer := newChannelServer(numMessages*2, 10*time.Millisecond)
	channelServer.brokenRange = 13
	serverTransport.ListenIncomingRaftMessages(server.StoreID, channelServer)

	for i := 0; i < numMessages; i++ {
		for _, rangeID := range []roachpb.RangeID{1, 13} {
			if !rttc.Send(client, server, rangeID, raftpb.Message{Commit: uint64(i)}) {
				t.Errorf("failed to send message %d to range %s", i, rangeID)
			}
		}
	}
	for i := 0; i < numMessages; i++ {
		select {
		case msg := <-channelServer.ch:
			if msg.Message.Commit != uint64(i) {
				t.Errorf("got message %d while expecting %d", msg.Message.Commit, i)
			}
		case <-time.After(time.Second):
			t.Fatalf("timeout waiting for message %d", i)
		}
	}
}

// TestReopenConnection verifies that if a raft response indicates that the
// expected store isn't present on the node, that the connection gets
// terminated and reopened before retrying, to ensure that the transport
// doesn't get stuck in an endless retry loop against the wrong node.
func TestReopenConnection(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	rttc := newRaftTransportTestContext(t, cluster.MakeTestingClusterSettings())
	defer rttc.Stop()

	// Use a special stopper for the initial server so that we can fully stop it
	// (releasing its bound network address) before the rest of the test pieces.
	serverStopper := stop.NewStopper()
	serverReplica := roachpb.ReplicaDescriptor{
		NodeID:    2,
		StoreID:   2,
		ReplicaID: 2,
	}

	// We're re-listening on an old address here, but the port may be
	// in use. In the very rare case of this happening, skip the test.
	// See: https://github.com/cockroachdb/cockroach/issues/146175.
	rttc.skipOnListenErr = true
	serverTransport, serverAddr :=
		rttc.AddNodeWithoutGossip(
			serverReplica.NodeID,
			util.TestAddr,
			serverStopper,
			(*node_rac2.AdmittedPiggybacker)(nil),
			nil, nil,
		)
	rttc.GossipNode(serverReplica.NodeID, serverAddr)
	rttc.ListenStore(serverReplica.NodeID, serverReplica.StoreID)

	clientReplica := roachpb.ReplicaDescriptor{
		NodeID:    1,
		StoreID:   1,
		ReplicaID: 1,
	}
	rttc.AddNode(clientReplica.NodeID)
	rttc.ListenStore(clientReplica.NodeID, clientReplica.StoreID)

	// Take down the old server and start a new one at the same address.
	serverTransport.StopIncomingRaftMessages(serverReplica.StoreID)
	serverStopper.Stop(context.Background())

	// With the old server down, nothing is listening no the address right now
	// so the circuit breaker should trip.
	testutils.SucceedsSoon(t, func() error {
		if rttc.Send(clientReplica, serverReplica, 1, raftpb.Message{Commit: 1}) {
			return errors.New("expected circuit breaker to trip")
		}
		return nil
	})

	replacementReplica := roachpb.ReplicaDescriptor{
		NodeID:    3,
		StoreID:   3,
		ReplicaID: 3,
	}

	rttc.AddNodeWithoutGossip(
		replacementReplica.NodeID,
		serverAddr,
		rttc.stopper,
		(*node_rac2.AdmittedPiggybacker)(nil),
		nil, nil,
	)
	replacementChannel := rttc.ListenStore(replacementReplica.NodeID, replacementReplica.StoreID)

	// Try sending a message to the old server's store (at the address its
	// replacement is now running at) before its replacement has been gossiped.
	// We just want to ensure that doing so doesn't deadlock the client transport.
	if rttc.Send(clientReplica, serverReplica, 1, raftpb.Message{Commit: 1}) {
		t.Fatal("unexpectedly managed to send to recently downed node")
	}

	// Then, to ensure the client hasn't been deadlocked, add the replacement node
	// to the gossip network and send it a request. Note that this will remove the
	// gossip record for serverReplica.NodeID (n2) since they share the same address.
	// This explains why we we can't really assert whether n2 becomes unreachable or
	// not. If a healthy connection makes it into the rpc context before gossip
	// makes the node unresolvable, it's possible. In the other case, it's not.
	rttc.GossipNode(replacementReplica.NodeID, serverAddr)

	testutils.SucceedsSoon(t, func() error {
		// Sending messages to the old store does not deadlock. See the comment above
		// to understand why we don't check the returned value.
		rttc.Send(clientReplica, serverReplica, 1, raftpb.Message{Commit: 1})
		// It won't be long until we can send to the new replica. The only reason
		// this might fail is that the failed connection is still in the RPC
		// connection pool and we have to wait out a health check interval.
		if !rttc.Send(clientReplica, replacementReplica, 1, raftpb.Message{Commit: 1}) {
			return errors.New("unable to send to replacement replica")
		}
		return nil
	})

	// Send commit=2 to the replacement replica. This should work now because we've
	// just used it successfully above and didn't change anything about the networking.
	if !rttc.Send(clientReplica, replacementReplica, 1, raftpb.Message{Commit: 2}) {
		t.Fatal("replacement node still unhealthy")

	}
	testutils.SucceedsSoon(t, func() error {
		select {
		case req := <-replacementChannel.ch:
			// There could be a few stray messages with `c==1` in the channel,
			// so throw those away.
			if c := req.Message.Commit; c == 2 {
				return nil
			}
		default:
		}
		return errors.New("still waiting")
	})
}

// This test ensures that blocking by a node dialer attempting to dial a
// remote node does not block calls to SendAsync.
func TestSendFailureToConnectDoesNotHangRaft(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	rttc := newRaftTransportTestContext(t, cluster.MakeTestingClusterSettings())
	defer rttc.Stop()

	// Create a single server from which we're going to call send.
	// We'll then set up a bogus target server which will not be serving gRPC
	// and will block during connection setup (leading to blocking in the Dial
	// call). The test ensures that the Send call does not block.
	const rangeID, from, to = 1, 1, 2
	transport := rttc.AddNode(from)
	// Set up a plain old TCP listener that's not going to accept any connecitons
	// which will lead to blocking during dial.
	ln, err := net.Listen("tcp", util.TestAddr.String())
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	rttc.GossipNode(to, ln.Addr())
	// Try to send a message, make sure we don't block waiting to set up the
	// connection.
	transport.SendAsync(&kvserverpb.RaftMessageRequest{
		RangeID: rangeID,
		ToReplica: roachpb.ReplicaDescriptor{
			StoreID:   to,
			NodeID:    to,
			ReplicaID: to,
		},
		FromReplica: roachpb.ReplicaDescriptor{
			StoreID:   from,
			NodeID:    from,
			ReplicaID: from,
		},
		Message: raftpb.Message{To: to, From: from},
	}, rpcbase.DefaultClass)
}

// TestRaftTransportClockPropagation verifies that hlc clock timestamps are
// propagated across the RaftTransport.
func TestRaftTransportClockPropagation(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	rttc := newRaftTransportTestContext(t, cluster.MakeTestingClusterSettings())
	defer rttc.Stop()

	serverReplica := roachpb.ReplicaDescriptor{
		NodeID:    2,
		StoreID:   2,
		ReplicaID: 2,
	}
	rttc.AddNode(serverReplica.NodeID)
	serverChannel := rttc.ListenStore(serverReplica.NodeID, serverReplica.StoreID)

	clientReplica := roachpb.ReplicaDescriptor{
		NodeID:    1,
		StoreID:   1,
		ReplicaID: 1,
	}
	rttc.AddNode(clientReplica.NodeID)

	// Pause all clocks.
	for _, c := range rttc.clocks {
		c.manual.Pause()
	}

	// Advance the client's clock beyond the server's.
	serverNow := rttc.clocks[serverReplica.NodeID].clock.Now()
	var clientNow hlc.Timestamp
	for clientNow.LessEq(serverNow) {
		rttc.clocks[clientReplica.NodeID].manual.Increment(1000000)
		clientNow = rttc.clocks[clientReplica.NodeID].clock.Now()
	}

	// Send a message from the client to the server.
	sent := rttc.Send(clientReplica, serverReplica, 1 /* rangeID */, raftpb.Message{Commit: 10})
	require.True(t, sent, "failed to send message")
	req := <-serverChannel.ch
	require.Equal(t, uint64(10), req.Message.Commit)

	// The server's clock should have been updated to the client's time.
	serverNow = rttc.clocks[serverReplica.NodeID].clock.Now()
	require.False(t, serverNow.Less(clientNow))
}
