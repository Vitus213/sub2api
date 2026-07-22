package modeltrace

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

const (
	// SettingKeyModelTraceConfig stores the complete runtime override as one JSON value.
	SettingKeyModelTraceConfig = "model_trace_config"

	ConfigSourceDisabled         = "disabled"
	ConfigSourceDeployment       = "deployment"
	ConfigSourceRuntime          = "runtime"
	defaultConfigRefreshInterval = 5 * time.Second
)

// SettingsStore is the narrow settings-table boundary used by model tracing.
type SettingsStore interface {
	GetValue(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string) error
}

type compareAndSetSettingsStore interface {
	CompareAndSet(ctx context.Context, key, oldValue, newValue string) (bool, error)
}

// SecretEncryptor matches service.SecretEncryptor without coupling callers to a concrete implementation.
type SecretEncryptor interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(ciphertext string) (string, error)
}

// ConfigSnapshot is the immutable effective model tracing configuration.
type ConfigSnapshot struct {
	Config               config.ModelTracingConfig
	Source               string
	ConfigVersion        int64
	UpdatedAt            time.Time
	UpdatedBy            int64
	allowSourceDowngrade bool
}

// RuntimeConfig is the single settings JSON document. SecretKeyEncrypted never leaves storage APIs.
type RuntimeConfig struct {
	Configured          bool      `json:"configured"`
	Enabled             bool      `json:"enabled"`
	Endpoint            string    `json:"endpoint"`
	PublicKey           string    `json:"public_key"`
	SecretKeyEncrypted  string    `json:"secret_key_encrypted,omitempty"`
	PromptMaxBytes      int       `json:"prompt_max_bytes"`
	ResponseMaxBytes    int       `json:"response_max_bytes"`
	MediaMaxBytes       int       `json:"media_max_bytes"`
	CaptureMediaContent bool      `json:"capture_media_content"`
	ConfigVersion       int64     `json:"config_version"`
	UpdatedAt           time.Time `json:"updated_at"`
	UpdatedBy           int64     `json:"updated_by"`
}

// PublicConfig is safe for admin API responses and never contains secret material.
type PublicConfig struct {
	Configured          bool      `json:"configured"`
	Enabled             bool      `json:"enabled"`
	Endpoint            string    `json:"endpoint"`
	PublicKey           string    `json:"public_key"`
	HasSecret           bool      `json:"has_secret"`
	PromptMaxBytes      int       `json:"prompt_max_bytes"`
	ResponseMaxBytes    int       `json:"response_max_bytes"`
	MediaMaxBytes       int       `json:"media_max_bytes"`
	CaptureMediaContent bool      `json:"capture_media_content"`
	Source              string    `json:"source"`
	ConfigVersion       int64     `json:"config_version"`
	UpdatedAt           time.Time `json:"updated_at,omitempty"`
	UpdatedBy           int64     `json:"updated_by,omitempty"`
}

// UpdateConfigRequest replaces the whole runtime snapshot. A nil SecretKey preserves
// the current secret, a non-empty value replaces it, and an empty value clears it.
type UpdateConfigRequest struct {
	ExpectedConfigVersion int64   `json:"expected_config_version"`
	Enabled               bool    `json:"enabled"`
	Endpoint              string  `json:"endpoint"`
	PublicKey             string  `json:"public_key"`
	SecretKey             *string `json:"secret_key"`
	PromptMaxBytes        int     `json:"prompt_max_bytes"`
	ResponseMaxBytes      int     `json:"response_max_bytes"`
	MediaMaxBytes         int     `json:"media_max_bytes"`
	CaptureMediaContent   bool    `json:"capture_media_content"`
}

// ConfigManager resolves runtime settings over deployment defaults and serializes updates.
type ConfigManager struct {
	deployment              config.ModelTracingConfig
	settings                SettingsStore
	encryptor               SecretEncryptor
	encryptionKeyConfigured bool
	runtime                 *Manager
	mu                      sync.Mutex
	now                     func() time.Time
	refreshInterval         time.Duration
	lifecycleMu             sync.Mutex
	cancel                  context.CancelFunc
	wg                      sync.WaitGroup
}

