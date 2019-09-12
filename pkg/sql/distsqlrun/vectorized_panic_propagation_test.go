// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package distsqlrun

import (
	"context"
	"sync"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/col/coldata"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/distsqlpb"
	"github.com/cockroachdb/cockroach/pkg/sql/exec"
	"github.com/cockroachdb/cockroach/pkg/sql/exec/execerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/stretchr/testify/require"
)

// TestVectorizedInternalPanic verifies that materializers successfully
// handle panics coming from exec package. It sets up the following chain:
// RowSource -> columnarizer -> test panic emitter -> materializer,
// and makes sure that a panic doesn't occur yet the error is propagated.
func TestVectorizedInternalPanic(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()
	st := cluster.MakeTestingClusterSettings()
	evalCtx := tree.MakeTestingEvalContext(st)
	defer evalCtx.Stop(ctx)

	flowCtx := FlowCtx{
		EvalCtx: &evalCtx,
		Cfg:     &ServerConfig{Settings: cluster.MakeTestingClusterSettings()},
	}

	nRows, nCols := 1, 1
	types := sqlbase.OneIntCol
	input := NewRepeatableRowSource(types, sqlbase.MakeIntRows(nRows, nCols))

	col, err := newColumnarizer(ctx, &flowCtx, 0 /* processorID */, input)
	if err != nil {
		t.Fatal(err)
	}

	vee := newTestVectorizedPanicEmitter(col, execerror.VectorizedInternalPanic)
	mat, err := newMaterializer(
		&flowCtx,
		1, /* processorID */
		vee,
		types,
		&distsqlpb.PostProcessSpec{},
		nil, /* output */
		nil, /* metadataSourceQueue */
		nil, /* outputStatsToTrace */
		nil, /* cancelFlow */
	)
	if err != nil {
		t.Fatal(err)
	}
	mat.Start(ctx)

	var meta *distsqlpb.ProducerMetadata
	require.NotPanics(t, func() { _, meta = mat.Next() }, "VectorizedInternalPanic was not caught")
	require.NotNil(t, meta.Err, "VectorizedInternalPanic was not propagated as metadata")
}

// TestNonVectorizedPanicPropagation verifies that materializers do not handle
// panics coming not from exec package. It sets up the following chain:
// RowSource -> columnarizer -> test panic emitter -> materializer,
// and makes sure that a panic is emitted all the way through the chain.
func TestNonVectorizedPanicPropagation(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()
	st := cluster.MakeTestingClusterSettings()
	evalCtx := tree.MakeTestingEvalContext(st)
	defer evalCtx.Stop(ctx)

	flowCtx := FlowCtx{
		EvalCtx: &evalCtx,
		Cfg:     &ServerConfig{Settings: cluster.MakeTestingClusterSettings()},
	}

	nRows, nCols := 1, 1
	types := sqlbase.OneIntCol
	input := NewRepeatableRowSource(types, sqlbase.MakeIntRows(nRows, nCols))

	col, err := newColumnarizer(ctx, &flowCtx, 0 /* processorID */, input)
	if err != nil {
		t.Fatal(err)
	}

	nvee := newTestVectorizedPanicEmitter(col, nil /* panicFn */)
	mat, err := newMaterializer(
		&flowCtx,
		1, /* processorID */
		nvee,
		types,
		&distsqlpb.PostProcessSpec{},
		nil, /* output */
		nil, /* metadataSourceQueue */
		nil, /* outputStatsToTrace */
		nil, /* cancelFlow */
	)
	if err != nil {
		t.Fatal(err)
	}
	mat.Start(ctx)

	require.Panics(t, func() { mat.Next() }, "NonVectorizedPanic was caught by the operators")
}

// TestNonVectorizedPanicDoesntHangServer verifies that propagating a non
// vectorized panic doesn't result in a hang as described in:
// https://github.com/cockroachdb/cockroach/issues/39779
func TestNonVectorizedPanicDoesntHangServer(t *testing.T) {
	defer leaktest.AfterTest(t)()

	mat := &materializer{
		input: &exec.CallbackOperator{
			NextCb: func(ctx context.Context) coldata.Batch {
				a := []int{0}
				// Trigger an index out of bounds panic.
				a[0] = a[100]
				return nil
			},
		},
	}
	// Avoid uninitialized output panic.
	mat.out.output = &RowBuffer{}
	flow := &Flow{
		processors: []Processor{mat},
		// This test specifically verifies that a flow doesn't get stuck in Wait for
		// asynchronous components that haven't been signaled to exit. To simulate
		// this we just create a mock startable.
		startables: []startable{
			startableFn(func(ctx context.Context, wg *sync.WaitGroup, _ context.CancelFunc) {
				wg.Add(1)
				go func() {
					// Ensure context is canceled.
					<-ctx.Done()
					wg.Done()
				}()
			}),
		},
	}

	require.Panics(t, func() { require.NoError(t, flow.Run(context.Background(), nil)) })
}

// testVectorizedPanicEmitter is an exec.Operator that panics on every
// odd-numbered invocation of Next() and returns the next batch from the input
// on every even-numbered (i.e. it becomes a noop for those iterations). Used
// for tests only. If panicFn is non-nil, it is used; otherwise, the builtin
// panic function is called.
type testVectorizedPanicEmitter struct {
	exec.OneInputNode
	emitBatch bool

	panicFn func(interface{})
}

var _ exec.Operator = &testVectorizedPanicEmitter{}

func newTestVectorizedPanicEmitter(input exec.Operator, panicFn func(interface{})) exec.Operator {
	return &testVectorizedPanicEmitter{
		OneInputNode: exec.NewOneInputNode(input),
		panicFn:      panicFn,
	}
}

// Init is part of exec.Operator interface.
func (e *testVectorizedPanicEmitter) Init() {
	e.Input().Init()
}

// Next is part of exec.Operator interface.
func (e *testVectorizedPanicEmitter) Next(ctx context.Context) coldata.Batch {
	if !e.emitBatch {
		e.emitBatch = true
		if e.panicFn != nil {
			e.panicFn("")
		}
		panic("")
	}

	e.emitBatch = false
	return e.Input().Next(ctx)
}