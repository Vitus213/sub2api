package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/modeltrace"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestModelTraceProtocolMatrix(t *testing.T) {
	cases := []struct {
		name            string
		routePattern    string
		clientPath      string
		upstreamPath    string
		clientBody      string
		wireBody        string
		wantOperation   string
		wantModel       string
		wantClientModel string
		wantProtocol    string
		deferred        bool
	}{
		{name: "Responses", routePattern: "/v1/responses", clientPath: "/v1/responses", upstreamPath: "/v1/responses", clientBody: `{"model":"gpt-client","input":"client responses"}`, wireBody: `{"model":"gpt-upstream","input":"wire responses"}`, wantOperation: "responses", wantModel: "gpt-upstream", wantClientModel: "gpt-client", wantProtocol: "openai.responses"},
		{name: "Anthropic Messages", routePattern: "/v1/messages", clientPath: "/v1/messages", upstreamPath: "/v1/messages", clientBody: `{"model":"claude-client","messages":[{"role":"user","content":"client messages"}]}`, wireBody: `{"model":"claude-upstream","messages":[{"role":"user","content":"wire messages"}]}`, wantOperation: "messages", wantModel: "claude-upstream", wantClientModel: "claude-client", wantProtocol: "anthropic.messages"},
		{name: "Gemini generateContent", routePattern: "/v1beta/models/*modelAction", clientPath: "/v1beta/models/gemini-client:generateContent", upstreamPath: "/v1beta/models/gemini-upstream:generateContent", clientBody: `{"contents":[{"parts":[{"text":"client gemini"}]}]}`, wireBody: `{"contents":[{"parts":[{"text":"wire gemini"}]}]}`, wantOperation: "generateContent", wantModel: "gemini-upstream", wantClientModel: "gemini-client", wantProtocol: "gemini.generateContent"},
		{name: "Gemini streamGenerateContent", routePattern: "/v1beta/models/*modelAction", clientPath: "/v1beta/models/gemini-client:streamGenerateContent", upstreamPath: "/v1beta/models/gemini-upstream:streamGenerateContent?alt=sse", clientBody: `{"contents":[{"parts":[{"text":"client gemini stream"}]}]}`, wireBody: `{"contents":[{"parts":[{"text":"wire gemini stream"}]}]}`, wantOperation: "streamGenerateContent", wantModel: "gemini-upstream", wantClientModel: "gemini-client", wantProtocol: "gemini.streamGenerateContent"},
		{name: "Embeddings", routePattern: "/v1/embeddings", clientPath: "/v1/embeddings", upstreamPath: "/v1/embeddings", clientBody: `{"model":"embed-client","input":"client embedding"}`, wireBody: `{"model":"embed-upstream","input":"wire embedding"}`, wantOperation: "embeddings", wantModel: "embed-upstream", wantClientModel: "embed-client", wantProtocol: "openai.embeddings"},
		{name: "Search", routePattern: "/v1/alpha/search", clientPath: "/v1/alpha/search", upstreamPath: "/v1/responses", clientBody: `{"model":"search-client","query":"client query"}`, wireBody: `{"model":"search-upstream","input":"wire query"}`, wantOperation: "responses", wantModel: "search-upstream", wantClientModel: "search-client", wantProtocol: "openai.search"},
		{name: "Count Tokens upstream", routePattern: "/v1/messages/count_tokens", clientPath: "/v1/messages/count_tokens", upstreamPath: "/v1/messages/count_tokens", clientBody: `{"model":"claude-client","messages":[{"role":"user","content":"count me"}]}`, wireBody: `{"model":"claude-upstream","messages":[{"role":"user","content":"wire count"}]}`, wantOperation: "count_tokens", wantModel: "claude-upstream", wantClientModel: "claude-client", wantProtocol: "anthropic.count_tokens", deferred: true},
		{name: "Image generation", routePattern: "/v1/images/generations", clientPath: "/v1/images/generations", upstreamPath: "/v1/images/generations", clientBody: `{"model":"image-client","prompt":"client image"}`, wireBody: `{"model":"image-upstream","prompt":"wire image"}`, wantOperation: "generations", wantModel: "image-upstream", wantClientModel: "image-client", wantProtocol: "openai.images.generations"},
		{name: "Image edit", routePattern: "/v1/images/edits", clientPath: "/v1/images/edits", upstreamPath: "/v1/images/edits", clientBody: `{"model":"image-client","prompt":"client edit"}`, wireBody: `{"model":"image-upstream","prompt":"wire edit"}`, wantOperation: "edits", wantModel: "image-upstream", wantClientModel: "image-client", wantProtocol: "openai.images.edits"},
		{name: "Video generation", routePattern: "/v1/videos/generations", clientPath: "/v1/videos/generations", upstreamPath: "/v1/videos/generations", clientBody: `{"model":"video-client","prompt":"client video"}`, wireBody: `{"model":"video-upstream","prompt":"wire video"}`, wantOperation: "generations", wantModel: "video-upstream", wantClientModel: "video-client", wantProtocol: "openai.videos.generations"},
		{name: "Video edit", routePattern: "/v1/videos/edits", clientPath: "/v1/videos/edits", upstreamPath: "/v1/videos/edits", clientBody: `{"model":"video-client","prompt":"client video edit"}`, wireBody: `{"model":"video-upstream","prompt":"wire video edit"}`, wantOperation: "edits", wantModel: "video-upstream", wantClientModel: "video-client", wantProtocol: "openai.videos.edits"},
		{name: "Video extension", routePattern: "/v1/videos/extensions", clientPath: "/v1/videos/extensions", upstreamPath: "/v1/videos/extensions", clientBody: `{"model":"video-client","prompt":"client video extension"}`, wireBody: `{"model":"video-upstream","prompt":"wire video extension"}`, wantOperation: "extensions", wantModel: "video-upstream", wantClientModel: "video-client", wantProtocol: "openai.videos.extensions"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			upstreamOutput, err := json.Marshal(map[string]string{"view": "raw upstream", "case": tc.name})
			require.NoError(t, err)
			upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				body, readErr := io.ReadAll(r.Body)
				if readErr != nil || !json.Valid(body) {
					http.Error(w, "invalid body", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(upstreamOutput)
			}))
			defer upstreamServer.Close()

			otlp := newRouteModelTraceOTLPFake(t)
			manager, err := modeltrace.NewManager(context.Background(), config.ModelTracingConfig{
				Enabled: true, Endpoint: otlp.server.URL + "/api/public/otel",
				PublicKey: "matrix-public", SecretKey: "matrix-secret",
				PromptMaxBytes: 4096, ResponseMaxBytes: 4096,
			})
			require.NoError(t, err)
			upstream := repository.NewHTTPUpstream(nil)
			clientOutput, err := json.Marshal(map[string]string{"view": "final client", "case": tc.name})
			require.NoError(t, err)

			candidate := manager.CandidateMiddleware()
			if tc.deferred {
				candidate = manager.DeferredCandidateMiddleware()
			}
			router := gin.New()
			router.POST(tc.routePattern,
				candidate,
				func(c *gin.Context) {
					groupID := int64(19)
					apiKey := &service.APIKey{ID: 71, UserID: 73, User: &service.User{ID: 73}, GroupID: &groupID, Group: &service.Group{ID: groupID}}
					servermiddleware.SetOpsFallbackAPIKey(c, apiKey)
					c.Next()
				},
				func(c *gin.Context) {
					if tc.deferred {
						modeltrace.ActivateDeferredCandidate(c)
					}
					clientInput, readErr := io.ReadAll(c.Request.Body)
					require.NoError(t, readErr)
					require.JSONEq(t, tc.clientBody, string(clientInput))
					request, requestErr := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstreamServer.URL+tc.upstreamPath, bytes.NewBufferString(tc.wireBody))
					require.NoError(t, requestErr)
					response, requestErr := upstream.Do(request, "", 29, 1)
					require.NoError(t, requestErr)
					rawOutput, readErr := io.ReadAll(response.Body)
					require.NoError(t, readErr)
					require.NoError(t, response.Body.Close())
					require.JSONEq(t, string(upstreamOutput), string(rawOutput))
					c.Data(http.StatusOK, "application/json", clientOutput)
				},
			)

			request := httptest.NewRequest(http.MethodPost, tc.clientPath, bytes.NewBufferString(tc.clientBody))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code)
			require.JSONEq(t, string(clientOutput), response.Body.String())
			require.Equal(t, int32(1), upstreamCalls.Load())

			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			require.NoError(t, manager.Shutdown(shutdownCtx))
			cancel()
			requests, exportErrors := otlp.traceSnapshot()
			require.Empty(t, exportErrors)
			spans := protocolMatrixSpans(requests)
			require.Len(t, spans, 2)
			root := protocolMatrixSpanNamed(t, spans, "model.request")
			attempt := protocolMatrixSpanNamed(t, spans, "upstream.attempt.1")
			require.Equal(t, root.TraceId, attempt.TraceId)
			require.Equal(t, root.SpanId, attempt.ParentSpanId)

			rootAttrs := protocolMatrixAttributes(root.Attributes)
			attemptAttrs := protocolMatrixAttributes(attempt.Attributes)
			require.JSONEq(t, tc.clientBody, rootAttrs["langfuse.observation.input"].GetStringValue())
			require.JSONEq(t, string(clientOutput), rootAttrs["langfuse.observation.output"].GetStringValue())
			require.JSONEq(t, tc.wireBody, attemptAttrs["langfuse.observation.input"].GetStringValue())
			require.JSONEq(t, string(upstreamOutput), attemptAttrs["langfuse.observation.output"].GetStringValue())
			require.Equal(t, tc.wantOperation, attemptAttrs["gen_ai.operation.name"].GetStringValue())
			require.Equal(t, tc.wantModel, attemptAttrs["gen_ai.request.model"].GetStringValue())
			require.Equal(t, tc.wantProtocol, rootAttrs["modeltrace.entry.protocol"].GetStringValue())
			require.Equal(t, tc.wantProtocol, rootAttrs["langfuse.trace.metadata.entry_protocol"].GetStringValue())
			require.Equal(t, tc.wantClientModel, rootAttrs["modeltrace.client.request.model"].GetStringValue())
			require.Equal(t, tc.wantClientModel, rootAttrs["langfuse.trace.metadata.client_model"].GetStringValue())
			tags := make([]string, 0, len(rootAttrs["langfuse.trace.tags"].GetArrayValue().Values))
			for _, tag := range rootAttrs["langfuse.trace.tags"].GetArrayValue().Values {
				tags = append(tags, tag.GetStringValue())
			}
			require.Contains(t, tags, "entry_protocol:"+tc.wantProtocol)
			require.Contains(t, tags, "client_model:"+tc.wantClientModel)
		})
	}
}

