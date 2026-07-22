package modeltrace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type modelTraceSettingsStore struct {
	value string
	err   error
}

func (s *modelTraceSettingsStore) GetValue(context.Context, string) (string, error) {
	return s.value, s.err
}

func (s *modelTraceSettingsStore) Set(_ context.Context, _ string, value string) error {
	if s.err != nil {
		return s.err
	}
	s.value = value
	return nil
}

type modelTracePrefixEncryptor struct{}

func (modelTracePrefixEncryptor) Encrypt(value string) (string, error) { return "enc:" + value, nil }
func (modelTracePrefixEncryptor) Decrypt(value string) (string, error) {
	if !strings.HasPrefix(value, "enc:") {
		return "", errors.New("invalid ciphertext")
	}
	return strings.TrimPrefix(value, "enc:"), nil
}

func decodeModelTraceRuntime(t *testing.T, store *modelTraceSettingsStore) RuntimeConfig {
	t.Helper()
	var stored RuntimeConfig
	require.NoError(t, json.Unmarshal([]byte(store.value), &stored))
	return stored
}

func TestModelTraceConfigUsesDeploymentOnly(t *testing.T) {
	deployment := config.ModelTracingConfig{
		Enabled:             true,
		Endpoint:            "https://langfuse.example.test/api/public/otel",
		PublicKey:           "deployment-public",
		SecretKey:           "deployment-secret",
		PromptMaxBytes:      101,
		ResponseMaxBytes:    202,
		MediaMaxBytes:       303,
		CaptureMediaContent: true,
	}

	manager := NewConfigManager(deployment, nil, nil, true)
	got := manager.Resolve(context.Background())

	require.Equal(t, ConfigSourceDeployment, got.Source)
	require.Equal(t, deployment, got.Config)
	require.Zero(t, got.ConfigVersion)
}

func TestModelTraceConfigRuntimeOverridesDeployment(t *testing.T) {
	runtime := RuntimeConfig{
		Configured:          true,
		Enabled:             true,
		Endpoint:            "https://runtime-langfuse.example.test/api/public/otel",
		PublicKey:           "runtime-public",
		SecretKeyEncrypted:  "enc:runtime-secret",
		PromptMaxBytes:      404,
		ResponseMaxBytes:    505,
		MediaMaxBytes:       606,
		CaptureMediaContent: true,
		ConfigVersion:       7,
	}
	raw, err := json.Marshal(runtime)
	require.NoError(t, err)
	store := &modelTraceSettingsStore{value: string(raw)}
	manager := NewConfigManager(config.ModelTracingConfig{
		Enabled: true, Endpoint: "https://deployment.example.test/api/public/otel",
		PublicKey: "deployment-public", SecretKey: "deployment-secret",
	}, store, modelTracePrefixEncryptor{}, true)

	got := manager.Resolve(context.Background())

	require.Equal(t, ConfigSourceRuntime, got.Source)
	require.Equal(t, int64(7), got.ConfigVersion)
	require.Equal(t, "runtime-public", got.Config.PublicKey)
	require.Equal(t, "runtime-secret", got.Config.SecretKey)
	require.Equal(t, 404, got.Config.PromptMaxBytes)
}

func TestModelTraceConfigRuntimeCanDisableDeployment(t *testing.T) {
	raw, err := json.Marshal(RuntimeConfig{Configured: true, Enabled: false, ConfigVersion: 8})
	require.NoError(t, err)
	manager := NewConfigManager(config.ModelTracingConfig{
		Enabled: true, Endpoint: "https://deployment.example.test/api/public/otel",
		PublicKey: "deployment-public", SecretKey: "deployment-secret",
	}, &modelTraceSettingsStore{value: string(raw)}, modelTracePrefixEncryptor{}, true)

	got := manager.Resolve(context.Background())

	require.Equal(t, ConfigSourceRuntime, got.Source)
	require.False(t, got.Config.Enabled)
	require.Equal(t, int64(8), got.ConfigVersion)
}

