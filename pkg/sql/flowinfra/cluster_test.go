// Copyright 2016 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package flowinfra_test

import (
	"context"
	gosql "database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	_ "github.com/cockroachdb/cockroach/pkg/ccl/kvccl/kvtenantccl"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/concurrency/isolation"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvserverbase"
	"github.com/cockroachdb/cockroach/pkg/multitenant/tenantcapabilitiespb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/rpc/rpcbase"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/desctestutils"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/fetchpb"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/execversion"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlstats"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/stretchr/testify/require"
)

func runTestClusterFlow(
	t *testing.T,
	servers []serverutils.ApplicationLayerInterface,
	conns []*gosql.DB,
	clients []execinfrapb.RPCDistSQLClient,
) {
	ctx := context.Background()
	const numRows = 100
	kvDB, codec := servers[0].DB(), servers[0].Codec()

	sumDigitsFn := func(row int) tree.Datum {
		sum := 0
		for row > 0 {
			sum += row % 10
			row /= 10
		}
		return tree.NewDInt(tree.DInt(sum))
	}

	sqlutils.CreateTable(t, conns[0], "t",
		"num INT PRIMARY KEY, digitsum INT, numstr STRING, INDEX s (digitsum)",
		numRows,
		sqlutils.ToRowFn(sqlutils.RowIdxFn, sumDigitsFn, sqlutils.RowEnglishFn))

	desc := desctestutils.TestingGetPublicTableDescriptor(kvDB, codec, sqlutils.TestDB, "t")
	makeIndexSpan := func(start, end int) roachpb.Span {
		var span roachpb.Span
		prefix := roachpb.Key(rowenc.MakeIndexKeyPrefix(codec, desc.GetID(), desc.PublicNonPrimaryIndexes()[0].GetID()))
		span.Key = append(prefix, encoding.EncodeVarintAscending(nil, int64(start))...)
		span.EndKey = append(span.EndKey, prefix...)
		span.EndKey = append(span.EndKey, encoding.EncodeVarintAscending(nil, int64(end))...)
		return span
	}

	// Set up table readers on three hosts feeding data into a join reader on
	// the third host. This is a basic test for the distributed flow
	// infrastructure, including local and remote streams.
	//
	// Note that the ranges won't necessarily be local to the table readers, but
	// that doesn't matter for the purposes of this test.

	now := servers[0].Clock().NowAsClockTimestamp()
	txnProto := roachpb.MakeTransaction(
		"cluster-test",
		nil, // baseKey
		isolation.Serializable,
		roachpb.NormalUserPriority,
		now.ToTimestamp(),
		0, // maxOffsetNs
		int32(servers[0].SQLInstanceID()),
		0,
		false, // omitInRangefeeds
	)
	txn := kv.NewTxnFromProto(ctx, kvDB, roachpb.NodeID(servers[0].SQLInstanceID()), now, kv.RootTxn, &txnProto)
	leafInputState, err := txn.GetLeafTxnInputState(ctx, nil /* readsTree */)
	require.NoError(t, err)

	var spec fetchpb.IndexFetchSpec
	if err := rowenc.InitIndexFetchSpec(&spec, codec, desc, desc.ActiveIndexes()[1], []descpb.ColumnID{1, 2}); err != nil {
		t.Fatal(err)
	}

	tr1 := execinfrapb.TableReaderSpec{
		FetchSpec: spec,
		Spans:     []roachpb.Span{makeIndexSpan(0, 8)},
	}

	tr2 := execinfrapb.TableReaderSpec{
		FetchSpec: spec,
		Spans:     []roachpb.Span{makeIndexSpan(8, 12)},
	}

	tr3 := execinfrapb.TableReaderSpec{
		FetchSpec: spec,
		Spans:     []roachpb.Span{makeIndexSpan(12, 100)},
	}

	fid := execinfrapb.FlowID{UUID: uuid.MakeV4()}

	req1 := &execinfrapb.SetupFlowRequest{
		Version:           execversion.Latest,
		LeafTxnInputState: leafInputState,
		Flow: execinfrapb.FlowSpec{
			FlowID: fid,
			Processors: []execinfrapb.ProcessorSpec{{
				ProcessorID: 1,
				Core:        execinfrapb.ProcessorCoreUnion{TableReader: &tr1},
				Output: []execinfrapb.OutputRouterSpec{{
					Type: execinfrapb.OutputRouterSpec_PASS_THROUGH,
					Streams: []execinfrapb.StreamEndpointSpec{
						{Type: execinfrapb.StreamEndpointSpec_REMOTE, StreamID: 0, TargetNodeID: servers[2].SQLInstanceID()},
					},
				}},
				ResultTypes: types.TwoIntCols,
			}},
		},
	}

	req2 := &execinfrapb.SetupFlowRequest{
		Version:           execversion.Latest,
		LeafTxnInputState: leafInputState,
		Flow: execinfrapb.FlowSpec{
			FlowID: fid,
			Processors: []execinfrapb.ProcessorSpec{{
				ProcessorID: 2,
				Core:        execinfrapb.ProcessorCoreUnion{TableReader: &tr2},
				Output: []execinfrapb.OutputRouterSpec{{
					Type: execinfrapb.OutputRouterSpec_PASS_THROUGH,
					Streams: []execinfrapb.StreamEndpointSpec{
						{Type: execinfrapb.StreamEndpointSpec_REMOTE, StreamID: 1, TargetNodeID: servers[2].SQLInstanceID()},
					},
				}},
				ResultTypes: types.TwoIntCols,
			}},
		},
	}

	var pkSpec fetchpb.IndexFetchSpec
	if err := rowenc.InitIndexFetchSpec(
		&pkSpec, codec, desc, desc.GetPrimaryIndex(), []descpb.ColumnID{1, 2, 3},
	); err != nil {
		t.Fatal(err)
	}

	req3 := &execinfrapb.SetupFlowRequest{
		Version:           execversion.Latest,
		LeafTxnInputState: leafInputState,
		Flow: execinfrapb.FlowSpec{
			FlowID: fid,
			Processors: []execinfrapb.ProcessorSpec{
				{
					ProcessorID: 3,
					Core:        execinfrapb.ProcessorCoreUnion{TableReader: &tr3},
					Output: []execinfrapb.OutputRouterSpec{{
						Type: execinfrapb.OutputRouterSpec_PASS_THROUGH,
						Streams: []execinfrapb.StreamEndpointSpec{
							{Type: execinfrapb.StreamEndpointSpec_LOCAL, StreamID: 2},
						},
					}},
					ResultTypes: types.TwoIntCols,
				},
				{
					ProcessorID: 4,
					Input: []execinfrapb.InputSyncSpec{{
						Type: execinfrapb.InputSyncSpec_ORDERED,
						Ordering: execinfrapb.Ordering{Columns: []execinfrapb.Ordering_Column{
							{ColIdx: 1, Direction: execinfrapb.Ordering_Column_ASC}}},
						Streams: []execinfrapb.StreamEndpointSpec{
							{Type: execinfrapb.StreamEndpointSpec_REMOTE, StreamID: 0},
							{Type: execinfrapb.StreamEndpointSpec_REMOTE, StreamID: 1},
							{Type: execinfrapb.StreamEndpointSpec_LOCAL, StreamID: 2},
						},
						ColumnTypes: types.TwoIntCols,
					}},
					Core: execinfrapb.ProcessorCoreUnion{JoinReader: &execinfrapb.JoinReaderSpec{
						FetchSpec:        pkSpec,
						MaintainOrdering: true,
					}},
					Post: execinfrapb.PostProcessSpec{
						Projection:    true,
						OutputColumns: []uint32{2},
					},
					Output: []execinfrapb.OutputRouterSpec{{
						Type:    execinfrapb.OutputRouterSpec_PASS_THROUGH,
						Streams: []execinfrapb.StreamEndpointSpec{{Type: execinfrapb.StreamEndpointSpec_SYNC_RESPONSE}},
					}},
					ResultTypes: []*types.T{types.String},
				},
			},
		},
	}

	setupRemoteFlow := func(nodeIdx int, req *execinfrapb.SetupFlowRequest) {
		log.Infof(ctx, "Setting up flow on %d", nodeIdx)
		if resp, err := clients[nodeIdx].SetupFlow(ctx, req); err != nil {
			t.Fatal(err)
		} else if resp.Error != nil {
			t.Fatal(resp.Error)
		}
	}

	setupRemoteFlow(0 /* nodeIdx */, req1)
	setupRemoteFlow(1 /* nodeIdx */, req2)

	log.Infof(ctx, "Running local sync flow on 2")
	rows, err := runLocalFlowTenant(ctx, servers[2], req3)
	if err != nil {
		t.Fatal(err)
	}
	// The result should be all the numbers in string form, ordered by the
	// digit sum (and then by number).
	var results []string
	for sum := 1; sum <= 50; sum++ {
		for i := 1; i <= numRows; i++ {
			if int(tree.MustBeDInt(sumDigitsFn(i))) == sum {
				results = append(results, fmt.Sprintf("['%s']", sqlutils.IntToEnglish(i)))
			}
		}
	}
	expected := strings.Join(results, " ")
	expected = "[" + expected + "]"
	if rowStr := rows.String([]*types.T{types.String}); rowStr != expected {
		t.Errorf("Result: %s\n Expected: %s\n", rowStr, expected)
	}
}

