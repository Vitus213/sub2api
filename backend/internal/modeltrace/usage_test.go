package modeltrace

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/modeltrace/recording"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestModelTraceIdentityAndSession(t *testing.T) {
	manager, fake := newUsageTestManager(t)
	router := gin.New()
	router.POST("/v1/responses", manager.CandidateMiddleware(), installUsageTestIdentity(), func(c *gin.Context) {
		_, _ = c.GetRawData()
		c.JSON(http.StatusOK, gin.H{"id": "resp_session"})
	})

	for _, requestID := range []string{"request-1", "request-2"} {
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-test","session_id":"shared-session"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Client-Request-ID", requestID)
		router.ServeHTTP(httptest.NewRecorder(), request)
	}
	shutdownUsageTestManager(t, manager)
	requests, serverErrors := fake.snapshot()
	require.Empty(t, serverErrors)
	spans := exportedSpans(requests)
	require.Len(t, spans, 2)
	for _, span := range spans {
		attrs := attributesByKey(span.Attributes)
		require.Equal(t, "73", stringAttribute(t, attrs, "langfuse.user.id"))
		require.Equal(t, int64(71), intAttribute(t, attrs, "langfuse.trace.metadata.api_key_id"))
		require.Equal(t, int64(19), intAttribute(t, attrs, "langfuse.trace.metadata.group_id"))
		require.Equal(t, "shared-session", stringAttribute(t, attrs, "langfuse.session.id"))
	}
	require.NotEqual(t, spans[0].TraceId, spans[1].TraceId)
}

