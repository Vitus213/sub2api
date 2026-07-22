package handler

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/modeltrace/recording"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type usageDetachRecorder struct {
	detached atomic.Int32
	released atomic.Int32
	attempts atomic.Int32
}

func (r *usageDetachRecorder) BeginAttempt(recording.AttemptMetadata, []byte) recording.Attempt {
	r.attempts.Add(1)
	return usageDetachAttempt{}
}

func (r *usageDetachRecorder) DetachRecorder(base context.Context) (context.Context, func()) {
	r.detached.Add(1)
	return recording.WithRecorder(base, r), func() { r.released.Add(1) }
}

type usageDetachAttempt struct{}

func (usageDetachAttempt) ObserveResponse(_ int, body io.ReadCloser) io.ReadCloser { return body }
func (usageDetachAttempt) End(recording.AttemptResult)                             {}

func newUsageRecordTestPool(t *testing.T) *service.UsageRecordWorkerPool {
	t.Helper()
	pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{
		WorkerCount:           1,
		QueueSize:             8,
		TaskTimeout:           time.Second,
		OverflowPolicy:        "drop",
		OverflowSamplePercent: 0,
		AutoScaleEnabled:      false,
	})
	t.Cleanup(pool.Stop)
	return pool
}

func TestGatewayHandlerSubmitUsageRecordTask_WithPool(t *testing.T) {
	pool := newUsageRecordTestPool(t)
	h := &GatewayHandler{usageRecordWorkerPool: pool}

	done := make(chan struct{})
	h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
		close(done)
	})

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("task not executed")
	}
}

func TestGatewayHandlerSubmitUsageRecordTask_WithoutPoolSyncFallback(t *testing.T) {
	h := &GatewayHandler{}
	var called atomic.Bool

	h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("expected deadline in fallback context")
		}
		called.Store(true)
	})

	require.True(t, called.Load())
}

func TestGatewayHandlerSubmitUsageRecordTask_NilTask(t *testing.T) {
	h := &GatewayHandler{}
	require.NotPanics(t, func() {
		h.submitUsageRecordTask(context.Background(), nil)
	})
}

func TestGatewayHandlerSubmitUsageRecordTask_WithoutPool_TaskPanicRecovered(t *testing.T) {
	h := &GatewayHandler{}
	var called atomic.Bool

	require.NotPanics(t, func() {
		h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
			panic("usage task panic")
		})
	})

	h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
		called.Store(true)
	})
	require.True(t, called.Load(), "panic 后后续任务应仍可执行")
}

func TestGatewayHandlerSubmitUsageRecordTaskRetainsRecorderUntilTaskCompletes(t *testing.T) {
	pool := newUsageRecordTestPool(t)
	h := &GatewayHandler{usageRecordWorkerPool: pool}
	recorder := &usageDetachRecorder{}
	parent := recording.WithRecorder(context.Background(), recorder)

	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	pool.Submit(func(context.Context) {
		close(blockerStarted)
		<-releaseBlocker
	})
	<-blockerStarted

	taskDone := make(chan struct{})
	h.submitUsageRecordTask(parent, func(ctx context.Context) {
		recording.BeginAttempt(ctx, recording.AttemptMetadata{}, nil).End(recording.AttemptResult{})
		close(taskDone)
	})
	detachedWhileQueued := recorder.detached.Load()
	releasedWhileQueued := recorder.released.Load()
	close(releaseBlocker)
	select {
	case <-taskDone:
	case <-time.After(time.Second):
		t.Fatal("usage task not executed")
	}
	require.Equal(t, int32(1), detachedWhileQueued, "submission must retain the trace generation before queueing")
	require.Zero(t, releasedWhileQueued, "queued task must keep the generation retained")
	require.Eventually(t, func() bool { return recorder.released.Load() == 1 }, time.Second, 10*time.Millisecond)
	require.Equal(t, int32(1), recorder.attempts.Load(), "detached task must retain the original recorder")
}

