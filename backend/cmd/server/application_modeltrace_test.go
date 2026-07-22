package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/modeltrace"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type applicationModelTraceSettings struct {
	service.SettingRepository
	value string
}

func (s *applicationModelTraceSettings) GetValue(context.Context, string) (string, error) {
	if s.value == "" {
		return "", service.ErrSettingNotFound
	}
	return s.value, nil
}

func (s *applicationModelTraceSettings) Set(_ context.Context, _ string, value string) error {
	s.value = value
	return nil
}

type applicationModelTraceEncryptor struct{}

func (applicationModelTraceEncryptor) Encrypt(plaintext string) (string, error) {
	return "encrypted:" + plaintext, nil
}

func (applicationModelTraceEncryptor) Decrypt(ciphertext string) (string, error) {
	return strings.TrimPrefix(ciphertext, "encrypted:"), nil
}

func TestApplicationModelTracingActivationInstallsDeploymentConfigAndCleansUp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	modeltrace.InstallDefaultConfigManager(nil)
	t.Cleanup(func() { modeltrace.InstallDefaultConfigManager(nil) })

	deployment := config.ModelTracingConfig{
		Enabled:          true,
		Endpoint:         "https://deployment.example.test/api/public/otel",
		PublicKey:        "deployment-public",
		SecretKey:        "deployment-secret",
		PromptMaxBytes:   1024,
		ResponseMaxBytes: 2048,
		MediaMaxBytes:    4096,
	}
	cfg := &config.Config{
		ModelTracing: deployment,
		Totp:         config.TotpConfig{EncryptionKeyConfigured: true},
	}
	settings := &applicationModelTraceSettings{}
	runtime, err := modeltrace.NewManager(context.Background(), deployment)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, runtime.Shutdown(ctx))
	})
	manager := provideModelTraceConfigManager(cfg, settings, applicationModelTraceEncryptor{}, runtime)
	app := &Application{ModelTraceConfig: manager, Cleanup: func() {}}
	app.activate()

	handler := modeltrace.DefaultAdminHandler()
	require.NotNil(t, handler)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/model-tracing/config", nil)
	ctx.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})
	ctx.Set(string(middleware.ContextKeyUserRole), service.RoleAdmin)
	handler.GetConfig(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	var envelope struct {
		Code int                     `json:"code"`
		Data modeltrace.PublicConfig `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Zero(t, envelope.Code)
	require.Equal(t, modeltrace.ConfigSourceDeployment, envelope.Data.Source)
	require.Equal(t, deployment.Endpoint, envelope.Data.Endpoint)
	require.Equal(t, deployment.PublicKey, envelope.Data.PublicKey)
	require.True(t, envelope.Data.HasSecret)

	runtimeSecret := "runtime-secret"
	_, err = manager.Save(context.Background(), modeltrace.UpdateConfigRequest{
		ExpectedConfigVersion: 0,
		Enabled:               true,
		Endpoint:              "https://runtime.example.test/api/public/otel",
		PublicKey:             "runtime-public",
		SecretKey:             &runtimeSecret,
		PromptMaxBytes:        512,
		ResponseMaxBytes:      1024,
		MediaMaxBytes:         2048,
	}, 1)
	require.NoError(t, err)
	require.Contains(t, settings.value, "encrypted:runtime-secret")

	app.cleanup()
	require.Nil(t, modeltrace.DefaultAdminHandler())
}

type recordingModelTraceShutdowner struct {
	name  string
	calls *[]string
}

func (s recordingModelTraceShutdowner) Shutdown(context.Context) error {
	*s.calls = append(*s.calls, s.name)
	return nil
}

func TestShutdownModelTracingStopsRefreshBeforeRuntime(t *testing.T) {
	calls := make([]string, 0, 2)
	shutdownModelTracing(
		context.Background(),
		recordingModelTraceShutdowner{name: "config-manager", calls: &calls},
		recordingModelTraceShutdowner{name: "runtime-manager", calls: &calls},
	)

	require.Equal(t, []string{"config-manager", "runtime-manager"}, calls)
}