func TestModelTraceConfigBrokenRuntimeFallsBackToDeployment(t *testing.T) {
	deployment := config.ModelTracingConfig{
		Enabled: true, Endpoint: "https://deployment.example.test/api/public/otel",
		PublicKey: "deployment-public", SecretKey: "deployment-secret",
	}

	t.Run("malformed JSON", func(t *testing.T) {
		manager := NewConfigManager(deployment, &modelTraceSettingsStore{value: "not-json"}, modelTracePrefixEncryptor{}, true)
		got := manager.Resolve(context.Background())
		require.Equal(t, ConfigSourceDeployment, got.Source)
		require.Equal(t, "deployment-public", got.Config.PublicKey)
	})

	t.Run("secret cannot decrypt", func(t *testing.T) {
		raw, err := json.Marshal(RuntimeConfig{
			Configured: true, Enabled: true, Endpoint: "https://runtime.example.test/api/public/otel",
			PublicKey: "runtime-public", SecretKeyEncrypted: "broken", ConfigVersion: 9,
		})
		require.NoError(t, err)
		manager := NewConfigManager(deployment, &modelTraceSettingsStore{value: string(raw)}, modelTracePrefixEncryptor{}, true)
		got := manager.Resolve(context.Background())
		require.Equal(t, ConfigSourceDeployment, got.Source)
		require.Equal(t, "deployment-public", got.Config.PublicKey)
	})
}

func TestModelTraceConfigSecretTriStateAndCAS(t *testing.T) {
	raw, err := json.Marshal(RuntimeConfig{
		Configured: true, Enabled: true, Endpoint: "https://old.example.test/api/public/otel",
		PublicKey: "old-public", SecretKeyEncrypted: "enc:old-secret",
		PromptMaxBytes: 100, ResponseMaxBytes: 200, MediaMaxBytes: 300, ConfigVersion: 3,
	})
	require.NoError(t, err)
	store := &modelTraceSettingsStore{value: string(raw)}
	manager := NewConfigManager(config.ModelTracingConfig{}, store, modelTracePrefixEncryptor{}, true)

	preserved, err := manager.Save(context.Background(), UpdateConfigRequest{
		ExpectedConfigVersion: 3, Enabled: true,
		Endpoint: "https://preserved.example.test/api/public/otel", PublicKey: "preserved-public",
		PromptMaxBytes: 101, ResponseMaxBytes: 201, MediaMaxBytes: 301,
	}, 42)
	require.NoError(t, err)
	require.True(t, preserved.HasSecret)
	require.Equal(t, int64(4), preserved.ConfigVersion)
	stored := decodeModelTraceRuntime(t, store)
	require.Equal(t, "enc:old-secret", stored.SecretKeyEncrypted)
	require.Equal(t, int64(42), stored.UpdatedBy)

	replaced, err := manager.Save(context.Background(), UpdateConfigRequest{
		ExpectedConfigVersion: 4, Enabled: true,
		Endpoint: "https://replaced.example.test/api/public/otel", PublicKey: "replaced-public",
		SecretKey:      new("new-secret"),
		PromptMaxBytes: 102, ResponseMaxBytes: 202, MediaMaxBytes: 302,
	}, 43)
	require.NoError(t, err)
	require.True(t, replaced.HasSecret)
	require.Equal(t, int64(5), replaced.ConfigVersion)
	stored = decodeModelTraceRuntime(t, store)
	require.Equal(t, "enc:new-secret", stored.SecretKeyEncrypted)

	cleared, err := manager.Save(context.Background(), UpdateConfigRequest{
		ExpectedConfigVersion: 5, Enabled: false,
		Endpoint: "", PublicKey: "", SecretKey: new(""),
		PromptMaxBytes: 103, ResponseMaxBytes: 203, MediaMaxBytes: 303,
	}, 44)
	require.NoError(t, err)
	require.False(t, cleared.HasSecret)
	require.Equal(t, int64(6), cleared.ConfigVersion)
	stored = decodeModelTraceRuntime(t, store)
	require.Empty(t, stored.SecretKeyEncrypted)

	beforeConflict := store.value
	_, err = manager.Save(context.Background(), UpdateConfigRequest{
		ExpectedConfigVersion: 5, Enabled: false,
		PromptMaxBytes: 104, ResponseMaxBytes: 204, MediaMaxBytes: 304,
	}, 45)
	require.Error(t, err)
	require.Equal(t, beforeConflict, store.value)
}

