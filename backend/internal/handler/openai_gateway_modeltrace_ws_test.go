package handler

import (
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/modeltrace"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

type openAIWSTraceOTLPServer struct {
	server *httptest.Server
	mu     sync.Mutex
	spans  []*tracepb.Span
	errors []error
}

func newOpenAIWSTraceOTLPServer(t *testing.T) *openAIWSTraceOTLPServer {
	t.Helper()
	fake := &openAIWSTraceOTLPServer{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			fake.recordError(err)
			http.Error(w, "read failed", http.StatusBadRequest)
			return
		}
		var request collectortracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &request); err != nil {
			fake.recordError(err)
			http.Error(w, "decode failed", http.StatusBadRequest)
			return
		}
		fake.mu.Lock()
		for _, resourceSpans := range request.ResourceSpans {
			for _, scopeSpans := range resourceSpans.ScopeSpans {
				fake.spans = append(fake.spans, scopeSpans.Spans...)
			}
		}
		fake.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write([]byte{})
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *openAIWSTraceOTLPServer) recordError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errors = append(f.errors, err)
}

func (f *openAIWSTraceOTLPServer) snapshot() ([]*tracepb.Span, []error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*tracepb.Span(nil), f.spans...), append([]error(nil), f.errors...)
}

func newOpenAIWSTraceTestManager(t *testing.T, fake *openAIWSTraceOTLPServer) *modeltrace.Manager {
	t.Helper()
	manager, err := modeltrace.NewManager(context.Background(), config.ModelTracingConfig{
		Enabled: true, Endpoint: fake.server.URL + "/api/public/otel",
		PublicKey: "pk-test", SecretKey: "sk-test", PromptMaxBytes: 64, ResponseMaxBytes: 256,
	})
	require.NoError(t, err)
	return manager
}

func shutdownOpenAIWSTraceTestManager(t *testing.T, manager *modeltrace.Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, manager.Shutdown(ctx))
}

func TestOpenAIWSTraceTurnsStartsOnlyForValidResponseCreateFrames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fake := newOpenAIWSTraceOTLPServer(t)
	manager := newOpenAIWSTraceTestManager(t, fake)
	h := &OpenAIGatewayHandler{}
	h.SetModelTraceManager(manager)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/openai/v1/responses", nil)
	turns := newOpenAIWSTraceTurns(h, c, &service.APIKey{ID: 41}, servermiddleware.AuthSubject{UserID: 42})
	turns.start(1, []byte(`{"type":"session.update","session":{"model":"gpt-test"}}`), "gpt-test")
	turns.start(2, []byte(`{"type":"response.create"`), "gpt-test")
	turns.start(3, []byte(`{"type":"response.create","model":"gpt-test"}`), "gpt-test")
	turns.finishOpen()

	shutdownOpenAIWSTraceTestManager(t, manager)
	spans, exportErrors := fake.snapshot()
	require.Empty(t, exportErrors)
	require.Len(t, spans, 1, "control and invalid frames must not allocate model traces")
}

func TestOpenAIResponsesWebSocketTraceStartsBeforeResponseCreateSemanticValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name      string
		payload   string
		wantSpans int
	}{
		{name: "invalid json", payload: `{"type":"response.create"`, wantSpans: 0},
		{name: "control frame", payload: `{"type":"session.update","session":{"model":"gpt-test"}}`, wantSpans: 0},
		{name: "missing model", payload: `{"type":"response.create"}`, wantSpans: 1},
		{name: "message id continuation", payload: `{"type":"response.create","model":"gpt-test","previous_response_id":"msg_invalid"}`, wantSpans: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newOpenAIWSTraceOTLPServer(t)
			manager := newOpenAIWSTraceTestManager(t, fake)
			h := newOpenAIHandlerForPreviousResponseIDValidation(t, nil)
			h.SetModelTraceManager(manager)
			server := newOpenAIWSHandlerTestServer(t, h, servermiddleware.AuthSubject{UserID: 42, Concurrency: 1})
			defer server.Close()

			dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
			conn, _, err := websocket.Dial(dialCtx, "ws"+server.URL[len("http"):]+"/openai/v1/responses", nil)
			cancelDial()
			require.NoError(t, err)

			writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
			err = conn.Write(writeCtx, websocket.MessageText, []byte(tc.payload))
			cancelWrite()
			require.NoError(t, err)

			readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
			_, _, err = conn.Read(readCtx)
			cancelRead()
			require.Error(t, err)
			_ = conn.CloseNow()

			shutdownOpenAIWSTraceTestManager(t, manager)
			spans, exportErrors := fake.snapshot()
			require.Empty(t, exportErrors)
			require.Len(t, spans, tc.wantSpans)
		})
	}
}

func TestOpenAIWSTraceTurnsUsesExplicitPayloadSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fake := newOpenAIWSTraceOTLPServer(t)
	manager := newOpenAIWSTraceTestManager(t, fake)
	h := &OpenAIGatewayHandler{}
	h.SetModelTraceManager(manager)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/openai/v1/responses", nil)
	turns := newOpenAIWSTraceTurns(h, c, &service.APIKey{ID: 41}, servermiddleware.AuthSubject{UserID: 42})
	turns.start(1, []byte(`{"type":"response.create","model":"gpt-test","session_id":"session-client-owned","prompt_cache_key":"cache-not-a-session"}`), "gpt-test")
	turns.finishOpen()

	shutdownOpenAIWSTraceTestManager(t, manager)
	spans, exportErrors := fake.snapshot()
	require.Empty(t, exportErrors)
	require.Len(t, spans, 1)
	require.Equal(t, "session-client-owned", openAIWSTraceSpanStringAttribute(spans[0], "langfuse.session.id"))
}

func openAIWSTraceSpanStringAttribute(span *tracepb.Span, key string) string {
	if span == nil {
		return ""
	}
	for _, attr := range span.Attributes {
		if attr.Key == key {
			return attr.Value.GetStringValue()
		}
	}
	return ""
}

func TestOpenAIResponsesWebSocketUsageUsesTurnTraceContext(t *testing.T) {
	fake := newOpenAIWSTraceOTLPServer(t)
	manager := newOpenAIWSTraceTestManager(t, fake)
	result := runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload: `{"type":"response.create","model":"gpt-5.4","stream":false,"session_id":"usage-session"}`,
		traceManager: manager,
	})

	shutdownOpenAIWSTraceTestManager(t, manager)
	spans, exportErrors := fake.snapshot()
	require.Empty(t, exportErrors)
	var root *tracepb.Span
	for _, span := range spans {
		if span.Name == "model.request" {
			root = span
		}
	}
	require.NotNil(t, root)
	require.True(t, result.traceContinuation.Valid(), "usage repository must receive the detached turn recorder")
	require.Equal(t, hex.EncodeToString(root.TraceId), result.traceContinuation.TraceID)
}
