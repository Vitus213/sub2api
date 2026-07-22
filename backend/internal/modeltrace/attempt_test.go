package modeltrace

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/modeltrace/recording"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestModelTraceProtocolConversion(t *testing.T) {
	clientInput := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"client input"}]}`)
	upstreamInput := []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"upstream input"}]}`)
	upstreamOutput := []byte(`{"content":[{"type":"text","text":"upstream output"}]}`)
	clientOutput := []byte(`{"choices":[{"message":{"role":"assistant","content":"client output"}}]}`)

	spans := runAttemptTrace(t, clientInput, func(c *gin.Context) {
		_, err := c.GetRawData()
		require.NoError(t, err)
		attempt := recording.BeginAttempt(c.Request.Context(), recording.AttemptMetadata{
			Provider:      "anthropic",
			Operation:     "chat",
			ClientModel:   "gpt-4",
			UpstreamModel: "claude-3-5-sonnet",
			AccountID:     41,
			Endpoint:      "https://api.anthropic.example/v1/messages",
		}, upstreamInput)
		attempt.End(recording.AttemptResult{Output: upstreamOutput, HTTPStatus: http.StatusOK})
		c.Data(http.StatusOK, "application/json", clientOutput)
	})

	require.Len(t, spans, 2)
	root := spanNamed(t, spans, rootSpanName)
	attempt := spanNamed(t, spans, "upstream.attempt.1")
	require.Equal(t, root.SpanId, attempt.ParentSpanId)
	require.JSONEq(t, string(clientInput), stringAttribute(t, attributesByKey(root.Attributes), "langfuse.observation.input"))
	require.JSONEq(t, string(clientOutput), stringAttribute(t, attributesByKey(root.Attributes), "langfuse.observation.output"))
	attrs := attributesByKey(attempt.Attributes)
	require.Equal(t, "generation", stringAttribute(t, attrs, "langfuse.observation.type"))
	require.JSONEq(t, string(upstreamInput), stringAttribute(t, attrs, "langfuse.observation.input"))
	require.JSONEq(t, string(upstreamOutput), stringAttribute(t, attrs, "langfuse.observation.output"))
	require.Equal(t, "anthropic", stringAttribute(t, attrs, "gen_ai.provider.name"))
	require.Equal(t, "claude-3-5-sonnet", stringAttribute(t, attrs, "gen_ai.request.model"))
	require.Equal(t, "claude-3-5-sonnet", stringAttribute(t, attrs, "langfuse.observation.model.name"))
	require.Equal(t, int64(1), intAttribute(t, attrs, "modeltrace.attempt.index"))
	require.Equal(t, tracepb.Status_STATUS_CODE_OK, root.Status.Code)
	require.Equal(t, tracepb.Status_STATUS_CODE_OK, attempt.Status.Code)
}

func TestModelTraceContentCaptureRedactsRootAndAttempt(t *testing.T) {
	media := base64.StdEncoding.EncodeToString([]byte("private-image-bytes"))
	clientInput := []byte(fmt.Sprintf(`{"model":"gpt-4","api_key":"client-secret","messages":[{"role":"user","content":[{"type":"image_url","image_url":"data:image/png;base64,%s"}]}]}`, media))
	spans := runAttemptTrace(t, clientInput, func(c *gin.Context) {
		_, err := c.GetRawData()
		require.NoError(t, err)
		attempt := recording.BeginAttempt(c.Request.Context(), recording.AttemptMetadata{
			Provider: "anthropic", Operation: "chat", ClientModel: "gpt-4", UpstreamModel: "claude-secure",
			AccountID: 42, Endpoint: "https://user:password@anthropic.example/v1/messages?api_key=url-secret",
		}, []byte(`{"model":"claude-secure","authorization":"Bearer upstream-secret"}`))
		attempt.End(recording.AttemptResult{
			Output: []byte(`{"access_token":"response-secret","content":"ok"}`), HTTPStatus: http.StatusOK,
		})
		c.Data(http.StatusOK, "application/json", []byte(`{"cookie":"client-response-secret","choices":[]}`))
	})

	require.Len(t, spans, 2)
	rootAttrs := attributesByKey(spanNamed(t, spans, rootSpanName).Attributes)
	attemptAttrs := attributesByKey(spanNamed(t, spans, "upstream.attempt.1").Attributes)
	captured := strings.Join([]string{
		stringAttribute(t, rootAttrs, "langfuse.observation.input"),
		stringAttribute(t, rootAttrs, "langfuse.observation.output"),
		stringAttribute(t, attemptAttrs, "langfuse.observation.input"),
		stringAttribute(t, attemptAttrs, "langfuse.observation.output"),
		stringAttribute(t, attemptAttrs, "server.address"),
	}, "\n")
	for _, secret := range []string{"client-secret", "private-image-bytes", media, "password", "url-secret", "upstream-secret", "response-secret", "client-response-secret"} {
		require.NotContains(t, captured, secret)
	}
	require.Contains(t, captured, redactedValue)
	require.Contains(t, captured, `"fingerprint":"sha256:`)
	require.Contains(t, captured, "https://anthropic.example/v1/messages")
}