func TestClusterFlow(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	const numNodes = 3

	args := base.TestClusterArgs{ReplicationMode: base.ReplicationManual}
	tc := serverutils.StartCluster(t, numNodes, args)
	defer tc.Stopper().Stop(context.Background())

	servers := make([]serverutils.ApplicationLayerInterface, numNodes)
	conns := make([]*gosql.DB, numNodes)
	clients := make([]execinfrapb.RPCDistSQLClient, numNodes)
	for i := 0; i < numNodes; i++ {
		s := tc.Server(i).ApplicationLayer()
		servers[i] = s
		conns[i] = s.SQLConn(t)
		conn := s.RPCClientConn(t, username.RootUserName())
		clients[i] = conn.NewDistSQLClient()
	}

	runTestClusterFlow(t, servers, conns, clients)
}

func TestTenantClusterFlow(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	const numPods = 3

	args := base.TestClusterArgs{ReplicationMode: base.ReplicationManual}
	args.ServerArgs.DefaultTestTenant = base.TestControlsTenantsExplicitly
	tc := serverutils.StartCluster(t, 1, args)
	defer tc.Stopper().Stop(ctx)

	pods := make([]serverutils.ApplicationLayerInterface, numPods)
	podConns := make([]*gosql.DB, numPods)
	clients := make([]execinfrapb.RPCDistSQLClient, numPods)
	tenantID := serverutils.TestTenantID()
	for i := 0; i < numPods; i++ {
		pods[i], podConns[i] = serverutils.StartTenant(t, tc.Server(0), base.TestTenantArgs{
			TenantID: tenantID,
			TestingKnobs: base.TestingKnobs{
				SQLStatsKnobs: sqlstats.CreateTestingKnobs(),
			},
		})
		defer podConns[i].Close()
		pod := pods[i]
		conn, err := pod.RPCContext().GRPCDialPod(pod.RPCAddr(), pod.SQLInstanceID(), pod.Locality(), rpcbase.DefaultClass).Connect(ctx)
		if err != nil {
			t.Fatal(err)
		}
		clients[i] = execinfrapb.NewGRPCDistSQLClientAdapter(conn)
	}

	runTestClusterFlow(t, pods, podConns, clients)
}