func TestModelTraceUsageAndCost(t *testing.T) {
	manager, fake := newUsageTestManager(t)
	router := gin.New()
	router.POST("/v1/responses", manager.CandidateMiddleware(), installUsageTestIdentity(), func(c *gin.Context) {
		_, _ = c.GetRawData()
		durationMs := 321
		firstTokenMs := 45
		attempt := recording.BeginAttempt(c.Request.Context(), recording.AttemptMetadata{
			Provider: "openai", Operation: "responses", UpstreamModel: "gpt-upstream", AccountID: 29,
		}, []byte(`{"model":"gpt-upstream"}`))
		attempt.End(recording.AttemptResult{HTTPStatus: http.StatusOK, Output: []byte(`{"id":"resp_usage"}`)})
		recording.RecordUsage(c.Request.Context(), recording.UsageFacts{
			Known: true, RequestID: "usage-request-1", Model: "gpt-upstream", AccountID: 29,
			InputTokens: 11, OutputTokens: 7, CacheCreationTokens: 2, CacheReadTokens: 3,
			ImageInputTokens: 5, ImageOutputTokens: 13,
			InputCost: 0.11, OutputCost: 0.07, CacheCreationCost: 0.02, CacheReadCost: 0.03,
			ImageInputCost: 0.05, ImageOutputCost: 0.13,
			TotalCost: 0.23, ActualCost: 0.19, DurationMs: &durationMs, FirstTokenMs: &firstTokenMs,
		})
		c.JSON(http.StatusOK, gin.H{"id": "resp_usage"})
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-test"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(httptest.NewRecorder(), request)
	shutdownUsageTestManager(t, manager)
	requests, serverErrors := fake.snapshot()
	require.Empty(t, serverErrors)
	spans := exportedSpans(requests)
	require.Len(t, spans, 2)
	usage := spanNamed(t, spans, "upstream.attempt.1")
	attrs := attributesByKey(usage.Attributes)
	require.Equal(t, int64(11), intAttribute(t, attrs, "gen_ai.usage.input_tokens"))
	require.Equal(t, int64(7), intAttribute(t, attrs, "gen_ai.usage.output_tokens"))
	require.JSONEq(t, `{"input":11,"output":7,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"image_input_tokens":5,"image_output_tokens":13,"total":23}`, stringAttribute(t, attrs, "langfuse.observation.usage_details"))
	require.JSONEq(t, `{"input":0.11,"output":0.07,"cache_creation":0.02,"cache_read":0.03,"image_input":0.05,"image_output":0.13,"total":0.23,"actual":0.19}`, stringAttribute(t, attrs, "langfuse.observation.cost_details"))
	require.Equal(t, "gpt-upstream", stringAttribute(t, attrs, "gen_ai.response.model"))
	require.Equal(t, "usage-request-1", stringAttribute(t, attrs, "gen_ai.response.id"))
	require.Equal(t, "generation", stringAttribute(t, attrs, "langfuse.observation.type"))
	require.NotContains(t, attrs, "langfuse.trace.metadata.request_id")
	require.Equal(t, int64(29), intAttribute(t, attrs, "modeltrace.account.id"))
	require.Equal(t, int64(321), intAttribute(t, attrs, "modeltrace.duration_ms"))
	require.Equal(t, int64(45), intAttribute(t, attrs, firstOutputMsAttribute))
	require.Equal(t, "73", stringAttribute(t, attrs, "langfuse.user.id"))
}

func TestModelTraceUsageBelongsOnlyToSuccessfulAttempt(t *testing.T) {
	manager, fake := newUsageTestManager(t)
	router := gin.New()
	router.POST("/v1/responses", manager.CandidateMiddleware(), installUsageTestIdentity(), func(c *gin.Context) {
		_, err := c.GetRawData()
		require.NoError(t, err)
		failed := recording.BeginAttempt(c.Request.Context(), recording.AttemptMetadata{
			Provider: "openai", Operation: "responses", UpstreamModel: "gpt-failed", AccountID: 28,
		}, []byte(`{"model":"gpt-failed"}`))
		failed.End(recording.AttemptResult{HTTPStatus: http.StatusServiceUnavailable, Output: []byte(`{"error":"overloaded"}`)})
		succeeded := recording.BeginAttempt(c.Request.Context(), recording.AttemptMetadata{
			Provider: "openai", Operation: "responses", UpstreamModel: "gpt-upstream", AccountID: 29,
		}, []byte(`{"model":"gpt-upstream"}`))
		succeeded.End(recording.AttemptResult{HTTPStatus: http.StatusOK, Output: []byte(`{"id":"resp_usage"}`)})
		recording.RecordUsage(c.Request.Context(), recording.UsageFacts{
			Known: true, RequestID: "usage-request-1", AccountID: 29,
			InputTokens: 11, OutputTokens: 7, CacheCreationTokens: 2, CacheReadTokens: 3,
			InputCost: 0.11, OutputCost: 0.07, CacheCreationCost: 0.02, CacheReadCost: 0.03,
			TotalCost: 0.23, ActualCost: 0.19,
		})
		c.JSON(http.StatusOK, gin.H{"id": "resp_usage"})
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-test"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(httptest.NewRecorder(), request)
	shutdownUsageTestManager(t, manager)
	requests, serverErrors := fake.snapshot()
	require.Empty(t, serverErrors)
	spans := exportedSpans(requests)
	require.Len(t, spans, 3)

	failed := spanNamed(t, spans, "upstream.attempt.1")
	succeeded := spanNamed(t, spans, "upstream.attempt.2")
	failedAttrs := attributesByKey(failed.Attributes)
	succeededAttrs := attributesByKey(succeeded.Attributes)
	require.NotContains(t, failedAttrs, "gen_ai.usage.input_tokens")
	require.NotContains(t, failedAttrs, "langfuse.observation.usage_details")
	require.Equal(t, int64(11), intAttribute(t, succeededAttrs, "gen_ai.usage.input_tokens"))
	require.Equal(t, int64(7), intAttribute(t, succeededAttrs, "gen_ai.usage.output_tokens"))
	require.JSONEq(t, `{"input":11,"output":7,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"total":23}`, stringAttribute(t, succeededAttrs, "langfuse.observation.usage_details"))
	require.JSONEq(t, `{"input":0.11,"output":0.07,"cache_creation":0.02,"cache_read":0.03,"total":0.23,"actual":0.19}`, stringAttribute(t, succeededAttrs, "langfuse.observation.cost_details"))
	require.Less(t, failed.StartTimeUnixNano, failed.EndTimeUnixNano)
	require.Less(t, succeeded.StartTimeUnixNano, succeeded.EndTimeUnixNano)
	for _, span := range spans {
		require.NotEqual(t, "usage.final", span.Name)
	}
}

func TestModelTraceDetachedUsageUpdatesSuccessfulAttemptAfterRequestFinish(t *testing.T) {
	manager, fake := newUsageTestManager(t)
	var detached context.Context
	var release func()
	router := gin.New()
	router.POST("/v1/responses", manager.CandidateMiddleware(), installUsageTestIdentity(), func(c *gin.Context) {
		_, err := c.GetRawData()
		require.NoError(t, err)
		attempt := recording.BeginAttempt(c.Request.Context(), recording.AttemptMetadata{
			Provider: "openai", Operation: "responses", UpstreamModel: "gpt-upstream", AccountID: 29,
		}, []byte(`{"model":"gpt-upstream"}`))
		attempt.End(recording.AttemptResult{HTTPStatus: http.StatusOK, Output: []byte(`{"id":"resp_async_usage"}`)})
		detached, release = recording.Detach(c.Request.Context(), context.Background())
		c.JSON(http.StatusOK, gin.H{"id": "resp_async_usage"})
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-test"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(httptest.NewRecorder(), request)
	require.NotNil(t, detached)
	require.NotNil(t, release)
	recording.RecordUsage(detached, recording.UsageFacts{
		Known: true, RequestID: "usage-after-root", Model: "gpt-upstream", AccountID: 29,
		InputTokens: 3, OutputTokens: 2, TotalCost: 0.05, ActualCost: 0.04,
	})
	release()
	shutdownUsageTestManager(t, manager)

	requests, serverErrors := fake.snapshot()
	require.Empty(t, serverErrors)
	spans := exportedSpans(requests)
	require.Len(t, spans, 2)
	attempt := spanNamed(t, spans, "upstream.attempt.1")
	attrs := attributesByKey(attempt.Attributes)
	require.Equal(t, int64(3), intAttribute(t, attrs, "gen_ai.usage.input_tokens"))
	require.Equal(t, "usage-after-root", stringAttribute(t, attrs, "gen_ai.response.id"))
	require.Less(t, attempt.StartTimeUnixNano, attempt.EndTimeUnixNano)
}

func TestModelTraceDetachedUsageReleaseEndsSuccessfulAttemptWithoutFacts(t *testing.T) {
	manager, fake := newUsageTestManager(t)
	var release func()
	router := gin.New()
	router.POST("/v1/responses", manager.CandidateMiddleware(), installUsageTestIdentity(), func(c *gin.Context) {
		_, err := c.GetRawData()
		require.NoError(t, err)
		attempt := recording.BeginAttempt(c.Request.Context(), recording.AttemptMetadata{
			Provider: "openai", Operation: "responses", UpstreamModel: "gpt-upstream", AccountID: 29,
		}, []byte(`{"model":"gpt-upstream"}`))
		attempt.End(recording.AttemptResult{HTTPStatus: http.StatusOK, Output: []byte(`{"id":"resp_no_usage"}`)})
		_, release = recording.Detach(c.Request.Context(), context.Background())
		c.JSON(http.StatusOK, gin.H{"id": "resp_no_usage"})
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-test"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(httptest.NewRecorder(), request)
	require.NotNil(t, release)
	release()
	shutdownUsageTestManager(t, manager)

	requests, serverErrors := fake.snapshot()
	require.Empty(t, serverErrors)
	spans := exportedSpans(requests)
	require.Len(t, spans, 2)
	attempt := spanNamed(t, spans, "upstream.attempt.1")
	attrs := attributesByKey(attempt.Attributes)
	require.NotContains(t, attrs, "gen_ai.usage.input_tokens")
	require.Less(t, attempt.StartTimeUnixNano, attempt.EndTimeUnixNano)
}

func TestModelTraceUnknownUsage(t *testing.T) {
	manager, fake := newUsageTestManager(t)
	router := gin.New()
	router.POST("/v1/responses", manager.CandidateMiddleware(), installUsageTestIdentity(), func(c *gin.Context) {
		_, _ = c.GetRawData()
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream failed"})
	})
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-test"}`)))
	shutdownUsageTestManager(t, manager)
	requests, serverErrors := fake.snapshot()
	require.Empty(t, serverErrors)
	spans := exportedSpans(requests)
	require.Len(t, spans, 1)
	attrs := attributesByKey(spans[0].Attributes)
	require.NotContains(t, attrs, "gen_ai.usage.input_tokens")
	require.NotContains(t, attrs, "langfuse.observation.usage_details")
	require.NotContains(t, attrs, "langfuse.observation.cost_details")
}

func installUsageTestIdentity() gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := int64(19)
		servermiddleware.SetOpsFallbackAPIKey(c, &service.APIKey{
			ID: 71, UserID: 73, User: &service.User{ID: 73},
			GroupID: &groupID, Group: &service.Group{ID: groupID},
		})
		c.Next()
	}
}

func newUsageTestManager(t *testing.T) (*Manager, *fakeOTLPServer) {
	t.Helper()
	fake := newFakeOTLPServer(t)
	manager, err := NewManager(context.Background(), config.ModelTracingConfig{
		Enabled: true, Endpoint: fake.server.URL + "/api/public/otel", PublicKey: testPublicKey, SecretKey: testSecretKey,
		PromptMaxBytes: 4096, ResponseMaxBytes: 4096,
	})
	require.NoError(t, err)
	return manager, fake
}

func shutdownUsageTestManager(t *testing.T, manager *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, manager.Shutdown(ctx))
}
