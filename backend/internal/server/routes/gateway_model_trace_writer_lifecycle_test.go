package routes

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/modeltrace"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestGatewayModelTraceWriterLifecyclePreservesResponses(t *testing.T) {
	const requestBody = `{"model":"gpt-test","stream":true}`
	tests := []struct {
		name, contentType, responseBody string
		status                          int
		flush, wantStream               bool
		wantSpanStatus                  tracepb.Status_StatusCode
	}{
		{
			name: "2xx JSON", contentType: "application/json", status: http.StatusOK,
			responseBody: `{"id":"resp-ok","output":"hello"}`, wantSpanStatus: tracepb.Status_STATUS_CODE_OK,
		},
		{
			name: "4xx JSON", contentType: "application/json", status: http.StatusForbidden,
			responseBody: `{"error":{"type":"forbidden","message":"denied"}}`, wantSpanStatus: tracepb.Status_STATUS_CODE_ERROR,
		},
		{
			name: "SSE", contentType: "text/event-stream", status: http.StatusOK, flush: true, wantStream: true,
			responseBody:   "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: [DONE]\n\n",
			wantSpanStatus: tracepb.Status_STATUS_CODE_OK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newRouteModelTraceOTLPFake(t)
			manager, err := modeltrace.NewManager(context.Background(), config.ModelTracingConfig{
				Enabled: true, Endpoint: fake.server.URL + "/api/public/otel",
				PublicKey: "writer-public", SecretKey: "writer-secret",
				PromptMaxBytes: 4096, ResponseMaxBytes: 4096,
			})
			require.NoError(t, err)

			router := newGatewayModelTraceLifecycleRouter(manager, func(c *gin.Context) {
				installGatewayModelTraceIdentity(c)
				c.Header("Content-Type", tt.contentType)
				c.Status(tt.status)
				_, writeErr := c.Writer.WriteString(tt.responseBody)
				require.NoError(t, writeErr)
				if tt.flush {
					c.Writer.Flush()
				}
				c.Abort()
			})
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(requestBody))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			require.NotPanics(t, func() { router.ServeHTTP(response, request) })
			require.Equal(t, tt.status, response.Code)
			require.Equal(t, tt.responseBody, response.Body.String())

			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			require.NoError(t, manager.Shutdown(shutdownCtx))
			cancel()
			requests, exportErrors := fake.traceSnapshot()
			require.Empty(t, exportErrors)
			spans := protocolMatrixSpans(requests)
			require.Len(t, spans, 1)
			root := protocolMatrixSpanNamed(t, spans, "model.request")
			require.Greater(t, root.EndTimeUnixNano, root.StartTimeUnixNano)
			require.Equal(t, tt.wantSpanStatus, root.Status.Code)
			attrs := protocolMatrixAttributes(root.Attributes)
			require.Equal(t, int64(tt.status), attrs["http.response.status_code"].GetIntValue())
			require.Equal(t, tt.responseBody, attrs["langfuse.observation.output"].GetStringValue())
			if tt.wantStream {
				require.Equal(t, "completed", attrs["modeltrace.stream.status"].GetStringValue())
			} else {
				require.NotContains(t, attrs, "modeltrace.stream.status")
			}
		})
	}
}

func TestGatewayModelTraceWriterLifecycleAnonymousRequestStaysUntraced(t *testing.T) {
	const responseBody = `{"error":{"type":"unauthorized","message":"missing API key"}}`
	fake := newRouteModelTraceOTLPFake(t)
	manager, err := modeltrace.NewManager(context.Background(), config.ModelTracingConfig{
		Enabled: true, Endpoint: fake.server.URL + "/api/public/otel",
		PublicKey: "writer-public", SecretKey: "writer-secret",
		PromptMaxBytes: 4096, ResponseMaxBytes: 4096,
	})
	require.NoError(t, err)

	router := newGatewayModelTraceLifecycleRouter(manager, func(c *gin.Context) {
		c.Data(http.StatusUnauthorized, "application/json", []byte(responseBody))
		c.Abort()
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-test"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	require.NotPanics(t, func() { router.ServeHTTP(response, request) })
	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Equal(t, responseBody, response.Body.String())

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	require.NoError(t, manager.Shutdown(shutdownCtx))
	cancel()
	requests, exportErrors := fake.traceSnapshot()
	require.Empty(t, exportErrors)
	require.Empty(t, protocolMatrixSpans(requests))
}

func newGatewayModelTraceLifecycleRouter(manager *modeltrace.Manager, auth servermiddleware.APIKeyAuthMiddleware) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterGatewayRoutes(
		router,
		&handler.Handlers{
			Gateway:       &handler.GatewayHandler{},
			OpenAIGateway: &handler.OpenAIGatewayHandler{},
			AsyncImage:    handler.NewAsyncImageHandler(nil, nil),
		},
		auth,
		nil,
		nil,
		nil,
		nil,
		&config.Config{Gateway: config.GatewayConfig{MaxBodySize: 1024 * 1024}},
		manager,
	)
	return router
}

func installGatewayModelTraceIdentity(c *gin.Context) {
	groupID := int64(19)
	apiKey := &service.APIKey{
		ID: 71, UserID: 73, User: &service.User{ID: 73},
		GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI},
	}
	servermiddleware.SetOpsFallbackAPIKey(c, apiKey)
}