// TestLimitedBufferingDeadlock sets up a scenario which leads to deadlock if
// a single consumer can block the entire router (#17097).
func TestLimitedBufferingDeadlock(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	tc := serverutils.StartCluster(t, 1, base.TestClusterArgs{})
	defer tc.Stopper().Stop(context.Background())

	// Set up the following network - a simplification of the one described in
	// #17097 (the numbers on the streams are the StreamIDs in the spec below):
	//
	//
	//  +----------+        +----------+
	//  |  Values  |        |  Values  |
	//  +----------+        +-+------+-+
	//         |              | hash |
	//         |              +------+
	//       1 |               |    |
	//         |             2 |    |
	//         v               v    |
	//      +-------------------+   |
	//      |     MergeJoin     |   |
	//      +-------------------+   | 3
	//                |             |
	//                |             |
	//              4 |             |
	//                |             |
	//                v             v
	//              +-----------------+
	//              |  ordered sync   |
	//            +-+-----------------+-+
	//            |       Response      |
	//            +---------------------+
	//
	//
	// This is not something we would end up with from a real SQL query but it's
	// simple and exposes the deadlock: if the hash router outputs a large set of
	// consecutive rows to the left side (which we can ensure by having a bunch of
	// identical rows), the MergeJoiner would be blocked trying to write to the
	// ordered sync, which in turn would block because it's trying to read from
	// the other stream from the hash router. The other stream is blocked because
	// the hash router is already in the process of pushing a row, and we have a
	// deadlock.
	//
	// We set up the left Values processor to emit rows with consecutive values,
	// and the right Values processor to emit groups of identical rows for each
	// value.

	// All our rows have a single integer column.
	typs := []*types.T{types.Int}

	// The left values rows are consecutive values.
	leftRows := make(rowenc.EncDatumRows, 20)
	for i := range leftRows {
		leftRows[i] = rowenc.EncDatumRow{
			rowenc.DatumToEncDatum(typs[0], tree.NewDInt(tree.DInt(i))),
		}
	}
	leftValuesSpec, err := execinfra.GenerateValuesSpec(typs, leftRows)
	if err != nil {
		t.Fatal(err)
	}

	// The right values rows have groups of identical values (ensuring that large
	// groups of rows go to the same hash bucket).
	rightRows := make(rowenc.EncDatumRows, 0)
	for i := 0; i < 20; i++ {
		for j := 1; j <= 4*execinfra.RowChannelBufSize; j++ {
			rightRows = append(rightRows, rowenc.EncDatumRow{
				rowenc.DatumToEncDatum(typs[0], tree.NewDInt(tree.DInt(i))),
			})
		}
	}

	rightValuesSpec, err := execinfra.GenerateValuesSpec(typs, rightRows)
	if err != nil {
		t.Fatal(err)
	}

	joinerSpec := execinfrapb.MergeJoinerSpec{
		LeftOrdering: execinfrapb.Ordering{
			Columns: []execinfrapb.Ordering_Column{{ColIdx: 0, Direction: execinfrapb.Ordering_Column_ASC}},
		},
		RightOrdering: execinfrapb.Ordering{
			Columns: []execinfrapb.Ordering_Column{{ColIdx: 0, Direction: execinfrapb.Ordering_Column_ASC}},
		},
		Type: descpb.InnerJoin,
	}

	now := tc.Server(0).Clock().NowAsClockTimestamp()
	txnProto := roachpb.MakeTransaction(
		"deadlock-test",
		nil, // baseKey
		isolation.Serializable,
		roachpb.NormalUserPriority,
		now.ToTimestamp(),
		0, // maxOffsetNs
		int32(tc.Server(0).SQLInstanceID()),
		0,
		false, // omitInRangefeeds
	)
	txn := kv.NewTxnFromProto(
		context.Background(), tc.Server(0).DB(), tc.Server(0).NodeID(),
		now, kv.RootTxn, &txnProto)
	leafInputState, err := txn.GetLeafTxnInputState(context.Background(), nil /* readsTree */)
	require.NoError(t, err)

	req := execinfrapb.SetupFlowRequest{
		Version:           execversion.Latest,
		LeafTxnInputState: leafInputState,
		Flow: execinfrapb.FlowSpec{
			FlowID: execinfrapb.FlowID{UUID: uuid.MakeV4()},
			// The left-hand Values processor in the diagram above.
			Processors: []execinfrapb.ProcessorSpec{
				{
					Core: execinfrapb.ProcessorCoreUnion{Values: &leftValuesSpec},
					Output: []execinfrapb.OutputRouterSpec{{
						Type: execinfrapb.OutputRouterSpec_PASS_THROUGH,
						Streams: []execinfrapb.StreamEndpointSpec{
							{Type: execinfrapb.StreamEndpointSpec_LOCAL, StreamID: 1},
						},
					}},
					ResultTypes: typs,
				},
				// The right-hand Values processor in the diagram above.
				{
					Core: execinfrapb.ProcessorCoreUnion{Values: &rightValuesSpec},
					Output: []execinfrapb.OutputRouterSpec{{
						Type:        execinfrapb.OutputRouterSpec_BY_HASH,
						HashColumns: []uint32{0},
						Streams: []execinfrapb.StreamEndpointSpec{
							{Type: execinfrapb.StreamEndpointSpec_LOCAL, StreamID: 2},
							{Type: execinfrapb.StreamEndpointSpec_LOCAL, StreamID: 3},
						},
					}},
					ResultTypes: typs,
				},
				// The MergeJoin processor.
				{
					Input: []execinfrapb.InputSyncSpec{
						{
							Type:        execinfrapb.InputSyncSpec_PARALLEL_UNORDERED,
							Streams:     []execinfrapb.StreamEndpointSpec{{Type: execinfrapb.StreamEndpointSpec_LOCAL, StreamID: 1}},
							ColumnTypes: typs,
						},
						{
							Type:        execinfrapb.InputSyncSpec_PARALLEL_UNORDERED,
							Streams:     []execinfrapb.StreamEndpointSpec{{Type: execinfrapb.StreamEndpointSpec_LOCAL, StreamID: 2}},
							ColumnTypes: typs,
						},
					},
					Core: execinfrapb.ProcessorCoreUnion{MergeJoiner: &joinerSpec},
					Post: execinfrapb.PostProcessSpec{
						// Output only one (the left) column.
						Projection:    true,
						OutputColumns: []uint32{0},
					},
					Output: []execinfrapb.OutputRouterSpec{{
						Type: execinfrapb.OutputRouterSpec_PASS_THROUGH,
						Streams: []execinfrapb.StreamEndpointSpec{
							{Type: execinfrapb.StreamEndpointSpec_LOCAL, StreamID: 4},
						},
					}},
					ResultTypes: typs,
				},
				// The final (Response) processor.
				{
					Input: []execinfrapb.InputSyncSpec{{
						Type: execinfrapb.InputSyncSpec_ORDERED,
						Ordering: execinfrapb.Ordering{Columns: []execinfrapb.Ordering_Column{
							{ColIdx: 0, Direction: execinfrapb.Ordering_Column_ASC}}},
						Streams: []execinfrapb.StreamEndpointSpec{
							{Type: execinfrapb.StreamEndpointSpec_LOCAL, StreamID: 4},
							{Type: execinfrapb.StreamEndpointSpec_LOCAL, StreamID: 3},
						},
						ColumnTypes: typs,
					}},
					Core: execinfrapb.ProcessorCoreUnion{Noop: &execinfrapb.NoopCoreSpec{}},
					Output: []execinfrapb.OutputRouterSpec{{
						Type:    execinfrapb.OutputRouterSpec_PASS_THROUGH,
						Streams: []execinfrapb.StreamEndpointSpec{{Type: execinfrapb.StreamEndpointSpec_SYNC_RESPONSE}},
					}},
					ResultTypes: typs,
				},
			},
		},
	}
	rows, err := runLocalFlow(context.Background(), tc.Server(0), &req)
	require.NoError(t, err)
	require.Equal(t, rightRows.String(typs), rows.String(typs))
}