func TestModelTraceConfigAdminGETPUTHidesSecretAndEnforcesCAS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	raw, err := json.Marshal(RuntimeConfig{
		Configured: true, Enabled: true, Endpoint: "https://admin.example.test/api/public/otel",
		PublicKey: "admin-public", SecretKeyEncrypted: "enc:admin-secret",
		PromptMaxBytes: 100, ResponseMaxBytes: 200, MediaMaxBytes: 300, ConfigVersion: 10,
	})
	require.NoError(t, err)
	store := &modelTraceSettingsStore{value: string(raw)}
	handler := NewAdminHandler(NewConfigManager(config.ModelTracingConfig{}, store, modelTracePrefixEncryptor{}, true))
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 99})
		c.Set(string(middleware.ContextKeyUserRole), "admin")
		c.Next()
	})
	router.GET("/api/v1/admin/model-tracing/config", handler.GetConfig)
	router.PUT("/api/v1/admin/model-tracing/config", handler.UpdateConfig)

	getRecorder := httptest.NewRecorder()
	router.ServeHTTP(getRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/admin/model-tracing/config", nil))
	require.Equal(t, http.StatusOK, getRecorder.Code)
	require.Contains(t, getRecorder.Body.String(), `"has_secret":true`)
	require.NotContains(t, getRecorder.Body.String(), "admin-secret")
	require.NotContains(t, getRecorder.Body.String(), "secret_key_encrypted")

	request := UpdateConfigRequest{
		ExpectedConfigVersion: 10, Enabled: true,
		Endpoint: "https://updated.example.test/api/public/otel", PublicKey: "updated-public",
		SecretKey: new("updated-secret"), PromptMaxBytes: 101, ResponseMaxBytes: 201, MediaMaxBytes: 301,
	}
	body, err := json.Marshal(request)
	require.NoError(t, err)
	putRecorder := httptest.NewRecorder()
	router.ServeHTTP(putRecorder, httptest.NewRequest(http.MethodPut, "/api/v1/admin/model-tracing/config", bytes.NewReader(body)))
	require.Equal(t, http.StatusOK, putRecorder.Code)
	require.NotContains(t, putRecorder.Body.String(), "updated-secret")
	require.Equal(t, "enc:updated-secret", decodeModelTraceRuntime(t, store).SecretKeyEncrypted)

	conflictRecorder := httptest.NewRecorder()
	router.ServeHTTP(conflictRecorder, httptest.NewRequest(http.MethodPut, "/api/v1/admin/model-tracing/config", bytes.NewReader(body)))
	require.Equal(t, http.StatusConflict, conflictRecorder.Code)
}

func TestModelTraceConfigAdminRejectsNonAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewAdminHandler(NewConfigManager(config.ModelTracingConfig{}, &modelTraceSettingsStore{}, modelTracePrefixEncryptor{}, true))
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 100})
		c.Set(string(middleware.ContextKeyUserRole), "user")
		c.Next()
	})
	router.GET("/api/v1/admin/model-tracing/config", handler.GetConfig)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/admin/model-tracing/config", nil))
	require.Equal(t, http.StatusForbidden, recorder.Code)
}

func TestModelTraceConfigRejectsNewSecretWithoutDurableEncryptionKey(t *testing.T) {
	store := &modelTraceSettingsStore{}
	manager := NewConfigManager(config.ModelTracingConfig{}, store, modelTracePrefixEncryptor{}, false)

	_, err := manager.Save(context.Background(), UpdateConfigRequest{
		ExpectedConfigVersion: 0, Enabled: true,
		Endpoint: "https://runtime.example.test/api/public/otel", PublicKey: "runtime-public",
		SecretKey:      new("must-not-persist"),
		PromptMaxBytes: 100, ResponseMaxBytes: 200, MediaMaxBytes: 300,
	}, 99)

	require.Error(t, err)
	require.Empty(t, store.value)
}

