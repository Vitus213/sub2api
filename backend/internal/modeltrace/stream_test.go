package modeltrace

import (
	"bytes"
	"context"
	"errors"
	"io"
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
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

const (
	expectedStreamCompleted          = "completed"
	expectedStreamClientDisconnected = "client_disconnected"
	expectedStreamError              = "stream_error"
)

func TestModelTraceSSEComplete(t *testing.T) {
	const output = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: [DONE]\n\n"
	spans, _ := runStreamTrace(t, httptest.NewRecorder(), func(c *gin.Context) {
		attempt := recording.BeginAttempt(c.Request.Context(), recording.AttemptMetadata{
			Provider: "openai", Operation: "chat", ClientModel: "gpt-test", UpstreamModel: "gpt-upstream",
			AccountID: 51, Endpoint: "https://api.openai.example/v1/responses",
		}, []byte(`{"model":"gpt-upstream","stream":true}`))
		body := attempt.ObserveResponse(http.StatusOK, io.NopCloser(bytes.NewBufferString(output)))
		defer func() { require.NoError(t, body.Close()) }()
		c.Header("Content-Type", "text/event-stream")
		_, err := io.Copy(c.Writer, body)
		require.NoError(t, err)
	})

	require.Len(t, spans, 2)
	root := spanNamed(t, spans, rootSpanName)
	attrs := attributesByKey(root.Attributes)
	require.Equal(t, expectedStreamCompleted, stringAttribute(t, attrs, streamStatusAttribute))
	require.JSONEq(t, `{"type":"response.output_text.delta","delta":"hello"}`, firstSSEDataJSON(t, stringAttribute(t, attrs, "langfuse.observation.output")))
	require.GreaterOrEqual(t, intAttribute(t, attrs, firstOutputMsAttribute), int64(0))
	require.Less(t, root.StartTimeUnixNano, root.EndTimeUnixNano)
	require.Equal(t, tracepb.Status_STATUS_CODE_OK, root.Status.Code)
}

func TestModelTraceClientDisconnect(t *testing.T) {
	writer := newDisconnectingResponseWriter(23)
	spans, captured := runStreamTrace(t, writer, func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.Write([]byte("data: partial-client-output\n\n"))
	})

	require.Equal(t, "data: partial-client-ou", captured)
	root := spanNamed(t, spans, rootSpanName)
	attrs := attributesByKey(root.Attributes)
	require.Equal(t, expectedStreamClientDisconnected, stringAttribute(t, attrs, streamStatusAttribute))
	require.Equal(t, "downstream_write", stringAttribute(t, attrs, streamErrorStageAttribute))
	require.Equal(t, captured, stringAttribute(t, attrs, "langfuse.observation.output"))
	require.Equal(t, tracepb.Status_STATUS_CODE_ERROR, root.Status.Code)
}

func TestModelTraceUpstreamStreamError(t *testing.T) {
	upstreamErr := errors.New("upstream stream reset")
	spans, captured := runStreamTrace(t, httptest.NewRecorder(), func(c *gin.Context) {
		attempt := recording.BeginAttempt(c.Request.Context(), recording.AttemptMetadata{
			Provider: "openai", Operation: "chat", ClientModel: "gpt-test", UpstreamModel: "gpt-upstream",
			AccountID: 52, Endpoint: "https://api.openai.example/v1/responses",
		}, []byte(`{"model":"gpt-upstream","stream":true}`))
		body := attempt.ObserveResponse(http.StatusOK, &partialErrorReadCloser{
			data: []byte("data: partial-upstream-output\n\n"), err: upstreamErr,
		})
		defer func() { require.NoError(t, body.Close()) }()
		c.Header("Content-Type", "text/event-stream")
		_, copyErr := io.Copy(c.Writer, body)
		require.ErrorIs(t, copyErr, upstreamErr)
	})

	require.Equal(t, "data: partial-upstream-output\n\n", captured)
	require.Len(t, spans, 2)
	root := spanNamed(t, spans, rootSpanName)
	rootAttrs := attributesByKey(root.Attributes)
	require.Equal(t, expectedStreamError, stringAttribute(t, rootAttrs, streamStatusAttribute))
	require.Equal(t, "upstream_read", stringAttribute(t, rootAttrs, streamErrorStageAttribute))
	require.Equal(t, captured, stringAttribute(t, rootAttrs, "langfuse.observation.output"))
	require.Equal(t, tracepb.Status_STATUS_CODE_ERROR, root.Status.Code)

	attempt := spanNamed(t, spans, "upstream.attempt.1")
	require.Equal(t, captured, stringAttribute(t, attributesByKey(attempt.Attributes), "langfuse.observation.output"))
	require.Equal(t, tracepb.Status_STATUS_CODE_ERROR, attempt.Status.Code)
}

func TestModelTraceCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-test","stream":true}`)).WithContext(ctx)
	spans, captured := runStreamTraceRequest(t, httptest.NewRecorder(), request, func(c *gin.Context) {
		attempt := recording.BeginAttempt(c.Request.Context(), recording.AttemptMetadata{
			Provider: "openai", Operation: "chat", ClientModel: "gpt-test", UpstreamModel: "gpt-upstream",
			AccountID: 53, Endpoint: "https://api.openai.example/v1/responses",
		}, []byte(`{"model":"gpt-upstream","stream":true}`))
		body := attempt.ObserveResponse(http.StatusOK, io.NopCloser(bytes.NewBufferString("data: partial-before-cancel\n\nmore")))
		buf := make([]byte, len("data: partial-before-cancel\n\n"))
		_, err := io.ReadFull(body, buf)
		require.NoError(t, err)
		c.Header("Content-Type", "text/event-stream")
		_, err = c.Writer.Write(buf)
		require.NoError(t, err)
		cancel()
		require.NoError(t, body.Close())
	})

	require.Equal(t, "data: partial-before-cancel\n\n", captured)
	root := spanNamed(t, spans, rootSpanName)
	attrs := attributesByKey(root.Attributes)
	require.Equal(t, streamStatusCancelled, stringAttribute(t, attrs, streamStatusAttribute))
	require.Equal(t, "upstream_close", stringAttribute(t, attrs, streamErrorStageAttribute))
	require.Equal(t, tracepb.Status_STATUS_CODE_ERROR, root.Status.Code)
}

func TestModelTraceSlowExporterDoesNotDelayStream(t *testing.T) {
	exportStarted := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(exportStarted) })
		time.Sleep(350 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	manager, err := NewManager(context.Background(), config.ModelTracingConfig{
		Enabled: true, Endpoint: server.URL + "/api/public/otel", PublicKey: testPublicKey, SecretKey: testSecretKey,
		PromptMaxBytes: 4096, ResponseMaxBytes: 4096,
	})
	require.NoError(t, err)

	router := identifiedStreamRouter(manager, func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		_, writeErr := c.Writer.Write([]byte("data: ready\n\n"))
		require.NoError(t, writeErr)
		c.Writer.Flush()
	})
	w := httptest.NewRecorder()
	started := time.Now()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-test","stream":true}`)))
	requestDuration := time.Since(started)
	require.Less(t, requestDuration, 150*time.Millisecond, "OTLP export must remain off the streaming response path")
	require.Equal(t, "data: ready\n\n", w.Body.String())

	select {
	case <-exportStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for asynchronous export")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, manager.Shutdown(shutdownCtx))
}

func runStreamTrace(t *testing.T, writer http.ResponseWriter, handler gin.HandlerFunc) ([]*tracepb.Span, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-test","stream":true}`))
	return runStreamTraceRequest(t, writer, request, handler)
}

func runStreamTraceRequest(t *testing.T, writer http.ResponseWriter, request *http.Request, handler gin.HandlerFunc) ([]*tracepb.Span, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	fake := newFakeOTLPServer(t)
	manager, err := NewManager(context.Background(), config.ModelTracingConfig{
		Enabled: true, Endpoint: fake.server.URL + "/api/public/otel", PublicKey: testPublicKey, SecretKey: testSecretKey,
		PromptMaxBytes: 4096, ResponseMaxBytes: 4096,
	})
	require.NoError(t, err)

	router := identifiedStreamRouter(manager, handler)
	router.ServeHTTP(writer, request)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, manager.Shutdown(shutdownCtx))
	requests, serverErrors := fake.snapshot()
	require.Empty(t, serverErrors)

	var captured string
	switch value := writer.(type) {
	case *httptest.ResponseRecorder:
		captured = value.Body.String()
	case *disconnectingResponseWriter:
		captured = value.body.String()
	}
	return exportedSpans(requests), captured
}

func identifiedStreamRouter(manager *Manager, handler gin.HandlerFunc) *gin.Engine {
	router := gin.New()
	router.POST("/v1/responses", manager.CandidateMiddleware(), func(c *gin.Context) {
		groupID := int64(19)
		servermiddleware.SetOpsFallbackAPIKey(c, &service.APIKey{
			ID: 71, UserID: 73, User: &service.User{ID: 73},
			GroupID: &groupID, Group: &service.Group{ID: groupID},
		})
		c.Next()
	}, handler)
	return router
}

type disconnectingResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	limit  int
}

func newDisconnectingResponseWriter(limit int) *disconnectingResponseWriter {
	return &disconnectingResponseWriter{header: make(http.Header), limit: limit}
}

func (w *disconnectingResponseWriter) Header() http.Header { return w.header }
func (w *disconnectingResponseWriter) WriteHeader(_ int)   {}
func (w *disconnectingResponseWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.body.Len()
	if remaining <= 0 {
		return 0, io.ErrClosedPipe
	}
	if remaining > len(p) {
		remaining = len(p)
	}
	n, _ := w.body.Write(p[:remaining])
	if n != len(p) {
		return n, io.ErrClosedPipe
	}
	return n, nil
}

type partialErrorReadCloser struct {
	data []byte
	err  error
	done bool
}

func (r *partialErrorReadCloser) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	if !r.done {
		r.done = true
		return 0, r.err
	}
	return 0, io.EOF
}
func (*partialErrorReadCloser) Close() error { return nil }

func firstSSEDataJSON(t *testing.T, value string) string {
	t.Helper()
	const prefix = "data: "
	for _, line := range bytes.Split([]byte(value), []byte("\n")) {
		if bytes.HasPrefix(line, []byte(prefix)) && !bytes.Equal(line, []byte("data: [DONE]")) {
			return string(bytes.TrimPrefix(line, []byte(prefix)))
		}
	}
	t.Fatalf("no SSE JSON data line in %q", value)
	return ""
}
