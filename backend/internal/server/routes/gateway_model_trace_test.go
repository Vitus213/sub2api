package routes

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/modeltrace"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

type modelTraceRouteKey struct {
	method string
	path   string
}

func TestModelTraceRouteMatrixClassifiesEveryGatewayRoute(t *testing.T) {
	manifest := []struct {
		class  string
		routes []modelTraceRouteKey
	}{
		{
			class: "candidate",
			routes: []modelTraceRouteKey{
				{http.MethodPost, "/v1/messages"},
				{http.MethodPost, "/v1/responses"},
				{http.MethodPost, "/v1/responses/*subpath"},
				{http.MethodPost, "/v1/alpha/search"},
				{http.MethodPost, "/v1/chat/completions"},
				{http.MethodPost, "/v1/embeddings"},
				{http.MethodPost, "/v1/images/generations"},
				{http.MethodPost, "/v1/images/edits"},
				{http.MethodPost, "/v1/images/generations/async"},
				{http.MethodPost, "/v1/images/edits/async"},
				{http.MethodPost, "/v1/images/batches"},
				{http.MethodPost, "/v1/videos/generations"},
				{http.MethodPost, "/v1/videos/edits"},
				{http.MethodPost, "/v1/videos/extensions"},
				{http.MethodPost, "/v1beta/models/*modelAction"},
				{http.MethodPost, "/responses"},
				{http.MethodPost, "/responses/*subpath"},
				{http.MethodPost, "/alpha/search"},
				{http.MethodPost, "/backend-api/codex/responses"},
				{http.MethodPost, "/backend-api/codex/responses/*subpath"},
				{http.MethodPost, "/backend-api/codex/alpha/search"},
				{http.MethodPost, "/chat/completions"},
				{http.MethodPost, "/embeddings"},
				{http.MethodPost, "/images/generations"},
				{http.MethodPost, "/images/edits"},
				{http.MethodPost, "/images/generations/async"},
				{http.MethodPost, "/images/edits/async"},
				{http.MethodPost, "/videos/generations"},
				{http.MethodPost, "/videos/edits"},
				{http.MethodPost, "/videos/extensions"},
				{http.MethodPost, "/antigravity/v1/messages"},
				{http.MethodPost, "/antigravity/v1beta/models/*modelAction"},
			},
		},
		{
			class: "control",
			routes: []modelTraceRouteKey{
				{http.MethodGet, "/v1/sub2api/billing"},
				{http.MethodGet, "/v1/models"},
				{http.MethodGet, "/v1/usage"},
				{http.MethodGet, "/v1/images/tasks/:task_id"},
				{http.MethodGet, "/v1/images/batches"},
				{http.MethodGet, "/v1/images/batches/models"},
				{http.MethodGet, "/v1/images/batches/:id"},
				{http.MethodGet, "/v1/images/batches/:id/items"},
				{http.MethodGet, "/v1/images/batches/:id/items/:custom_id/content"},
				{http.MethodGet, "/v1/images/batches/:id/download"},
				{http.MethodPost, "/v1/images/batches/:id/cancel"},
				{http.MethodDelete, "/v1/images/batches/:id"},
				{http.MethodDelete, "/v1/images/batches/:id/outputs"},
				{http.MethodGet, "/v1/videos/:request_id"},
				{http.MethodGet, "/v1/videos/:request_id/content"},
				{http.MethodGet, "/v1beta/models"},
				{http.MethodGet, "/v1beta/models/:model"},
				{http.MethodGet, "/models"},
				{http.MethodGet, "/backend-api/codex/models"},
				{http.MethodGet, "/images/tasks/:task_id"},
				{http.MethodGet, "/videos/:request_id"},
				{http.MethodGet, "/videos/:request_id/content"},
				{http.MethodGet, "/antigravity/models"},
				{http.MethodGet, "/antigravity/v1/models"},
				{http.MethodGet, "/antigravity/v1/usage"},
				{http.MethodGet, "/antigravity/v1beta/models"},
				{http.MethodGet, "/antigravity/v1beta/models/:model"},
			},
		},
		{
			class: "deferred_websocket",
			routes: []modelTraceRouteKey{
				{http.MethodGet, "/v1/responses"},
				{http.MethodGet, "/responses"},
				{http.MethodGet, "/backend-api/codex/responses"},
			},
		},
		{
			class: "deferred_count_tokens",
			routes: []modelTraceRouteKey{
				{http.MethodPost, "/v1/messages/count_tokens"},
				{http.MethodPost, "/messages/count_tokens"},
				{http.MethodPost, "/antigravity/v1/messages/count_tokens"},
			},
		},
	}

	classified := make(map[modelTraceRouteKey]string)
	for _, section := range manifest {
		for _, route := range section.routes {
			if previous, exists := classified[route]; exists {
				t.Fatalf("route %s %s is classified as both %s and %s", route.method, route.path, previous, section.class)
			}
			classified[route] = section.class
		}
	}

	router := newGatewayRoutesTestRouter()
	registered := make(map[modelTraceRouteKey]struct{})
	for _, route := range router.Routes() {
		key := modelTraceRouteKey{method: route.Method, path: route.Path}
		registered[key] = struct{}{}
		_, ok := classified[key]
		require.True(t, ok, "registered gateway route %s %s is missing from the model-trace manifest", route.Method, route.Path)
	}
	for route, class := range classified {
		_, ok := registered[route]
		require.True(t, ok, "%s route %s %s is not registered", class, route.method, route.path)
	}
	require.Len(t, registered, len(classified), "manifest and registered gateway routes must be a disjoint, exhaustive match")
}

