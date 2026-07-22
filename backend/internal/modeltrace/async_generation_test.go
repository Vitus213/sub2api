package modeltrace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/modeltrace/recording"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestAsyncImageTracePropagation(t *testing.T) {
	fake := newFakeOTLPServer(t)
	manager := newAsyncTestManager(t, fake.server.URL)

	snapshot := manager.Acquire()
	ctx, submission := snapshot.Tracer().Start(context.Background(), "async.image.submission")
	recorder := newTraceRecorder(ctx, snapshot.Tracer(), servermiddleware.ResolvedIdentity{UserID: 11, APIKeyID: 22}, 4096, 4096, capturePolicy{}, snapshot)
	ctx = recording.WithRecorder(ctx, recorder)
	continuation, ok := recording.ContinuationFromContext(ctx)
	require.True(t, ok)

	store := &capturingImageTaskStore{}
	tasks := service.NewImageTaskService(store)
	_, err := tasks.CreateWithContinuation(ctx, service.ImageTaskOwner{UserID: 11, APIKeyID: 22}, continuation)
	require.NoError(t, err)
	require.NotNil(t, store.saved.TraceContinuation)
	require.Equal(t, continuation, *store.saved.TraceContinuation)
	persisted, err := json.Marshal(store.saved)
	require.NoError(t, err)
	require.NotContains(t, string(persisted), testPublicKey)
	require.NotContains(t, string(persisted), testSecretKey)
	require.NotContains(t, string(persisted), fake.server.URL)

	detached, release := recording.Detach(ctx, context.Background())
	submission.End()
	snapshot.Release()
	attempt := recording.BeginAttempt(detached, recording.AttemptMetadata{Provider: "openai", Operation: "image_generation", UpstreamModel: "gpt-image-test"}, []byte(`{"prompt":"cat"}`))
	attempt.End(recording.AttemptResult{HTTPStatus: http.StatusOK, Output: []byte(`{"data":[{"url":"https://example.test/image.png"}]}`)})
	release()
	shutdownManager(t, manager)

	spans := fakeSpans(t, fake)
	require.Len(t, spans, 2)
	root := spanNamed(t, spans, "async.image.submission")
	child := spanNamed(t, spans, "upstream.attempt.1")
	require.Equal(t, root.TraceId, child.TraceId)
	require.Equal(t, root.SpanId, child.ParentSpanId)
}

func TestAsyncExecutionMultipartInputOmitsFileContent(t *testing.T) {
	const canary = "async-multipart-canary-must-not-export"
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", "gpt-image-test"))
	part, err := writer.CreateFormFile("image", "canary.png")
	require.NoError(t, err)
	_, err = part.Write([]byte(canary))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	fake := newFakeOTLPServer(t)
	manager := newAsyncTestManager(t, fake.server.URL)
	execution := manager.StartAsyncExecution(context.Background(), recording.TraceContinuation{}, AsyncExecutionMetadata{
		TaskID: "imgtask-multipart", Model: "gpt-image-test", Operation: "image.edit",
		ContentType: writer.FormDataContentType(),
	}, body.Bytes())
	require.NotNil(t, execution)
	execution.End("completed", []byte(`{"ok":true}`), nil)
	shutdownManager(t, manager)

	span := spanNamed(t, fakeSpans(t, fake), "model.async.execution")
	captured := stringAttribute(t, attributesByKey(span.Attributes), "langfuse.observation.input")
	require.NotContains(t, captured, canary)
	require.Contains(t, captured, `"media_count":1`)
	require.Contains(t, captured, `"fingerprint":"sha256:`)
}