// Test that DistSQL reads fill the BatchRequest.Header.GatewayNodeID field with
// the ID of the gateway (as opposed to the ID of the node that created the
// batch). Important to lease follow-the-workload transfers.
func TestDistSQLReadsFillGatewayID(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// We're going to distribute a table and then read it, and we'll expect all
	// the ScanRequests (produced by the different nodes) to identify the one and
	// only gateway.

	var foundReq int64 // written atomically
	var expectedGateway roachpb.NodeID

	var tableID atomic.Value
	tc := serverutils.StartCluster(t, 3, /* numNodes */
		base.TestClusterArgs{
			ReplicationMode: base.ReplicationManual,
			ServerArgs: base.TestServerArgs{
				UseDatabase:       "test",
				DefaultTestTenant: base.TestIsForStuffThatShouldWorkWithSecondaryTenantsButDoesntYet(109392),
				Knobs: base.TestingKnobs{Store: &kvserver.StoreTestingKnobs{
					EvalKnobs: kvserverbase.BatchEvalTestingKnobs{
						TestingEvalFilter: func(filterArgs kvserverbase.FilterArgs) *kvpb.Error {
							scanReq, ok := filterArgs.Req.(*kvpb.ScanRequest)
							if !ok {
								return nil
							}
							if !strings.HasPrefix(
								scanReq.Key.String(),
								fmt.Sprintf("/Table/%d/1", tableID.Load()),
							) {
								return nil
							}

							atomic.StoreInt64(&foundReq, 1)
							if gw := filterArgs.Hdr.GatewayNodeID; gw != expectedGateway {
								return kvpb.NewErrorf(
									"expected all scans to have gateway 3, found: %d",
									gw)
							}
							return nil
						},
					}},
				},
			},
		})
	defer tc.Stopper().Stop(context.Background())

	db := tc.ServerConn(0)
	sqlutils.CreateTable(t, db, "t",
		"num INT PRIMARY KEY",
		0, /* numRows */
		sqlutils.ToRowFn(sqlutils.RowIdxFn))
	tableID.Store(sqlutils.QueryTableID(
		t, db, sqlutils.TestDB, "public", "t",
	))

	if _, err := db.Exec(`
ALTER TABLE t SPLIT AT VALUES (1), (2), (3);
ALTER TABLE t EXPERIMENTAL_RELOCATE VALUES (ARRAY[2], 1), (ARRAY[1], 2), (ARRAY[3], 3);
`); err != nil {
		t.Fatal(err)
	}

	expectedGateway = tc.Server(2).NodeID()
	if _, err := tc.ServerConn(2).Exec("SELECT * FROM t"); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&foundReq) != 1 {
		t.Fatal("TestingEvalFilter failed to find any requests")
	}
}