func TestModelTraceConfigFirstRuntimeDisableNeedsNoDurableSecret(t *testing.T) {
	deployment := config.ModelTracingConfig{
		Enabled: true, Endpoint: "https://deployment.example.test/api/public/otel",
		PublicKey: "deployment-public", SecretKey: "deployment-secret",
	}
	store := &modelTraceSettingsStore{}
	runtime, err := NewManager(context.Background(), deployment)
	require.NoError(t, err)
	manager := NewConfigManager(deployment, store, modelTracePrefixEncryptor{}, false, runtime)

	disabled, err := manager.Save(context.Background(), UpdateConfigRequest{
		ExpectedConfigVersion: 0,
		Enabled:               false,
		PromptMaxBytes:        100,
		ResponseMaxBytes:      200,
		MediaMaxBytes:         300,
	}, 99)
	require.NoError(t, err)
	require.Equal(t, ConfigSourceRuntime, disabled.Source)
	require.Equal(t, int64(1), disabled.ConfigVersion)
	require.False(t, disabled.Enabled)
	require.False(t, disabled.HasSecret)
	require.Empty(t, decodeModelTraceRuntime(t, store).SecretKeyEncrypted)

	active := runtime.Acquire()
	require.Equal(t, ConfigSourceRuntime, active.Source())
	require.Equal(t, int64(1), active.Version())
	require.False(t, active.Enabled())
	active.Release()

	storedDisabled := store.value
	_, err = manager.Save(context.Background(), UpdateConfigRequest{
		ExpectedConfigVersion: 1,
		Enabled:               true,
		Endpoint:              deployment.Endpoint,
		PublicKey:             deployment.PublicKey,
		PromptMaxBytes:        100,
		ResponseMaxBytes:      200,
		MediaMaxBytes:         300,
	}, 100)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	require.Equal(t, storedDisabled, store.value)
	require.False(t, runtime.Enabled())

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, runtime.Shutdown(shutdownCtx))
}

type casModelTraceSettingsStore struct {
	mu    sync.Mutex
	value string
}

func (s *casModelTraceSettingsStore) GetValue(context.Context, string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.value == "" {
		return "", service.ErrSettingNotFound
	}
	return s.value, nil
}

func (s *casModelTraceSettingsStore) Set(_ context.Context, _ string, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = value
	return nil
}

func (s *casModelTraceSettingsStore) CompareAndSet(_ context.Context, _ string, oldValue, newValue string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.value != oldValue {
		return false, nil
	}
	s.value = newValue
	return true, nil
}

type refreshModelTraceSettingsStore struct {
	mu    sync.Mutex
	value string
	err   error
	reads int
}

func (s *refreshModelTraceSettingsStore) GetValue(context.Context, string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.err != nil {
		return "", s.err
	}
	if s.value == "" {
		return "", service.ErrSettingNotFound
	}
	return s.value, nil
}

func (s *refreshModelTraceSettingsStore) Set(_ context.Context, _ string, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = value
	return nil
}

func (s *refreshModelTraceSettingsStore) replace(value string, err error) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = value
	s.err = err
	return s.reads
}