func TestAsyncTraceGenerationMismatch(t *testing.T) {
	oldTarget := newFakeOTLPServer(t)
	newTarget := newFakeOTLPServer(t)
	manager := newAsyncTestManager(t, oldTarget.server.URL)

	snapshot := manager.Acquire()
	ctx, submission := snapshot.Tracer().Start(context.Background(), "async.submission")
	recorder := newTraceRecorder(ctx, snapshot.Tracer(), servermiddleware.ResolvedIdentity{UserID: 31, APIKeyID: 32}, 4096, 4096, capturePolicy{}, snapshot)
	ctx = recording.WithRecorder(ctx, recorder)
	continuation, ok := recording.ContinuationFromContext(ctx)
	require.True(t, ok)
	submission.End()
	snapshot.Release()

	require.NoError(t, manager.ApplySnapshot(context.Background(), ConfigSnapshot{
		Config:        asyncTestConfig(newTarget.server.URL),
		Source:        ConfigSourceRuntime,
		ConfigVersion: 1,
	}))
	execution := manager.StartAsyncExecution(context.Background(), continuation, AsyncExecutionMetadata{
		Identity: servermiddleware.ResolvedIdentity{UserID: 31, APIKeyID: 32},
		TaskID:   "imgtask-1",
		Model:    "gpt-image-test",
	}, []byte(`{"prompt":"cat"}`))
	require.NotNil(t, execution)
	execution.End("completed", []byte(`{"ok":true}`), nil)
	require.NoError(t, manager.ApplySnapshot(context.Background(), ConfigSnapshot{
		Config:        config.ModelTracingConfig{},
		Source:        ConfigSourceRuntime,
		ConfigVersion: 2,
	}))
	require.Nil(t, manager.StartAsyncExecution(context.Background(), continuation, AsyncExecutionMetadata{TaskID: "disabled"}, nil))
	shutdownManager(t, manager)

	oldSpans := fakeSpans(t, oldTarget)
	newSpans := fakeSpans(t, newTarget)
	require.Len(t, oldSpans, 1)
	require.Len(t, newSpans, 1)
	oldRoot := spanNamed(t, oldSpans, "async.submission")
	newRoot := spanNamed(t, newSpans, "model.async.execution")
	require.NotEqual(t, oldRoot.TraceId, newRoot.TraceId)
	require.Empty(t, newRoot.ParentSpanId)
	require.Len(t, newRoot.Links, 1)
	require.Equal(t, oldRoot.TraceId, newRoot.Links[0].TraceId)
	attrs := attributesByKey(newRoot.Attributes)
	require.Equal(t, traceIDHex(oldRoot), stringAttribute(t, attrs, "langfuse.trace.metadata.submission_trace_id"))
	require.Equal(t, "imgtask-1", stringAttribute(t, attrs, "langfuse.trace.metadata.task_id"))

}

func TestBatchImageTraceItems(t *testing.T) {
	fake := newFakeOTLPServer(t)
	manager := newAsyncTestManager(t, fake.server.URL)
	snapshot := manager.Acquire()
	ctx, submission := snapshot.Tracer().Start(context.Background(), "batch.image.submission")
	recorder := newTraceRecorder(ctx, snapshot.Tracer(), servermiddleware.ResolvedIdentity{UserID: 41, APIKeyID: 42}, 4096, 4096, capturePolicy{}, snapshot)
	ctx = recording.WithRecorder(ctx, recorder)
	recording.RecordAsyncSubmission(ctx, "batch-1", []string{"item-a", "item-b"})
	continuation, ok := recording.ContinuationFromContext(ctx)
	require.True(t, ok)
	submission.End()
	snapshot.Release()

	for _, itemID := range []string{"item-a", "item-b"} {
		execution := manager.StartAsyncExecution(context.Background(), continuation, AsyncExecutionMetadata{
			Identity: servermiddleware.ResolvedIdentity{UserID: 41, APIKeyID: 42},
			TaskID:   "batch-1",
			ItemID:   itemID,
			Model:    "imagen-test",
		}, []byte(`{"prompt":"bounded"}`))
		require.NotNil(t, execution)
		execution.End("completed", []byte(`{"image_count":1}`), nil)
	}
	shutdownManager(t, manager)

	spans := fakeSpans(t, fake)
	require.Len(t, spans, 3)
	root := spanNamed(t, spans, "batch.image.submission")
	seen := map[string]bool{}
	for _, span := range spans {
		if span.Name != "model.async.execution" {
			continue
		}
		require.Equal(t, root.TraceId, span.TraceId)
		attrs := attributesByKey(span.Attributes)
		require.Equal(t, "batch-1", stringAttribute(t, attrs, "langfuse.trace.metadata.task_id"))
		require.NotContains(t, attrs, "langfuse.trace.metadata.item_id")
		var observationMetadata map[string]string
		require.NoError(t, json.Unmarshal([]byte(stringAttribute(t, attrs, "langfuse.observation.metadata")), &observationMetadata))
		require.Equal(t, "batch-1", observationMetadata["task_id"])
		seen[observationMetadata["item_id"]] = true
	}
	require.Equal(t, map[string]bool{"item-a": true, "item-b": true}, seen)
}