func TestGatewayHandlerSubmitUsageRecordTaskFinalizesDroppedRecorder(t *testing.T) {
	pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{
		WorkerCount: 1, QueueSize: 1, TaskTimeout: time.Second,
		OverflowPolicy: "drop", OverflowSamplePercent: 0, AutoScaleEnabled: false,
	})
	t.Cleanup(pool.Stop)
	h := &GatewayHandler{usageRecordWorkerPool: pool}
	recorder := &usageDetachRecorder{}
	parent := recording.WithRecorder(context.Background(), recorder)

	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	pool.Submit(func(context.Context) {
		close(blockerStarted)
		<-releaseBlocker
	})
	<-blockerStarted
	pool.Submit(func(context.Context) {})
	h.submitUsageRecordTask(parent, func(ctx context.Context) {
		recording.BeginAttempt(ctx, recording.AttemptMetadata{}, nil).End(recording.AttemptResult{})
	})
	close(releaseBlocker)

	require.Equal(t, int32(1), recorder.detached.Load())
	require.Equal(t, int32(1), recorder.released.Load(), "dropped task must finalize and release its retained generation")
	require.Zero(t, recorder.attempts.Load())
}

func TestOpenAIGatewayHandlerSubmitUsageRecordTask_WithPool(t *testing.T) {
	pool := newUsageRecordTestPool(t)
	h := &OpenAIGatewayHandler{usageRecordWorkerPool: pool}

	done := make(chan struct{})
	h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
		close(done)
	})

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("task not executed")
	}
}

func TestOpenAIGatewayHandlerSubmitUsageRecordTaskRetainsRecorderUntilTaskCompletes(t *testing.T) {
	pool := newUsageRecordTestPool(t)
	h := &OpenAIGatewayHandler{usageRecordWorkerPool: pool}
	recorder := &usageDetachRecorder{}
	parent := recording.WithRecorder(context.Background(), recorder)

	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	pool.Submit(func(context.Context) {
		close(blockerStarted)
		<-releaseBlocker
	})
	<-blockerStarted

	taskDone := make(chan struct{})
	h.submitUsageRecordTask(parent, func(ctx context.Context) {
		recording.BeginAttempt(ctx, recording.AttemptMetadata{}, nil).End(recording.AttemptResult{})
		close(taskDone)
	})
	detachedWhileQueued := recorder.detached.Load()
	releasedWhileQueued := recorder.released.Load()
	close(releaseBlocker)
	select {
	case <-taskDone:
	case <-time.After(time.Second):
		t.Fatal("usage task not executed")
	}
	require.Equal(t, int32(1), detachedWhileQueued, "submission must retain the trace generation before queueing")
	require.Zero(t, releasedWhileQueued, "queued task must keep the generation retained")
	require.Eventually(t, func() bool { return recorder.released.Load() == 1 }, time.Second, 10*time.Millisecond)
	require.Equal(t, int32(1), recorder.attempts.Load(), "detached task must retain the original recorder")
}

func TestOpenAIGatewayHandlerSubmitUsageRecordTaskFinalizesDroppedRecorder(t *testing.T) {
	pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{
		WorkerCount: 1, QueueSize: 1, TaskTimeout: time.Second,
		OverflowPolicy: "drop", OverflowSamplePercent: 0, AutoScaleEnabled: false,
	})
	t.Cleanup(pool.Stop)
	h := &OpenAIGatewayHandler{usageRecordWorkerPool: pool}
	recorder := &usageDetachRecorder{}
	parent := recording.WithRecorder(context.Background(), recorder)

	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	pool.Submit(func(context.Context) {
		close(blockerStarted)
		<-releaseBlocker
	})
	<-blockerStarted
	pool.Submit(func(context.Context) {})
	h.submitUsageRecordTask(parent, func(ctx context.Context) {
		recording.BeginAttempt(ctx, recording.AttemptMetadata{}, nil).End(recording.AttemptResult{})
	})
	close(releaseBlocker)

	require.Equal(t, int32(1), recorder.detached.Load())
	require.Equal(t, int32(1), recorder.released.Load(), "dropped task must finalize and release its retained generation")
	require.Zero(t, recorder.attempts.Load())
}