func (s *refreshModelTraceSettingsStore) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func TestModelTraceConfigRefreshErrorKeepsActiveRuntime(t *testing.T) {
	validDeployment := config.ModelTracingConfig{
		Enabled: true, Endpoint: "https://deployment.example.test/api/public/otel",
		PublicKey: "deployment-public", SecretKey: "deployment-secret",
	}
	for _, tt := range []struct {
		name       string
		deployment config.ModelTracingConfig
	}{
		{name: "valid deployment", deployment: validDeployment},
		{name: "disabled deployment", deployment: config.ModelTracingConfig{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			encodeRuntime := func(endpoint string, version int64) string {
				raw, err := json.Marshal(RuntimeConfig{
					Configured: true, Enabled: true, Endpoint: endpoint,
					PublicKey: "runtime-public", SecretKeyEncrypted: "enc:runtime-secret",
					PromptMaxBytes: 100, ResponseMaxBytes: 200, MediaMaxBytes: 300,
					ConfigVersion: version,
				})
				require.NoError(t, err)
				return string(raw)
			}

			store := &refreshModelTraceSettingsStore{value: encodeRuntime("https://runtime-v1.example.test/api/public/otel", 1)}
			runtime, err := NewManager(context.Background(), tt.deployment)
			require.NoError(t, err)
			manager := NewConfigManager(tt.deployment, store, modelTracePrefixEncryptor{}, true, runtime)
			manager.refreshInterval = 5 * time.Millisecond
			require.NoError(t, manager.Start(context.Background()))

			before := runtime.Acquire()
			require.Equal(t, ConfigSourceRuntime, before.Source())
			require.Equal(t, int64(1), before.Version())
			require.Equal(t, "https://runtime-v1.example.test/api/public/otel", before.Config().Endpoint)
			beforeFingerprint := before.Fingerprint()
			before.Release()

			readBaseline := store.replace("", errors.New("temporary settings read failure"))
			require.Eventually(t, func() bool { return store.readCount() > readBaseline }, time.Second, time.Millisecond)
			duringError := runtime.Acquire()
			require.Equal(t, ConfigSourceRuntime, duringError.Source())
			require.Equal(t, int64(1), duringError.Version())
			require.Equal(t, beforeFingerprint, duringError.Fingerprint())
			require.Equal(t, "https://runtime-v1.example.test/api/public/otel", duringError.Config().Endpoint)
			require.True(t, duringError.Enabled())
			duringError.Release()

			store.replace("not-json", nil)
			require.Eventually(t, func() bool {
				fallback := runtime.Acquire()
				defer fallback.Release()
				return fallback.Source() == ConfigSourceDeployment &&
					fallback.Enabled() == tt.deployment.Enabled &&
					fallback.Config().Endpoint == tt.deployment.Endpoint
			}, time.Second, time.Millisecond)

			store.replace(encodeRuntime("https://runtime-v2.example.test/api/public/otel", 2), nil)
			require.Eventually(t, func() bool {
				after := runtime.Acquire()
				defer after.Release()
				return after.Source() == ConfigSourceRuntime && after.Version() == 2 &&
					after.Config().Endpoint == "https://runtime-v2.example.test/api/public/otel"
			}, time.Second, time.Millisecond)

			shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			require.NoError(t, manager.Shutdown(shutdownCtx))
			require.NoError(t, runtime.Shutdown(shutdownCtx))
		})
	}
}

func TestModelTraceConfigStaleRuntimeSnapshotCannotOverwriteSave(t *testing.T) {
	oldTarget := newFakeOTLPServer(t)
	newTarget := newFakeOTLPServer(t)
	deployment := asyncTestConfig(oldTarget.server.URL)
	stored := RuntimeConfig{
		Configured: true, Enabled: true,
		Endpoint: oldTarget.server.URL + "/api/public/otel", PublicKey: testPublicKey,
		SecretKeyEncrypted: "enc:" + testSecretKey,
		PromptMaxBytes:     100, ResponseMaxBytes: 200, MediaMaxBytes: 300,
		ConfigVersion: 1,
	}
	raw, err := json.Marshal(stored)
	require.NoError(t, err)
	store := &modelTraceSettingsStore{value: string(raw)}
	runtime, err := NewManager(context.Background(), deployment)
	require.NoError(t, err)
	manager := NewConfigManager(deployment, store, modelTracePrefixEncryptor{}, true, runtime)

	stalePollResult := manager.Resolve(context.Background())
	require.NoError(t, runtime.ApplySnapshot(context.Background(), stalePollResult))
	updated, err := manager.Save(context.Background(), UpdateConfigRequest{
		ExpectedConfigVersion: 1,
		Enabled:               true,
		Endpoint:              newTarget.server.URL + "/api/public/otel",
		PublicKey:             testPublicKey,
		PromptMaxBytes:        101,
		ResponseMaxBytes:      201,
		MediaMaxBytes:         301,
	}, 7)
	require.NoError(t, err)
	require.Equal(t, int64(2), updated.ConfigVersion)

	require.NoError(t, runtime.ApplySnapshot(context.Background(), stalePollResult))
	active := runtime.Acquire()
	require.Equal(t, ConfigSourceRuntime, active.Source())
	require.Equal(t, int64(2), active.Version())
	require.Equal(t, newTarget.server.URL+"/api/public/otel", active.Config().Endpoint)
	require.Equal(t, 101, active.Config().PromptMaxBytes)
	_, span := active.Tracer().Start(context.Background(), "after-stale-refresh")
	span.End()
	active.Release()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, runtime.Shutdown(shutdownCtx))
	require.Empty(t, fakeSpans(t, oldTarget))
	newSpans := fakeSpans(t, newTarget)
	require.Len(t, newSpans, 1)
	require.Equal(t, "after-stale-refresh", newSpans[0].Name)
}

