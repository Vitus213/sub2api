package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/modeltrace/recording"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type captureAttemptRecorder struct {
	metadata recording.AttemptMetadata
	input    []byte
	attempt  *captureAttempt
}

func (r *captureAttemptRecorder) BeginAttempt(metadata recording.AttemptMetadata, input []byte) recording.Attempt {
	r.metadata = metadata
	r.input = bytes.Clone(input)
	r.attempt = &captureAttempt{}
	return r.attempt
}

type captureAttempt struct {
	statusCode int
	response   bytes.Buffer
	result     recording.AttemptResult
}

func (a *captureAttempt) ObserveResponse(statusCode int, body io.ReadCloser) io.ReadCloser {
	a.statusCode = statusCode
	return &captureResponseReadCloser{ReadCloser: body, output: &a.response}
}

func (a *captureAttempt) End(result recording.AttemptResult) { a.result = result }

type captureResponseReadCloser struct {
	io.ReadCloser
	output *bytes.Buffer
}

func (r *captureResponseReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		_, _ = r.output.Write(p[:n])
	}
	return n, err
}

func TestForwardAsChatCompletionsRecordsFinalAnthropicWireAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)

	clientBody := []byte(`{"model":"gpt-client","stream":false,"messages":[{"role":"user","content":"hello wire"}]}`)
	upstreamSSE := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_trace","type":"message","role":"assistant","content":[],"model":"claude-mapped","stop_reason":"","usage":{"input_tokens":7}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"wire response"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		``,
	}, "\n")
	upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamSSE)),
	}}
	traceCapture := &captureAttemptRecorder{}
	ctx := recording.WithRecorder(context.Background(), traceCapture)

	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(clientBody)).WithContext(ctx)
	account := &Account{
		ID:          401,
		Name:        "attempt-trace-test",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":       "upstream-secret",
			"base_url":      "https://api.anthropic.com",
			"model_mapping": map[string]any{"gpt-client": "claude-mapped"},
		},
		Status:      StatusActive,
		Schedulable: true,
	}
	service := &GatewayService{
		cfg:                  &config.Config{},
		httpUpstream:         upstream,
		tlsFPProfileService:  &TLSFingerprintProfileService{},
		responseHeaderFilter: compileResponseHeaderFilter(&config.Config{}),
		rateLimitService:     &RateLimitService{},
	}

	result, err := service.ForwardAsChatCompletions(ctx, c, account, clientBody, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, traceCapture.attempt)
	require.Equal(t, upstream.lastBody, traceCapture.input, "generation input must be the final bytes sent upstream")
	require.NotEqual(t, clientBody, traceCapture.input, "client Chat Completions input must not masquerade as Anthropic wire input")
	require.Equal(t, "claude-mapped", gjson.GetBytes(traceCapture.input, "model").String())
	require.True(t, gjson.GetBytes(traceCapture.input, "stream").Bool())
	require.Equal(t, int64(401), traceCapture.metadata.AccountID)
	require.Equal(t, PlatformAnthropic, traceCapture.metadata.Provider)
	require.Equal(t, "gpt-client", traceCapture.metadata.ClientModel)
	require.Equal(t, "claude-mapped", traceCapture.metadata.UpstreamModel)
	require.Equal(t, "https://api.anthropic.com/v1/messages", strings.TrimSuffix(traceCapture.metadata.Endpoint, "?beta=true"))
	require.Equal(t, http.StatusOK, traceCapture.attempt.statusCode)
	require.Equal(t, upstreamSSE, traceCapture.attempt.response.String(), "generation output must be the raw Anthropic SSE consumed by the converter")
	require.Contains(t, response.Body.String(), `"content":"wire response"`)
	require.NotContains(t, traceCapture.input, "upstream-secret")
}