type routeModelTraceAPIKeyRepo struct {
	service.APIKeyRepository
	apiKey *service.APIKey
}

func (r *routeModelTraceAPIKeyRepo) GetByKeyForAuth(_ context.Context, key string) (*service.APIKey, error) {
	if r.apiKey == nil || key != r.apiKey.Key {
		return nil, service.ErrAPIKeyNotFound
	}
	clone := *r.apiKey
	return &clone, nil
}

type routeModelTraceOTLPFake struct {
	server   *httptest.Server
	mu       sync.Mutex
	spans    int
	requests []*collectortracepb.ExportTraceServiceRequest
	errors   []error
}

func newRouteModelTraceOTLPFake(t *testing.T) *routeModelTraceOTLPFake {
	t.Helper()
	fake := &routeModelTraceOTLPFake{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			fake.recordError(err)
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		request := &collectortracepb.ExportTraceServiceRequest{}
		if err := proto.Unmarshal(body, request); err != nil {
			fake.recordError(err)
			http.Error(w, "decode protobuf", http.StatusBadRequest)
			return
		}
		spanCount := 0
		for _, resourceSpans := range request.ResourceSpans {
			for _, scopeSpans := range resourceSpans.ScopeSpans {
				for _, span := range scopeSpans.Spans {
					if span.Name == "model.request" && len(span.ParentSpanId) == 0 {
						spanCount++
					}
				}
			}
		}
		fake.mu.Lock()
		fake.requests = append(fake.requests, request)
		fake.spans += spanCount
		fake.mu.Unlock()
		response, _ := proto.Marshal(&collectortracepb.ExportTraceServiceResponse{})
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(response)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *routeModelTraceOTLPFake) recordError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errors = append(f.errors, err)
}

func (f *routeModelTraceOTLPFake) snapshot() (int, []error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spans, append([]error(nil), f.errors...)
}

func (f *routeModelTraceOTLPFake) traceSnapshot() ([]*collectortracepb.ExportTraceServiceRequest, []error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*collectortracepb.ExportTraceServiceRequest(nil), f.requests...), append([]error(nil), f.errors...)
}