func TestModelTraceConfigHotSwitchConvergesAcrossInstances(t *testing.T) {
	firstTarget := newFakeOTLPServer(t)
	secondTarget := newFakeOTLPServer(t)
	deployment := asyncTestConfig(firstTarget.server.URL)
	store := &casModelTraceSettingsStore{}
	firstRuntime, err := NewManager(context.Background(), deployment)
	require.NoError(t, err)
	secondRuntime, err := NewManager(context.Background(), deployment)
	require.NoError(t, err)
	first := NewConfigManager(deployment, store, modelTracePrefixEncryptor{}, true, firstRuntime)
	second := NewConfigManager(deployment, store, modelTracePrefixEncryptor{}, true, secondRuntime)
	first.refreshInterval = 10 * time.Millisecond
	second.refreshInterval = 10 * time.Millisecond
	require.NoError(t, first.Start(context.Background()))
	require.NoError(t, second.Start(context.Background()))

	secret := testSecretKey
	_, err = first.Save(context.Background(), UpdateConfigRequest{
		ExpectedConfigVersion: 0,
		Enabled:               true,
		Endpoint:              secondTarget.server.URL + "/api/public/otel",
		PublicKey:             testPublicKey,
		SecretKey:             &secret,
		PromptMaxBytes:        1024,
		ResponseMaxBytes:      2048,
		MediaMaxBytes:         4096,
	}, 99)
	require.NoError(t, err)
	require.Equal(t, secondTarget.server.URL+"/api/public/otel", firstRuntime.Config().Endpoint)
	require.Eventually(t, func() bool {
		return secondRuntime.Config().Endpoint == secondTarget.server.URL+"/api/public/otel"
	}, time.Second, 10*time.Millisecond)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, first.Shutdown(shutdownCtx))
	require.NoError(t, second.Shutdown(shutdownCtx))
	require.NoError(t, firstRuntime.Shutdown(shutdownCtx))
	require.NoError(t, secondRuntime.Shutdown(shutdownCtx))
}

func TestModelTraceConfigConcurrentCASAllowsOneWinner(t *testing.T) {
	store := &casModelTraceSettingsStore{}
	first := NewConfigManager(config.ModelTracingConfig{}, store, modelTracePrefixEncryptor{}, true)
	second := NewConfigManager(config.ModelTracingConfig{}, store, modelTracePrefixEncryptor{}, true)
	secret := "runtime-secret"
	request := UpdateConfigRequest{
		ExpectedConfigVersion: 0,
		Enabled:               true,
		Endpoint:              "https://langfuse.example.test/api/public/otel",
		PublicKey:             "public",
		SecretKey:             &secret,
		PromptMaxBytes:        1024,
		ResponseMaxBytes:      1024,
		MediaMaxBytes:         1024,
	}
	results := make(chan error, 2)
	go func() { _, err := first.Save(context.Background(), request, 1); results <- err }()
	go func() { _, err := second.Save(context.Background(), request, 2); results <- err }()
	firstErr := <-results
	secondErr := <-results
	wins := 0
	conflicts := 0
	for _, err := range []error{firstErr, secondErr} {
		if err == nil {
			wins++
			continue
		}
		if infraerrors.Code(err) == http.StatusConflict {
			conflicts++
		}
	}
	require.Equal(t, 1, wins)
	require.Equal(t, 1, conflicts)
}
