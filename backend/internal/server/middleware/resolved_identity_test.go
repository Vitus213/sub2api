//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSetOpsFallbackAPIKeyWithoutIdentityHookRemainsNoOp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	apiKey := &service.APIKey{ID: 41}

	require.NotPanics(t, func() { SetOpsFallbackAPIKey(c, apiKey) })
	fallback, ok := GetOpsFallbackAPIKey(c)
	require.True(t, ok)
	require.Same(t, apiKey, fallback)
}

func TestSetOpsFallbackAPIKeyPublishesOnlyResolvedNumericIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(19)

	tests := []struct {
		name string
		key  *service.APIKey
		want ResolvedIdentity
	}{
		{
			name: "loaded user and group",
			key: &service.APIKey{
				ID:      11,
				UserID:  17,
				GroupID: &groupID,
				Key:     "must-not-be-published",
				User:    &service.User{ID: 17},
				Group:   &service.Group{ID: groupID},
			},
			want: ResolvedIdentity{APIKeyID: 11, UserID: 17, GroupID: 19},
		},
		{
			name: "missing associations remain unknown",
			key: &service.APIKey{
				ID:      23,
				UserID:  29,
				GroupID: &groupID,
				Key:     "must-not-be-published",
			},
			want: ResolvedIdentity{APIKeyID: 23},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			var got []ResolvedIdentity
			SetIdentityEstablishedHook(c, func(_ *gin.Context, identity ResolvedIdentity) {
				got = append(got, identity)
			})

			SetOpsFallbackAPIKey(c, tt.key)

			require.Equal(t, []ResolvedIdentity{tt.want}, got)
		})
	}
}

func TestSetOpsFallbackAPIKeyNotifiesExactlyOncePerResolution(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	calls := 0
	SetIdentityEstablishedHook(c, func(_ *gin.Context, identity ResolvedIdentity) {
		calls++
		require.Equal(t, int64(31), identity.APIKeyID)
	})

	SetOpsFallbackAPIKey(c, &service.APIKey{ID: 31})

	require.Equal(t, 1, calls)
}

func TestSetOpsFallbackAPIKeyHookFailureIsFailOpen(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	called := false
	SetIdentityEstablishedHook(c, func(_ *gin.Context, _ ResolvedIdentity) {
		called = true
		panic("observer failure")
	})
	apiKey := &service.APIKey{ID: 37}

	require.NotPanics(t, func() { SetOpsFallbackAPIKey(c, apiKey) })
	require.True(t, called)
	fallback, ok := GetOpsFallbackAPIKey(c)
	require.True(t, ok)
	require.Same(t, apiKey, fallback)
}

