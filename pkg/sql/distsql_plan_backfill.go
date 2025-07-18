// Copyright 2016 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sql

import (
	"context"
	"time"
	"unsafe"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/physicalplan"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/interval"
	"github.com/cockroachdb/errors"
)

func initColumnBackfillerSpec(
	tbl catalog.TableDescriptor,
	duration time.Duration,
	chunkSize int64,
	updateChunkSizeThresholdBytes uint64,
	readAsOf hlc.Timestamp,
) execinfrapb.BackfillerSpec {
	return execinfrapb.BackfillerSpec{
		Table:                         *tbl.TableDesc(),
		Duration:                      duration,
		ChunkSize:                     chunkSize,
		UpdateChunkSizeThresholdBytes: updateChunkSizeThresholdBytes,
		ReadAsOf:                      readAsOf,
		Type:                          execinfrapb.BackfillerSpec_Column,
	}
}

func initIndexBackfillerSpec(
	desc descpb.TableDescriptor,
	writeAsOf hlc.Timestamp,
	writeAtBatchTimestamp bool,
	chunkSize int64,
	indexesToBackfill []descpb.IndexID,
	sourceIndexID descpb.IndexID,
) execinfrapb.BackfillerSpec {
	return execinfrapb.BackfillerSpec{
		Table:                 desc,
		WriteAsOf:             writeAsOf,
		WriteAtBatchTimestamp: writeAtBatchTimestamp,
		Type:                  execinfrapb.BackfillerSpec_Index,
		ChunkSize:             chunkSize,
		IndexesToBackfill:     indexesToBackfill,
		SourceIndexID:         sourceIndexID,
	}
}

func initIndexBackfillMergerSpec(
	desc descpb.TableDescriptor,
	addedIndexes []descpb.IndexID,
	temporaryIndexes []descpb.IndexID,
	mergeTimestamp hlc.Timestamp,
) execinfrapb.IndexBackfillMergerSpec {
	return execinfrapb.IndexBackfillMergerSpec{
		Table:            desc,
		AddedIndexes:     addedIndexes,
		TemporaryIndexes: temporaryIndexes,
		MergeTimestamp:   mergeTimestamp,
	}
}

var initialSplitsPerProcessor = settings.RegisterIntSetting(
	settings.ApplicationLevel,
	"bulkio.index_backfill.initial_splits_per_processor",
	"number of initial splits each index backfill processor with enough data will create",
	3,
	settings.NonNegativeInt,
)

// createBackfiller generates a plan consisting of index/column backfiller
// processors, one for each node that has spans that we are reading. The plan is
// finalized.
func (dsp *DistSQLPlanner) createBackfillerPhysicalPlan(
	ctx context.Context, planCtx *PlanningCtx, spec execinfrapb.BackfillerSpec, spans []roachpb.Span,
) (*PhysicalPlan, error) {
	spanPartitions, err := dsp.PartitionSpans(ctx, planCtx, spans, PartitionSpansBoundDefault)
	if err != nil {
		return nil, err
	}

	p := planCtx.NewPhysicalPlan()
	var containsRemoteProcessor bool
	for _, sp := range spanPartitions {
		if sp.SQLInstanceID != p.GatewaySQLInstanceID {
			containsRemoteProcessor = true
			break
		}
	}
	stageID := p.NewStage(containsRemoteProcessor, false /* allowPartialDistribution */)
	p.ResultRouters = make([]physicalplan.ProcessorIdx, len(spanPartitions))
	initialSplits := initialSplitsPerProcessor.Get(&planCtx.ExtendedEvalCtx.Settings.SV)
	for i, sp := range spanPartitions {
		ib := &execinfrapb.BackfillerSpec{}
		*ib = spec
		ib.InitialSplits = int32(initialSplits)
		ib.Spans = sp.Spans

		proc := physicalplan.Processor{
			SQLInstanceID: sp.SQLInstanceID,
			Spec: execinfrapb.ProcessorSpec{
				Core:        execinfrapb.ProcessorCoreUnion{Backfiller: ib},
				Output:      []execinfrapb.OutputRouterSpec{{Type: execinfrapb.OutputRouterSpec_PASS_THROUGH}},
				StageID:     stageID,
				ResultTypes: []*types.T{},
			},
		}

		pIdx := p.AddProcessor(proc)
		p.ResultRouters[i] = pIdx
	}
	FinalizePlan(ctx, planCtx, p)
	return p, nil
}