func TestAsyncModelTraceCancellation(t *testing.T) {
	fake := newFakeOTLPServer(t)
	manager := newAsyncTestManager(t, fake.server.URL)
	snapshot := manager.Acquire()
	ctx, submission := snapshot.Tracer().Start(context.Background(), "cancel.submission")
	recorder := newTraceRecorder(ctx, snapshot.Tracer(), servermiddleware.ResolvedIdentity{UserID: 51, APIKeyID: 52}, 4096, 4096, capturePolicy{}, snapshot)
	ctx = recording.WithRecorder(ctx, recorder)
	continuation, ok := recording.ContinuationFromContext(ctx)
	require.True(t, ok)
	submission.End()
	snapshot.Release()

	execution := manager.StartAsyncExecution(context.Background(), continuation, AsyncExecutionMetadata{TaskID: "cancel-1"}, nil)
	require.NotNil(t, execution)
	execution.End("cancelled", nil, context.Canceled)
	shutdownManager(t, manager)

	span := spanNamed(t, fakeSpans(t, fake), "model.async.execution")
	require.Equal(t, tracepb.Status_STATUS_CODE_ERROR, span.Status.Code)
	require.Equal(t, "cancelled", stringAttribute(t, attributesByKey(span.Attributes), "modeltrace.async.status"))
}

func TestModelTraceGenerationSwap(t *testing.T) {
	firstTarget := newFakeOTLPServer(t)
	secondTarget := newFakeOTLPServer(t)
	manager := newAsyncTestManager(t, firstTarget.server.URL)

	first := manager.Acquire()
	_, firstSpan := first.Tracer().Start(context.Background(), "in-flight-first")
	require.NoError(t, manager.ApplySnapshot(context.Background(), ConfigSnapshot{
		Config: asyncTestConfig(secondTarget.server.URL), Source: ConfigSourceRuntime, ConfigVersion: 1,
	}))
	second := manager.Acquire()
	_, secondSpan := second.Tracer().Start(context.Background(), "new-second")
	secondSpan.End()
	second.Release()
	firstSpan.End()
	first.Release()
	shutdownManager(t, manager)

	require.Len(t, fakeSpans(t, firstTarget), 1)
	require.Equal(t, "in-flight-first", fakeSpans(t, firstTarget)[0].Name)
	require.Len(t, fakeSpans(t, secondTarget), 1)
	require.Equal(t, "new-second", fakeSpans(t, secondTarget)[0].Name)
}

func TestModelTraceDisableInFlight(t *testing.T) {
	fake := newFakeOTLPServer(t)
	manager := newAsyncTestManager(t, fake.server.URL)
	inFlight := manager.Acquire()
	_, span := inFlight.Tracer().Start(context.Background(), "in-flight-before-disable")
	require.NoError(t, manager.ApplySnapshot(context.Background(), ConfigSnapshot{
		Config: config.ModelTracingConfig{}, Source: ConfigSourceRuntime, ConfigVersion: 1,
	}))
	require.False(t, manager.Enabled())
	span.End()
	inFlight.Release()
	shutdownManager(t, manager)
	require.Len(t, fakeSpans(t, fake), 1)
}

func TestModelTraceExporterFailOpen(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name     string
		exporter sdktrace.SpanExporter
	}{
		{name: "panic", exporter: panicSpanExporter{}},
		{name: "error", exporter: errorSpanExporter{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := managerWithTestExporter(tc.exporter)
			response := serveFailOpenRequest(t, manager)
			require.Equal(t, http.StatusOK, response.Code)
			require.JSONEq(t, `{"ok":true}`, response.Body.String())
			shutdownManager(t, manager)
		})
	}

	t.Run("queue saturation never blocks response", func(t *testing.T) {
		blocker := &blockingSpanExporter{release: make(chan struct{})}
		manager := managerWithTestExporter(blocker)
		for i := 0; i < 64; i++ {
			response := serveFailOpenRequest(t, manager)
			require.Equal(t, http.StatusOK, response.Code)
		}
		blocker.once.Do(func() { close(blocker.release) })
		shutdownManager(t, manager)
	})
}

func TestModelTraceProductionQueueRetainsMaximumAsyncBatchBurst(t *testing.T) {
	const maximumAsyncBatchItems = 200
	exporter := &blockingCountingSpanExporter{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	processor := sdktrace.NewBatchSpanProcessor(
		failOpenExporter{delegate: exporter},
		sdktrace.WithMaxQueueSize(defaultMaxQueueSize),
		sdktrace.WithMaxExportBatchSize(defaultMaxExportBatch),
		sdktrace.WithBatchTimeout(defaultBatchTimeout),
		sdktrace.WithExportTimeout(defaultExportTimeout),
	)
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(processor))
	tracer := provider.Tracer(tracerName)

	// Fill one export batch and block it in the exporter. The following burst is
	// the worst case produced by a maximum-sized async image job.
	for i := 0; i < defaultMaxExportBatch; i++ {
		_, span := tracer.Start(context.Background(), "preexisting")
		span.End()
	}
	select {
	case <-exporter.entered:
	case <-time.After(time.Second):
		t.Fatal("batch exporter did not start")
	}
	for i := 0; i < maximumAsyncBatchItems; i++ {
		_, span := tracer.Start(context.Background(), "model.async.execution")
		span.End()
	}
	close(exporter.release)
	require.NoError(t, provider.Shutdown(context.Background()))
	require.Equal(t, defaultMaxExportBatch+maximumAsyncBatchItems, exporter.count())
}

