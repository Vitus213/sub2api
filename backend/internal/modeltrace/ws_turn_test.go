package modeltrace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/modeltrace/recording"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"

	"github.com/stretchr/testify/require"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestModelTraceResponsesWebSocketTurns(t *testing.T) {
	manager, fake := newWSTurnTestManager(t)
	identity := servermiddleware.ResolvedIdentity{APIKeyID: 71, UserID: 73, GroupID: 19}

	first := manager.StartResponsesWSTurn(context.Background(), ResponsesWSTurnMetadata{
		Identity: identity, ConnectionRequestID: "connection-1", TurnRequestID: "turn-request-1",
		TurnIndex: 1, Path: "/v1/responses", Model: "gpt-test",
	}, []byte(`{"type":"response.create","model":"gpt-test","input":"first"}`))
	require.NotNil(t, first)
	first.ObserveClientWrite([]byte(`{"type":"response.output_text.delta","delta":"one"}`), nil)
	first.ObserveClientWrite([]byte(`{"type":"response.completed","response":{"id":"resp_1"}}`), nil)
	first.End(streamStatusCompleted, "", nil)

	second := manager.StartResponsesWSTurn(context.Background(), ResponsesWSTurnMetadata{
		Identity: identity, ConnectionRequestID: "connection-1", TurnRequestID: "turn-request-2",
		TurnIndex: 2, Path: "/v1/responses", Model: "gpt-test",
	}, []byte(`{"type":"response.create","model":"gpt-test","previous_response_id":"resp_1","input":"second"}`))
	require.NotNil(t, second)
	second.ObserveClientWrite([]byte(`{"type":"response.completed","response":{"id":"resp_2"}}`), nil)
	second.End(streamStatusCompleted, "", nil)

	shutdownWSTurnManager(t, manager)
	requests, serverErrors := fake.snapshot()
	require.Empty(t, serverErrors)
	spans := exportedSpans(requests)
	require.Len(t, spans, 2)
	for _, span := range spans {
		require.Empty(t, span.ParentSpanId)
		require.Equal(t, tracepb.Status_STATUS_CODE_OK, span.Status.Code)
		attrs := attributesByKey(span.Attributes)
		require.Equal(t, "connection-1", stringAttribute(t, attrs, "langfuse.trace.metadata.connection_request_id"))
		require.NotContains(t, attrs, "langfuse.session.id", "connection correlation must not become a Langfuse Session")
		require.Equal(t, streamStatusCompleted, stringAttribute(t, attrs, streamStatusAttribute))
	}
	require.NotEqual(t, spans[0].TraceId, spans[1].TraceId)
	require.NotEqual(t,
		stringAttribute(t, attributesByKey(spans[0].Attributes), "langfuse.trace.metadata.turn_request_id"),
		stringAttribute(t, attributesByKey(spans[1].Attributes), "langfuse.trace.metadata.turn_request_id"),
	)
	require.Equal(t, int64(1), intAttribute(t, attributesByKey(spans[0].Attributes), "langfuse.trace.metadata.turn_index"))
	require.Equal(t, int64(2), intAttribute(t, attributesByKey(spans[1].Attributes), "langfuse.trace.metadata.turn_index"))
}

func TestModelTraceResponsesWebSocketConfigSwitch(t *testing.T) {
	firstManager, firstFake := newWSTurnTestManager(t)
	secondManager, secondFake := newWSTurnTestManager(t)
	metadata := ResponsesWSTurnMetadata{
		Identity:            servermiddleware.ResolvedIdentity{APIKeyID: 71, UserID: 73, GroupID: 19},
		ConnectionRequestID: "connection-switch", Path: "/v1/responses", Model: "gpt-test",
	}

	metadata.TurnIndex, metadata.TurnRequestID = 1, "switch-turn-1"
	first := firstManager.StartResponsesWSTurn(context.Background(), metadata, []byte(`{"type":"response.create","model":"gpt-test","input":"old target"}`))
	first.ObserveClientWrite([]byte(`{"type":"response.completed","response":{"id":"resp_old"}}`), nil)
	first.End(streamStatusCompleted, "", nil)

	metadata.TurnIndex, metadata.TurnRequestID = 2, "switch-turn-2"
	second := secondManager.StartResponsesWSTurn(context.Background(), metadata, []byte(`{"type":"response.create","model":"gpt-test","input":"new target"}`))
	second.ObserveClientWrite([]byte(`{"type":"response.completed","response":{"id":"resp_new"}}`), nil)
	second.End(streamStatusCompleted, "", nil)

	shutdownWSTurnManager(t, firstManager)
	shutdownWSTurnManager(t, secondManager)
	firstRequests, firstErrors := firstFake.snapshot()
	secondRequests, secondErrors := secondFake.snapshot()
	require.Empty(t, firstErrors)
	require.Empty(t, secondErrors)
	require.Len(t, exportedSpans(firstRequests), 1)
	require.Len(t, exportedSpans(secondRequests), 1)
	require.Contains(t, stringAttribute(t, attributesByKey(exportedSpans(firstRequests)[0].Attributes), "langfuse.observation.input"), "old target")
	require.Contains(t, stringAttribute(t, attributesByKey(exportedSpans(secondRequests)[0].Attributes), "langfuse.observation.input"), "new target")
}