// Test that we can evaluate built-in functions that use the txn on remote
// nodes. We have a bug where the EvalCtx.Txn field was only correctly populated
// on the gateway.
func TestEvalCtxTxnOnRemoteNodes(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	ctx := context.Background()

	tc := serverutils.StartCluster(t, 2, /* numNodes */
		base.TestClusterArgs{
			ReplicationMode: base.ReplicationManual,
			ServerArgs: base.TestServerArgs{
				UseDatabase: "test",
			},
		})
	defer tc.Stopper().Stop(ctx)

	if tc.DefaultTenantDeploymentMode().IsExternal() {
		tc.GrantTenantCapabilities(
			ctx, t, serverutils.TestTenantID(),
			map[tenantcapabilitiespb.ID]string{tenantcapabilitiespb.CanAdminRelocateRange: "true"})
	}

	db := tc.ServerConn(0)
	sqlutils.CreateTable(t, db, "t",
		"num INT PRIMARY KEY",
		1, /* numRows */
		sqlutils.ToRowFn(sqlutils.RowIdxFn))

	// Relocate the table to a remote node. We use SucceedsSoon since in very
	// rare circumstances the relocation query can result in an error (e.g.
	// "cannot up-replicate to s2; missing gossiped StoreDescriptor") which
	// shouldn't fail the test.
	testutils.SucceedsSoon(t, func() error {
		_, err := db.Exec("ALTER TABLE t EXPERIMENTAL_RELOCATE VALUES (ARRAY[2], 1)")
		return err
	})

	testutils.RunTrueAndFalse(t, "vectorize", func(t *testing.T, vectorize bool) {
		// We're going to use the first node as the gateway and expect everything to
		// be planned remotely.
		db := tc.ServerConn(0)
		var opt string
		if vectorize {
			opt = "experimental_always"
		} else {
			opt = "off"
		}
		_, err := db.Exec(fmt.Sprintf("set vectorize=%s", opt))
		require.NoError(t, err)

		// Query using a builtin function which uses the transaction (for example,
		// cluster_logical_timestamp()) and expect not to crash.
		_, err = db.Exec("SELECT cluster_logical_timestamp() FROM t")
		require.NoError(t, err)

		// Query again just in case the previous query executed on the gateway
		// because the we didn't have a leaseholder in the cache and we fooled
		// ourselves.
		_, err = db.Exec("SELECT cluster_logical_timestamp() FROM t")
		require.NoError(t, err)
	})
}

