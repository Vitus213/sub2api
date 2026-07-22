package modeltrace

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"

	"go.opentelemetry.io/otel"
)

func TestModelTraceEndpointTransport(t *testing.T) {
	t.Run("accepts only HTTPS or loopback HTTP", func(t *testing.T) {
		tests := []struct {
			name      string
			endpoint  string
			wantError bool
		}{
			{name: "remote HTTPS", endpoint: "https://langfuse.example.com"},
			{name: "localhost HTTP", endpoint: "http://localhost:3000"},
			{name: "IPv4 loopback HTTP", endpoint: "http://127.0.0.1:3000"},
			{name: "IPv6 loopback HTTP", endpoint: "http://[::1]:3000"},
			{name: "remote domain HTTP", endpoint: "http://langfuse.example.com", wantError: true},
			{name: "remote IP HTTP", endpoint: "http://192.0.2.10:3000", wantError: true},
			{name: "unsupported scheme", endpoint: "ftp://localhost/traces", wantError: true},
			{name: "empty endpoint", endpoint: "", wantError: true},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				err := ValidateEndpoint(tt.endpoint)
				if tt.wantError && err == nil {
					t.Fatalf("ValidateEndpoint(%q) succeeded, want rejection", tt.endpoint)
				}
				if !tt.wantError && err != nil {
					t.Fatalf("ValidateEndpoint(%q) rejected a safe transport: %v", tt.endpoint, err)
				}
			})
		}
	})

	t.Run("remote HTTP is rejected before OTLP export", func(t *testing.T) {
		var requests atomic.Int32
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer proxy.Close()

		t.Setenv("HTTP_PROXY", proxy.URL)
		t.Setenv("http_proxy", proxy.URL)
		t.Setenv("NO_PROXY", "")
		t.Setenv("no_proxy", "")

		manager, err := NewManager(context.Background(), config.ModelTracingConfig{
			Enabled:   true,
			Endpoint:  "http://192.0.2.10:4318",
			PublicKey: "public",
			SecretKey: "secret",
		})
		if err == nil {
			_, span := manager.Tracer().Start(context.Background(), "must-not-export")
			span.End()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = manager.Shutdown(shutdownCtx)
			t.Error("NewManager accepted a remote plaintext endpoint")
		}
		if got := requests.Load(); got != 0 {
			t.Fatalf("remote HTTP rejection emitted %d OTLP requests, want 0", got)
		}
	})

	t.Run("HTTPS keeps standard certificate verification", func(t *testing.T) {
		previousErrorHandler := otel.GetErrorHandler()
		exportErrors := make(chan error, 1)
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
			select {
			case exportErrors <- err:
			default:
			}
		}))
		defer otel.SetErrorHandler(previousErrorHandler)
		var requests atomic.Int32
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		manager, err := NewManager(context.Background(), config.ModelTracingConfig{
			Enabled:   true,
			Endpoint:  server.URL,
			PublicKey: "public",
			SecretKey: "secret",
		})
		if err != nil {
			t.Fatalf("NewManager rejected syntactically valid HTTPS endpoint: %v", err)
		}
		_, span := manager.Tracer().Start(context.Background(), "certificate-check")
		span.End()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = manager.Shutdown(shutdownCtx)
		select {
		case err := <-exportErrors:
			message := strings.ToLower(err.Error())
			if !strings.Contains(message, "certificate") && !strings.Contains(message, "x509") {
				t.Fatalf("HTTPS export failed for an unexpected reason: %v", err)
			}
		default:
			t.Fatal("export through an untrusted TLS certificate reported no verification failure")
		}
		if got := requests.Load(); got != 0 {
			t.Fatalf("untrusted TLS server received %d HTTP requests, want 0", got)
		}
	})

	t.Run("configuration exposes no certificate verification bypass", func(t *testing.T) {
		configType := reflect.TypeOf(config.ModelTracingConfig{})
		for i := 0; i < configType.NumField(); i++ {
			field := configType.Field(i)
			surface := strings.ToLower(field.Name + " " + field.Tag.Get("mapstructure") + " " + field.Tag.Get("json") + " " + field.Tag.Get("yaml"))
			if strings.Contains(surface, "skip") && strings.Contains(surface, "verify") {
				t.Fatalf("ModelTracingConfig exposes certificate verification bypass through %s", field.Name)
			}
		}
	})
}

func TestManagerConcurrentShutdownWaitsForEveryPublishedGeneration(t *testing.T) {
	initialClosed := make(chan struct{})
	nextClosed := make(chan struct{})
	initial := &generation{
		source: ConfigSourceDeployment, fingerprint: "initial",
		shutdown: func(context.Context) error {
			close(initialClosed)
			return nil
		},
	}
	manager := &Manager{active: initial}
	initialHeld := manager.Acquire()
	next := &generation{
		source: ConfigSourceRuntime, version: 1, fingerprint: "next",
		shutdown: func(context.Context) error {
			close(nextClosed)
			return nil
		},
	}
	if err := manager.installGeneration(next); err != nil {
		t.Fatalf("install generation: %v", err)
	}
	nextHeld := manager.Acquire()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	results := make(chan error, 2)
	go func() { results <- manager.Shutdown(shutdownCtx) }()
	go func() { results <- manager.Shutdown(shutdownCtx) }()

	select {
	case err := <-results:
		t.Fatalf("Shutdown returned while both generations were retained: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	nextHeld.Release()
	select {
	case <-nextClosed:
	case <-time.After(time.Second):
		t.Fatal("active generation was not shut down after release")
	}
	select {
	case err := <-results:
		t.Fatalf("Shutdown returned before the retired generation was released: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	initialHeld.Release()
	select {
	case <-initialClosed:
	case <-time.After(time.Second):
		t.Fatal("retired generation was not shut down after release")
	}
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("Shutdown returned an error: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent Shutdown did not wait for all generations")
		}
	}
}
