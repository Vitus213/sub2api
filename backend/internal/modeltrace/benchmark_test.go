package modeltrace

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// BenchmarkModelTraceRequestOverhead measures only the synchronous gateway cost.
// The enabled cases use the production BatchSpanProcessor limits with a local
// discard exporter, so no network latency is included in the request path.
func BenchmarkModelTraceRequestOverhead(b *testing.B) {
	b.Run("disabled", func(b *testing.B) { benchmarkModelTraceMode(b, false) })
	b.Run("enabled", func(b *testing.B) { benchmarkModelTraceMode(b, true) })
}

func BenchmarkModelTraceDisabled(b *testing.B) { benchmarkModelTraceMode(b, false) }

func BenchmarkModelTraceEnabled(b *testing.B) { benchmarkModelTraceMode(b, true) }

func benchmarkModelTraceMode(b *testing.B, enabled bool) {
	gin.SetMode(gin.TestMode)
	longPromptSize := defaultPromptBytes - 128
	longBody := []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"` + strings.Repeat("x", longPromptSize) + `"}]}`)
	cases := []struct {
		name        string
		requestBody []byte
		contentType string
		response    []byte
	}{
		{
			name:        "non_stream",
			requestBody: []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`),
			contentType: "application/json",
			response:    []byte(`{"id":"chatcmpl-benchmark","choices":[{"message":{"content":"hello"}}]}`),
		},
		{
			name:        "sse",
			requestBody: []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}],"stream":true}`),
			contentType: "text/event-stream",
			response:    []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"),
		},
		{
			name:        "long_context",
			requestBody: longBody,
			contentType: "application/json",
			response:    []byte(`{"id":"chatcmpl-long","choices":[{"message":{"content":"ok"}}]}`),
		},
	}
	for _, tc := range cases {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			manager := newBenchmarkManager(b, enabled)
			router := benchmarkRouter(manager, tc.contentType, tc.response)
			b.ReportAllocs()
			b.SetBytes(int64(len(tc.requestBody) + len(tc.response)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(tc.requestBody))
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, request)
				if recorder.Code != http.StatusOK || !bytes.Equal(recorder.Body.Bytes(), tc.response) {
					b.Fatalf("response changed: status=%d bytes=%d", recorder.Code, recorder.Body.Len())
				}
			}
		})
	}
}

type benchmarkDiscardExporter struct{}

func (benchmarkDiscardExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return nil
}
func (benchmarkDiscardExporter) Shutdown(context.Context) error { return nil }

func newBenchmarkManager(b *testing.B, enabled bool) *Manager {
	b.Helper()
	cfg := config.ModelTracingConfig{
		Enabled:          enabled,
		PromptMaxBytes:   defaultPromptBytes,
		ResponseMaxBytes: defaultResponseBytes,
		MediaMaxBytes:    defaultMediaBytes,
	}
	generation := &generation{
		cfg:         cfg,
		source:      ConfigSourceDeployment,
		fingerprint: generationFingerprint(cfg, ConfigSourceDeployment, 0),
	}
	if enabled {
		provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(
			benchmarkDiscardExporter{},
			sdktrace.WithMaxQueueSize(defaultMaxQueueSize),
			sdktrace.WithMaxExportBatchSize(defaultMaxExportBatch),
			sdktrace.WithBatchTimeout(defaultBatchTimeout),
			sdktrace.WithExportTimeout(defaultExportTimeout),
		))
		generation.provider = provider
		generation.tracer = provider.Tracer(tracerName)
		generation.shutdown = provider.Shutdown
	}
	manager := &Manager{active: generation}
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Shutdown(ctx); err != nil {
			b.Errorf("shutdown benchmark trace provider: %v", err)
		}
	})
	return manager
}

func benchmarkRouter(manager *Manager, contentType string, response []byte) *gin.Engine {
	router := gin.New()
	router.POST("/v1/chat/completions",
		manager.CandidateMiddleware(),
		func(c *gin.Context) {
			groupID := int64(19)
			servermiddleware.SetOpsFallbackAPIKey(c, &service.APIKey{
				ID: 71, UserID: 73, User: &service.User{ID: 73},
				GroupID: &groupID, Group: &service.Group{ID: groupID},
			})
			c.Next()
		},
		func(c *gin.Context) {
			_, _ = io.Copy(io.Discard, c.Request.Body)
			c.Header("Content-Type", contentType)
			_, _ = c.Writer.Write(response)
			if contentType == "text/event-stream" {
				c.Writer.Flush()
			}
		},
	)
	return router
}