func TestAPIKeyAuthDoesNotPublishIdentityBeforeResolution(t *testing.T) {
	gin.SetMode(gin.TestMode)

	flows := []struct {
		name       string
		headerName string
		middleware func(*service.APIKeyService, *config.Config) gin.HandlerFunc
	}{
		{
			name:       "standard",
			headerName: "x-api-key",
			middleware: func(svc *service.APIKeyService, cfg *config.Config) gin.HandlerFunc {
				return gin.HandlerFunc(NewAPIKeyAuthMiddleware(svc, nil, cfg))
			},
		},
		{
			name:       "google",
			headerName: "x-goog-api-key",
			middleware: func(svc *service.APIKeyService, cfg *config.Config) gin.HandlerFunc {
				return APIKeyAuthGoogle(svc, cfg)
			},
		},
	}

	for _, flow := range flows {
		t.Run(flow.name+" anonymous", func(t *testing.T) {
			repo := &stubApiKeyRepo{getByKey: func(context.Context, string) (*service.APIKey, error) {
				t.Fatal("anonymous request must not reach API key lookup")
				return nil, nil
			}}
			cfg := &config.Config{RunMode: config.RunModeSimple}
			svc := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
			calls := 0
			r := gin.New()
			r.Use(func(c *gin.Context) {
				SetIdentityEstablishedHook(c, func(*gin.Context, ResolvedIdentity) { calls++ })
				c.Next()
			})
			r.Use(flow.middleware(svc, cfg))
			r.GET("/t", func(c *gin.Context) { c.Status(http.StatusOK) })

			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/t", nil))

			require.Equal(t, http.StatusUnauthorized, w.Code)
			require.Zero(t, calls)
		})

		t.Run(flow.name+" unknown and rate limited", func(t *testing.T) {
			repoCalls := 0
			repo := &stubApiKeyRepo{getByKey: func(context.Context, string) (*service.APIKey, error) {
				repoCalls++
				return nil, service.ErrAPIKeyNotFound
			}}
			cfg := invalidAuthAbuseTestConfig(1)
			svc := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
			calls := 0
			r := gin.New()
			r.Use(func(c *gin.Context) {
				SetIdentityEstablishedHook(c, func(*gin.Context, ResolvedIdentity) { calls++ })
				c.Next()
			})
			r.Use(flow.middleware(svc, cfg))
			r.GET("/t", func(c *gin.Context) { c.Status(http.StatusOK) })

			unknown := httptest.NewRequest(http.MethodGet, "/t", nil)
			unknown.Header.Set(flow.headerName, "unknown-key")
			unknownResponse := httptest.NewRecorder()
			r.ServeHTTP(unknownResponse, unknown)
			require.Equal(t, http.StatusUnauthorized, unknownResponse.Code)
			require.Zero(t, calls)

			rateLimited := httptest.NewRequest(http.MethodGet, "/t", nil)
			rateLimited.Header.Set(flow.headerName, "another-unknown-key")
			rateLimitedResponse := httptest.NewRecorder()
			r.ServeHTTP(rateLimitedResponse, rateLimited)
			require.Equal(t, http.StatusTooManyRequests, rateLimitedResponse.Code)
			require.Zero(t, calls)
			require.Equal(t, 1, repoCalls)
		})
	}
}

func TestAPIKeyAuthPublishesRecognizedDisabledIdentityWithoutChangingRejection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(43)
	apiKey := &service.APIKey{
		ID:      41,
		UserID:  42,
		GroupID: &groupID,
		Key:     "recognized-disabled-key",
		Status:  service.StatusAPIKeyDisabled,
		User:    &service.User{ID: 42},
		Group:   &service.Group{ID: groupID},
	}
	wantIdentity := ResolvedIdentity{APIKeyID: 41, UserID: 42, GroupID: 43}

	flows := []struct {
		name       string
		headerName string
		wantBody   string
		middleware func(*service.APIKeyService, *config.Config) gin.HandlerFunc
	}{
		{
			name:       "standard",
			headerName: "x-api-key",
			wantBody:   `{"code":"API_KEY_DISABLED","message":"API key is disabled"}`,
			middleware: func(svc *service.APIKeyService, cfg *config.Config) gin.HandlerFunc {
				return gin.HandlerFunc(NewAPIKeyAuthMiddleware(svc, nil, cfg))
			},
		},
		{
			name:       "google",
			headerName: "x-goog-api-key",
			wantBody:   `{"error":{"code":401,"message":"API key is disabled","status":"UNAUTHENTICATED"}}`,
			middleware: func(svc *service.APIKeyService, cfg *config.Config) gin.HandlerFunc {
				return APIKeyAuthGoogle(svc, cfg)
			},
		},
	}

	for _, flow := range flows {
		t.Run(flow.name, func(t *testing.T) {
			repo := &stubApiKeyRepo{getByKey: func(_ context.Context, key string) (*service.APIKey, error) {
				require.Equal(t, apiKey.Key, key)
				clone := *apiKey
				return &clone, nil
			}}
			cfg := &config.Config{RunMode: config.RunModeSimple}
			svc := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
			var got []ResolvedIdentity
			r := gin.New()
			r.Use(func(c *gin.Context) {
				SetIdentityEstablishedHook(c, func(_ *gin.Context, identity ResolvedIdentity) {
					got = append(got, identity)
				})
				c.Next()
			})
			r.Use(flow.middleware(svc, cfg))
			r.GET("/t", func(c *gin.Context) { t.Fatal("disabled key must not reach handler") })

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/t", nil)
			req.Header.Set(flow.headerName, apiKey.Key)
			r.ServeHTTP(w, req)

			require.Equal(t, http.StatusUnauthorized, w.Code)
			require.JSONEq(t, flow.wantBody, w.Body.String())
			require.Equal(t, []ResolvedIdentity{wantIdentity}, got)
		})
	}
}