func TestModelTraceRootMultipartDefaultOmitsFileContent(t *testing.T) {
	const canary = "multipart-binary-canary-must-not-export"
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", "gpt-image-test"))
	file, err := writer.CreateFormFile("image", "canary.png")
	require.NoError(t, err)
	_, err = file.Write([]byte(canary))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	gin.SetMode(gin.TestMode)
	fake := newFakeOTLPServer(t)
	manager, err := NewManager(context.Background(), config.ModelTracingConfig{
		Enabled: true, Endpoint: fake.server.URL + "/api/public/otel",
		PublicKey: testPublicKey, SecretKey: testSecretKey,
		PromptMaxBytes: 4096, ResponseMaxBytes: 4096,
	})
	require.NoError(t, err)

	router := gin.New()
	router.POST("/v1/images/edits", manager.CandidateMiddleware(), installUsageTestIdentity(), func(c *gin.Context) {
		_, readErr := io.Copy(io.Discard, c.Request.Body)
		require.NoError(t, readErr)
		c.JSON(http.StatusOK, gin.H{"id": "image-edit"})
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body.Bytes()))
	request.Header.Set("Content-Type", writer.FormDataContentType())
	router.ServeHTTP(httptest.NewRecorder(), request)
	shutdownUsageTestManager(t, manager)

	requests, serverErrors := fake.snapshot()
	require.Empty(t, serverErrors)
	root := spanNamed(t, exportedSpans(requests), rootSpanName)
	captured := stringAttribute(t, attributesByKey(root.Attributes), "langfuse.observation.input")
	require.NotContains(t, captured, canary)
	require.Contains(t, captured, `"media_count":1`)
	require.Contains(t, captured, `"media_type":"application/octet-stream"`)
	require.Contains(t, captured, `"fingerprint":"sha256:`)
}

func TestModelTraceAttemptMultipartDefaultOmitsFileContent(t *testing.T) {
	const canary = "attempt-multipart-canary-must-not-export"
	var upstream bytes.Buffer
	writer := multipart.NewWriter(&upstream)
	require.NoError(t, writer.WriteField("model", "gpt-image-test"))
	part, err := writer.CreateFormFile("image", "canary.png")
	require.NoError(t, err)
	_, err = part.Write([]byte(canary))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	spans := runAttemptTrace(t, []byte(`{"model":"gpt-image-test"}`), func(c *gin.Context) {
		_, readErr := c.GetRawData()
		require.NoError(t, readErr)
		attempt := recording.BeginAttempt(c.Request.Context(), recording.AttemptMetadata{
			Provider: "openai", Operation: "edits", UpstreamModel: "gpt-image-test",
			ContentType: writer.FormDataContentType(),
		}, upstream.Bytes())
		attempt.End(recording.AttemptResult{HTTPStatus: http.StatusOK, Output: []byte(`{"ok":true}`)})
		c.JSON(http.StatusOK, gin.H{"id": "image-edit"})
	})

	attempt := spanNamed(t, spans, "upstream.attempt.1")
	captured := stringAttribute(t, attributesByKey(attempt.Attributes), "langfuse.observation.input")
	require.NotContains(t, captured, canary)
	require.Contains(t, captured, `"media_count":1`)
	require.Contains(t, captured, `"fingerprint":"sha256:`)
}