// createIndexBackfillerMergePhysicalPlan generates a plan consisting
// of index merger processors, one for each node that has spans that
// we are reading. The plan is finalized.
func (dsp *DistSQLPlanner) createIndexBackfillerMergePhysicalPlan(
	ctx context.Context,
	planCtx *PlanningCtx,
	spec execinfrapb.IndexBackfillMergerSpec,
	spans [][]roachpb.Span,
) (*PhysicalPlan, error) {

	var n int
	for _, sp := range spans {
		for range sp {
			n++
		}
	}
	indexSpans := make([]roachpb.Span, 0, n)
	spanIdxs := make([]spanAndIndex, 0, n)
	spanIdxTree := interval.NewTree(interval.ExclusiveOverlapper)
	for i := range spans {
		for j := range spans[i] {
			indexSpans = append(indexSpans, spans[i][j])
			spanIdxs = append(spanIdxs, spanAndIndex{Span: spans[i][j], idx: i})
			if err := spanIdxTree.Insert(&spanIdxs[len(spanIdxs)-1], true /* fast */); err != nil {
				return nil, err
			}

		}
	}
	spanIdxTree.AdjustRanges()
	getIndex := func(sp roachpb.Span) (idx int) {
		if !spanIdxTree.DoMatching(func(i interval.Interface) (done bool) {
			idx = i.(*spanAndIndex).idx
			return true
		}, sp.AsRange()) {
			panic(errors.AssertionFailedf("no matching index found for span: %s", sp))
		}
		return idx
	}

	spanPartitions, err := dsp.PartitionSpans(ctx, planCtx, indexSpans, PartitionSpansBoundDefault)
	if err != nil {
		return nil, err
	}

	p := planCtx.NewPhysicalPlan()
	var containsRemoteProcessor bool
	for _, sp := range spanPartitions {
		if sp.SQLInstanceID != p.GatewaySQLInstanceID {
			containsRemoteProcessor = true
			break
		}
	}
	stageID := p.NewStage(containsRemoteProcessor, false /* allowPartialDistribution */)
	p.ResultRouters = make([]physicalplan.ProcessorIdx, len(spanPartitions))
	for i, sp := range spanPartitions {
		ibm := &execinfrapb.IndexBackfillMergerSpec{}
		*ibm = spec

		ibm.Spans = sp.Spans
		for _, sp := range ibm.Spans {
			ibm.SpanIdx = append(ibm.SpanIdx, int32(getIndex(sp)))
		}

		proc := physicalplan.Processor{
			SQLInstanceID: sp.SQLInstanceID,
			Spec: execinfrapb.ProcessorSpec{
				Core:        execinfrapb.ProcessorCoreUnion{IndexBackfillMerger: ibm},
				Output:      []execinfrapb.OutputRouterSpec{{Type: execinfrapb.OutputRouterSpec_PASS_THROUGH}},
				StageID:     stageID,
				ResultTypes: []*types.T{},
			},
		}

		pIdx := p.AddProcessor(proc)
		p.ResultRouters[i] = pIdx
	}
	FinalizePlan(ctx, planCtx, p)
	return p, nil
}

type spanAndIndex struct {
	roachpb.Span
	idx int
}

var _ interval.Interface = (*spanAndIndex)(nil)

func (si *spanAndIndex) Range() interval.Range { return si.AsRange() }
func (si *spanAndIndex) ID() uintptr           { return uintptr(unsafe.Pointer(si)) }