func TestModelTraceResponsesWebSocketDisconnect(t *testing.T) {
	manager, fake := newWSTurnTestManager(t)
	turn := manager.StartResponsesWSTurn(context.Background(), ResponsesWSTurnMetadata{
		Identity:            servermiddleware.ResolvedIdentity{APIKeyID: 71, UserID: 73, GroupID: 19},
		ConnectionRequestID: "connection-disconnect", TurnRequestID: "disconnect-turn-1",
		TurnIndex: 1, Path: "/v1/responses", Model: "gpt-test",
	}, []byte(`{"type":"response.create","model":"gpt-test","input":"disconnect"}`))
	turn.ObserveClientWrite([]byte(`{"type":"response.output_text.delta","delta":"partial"}`), nil)
	turn.ObserveClientWrite([]byte(`{"type":"response.output_text.delta","delta":"lost"}`), errors.New("client websocket closed"))
	turn.End(streamStatusCompleted, "", nil)

	shutdownWSTurnManager(t, manager)
	requests, serverErrors := fake.snapshot()
	require.Empty(t, serverErrors)
	spans := exportedSpans(requests)
	require.Len(t, spans, 1)
	root := spans[0]
	attrs := attributesByKey(root.Attributes)
	require.Equal(t, streamStatusClientDisconnected, stringAttribute(t, attrs, streamStatusAttribute))
	require.Equal(t, "downstream_write", stringAttribute(t, attrs, streamErrorStageAttribute))
	require.Contains(t, stringAttribute(t, attrs, "langfuse.observation.output"), "partial")
	require.NotContains(t, stringAttribute(t, attrs, "langfuse.observation.output"), "lost")
	require.Equal(t, tracepb.Status_STATUS_CODE_ERROR, root.Status.Code)
}

func TestModelTraceResponsesWebSocketHTTPAttempt(t *testing.T) {
	manager, fake := newWSTurnTestManager(t)
	turn := manager.StartResponsesWSTurn(context.Background(), ResponsesWSTurnMetadata{
		Identity:            servermiddleware.ResolvedIdentity{APIKeyID: 71, UserID: 73, GroupID: 19},
		ConnectionRequestID: "connection-http", TurnRequestID: "http-turn-1",
		TurnIndex: 1, Path: "/v1/responses", Model: "gpt-test",
	}, []byte(`{"type":"response.create","model":"gpt-test"}`))
	attempt := recording.BeginAttempt(turn.Context(), recording.AttemptMetadata{
		Provider: "grok", Operation: "responses", ClientModel: "gpt-test", UpstreamModel: "grok-test",
		AccountID: 29, Endpoint: "https://api.x.ai/v1/responses",
	}, []byte(`{"model":"grok-test"}`))
	body := attempt.ObserveResponse(http.StatusOK, io.NopCloser(bytes.NewBufferString(`{"id":"resp_http"}`)))
	_, err := io.ReadAll(body)
	require.NoError(t, err)
	require.NoError(t, body.Close())
	turn.ObserveClientWrite([]byte(`{"type":"response.completed","response":{"id":"resp_http"}}`), nil)
	turn.End(streamStatusCompleted, "", nil)

	shutdownWSTurnManager(t, manager)
	requests, serverErrors := fake.snapshot()
	require.Empty(t, serverErrors)
	spans := exportedSpans(requests)
	require.Len(t, spans, 2)
	root := spanNamed(t, spans, rootSpanName)
	upstream := spanNamed(t, spans, "upstream.attempt.1")
	require.Equal(t, root.TraceId, upstream.TraceId)
	require.Equal(t, root.SpanId, upstream.ParentSpanId)
}

func TestModelTraceResponsesWebSocketInputCaptureIsBounded(t *testing.T) {
	manager, fake := newWSTurnTestManager(t)
	payload := []byte(`{"type":"response.create","model":"gpt-test","input":"` + strings.Repeat("x", 8192) + `"}`)
	turn := manager.StartResponsesWSTurn(context.Background(), ResponsesWSTurnMetadata{
		Identity: servermiddleware.ResolvedIdentity{APIKeyID: 71}, TurnRequestID: "bounded-turn-1",
		TurnIndex: 1, Path: "/v1/responses", Model: "gpt-test",
	}, payload)
	require.NotNil(t, turn)
	require.LessOrEqual(t, len(turn.input), 4096, "turn must retain only the configured prompt prefix")
	turn.End(streamStatusCompleted, "", nil)

	shutdownWSTurnManager(t, manager)
	requests, serverErrors := fake.snapshot()
	require.Empty(t, serverErrors)
	spans := exportedSpans(requests)
	require.Len(t, spans, 1)
	input := stringAttribute(t, attributesByKey(spans[0].Attributes), "langfuse.observation.input")
	require.Contains(t, input, fmt.Sprintf("[truncated:original_bytes=%d", len(payload)))
}

func newWSTurnTestManager(t *testing.T) (*Manager, *fakeOTLPServer) {
	t.Helper()
	fake := newFakeOTLPServer(t)
	manager, err := NewManager(context.Background(), config.ModelTracingConfig{
		Enabled: true, Endpoint: fake.server.URL + "/api/public/otel", PublicKey: testPublicKey, SecretKey: testSecretKey,
		PromptMaxBytes: 4096, ResponseMaxBytes: 4096,
	})
	require.NoError(t, err)
	return manager, fake
}

func shutdownWSTurnManager(t *testing.T, manager *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, manager.Shutdown(ctx))
}