func TestModelTraceFailoverAttempts(t *testing.T) {
	spans := runAttemptTrace(t, []byte(`{"model":"gpt-4"}`), func(c *gin.Context) {
		_, err := c.GetRawData()
		require.NoError(t, err)
		first := recording.BeginAttempt(c.Request.Context(), recording.AttemptMetadata{
			Provider: "anthropic", Operation: "chat", ClientModel: "gpt-4", UpstreamModel: "claude-a",
			AccountID: 51, Endpoint: "https://first.anthropic.example/v1/messages",
		}, []byte(`{"model":"claude-a","messages":["first"]}`))
		first.End(recording.AttemptResult{
			Output: []byte(`{"error":{"message":"overloaded"}}`), HTTPStatus: http.StatusServiceUnavailable,
			Err: errors.New("first upstream overloaded"),
		})
		second := recording.BeginAttempt(c.Request.Context(), recording.AttemptMetadata{
			Provider: "anthropic", Operation: "chat", ClientModel: "gpt-4", UpstreamModel: "claude-b",
			AccountID: 52, Endpoint: "https://second.anthropic.example/v1/messages",
		}, []byte(`{"model":"claude-b","messages":["second"]}`))
		second.End(recording.AttemptResult{Output: []byte(`{"content":"ok"}`), HTTPStatus: http.StatusOK})
		c.Data(http.StatusOK, "application/json", []byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	})

	require.Len(t, spans, 3)
	root := spanNamed(t, spans, rootSpanName)
	first := spanNamed(t, spans, "upstream.attempt.1")
	second := spanNamed(t, spans, "upstream.attempt.2")
	require.Equal(t, root.SpanId, first.ParentSpanId)
	require.Equal(t, root.SpanId, second.ParentSpanId)
	require.Equal(t, tracepb.Status_STATUS_CODE_ERROR, first.Status.Code)
	require.Contains(t, first.Status.Message, "first upstream overloaded")
	require.Equal(t, tracepb.Status_STATUS_CODE_OK, second.Status.Code)
	require.Equal(t, tracepb.Status_STATUS_CODE_OK, root.Status.Code, "successful failover must keep the client root successful")
	require.Less(t, first.StartTimeUnixNano, first.EndTimeUnixNano)
	require.Less(t, second.StartTimeUnixNano, second.EndTimeUnixNano)
	require.Equal(t, int64(51), intAttribute(t, attributesByKey(first.Attributes), "modeltrace.account.id"))
	require.Equal(t, int64(52), intAttribute(t, attributesByKey(second.Attributes), "modeltrace.account.id"))
}

func TestModelTraceAttemptHTTPErrorSetsErrorStatus(t *testing.T) {
	spans := runAttemptTrace(t, []byte(`{"model":"gpt-4"}`), func(c *gin.Context) {
		_, err := c.GetRawData()
		require.NoError(t, err)
		attempt := recording.BeginAttempt(c.Request.Context(), recording.AttemptMetadata{
			Provider: "anthropic", Operation: "chat", ClientModel: "gpt-4", UpstreamModel: "claude-rate-limited",
			AccountID: 53, Endpoint: "https://anthropic.example/v1/messages",
		}, []byte(`{"model":"claude-rate-limited"}`))
		attempt.End(recording.AttemptResult{
			Output: []byte(`{"error":{"message":"rate limited"}}`), HTTPStatus: http.StatusTooManyRequests,
		})
		c.Data(http.StatusOK, "application/json", []byte(`{"fallback":"local"}`))
	})

	require.Len(t, spans, 2)
	attempt := spanNamed(t, spans, "upstream.attempt.1")
	require.Equal(t, tracepb.Status_STATUS_CODE_ERROR, attempt.Status.Code)
	require.Equal(t, "client_error", attempt.Status.Message)
	require.Equal(t, tracepb.Status_STATUS_CODE_OK, spanNamed(t, spans, rootSpanName).Status.Code)
}

func TestModelTraceAllAttemptsFailed(t *testing.T) {
	spans := runAttemptTrace(t, []byte(`{"model":"gpt-4"}`), func(c *gin.Context) {
		_, err := c.GetRawData()
		require.NoError(t, err)
		for _, accountID := range []int64{61, 62} {
			attempt := recording.BeginAttempt(c.Request.Context(), recording.AttemptMetadata{
				Provider: "anthropic", Operation: "chat", ClientModel: "gpt-4", UpstreamModel: "claude-fail",
				AccountID: accountID, Endpoint: "https://anthropic.example/v1/messages",
			}, []byte(`{"model":"claude-fail"}`))
			attempt.End(recording.AttemptResult{
				Output: []byte(`{"error":{"message":"failed"}}`), HTTPStatus: http.StatusBadGateway,
				Err: errors.New("upstream attempt failed"),
			})
		}
		c.Data(http.StatusBadGateway, "application/json", []byte(`{"error":{"message":"all upstream attempts failed"}}`))
	})

	require.Len(t, spans, 3)
	root := spanNamed(t, spans, rootSpanName)
	require.Equal(t, tracepb.Status_STATUS_CODE_ERROR, root.Status.Code)
	for _, name := range []string{"upstream.attempt.1", "upstream.attempt.2"} {
		attempt := spanNamed(t, spans, name)
		require.Equal(t, root.SpanId, attempt.ParentSpanId)
		require.Equal(t, tracepb.Status_STATUS_CODE_ERROR, attempt.Status.Code)
		attrs := attributesByKey(attempt.Attributes)
		require.NotContains(t, attrs, "gen_ai.usage.input_tokens", "unknown usage must remain absent")
		require.NotContains(t, attrs, "gen_ai.usage.output_tokens", "unknown usage must remain absent")
		require.NotContains(t, attrs, "langfuse.observation.usage_details", "unknown usage must not be serialized as zeros")
	}
}

func TestModelTraceAttemptClosedBeforeEOFIsError(t *testing.T) {
	spans := runAttemptTrace(t, []byte(`{"model":"gpt-4"}`), func(c *gin.Context) {
		_, err := c.GetRawData()
		require.NoError(t, err)
		attempt := recording.BeginAttempt(c.Request.Context(), recording.AttemptMetadata{
			Provider: "anthropic", Operation: "chat", ClientModel: "gpt-4", UpstreamModel: "claude-partial",
			AccountID: 63, Endpoint: "https://anthropic.example/v1/messages",
		}, []byte(`{"model":"claude-partial"}`))
		body := attempt.ObserveResponse(http.StatusOK, io.NopCloser(strings.NewReader("partial-output")))
		buf := make([]byte, len("partial"))
		_, err = io.ReadFull(body, buf)
		require.NoError(t, err)
		require.NoError(t, body.Close())
		c.Data(http.StatusOK, "application/json", []byte(`{"choices":[]}`))
	})

	require.Len(t, spans, 2)
	attempt := spanNamed(t, spans, "upstream.attempt.1")
	require.Equal(t, tracepb.Status_STATUS_CODE_ERROR, attempt.Status.Code)
	require.Equal(t, errUpstreamResponseIncomplete.Error(), attempt.Status.Message)
	require.Equal(t, "partial", stringAttribute(t, attributesByKey(attempt.Attributes), "langfuse.observation.output"))
}

func runAttemptTrace(t *testing.T, clientInput []byte, handler gin.HandlerFunc) []*tracepb.Span {
	t.Helper()
	gin.SetMode(gin.TestMode)
	fake := newFakeOTLPServer(t)
	manager, err := NewManager(context.Background(), config.ModelTracingConfig{
		Enabled: true, Endpoint: fake.server.URL + "/api/public/otel",
		PublicKey: testPublicKey, SecretKey: testSecretKey,
		PromptMaxBytes: 4096, ResponseMaxBytes: 4096,
	})
	require.NoError(t, err)

	groupID := int64(19)
	router := gin.New()
	router.POST("/v1/chat/completions",
		manager.CandidateMiddleware(),
		func(c *gin.Context) {
			servermiddleware.SetOpsFallbackAPIKey(c, &service.APIKey{
				ID: 71, UserID: 73, User: &service.User{ID: 73},
				GroupID: &groupID, Group: &service.Group{ID: groupID},
			})
			c.Next()
		},
		handler,
	)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(clientInput)))

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, manager.Shutdown(shutdownCtx))
	requests, serverErrors := fake.snapshot()
	require.Empty(t, serverErrors)
	return exportedSpans(requests)
}