// BenchmarkInfrastructure sets up a flow that doesn't use KV at all and runs it
// repeatedly. The intention is to profile the distsql infrastructure itself.
func BenchmarkInfrastructure(b *testing.B) {
	defer leaktest.AfterTest(b)()
	defer log.Scope(b).Close(b)

	args := base.TestClusterArgs{ReplicationMode: base.ReplicationManual}
	tc := serverutils.StartCluster(b, 3, args)
	defer tc.Stopper().Stop(context.Background())

	for _, numNodes := range []int{1, 3} {
		b.Run(fmt.Sprintf("n%d", numNodes), func(b *testing.B) {
			for _, numRows := range []int{1, 100, 10000} {
				b.Run(fmt.Sprintf("r%d", numRows), func(b *testing.B) {
					// Generate some data sets, consisting of rows with three values; the
					// first value is increasing.
					rng, _ := randutil.NewTestRand()
					lastVal := 1
					valSpecs := make([]execinfrapb.ValuesCoreSpec, numNodes)
					for i := range valSpecs {
						rows := make(rowenc.EncDatumRows, numRows)
						for j := 0; j < numRows; j++ {
							row := make(rowenc.EncDatumRow, 3)
							lastVal += rng.Intn(10)
							row[0] = rowenc.DatumToEncDatum(types.Int, tree.NewDInt(tree.DInt(lastVal)))
							row[1] = rowenc.DatumToEncDatum(types.Int, tree.NewDInt(tree.DInt(rng.Intn(100000))))
							row[2] = rowenc.DatumToEncDatum(types.Int, tree.NewDInt(tree.DInt(rng.Intn(100000))))
							rows[j] = row
						}
						valSpec, err := execinfra.GenerateValuesSpec(types.ThreeIntCols, rows)
						if err != nil {
							b.Fatal(err)
						}
						valSpecs[i] = valSpec
					}

					// Set up the following network:
					//
					//         Node 0              Node 1          ...
					//
					//      +----------+        +----------+
					//      |  Values  |        |  Values  |       ...
					//      +----------+        +----------+
					//          |                  |
					//          |       stream 1   |
					// stream 0 |   /-------------/                ...
					//          |  |
					//          v  v
					//     +---------------+
					//     | ordered* sync |
					//  +--+---------------+--+
					//  |        No-op        |
					//  +---------------------+
					//
					// *unordered if we have a single node.

					reqs := make([]execinfrapb.SetupFlowRequest, numNodes)
					streamType := func(i int) execinfrapb.StreamEndpointSpec_Type {
						if i == 0 {
							return execinfrapb.StreamEndpointSpec_LOCAL
						}
						return execinfrapb.StreamEndpointSpec_REMOTE
					}
					now := tc.Server(0).Clock().NowAsClockTimestamp()
					txnProto := roachpb.MakeTransaction(
						"cluster-test",
						nil, // baseKey
						isolation.Serializable,
						roachpb.NormalUserPriority,
						now.ToTimestamp(),
						0, // maxOffsetNs
						int32(tc.Server(0).SQLInstanceID()),
						0,
						false, // omitInRangefeeds
					)
					txn := kv.NewTxnFromProto(
						context.Background(), tc.Server(0).DB(), tc.Server(0).NodeID(),
						now, kv.RootTxn, &txnProto)
					leafInputState, err := txn.GetLeafTxnInputState(context.Background(), nil /* readsTree */)
					require.NoError(b, err)
					for i := range reqs {
						reqs[i] = execinfrapb.SetupFlowRequest{
							Version:           execversion.Latest,
							LeafTxnInputState: leafInputState,
							Flow: execinfrapb.FlowSpec{
								Processors: []execinfrapb.ProcessorSpec{{
									Core: execinfrapb.ProcessorCoreUnion{Values: &valSpecs[i]},
									Output: []execinfrapb.OutputRouterSpec{{
										Type: execinfrapb.OutputRouterSpec_PASS_THROUGH,
										Streams: []execinfrapb.StreamEndpointSpec{
											{Type: streamType(i), StreamID: execinfrapb.StreamID(i), TargetNodeID: base.SQLInstanceID(tc.Server(0).NodeID())},
										},
									}},
									ResultTypes: types.ThreeIntCols,
								}},
							},
						}
					}

					reqs[0].Flow.Processors[0].Output[0].Streams[0] = execinfrapb.StreamEndpointSpec{
						Type:     execinfrapb.StreamEndpointSpec_LOCAL,
						StreamID: 0,
					}
					inStreams := make([]execinfrapb.StreamEndpointSpec, numNodes)
					for i := range inStreams {
						inStreams[i].Type = streamType(i)
						inStreams[i].StreamID = execinfrapb.StreamID(i)
					}

					lastProc := execinfrapb.ProcessorSpec{
						Input: []execinfrapb.InputSyncSpec{{
							Type: execinfrapb.InputSyncSpec_ORDERED,
							Ordering: execinfrapb.Ordering{Columns: []execinfrapb.Ordering_Column{
								{ColIdx: 0, Direction: execinfrapb.Ordering_Column_ASC}}},
							Streams:     inStreams,
							ColumnTypes: types.ThreeIntCols,
						}},
						Core: execinfrapb.ProcessorCoreUnion{Noop: &execinfrapb.NoopCoreSpec{}},
						Output: []execinfrapb.OutputRouterSpec{{
							Type:    execinfrapb.OutputRouterSpec_PASS_THROUGH,
							Streams: []execinfrapb.StreamEndpointSpec{{Type: execinfrapb.StreamEndpointSpec_SYNC_RESPONSE}},
						}},
						ResultTypes: types.ThreeIntCols,
					}
					if numNodes == 1 {
						lastProc.Input[0].Type = execinfrapb.InputSyncSpec_PARALLEL_UNORDERED
						lastProc.Input[0].Ordering = execinfrapb.Ordering{}
					}
					reqs[0].Flow.Processors = append(reqs[0].Flow.Processors, lastProc)

					var clients []execinfrapb.RPCDistSQLClient

					for i := 0; i < numNodes; i++ {
						s := tc.Server(i)
						conn := s.RPCClientConn(b, username.RootUserName())
						clients = append(clients, conn.NewDistSQLClient())
					}

					b.ResetTimer()
					for repeat := 0; repeat < b.N; repeat++ {
						fid := execinfrapb.FlowID{UUID: uuid.MakeV4()}
						for i := range reqs {
							reqs[i].Flow.FlowID = fid
						}

						for i := 1; i < numNodes; i++ {
							if resp, err := clients[i].SetupFlow(context.Background(), &reqs[i]); err != nil {
								b.Fatal(err)
							} else if resp.Error != nil {
								b.Fatal(resp.Error)
							}
						}
						rows, err := runLocalFlow(context.Background(), tc.Server(0), &reqs[0])
						if err != nil {
							b.Fatal(err)
						}
						if len(rows) != numNodes*numRows {
							b.Errorf("got %d rows, expected %d", len(rows), numNodes*numRows)
						}
						var a tree.DatumAlloc
						for i := range rows {
							if err := rows[i][0].EnsureDecoded(types.Int, &a); err != nil {
								b.Fatal(err)
							}
							if i > 0 {
								last := *rows[i-1][0].Datum.(*tree.DInt)
								curr := *rows[i][0].Datum.(*tree.DInt)
								if last > curr {
									b.Errorf("rows not ordered correctly (%d after %d, row %d)", curr, last, i)
									break
								}
							}
						}
					}
				})
			}
		})
	}
}