type blockingCountingSpanExporter struct {
	mu       sync.Mutex
	entered  chan struct{}
	release  chan struct{}
	exported int
}

func (e *blockingCountingSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	select {
	case e.entered <- struct{}{}:
	default:
	}
	select {
	case <-e.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	e.mu.Lock()
	e.exported += len(spans)
	e.mu.Unlock()
	return nil
}

func (e *blockingCountingSpanExporter) Shutdown(context.Context) error { return nil }

func (e *blockingCountingSpanExporter) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.exported
}

type capturingImageTaskStore struct {
	saved *service.ImageTaskRecord
}

func (s *capturingImageTaskStore) Save(_ context.Context, task *service.ImageTaskRecord, _ time.Duration) error {
	copy := *task
	s.saved = &copy
	return nil
}
func (s *capturingImageTaskStore) Get(_ context.Context, _ string) (*service.ImageTaskRecord, error) {
	if s.saved == nil {
		return nil, service.ErrImageTaskNotFound
	}
	copy := *s.saved
	return &copy, nil
}

type panicSpanExporter struct{}

func (panicSpanExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	panic("export panic")
}
func (panicSpanExporter) Shutdown(context.Context) error { return nil }

type errorSpanExporter struct{}

func (errorSpanExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return errors.New("serialization failed")
}
func (errorSpanExporter) Shutdown(context.Context) error { return nil }

type blockingSpanExporter struct {
	release chan struct{}
	once    sync.Once
}

func (e *blockingSpanExporter) ExportSpans(ctx context.Context, _ []sdktrace.ReadOnlySpan) error {
	select {
	case <-e.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (e *blockingSpanExporter) Shutdown(context.Context) error {
	e.once.Do(func() { close(e.release) })
	return nil
}

func newAsyncTestManager(t *testing.T, endpoint string) *Manager {
	t.Helper()
	manager, err := NewManager(context.Background(), asyncTestConfig(endpoint))
	require.NoError(t, err)
	return manager
}

func asyncTestConfig(endpoint string) config.ModelTracingConfig {
	return config.ModelTracingConfig{
		Enabled: true, Endpoint: endpoint + "/api/public/otel",
		PublicKey: testPublicKey, SecretKey: testSecretKey,
		PromptMaxBytes: 4096, ResponseMaxBytes: 4096, MediaMaxBytes: 4096,
	}
}

func managerWithTestExporter(exporter sdktrace.SpanExporter) *Manager {
	provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(
		failOpenExporter{delegate: exporter},
		sdktrace.WithMaxQueueSize(2),
		sdktrace.WithMaxExportBatchSize(1),
		sdktrace.WithBatchTimeout(time.Millisecond),
		sdktrace.WithExportTimeout(100*time.Millisecond),
	))
	cfg := config.ModelTracingConfig{Enabled: true, PromptMaxBytes: 4096, ResponseMaxBytes: 4096, MediaMaxBytes: 4096}
	generation := &generation{
		cfg: cfg, source: ConfigSourceDeployment,
		fingerprint: generationFingerprint(cfg, ConfigSourceDeployment, 0),
		provider:    provider, tracer: provider.Tracer(tracerName), shutdown: provider.Shutdown,
	}
	return &Manager{active: generation}
}

func serveFailOpenRequest(t *testing.T, manager *Manager) *httptest.ResponseRecorder {
	t.Helper()
	router := gin.New()
	router.POST("/v1/chat/completions",
		manager.CandidateMiddleware(),
		func(c *gin.Context) {
			apiKey := &service.APIKey{ID: 72, UserID: 71, User: &service.User{ID: 71}}
			servermiddleware.SetOpsFallbackAPIKey(c, apiKey)
			c.Set(string(servermiddleware.ContextKeyAPIKey), apiKey)
			c.Set(string(servermiddleware.ContextKeyUser), servermiddleware.AuthSubject{UserID: 71})
			c.Next()
		},
		func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) },
	)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"test"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func shutdownManager(t *testing.T, manager *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, manager.Shutdown(ctx))
}

func fakeSpans(t *testing.T, fake *fakeOTLPServer) []*tracepb.Span {
	t.Helper()
	requests, serverErrors := fake.snapshot()
	require.Empty(t, serverErrors)
	return exportedSpans(requests)
}

func traceIDHex(span *tracepb.Span) string {
	traceID := trace.TraceID(span.TraceId)
	return traceID.String()
}