func TestModelTraceRouteWiringExportsOnlyExecutionCandidates(t *testing.T) {
	cases := []struct {
		name      string
		method    string
		path      string
		googleKey bool
		wantRoots int
	}{
		{name: "v1 messages candidate", method: http.MethodPost, path: "/v1/messages", wantRoots: 1},
		{name: "v1 responses candidate", method: http.MethodPost, path: "/v1/responses", wantRoots: 1},
		{name: "v1 responses subpath candidate", method: http.MethodPost, path: "/v1/responses/compact", wantRoots: 1},
		{name: "v1 search candidate", method: http.MethodPost, path: "/v1/alpha/search", wantRoots: 1},
		{name: "v1 chat completions candidate", method: http.MethodPost, path: "/v1/chat/completions", wantRoots: 1},
		{name: "v1 embeddings candidate", method: http.MethodPost, path: "/v1/embeddings", wantRoots: 1},
		{name: "v1 image generation candidate", method: http.MethodPost, path: "/v1/images/generations", wantRoots: 1},
		{name: "v1 image edit candidate", method: http.MethodPost, path: "/v1/images/edits", wantRoots: 1},
		{name: "v1 async image generation candidate", method: http.MethodPost, path: "/v1/images/generations/async", wantRoots: 1},
		{name: "v1 async image edit candidate", method: http.MethodPost, path: "/v1/images/edits/async", wantRoots: 1},
		{name: "v1 batch image candidate", method: http.MethodPost, path: "/v1/images/batches", wantRoots: 1},
		{name: "v1 video generation candidate", method: http.MethodPost, path: "/v1/videos/generations", wantRoots: 1},
		{name: "v1 video edit candidate", method: http.MethodPost, path: "/v1/videos/edits", wantRoots: 1},
		{name: "v1 video extension candidate", method: http.MethodPost, path: "/v1/videos/extensions", wantRoots: 1},
		{name: "Gemini candidate", method: http.MethodPost, path: "/v1beta/models/gemini-2.0-flash:generateContent", googleKey: true, wantRoots: 1},
		{name: "root responses candidate", method: http.MethodPost, path: "/responses", wantRoots: 1},
		{name: "root responses subpath candidate", method: http.MethodPost, path: "/responses/compact", wantRoots: 1},
		{name: "root search candidate", method: http.MethodPost, path: "/alpha/search", wantRoots: 1},
		{name: "root chat completions candidate", method: http.MethodPost, path: "/chat/completions", wantRoots: 1},
		{name: "root embeddings candidate", method: http.MethodPost, path: "/embeddings", wantRoots: 1},
		{name: "root image generation candidate", method: http.MethodPost, path: "/images/generations", wantRoots: 1},
		{name: "root image edit candidate", method: http.MethodPost, path: "/images/edits", wantRoots: 1},
		{name: "root async image generation candidate", method: http.MethodPost, path: "/images/generations/async", wantRoots: 1},
		{name: "root async image edit candidate", method: http.MethodPost, path: "/images/edits/async", wantRoots: 1},
		{name: "root video generation candidate", method: http.MethodPost, path: "/videos/generations", wantRoots: 1},
		{name: "root video edit candidate", method: http.MethodPost, path: "/videos/edits", wantRoots: 1},
		{name: "root video extension candidate", method: http.MethodPost, path: "/videos/extensions", wantRoots: 1},
		{name: "Codex responses candidate", method: http.MethodPost, path: "/backend-api/codex/responses", wantRoots: 1},
		{name: "Codex responses subpath candidate", method: http.MethodPost, path: "/backend-api/codex/responses/compact", wantRoots: 1},
		{name: "Codex search candidate", method: http.MethodPost, path: "/backend-api/codex/alpha/search", wantRoots: 1},
		{name: "Antigravity messages candidate", method: http.MethodPost, path: "/antigravity/v1/messages", wantRoots: 1},
		{name: "Antigravity Gemini candidate", method: http.MethodPost, path: "/antigravity/v1beta/models/gemini-2.0-flash:generateContent", googleKey: true, wantRoots: 1},
		{name: "models control", method: http.MethodGet, path: "/v1/models"},
		{name: "WebSocket handshake deferred", method: http.MethodGet, path: "/responses"},
		{name: "count tokens deferred", method: http.MethodPost, path: "/v1/messages/count_tokens"},
		{name: "task polling control", method: http.MethodGet, path: "/v1/images/tasks/task-1"},
		{name: "batch download control", method: http.MethodGet, path: "/v1/images/batches/batch-1/download"},
		{name: "video status control", method: http.MethodGet, path: "/v1/videos/request-1"},
		{name: "Gemini models control", method: http.MethodGet, path: "/v1beta/models", googleKey: true},
		{name: "Antigravity usage control", method: http.MethodGet, path: "/antigravity/v1/usage"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newRouteModelTraceOTLPFake(t)
			manager, err := modeltrace.NewManager(context.Background(), config.ModelTracingConfig{
				Enabled:          true,
				Endpoint:         fake.server.URL + "/api/public/otel",
				PublicKey:        "route-public",
				SecretKey:        "route-secret",
				PromptMaxBytes:   4096,
				ResponseMaxBytes: 4096,
			})
			require.NoError(t, err)

			router, apiKey := newModelTraceRouteWiringRouter(manager)
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.googleKey {
				req.Header.Set("x-goog-api-key", apiKey.Key)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, http.StatusForbidden, w.Code)

			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			require.NoError(t, manager.Shutdown(shutdownCtx))
			cancel()

			roots, exportErrors := fake.snapshot()
			require.Empty(t, exportErrors)
			require.Equal(t, tc.wantRoots, roots)
		})
	}
}

func newModelTraceRouteWiringRouter(manager *modeltrace.Manager) (*gin.Engine, *service.APIKey) {
	gin.SetMode(gin.TestMode)
	groupID := int64(303)
	user := &service.User{ID: 101, Role: service.RoleUser, Status: service.StatusActive}
	apiKey := &service.APIKey{
		ID:      202,
		UserID:  user.ID,
		GroupID: &groupID,
		Key:     "route-model-trace-key",
		Status:  service.StatusActive,
		User:    user,
		Group: &service.Group{
			ID:       groupID,
			Status:   service.StatusDisabled,
			Hydrated: true,
			Platform: service.PlatformGemini,
		},
	}
	cfg := &config.Config{RunMode: config.RunModeStandard}
	apiKeyService := service.NewAPIKeyService(
		&routeModelTraceAPIKeyRepo{apiKey: apiKey}, nil, nil, nil, nil, nil, cfg,
	)
	testAuth := servermiddleware.APIKeyAuthMiddleware(func(c *gin.Context) {
		servermiddleware.SetOpsFallbackAPIKey(c, apiKey)
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "stop before handler"})
	})

	router := gin.New()
	RegisterGatewayRoutes(
		router,
		&handler.Handlers{
			Gateway:       &handler.GatewayHandler{},
			OpenAIGateway: &handler.OpenAIGatewayHandler{},
			AsyncImage:    handler.NewAsyncImageHandler(nil, nil),
		},
		testAuth,
		apiKeyService,
		nil,
		nil,
		nil,
		cfg,
		manager,
	)
	return router, apiKey
}