func protocolMatrixSpans(requests []*collectortracepb.ExportTraceServiceRequest) []*tracepb.Span {
	var spans []*tracepb.Span
	for _, request := range requests {
		for _, resourceSpans := range request.ResourceSpans {
			for _, scopeSpans := range resourceSpans.ScopeSpans {
				spans = append(spans, scopeSpans.Spans...)
			}
		}
	}
	return spans
}

func TestModelTraceProtocolMatrixLocalAndControlPathsStayZero(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		deferred   bool
	}{
		{name: "local Grok count tokens", path: "/v1/messages/count_tokens", deferred: true},
		{name: "models control", path: "/v1/models"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			otlp := newRouteModelTraceOTLPFake(t)
			manager, err := modeltrace.NewManager(context.Background(), config.ModelTracingConfig{
				Enabled: true, Endpoint: otlp.server.URL + "/api/public/otel",
				PublicKey: "matrix-public", SecretKey: "matrix-secret",
			})
			require.NoError(t, err)
			router := gin.New()
			if tc.deferred {
				router.POST(tc.path,
					manager.DeferredCandidateMiddleware(),
					func(c *gin.Context) {
						apiKey := &service.APIKey{ID: 71, UserID: 73, User: &service.User{ID: 73}}
						servermiddleware.SetOpsFallbackAPIKey(c, apiKey)
						c.Next()
					},
					func(c *gin.Context) {
						_, readErr := io.ReadAll(c.Request.Body)
						require.NoError(t, readErr)
						c.JSON(http.StatusOK, gin.H{"input_tokens": 3})
					},
				)
			} else {
				router.GET(tc.path, func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"data": []any{}}) })
			}
			method := http.MethodGet
			var body io.Reader
			if tc.deferred {
				method = http.MethodPost
				body = bytes.NewBufferString(`{"model":"grok-local"}`)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(method, tc.path, body))
			require.Equal(t, http.StatusOK, response.Code)

			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			require.NoError(t, manager.Shutdown(shutdownCtx))
			cancel()
			requests, exportErrors := otlp.traceSnapshot()
			require.Empty(t, exportErrors)
			require.Empty(t, protocolMatrixSpans(requests))
		})
	}
}

func protocolMatrixSpanNamed(t *testing.T, spans []*tracepb.Span, name string) *tracepb.Span {
	t.Helper()
	for _, span := range spans {
		if span.Name == name {
			return span
		}
	}
	t.Fatalf("span %q not found", name)
	return nil
}

func protocolMatrixAttributes(attributes []*commonpb.KeyValue) map[string]*commonpb.AnyValue {
	result := make(map[string]*commonpb.AnyValue, len(attributes))
	for _, attribute := range attributes {
		result[attribute.Key] = attribute.Value
	}
	return result
}