func TestOpenAIGatewayHandlerSubmitUsageRecordTaskFinalizesRecorderAfterPanic(t *testing.T) {
	h := &OpenAIGatewayHandler{}
	recorder := &usageDetachRecorder{}
	parent := recording.WithRecorder(context.Background(), recorder)

	require.NotPanics(t, func() {
		h.submitUsageRecordTask(parent, func(context.Context) { panic("record usage failed") })
	})
	require.Equal(t, int32(1), recorder.detached.Load())
	require.Equal(t, int32(1), recorder.released.Load(), "panic recovery must still finalize and release the retained generation")
}

func TestOpenAIGatewayHandlerSubmitUsageRecordTask_WithoutPoolSyncFallback(t *testing.T) {
	h := &OpenAIGatewayHandler{}
	var called atomic.Bool

	h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("expected deadline in fallback context")
		}
		called.Store(true)
	})

	require.True(t, called.Load())
}

func TestOpenAIGatewayHandlerSubmitUsageRecordTask_NilTask(t *testing.T) {
	h := &OpenAIGatewayHandler{}
	require.NotPanics(t, func() {
		h.submitUsageRecordTask(context.Background(), nil)
	})
}

func TestOpenAIGatewayHandlerSubmitUsageRecordTask_WithoutPool_TaskPanicRecovered(t *testing.T) {
	h := &OpenAIGatewayHandler{}
	var called atomic.Bool

	require.NotPanics(t, func() {
		h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
			panic("usage task panic")
		})
	})

	h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
		called.Store(true)
	})
	require.True(t, called.Load(), "panic 后后续任务应仍可执行")
}

func TestOpenAIGatewayHandlerSubmitMandatoryUsageRecordTask_DroppedTaskSyncFallback(t *testing.T) {
	pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{
		WorkerCount:           1,
		QueueSize:             1,
		TaskTimeout:           time.Second,
		OverflowPolicy:        "drop",
		OverflowSamplePercent: 0,
		AutoScaleEnabled:      false,
	})
	t.Cleanup(pool.Stop)
	h := &OpenAIGatewayHandler{usageRecordWorkerPool: pool}
	recorder := &usageDetachRecorder{}
	parent := recording.WithRecorder(context.Background(), recorder)

	block := make(chan struct{})
	release := make(chan struct{})
	pool.Submit(func(ctx context.Context) {
		close(block)
		<-release
	})
	<-block
	pool.Submit(func(ctx context.Context) {})

	var called atomic.Bool
	h.submitMandatoryUsageRecordTask(parent, func(ctx context.Context) {
		called.Store(true)
	})
	close(release)

	require.True(t, called.Load(), "mandatory usage task must run synchronously when async submit is dropped")
	require.Equal(t, int32(1), recorder.detached.Load())
	require.Equal(t, int32(1), recorder.released.Load(), "mandatory sync fallback must finalize its detached recorder")
}

func TestOpenAIGatewayHandlerSubmitOpenAIUsageRecordTask_ImageResultUsesMandatoryFallback(t *testing.T) {
	pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{
		WorkerCount:           1,
		QueueSize:             1,
		TaskTimeout:           time.Second,
		OverflowPolicy:        "drop",
		OverflowSamplePercent: 0,
		AutoScaleEnabled:      false,
	})
	t.Cleanup(pool.Stop)
	h := &OpenAIGatewayHandler{usageRecordWorkerPool: pool}

	block := make(chan struct{})
	release := make(chan struct{})
	pool.Submit(func(ctx context.Context) {
		close(block)
		<-release
	})
	<-block
	pool.Submit(func(ctx context.Context) {})

	var called atomic.Bool
	h.submitOpenAIUsageRecordTask(context.Background(), &service.OpenAIForwardResult{ImageCount: 1}, func(ctx context.Context) {
		called.Store(true)
	})
	close(release)

	require.True(t, called.Load(), "image usage task must be mandatory when async submit is dropped")
}