func NewConfigManager(deployment config.ModelTracingConfig, settings SettingsStore, encryptor SecretEncryptor, encryptionKeyConfigured bool, runtime ...*Manager) *ConfigManager {
	manager := &ConfigManager{
		deployment:              deployment,
		settings:                settings,
		encryptor:               encryptor,
		encryptionKeyConfigured: encryptionKeyConfigured,
		now:                     time.Now,
		refreshInterval:         defaultConfigRefreshInterval,
	}
	if len(runtime) > 0 {
		manager.runtime = runtime[0]
	}
	return manager
}

// Start applies the persisted effective config before serving requests and
// periodically converges this instance after another instance updates settings.
func (m *ConfigManager) Start(ctx context.Context) error {
	if m == nil || m.runtime == nil {
		return nil
	}
	if err := m.refresh(ctx); err != nil {
		return err
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if m.cancel != nil {
		return nil
	}
	refreshCtx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	interval := m.refreshInterval
	if interval <= 0 {
		interval = defaultConfigRefreshInterval
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-refreshCtx.Done():
				return
			case <-ticker.C:
				_ = m.refresh(refreshCtx)
			}
		}
	}()
	return nil
}

func (m *ConfigManager) Shutdown(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.lifecycleMu.Lock()
	cancel := m.cancel
	m.cancel = nil
	m.lifecycleMu.Unlock()
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Resolve selects a valid runtime snapshot first, then the deployment default.
func (m *ConfigManager) Resolve(ctx context.Context) ConfigSnapshot {
	snapshot, err := m.resolve(ctx)
	if err != nil {
		return m.fallbackSnapshot()
	}
	return snapshot
}

func (m *ConfigManager) refresh(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot, err := m.resolve(ctx)
	if err != nil {
		return err
	}
	return m.runtime.ApplySnapshot(ctx, snapshot)
}

func (m *ConfigManager) resolve(ctx context.Context) (ConfigSnapshot, error) {
	if m == nil {
		return ConfigSnapshot{Source: ConfigSourceDisabled}, nil
	}
	runtime, ok, err := m.loadRuntime(ctx)
	if err != nil {
		return ConfigSnapshot{}, err
	}
	if ok {
		return runtime, nil
	}
	return m.fallbackSnapshot(), nil
}

func (m *ConfigManager) fallbackSnapshot() ConfigSnapshot {
	if m == nil {
		return ConfigSnapshot{Source: ConfigSourceDisabled, allowSourceDowngrade: true}
	}
	if deployment, ok := normalizeConfig(m.deployment); ok {
		return ConfigSnapshot{Config: deployment, Source: ConfigSourceDeployment, allowSourceDowngrade: true}
	}
	return ConfigSnapshot{Source: ConfigSourceDisabled, allowSourceDowngrade: true}
}

func (m *ConfigManager) loadRuntime(ctx context.Context) (ConfigSnapshot, bool, error) {
	if m.settings == nil {
		return ConfigSnapshot{}, false, nil
	}
	raw, err := m.settings.GetValue(ctx, SettingKeyModelTraceConfig)
	if err != nil {
		if errors.Is(err, service.ErrSettingNotFound) {
			return ConfigSnapshot{}, false, nil
		}
		return ConfigSnapshot{}, false, err
	}
	if strings.TrimSpace(raw) == "" {
		return ConfigSnapshot{}, false, nil
	}
	var stored RuntimeConfig
	if json.Unmarshal([]byte(raw), &stored) != nil || !stored.Configured {
		return ConfigSnapshot{}, false, nil
	}
	secret := ""
	if stored.SecretKeyEncrypted != "" {
		if m.encryptor == nil {
			return ConfigSnapshot{}, false, nil
		}
		secret, err = m.encryptor.Decrypt(stored.SecretKeyEncrypted)
		if err != nil {
			return ConfigSnapshot{}, false, nil
		}
	}
	value, ok := normalizeConfig(config.ModelTracingConfig{
		Enabled: stored.Enabled, Endpoint: stored.Endpoint, PublicKey: stored.PublicKey, SecretKey: secret,
		PromptMaxBytes: stored.PromptMaxBytes, ResponseMaxBytes: stored.ResponseMaxBytes,
		MediaMaxBytes: stored.MediaMaxBytes, CaptureMediaContent: stored.CaptureMediaContent,
	})
	if !ok {
		return ConfigSnapshot{}, false, nil
	}
	return ConfigSnapshot{
		Config: value, Source: ConfigSourceRuntime, ConfigVersion: stored.ConfigVersion,
		UpdatedAt: stored.UpdatedAt, UpdatedBy: stored.UpdatedBy,
	}, true, nil
}

// GetConfig returns the effective public configuration without secret material.
func (m *ConfigManager) GetConfig(ctx context.Context) PublicConfig {
	return publicFromSnapshot(m.Resolve(ctx))
}

// Save atomically replaces the versioned runtime snapshot.
func (m *ConfigManager) Save(ctx context.Context, request UpdateConfigRequest, actorID int64) (PublicConfig, error) {
	if m == nil || m.settings == nil {
		return PublicConfig{}, infraerrors.ServiceUnavailable("MODEL_TRACE_CONFIG_UNAVAILABLE", "model tracing config store is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	current, currentRaw, err := m.readStoredRuntime(ctx)
	if err != nil {
		return PublicConfig{}, err
	}
	if current.ConfigVersion != request.ExpectedConfigVersion {
		return PublicConfig{}, infraerrors.Conflict("MODEL_TRACE_CONFIG_CONFLICT", "model tracing config was updated by another administrator")
	}

	secretCiphertext, secretPlaintext, err := m.resolveSecretForUpdate(current, request.SecretKey, request.Enabled)
	if err != nil {
		return PublicConfig{}, err
	}
	value, ok := normalizeConfig(config.ModelTracingConfig{
		Enabled: request.Enabled, Endpoint: strings.TrimSpace(request.Endpoint),
		PublicKey: strings.TrimSpace(request.PublicKey), SecretKey: secretPlaintext,
		PromptMaxBytes: request.PromptMaxBytes, ResponseMaxBytes: request.ResponseMaxBytes,
		MediaMaxBytes: request.MediaMaxBytes, CaptureMediaContent: request.CaptureMediaContent,
	})
	if !ok {
		return PublicConfig{}, infraerrors.BadRequest("MODEL_TRACE_CONFIG_INVALID", "enabled model tracing requires a valid endpoint, public key, and secret")
	}
	next := RuntimeConfig{
		Configured: true, Enabled: value.Enabled, Endpoint: value.Endpoint, PublicKey: value.PublicKey,
		SecretKeyEncrypted: secretCiphertext, PromptMaxBytes: value.PromptMaxBytes,
		ResponseMaxBytes: value.ResponseMaxBytes, MediaMaxBytes: value.MediaMaxBytes,
		CaptureMediaContent: value.CaptureMediaContent, ConfigVersion: current.ConfigVersion + 1,
		UpdatedAt: m.now().UTC(), UpdatedBy: actorID,
	}
	var prepared *generation
	if m.runtime != nil {
		prepared, err = buildGeneration(ctx, value, ConfigSourceRuntime, next.ConfigVersion)
		if err != nil {
			return PublicConfig{}, infraerrors.BadRequest("MODEL_TRACE_CONFIG_INVALID", "model tracing exporter could not be initialized").WithCause(err)
		}
	}
	raw, err := json.Marshal(next)
	if err != nil {
		m.discardPrepared(prepared)
		return PublicConfig{}, err
	}
	if err := m.storeSnapshot(ctx, currentRaw, string(raw)); err != nil {
		m.discardPrepared(prepared)
		return PublicConfig{}, err
	}
	if prepared != nil {
		if err := m.runtime.installGeneration(prepared); err != nil {
			return PublicConfig{}, err
		}
	}
	return publicFromRuntime(next), nil
}

func (m *ConfigManager) discardPrepared(prepared *generation) {
	if prepared == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = prepared.close(ctx)
}

func (m *ConfigManager) storeSnapshot(ctx context.Context, oldRaw, newRaw string) error {
	if store, ok := m.settings.(compareAndSetSettingsStore); ok {
		updated, err := store.CompareAndSet(ctx, SettingKeyModelTraceConfig, oldRaw, newRaw)
		if err != nil {
			return err
		}
		if !updated {
			return infraerrors.Conflict("MODEL_TRACE_CONFIG_CONFLICT", "model tracing config was updated by another administrator")
		}
		return nil
	}
	return m.settings.Set(ctx, SettingKeyModelTraceConfig, newRaw)
}

func (m *ConfigManager) readStoredRuntime(ctx context.Context) (RuntimeConfig, string, error) {
	raw, err := m.settings.GetValue(ctx, SettingKeyModelTraceConfig)
	if err != nil {
		if errors.Is(err, service.ErrSettingNotFound) {
			return RuntimeConfig{}, "", nil
		}
		return RuntimeConfig{}, "", err
	}
	if strings.TrimSpace(raw) == "" {
		return RuntimeConfig{}, raw, nil
	}
	var stored RuntimeConfig
	if json.Unmarshal([]byte(raw), &stored) != nil {
		// A corrupt snapshot has no trustworthy version. expected=0 lets an admin repair it.
		return RuntimeConfig{}, raw, nil
	}
	return stored, raw, nil
}

func (m *ConfigManager) resolveSecretForUpdate(current RuntimeConfig, requested *string, enabled bool) (ciphertext, plaintext string, err error) {
	if requested != nil {
		plaintext = *requested
		if plaintext == "" {
			return "", "", nil
		}
		return m.encryptNewSecret(plaintext)
	}
	if current.SecretKeyEncrypted != "" {
		if !enabled {
			return current.SecretKeyEncrypted, "", nil
		}
		if m.encryptor == nil {
			return "", "", infraerrors.ServiceUnavailable("MODEL_TRACE_SECRET_ENCRYPTOR_UNAVAILABLE", "model tracing secret encryptor is unavailable")
		}
		plaintext, err = m.encryptor.Decrypt(current.SecretKeyEncrypted)
		if err != nil {
			return "", "", infraerrors.BadRequest("MODEL_TRACE_SECRET_INVALID", "stored model tracing secret cannot be decrypted")
		}
		return current.SecretKeyEncrypted, plaintext, nil
	}
	if !enabled || m.deployment.SecretKey == "" {
		return "", "", nil
	}
	return m.encryptNewSecret(m.deployment.SecretKey)
}

func (m *ConfigManager) encryptNewSecret(plaintext string) (string, string, error) {
	if !m.encryptionKeyConfigured {
		return "", "", infraerrors.BadRequest("MODEL_TRACE_SECRET_KEY_NOT_DURABLE", "a restart-stable encryption key is required before persisting model tracing secrets")
	}
	if m.encryptor == nil {
		return "", "", infraerrors.ServiceUnavailable("MODEL_TRACE_SECRET_ENCRYPTOR_UNAVAILABLE", "model tracing secret encryptor is unavailable")
	}
	ciphertext, err := m.encryptor.Encrypt(plaintext)
	if err != nil {
		return "", "", err
	}
	return ciphertext, plaintext, nil
}

func publicFromSnapshot(snapshot ConfigSnapshot) PublicConfig {
	value := snapshot.Config
	return PublicConfig{
		Configured: snapshot.Source == ConfigSourceRuntime, Enabled: value.Enabled,
		Endpoint: value.Endpoint, PublicKey: value.PublicKey, HasSecret: value.SecretKey != "",
		PromptMaxBytes: value.PromptMaxBytes, ResponseMaxBytes: value.ResponseMaxBytes,
		MediaMaxBytes: value.MediaMaxBytes, CaptureMediaContent: value.CaptureMediaContent,
		Source: snapshot.Source, ConfigVersion: snapshot.ConfigVersion,
		UpdatedAt: snapshot.UpdatedAt, UpdatedBy: snapshot.UpdatedBy,
	}
}

func publicFromRuntime(stored RuntimeConfig) PublicConfig {
	return PublicConfig{
		Configured: true, Enabled: stored.Enabled, Endpoint: stored.Endpoint, PublicKey: stored.PublicKey,
		HasSecret: stored.SecretKeyEncrypted != "", PromptMaxBytes: stored.PromptMaxBytes,
		ResponseMaxBytes: stored.ResponseMaxBytes, MediaMaxBytes: stored.MediaMaxBytes,
		CaptureMediaContent: stored.CaptureMediaContent, Source: ConfigSourceRuntime,
		ConfigVersion: stored.ConfigVersion, UpdatedAt: stored.UpdatedAt, UpdatedBy: stored.UpdatedBy,
	}
}

func normalizeConfig(value config.ModelTracingConfig) (config.ModelTracingConfig, bool) {
	value.PromptMaxBytes, value.ResponseMaxBytes, value.MediaMaxBytes = boundedSizes(value)
	if !value.Enabled {
		return value, true
	}
	if ValidateEndpoint(value.Endpoint) != nil || value.PublicKey == "" || value.SecretKey == "" {
		return config.ModelTracingConfig{}, false
	}
	return value, true
}
