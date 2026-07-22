package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/modeltrace/recording"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestModelTraceResponsesConversionSSEComplete(t *testing.T) {
	capture := &streamOutcomeCapture{}
	c, rec := modelTraceResponsesContext(capture)
	resp := &http.Response{Header: http.Header{"x-request-id": []string{"rid_complete"}}, Body: io.NopCloser(strings.NewReader(strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-test","usage":{"input_tokens":2}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")))}

	result, err := (&GatewayService{}).handleResponsesStreamingResponse(resp, c, "gpt-client", "claude-test", nil, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), "response.completed")
	require.True(t, capture.started)
	require.Len(t, capture.outcomes, 1)
	require.Equal(t, recording.StreamCompleted, capture.outcomes[0].Status)
	require.NoError(t, capture.outcomes[0].Err)
}

func TestModelTraceResponsesConversionSSEUpstreamError(t *testing.T) {
	capture := &streamOutcomeCapture{}
	c, rec := modelTraceResponsesContext(capture)
	upstreamErr := errors.New("responses conversion upstream reset")
	resp := &http.Response{Header: http.Header{"x-request-id": []string{"rid_error"}}, Body: &modelTracePartialReadCloser{
		data: []byte(strings.Join([]string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-test","usage":{"input_tokens":2}}}`,
			``,
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
			``,
		}, "\n")),
		err: upstreamErr,
	}}

	result, err := (&GatewayService{}).handleResponsesStreamingResponse(resp, c, "gpt-client", "claude-test", nil, time.Now())
	require.NoError(t, err, "existing response semantics stay fail-open after partial output")
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), "partial")
	require.True(t, capture.started)
	require.Len(t, capture.outcomes, 1)
	require.Equal(t, recording.StreamError, capture.outcomes[0].Status)
	require.Equal(t, "upstream_read", capture.outcomes[0].ErrorStage)
	require.ErrorIs(t, capture.outcomes[0].Err, upstreamErr)
}

func TestModelTraceResponsesConversionSSECancelled(t *testing.T) {
	capture := &streamOutcomeCapture{}
	c, _ := modelTraceResponsesContext(capture)
	cancelled, cancel := context.WithCancel(c.Request.Context())
	cancel()
	c.Request = c.Request.WithContext(cancelled)
	resp := &http.Response{Header: http.Header{"x-request-id": []string{"rid_cancelled"}}, Body: io.NopCloser(strings.NewReader(""))}

	result, err := (&GatewayService{}).handleResponsesStreamingResponse(resp, c, "gpt-client", "claude-test", nil, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, capture.started)
	require.Len(t, capture.outcomes, 1)
	require.Equal(t, recording.StreamCancelled, capture.outcomes[0].Status)
	require.Equal(t, "upstream_read", capture.outcomes[0].ErrorStage)
	require.ErrorIs(t, capture.outcomes[0].Err, context.Canceled)
}

func TestModelTraceChatConversionSSEComplete(t *testing.T) {
	capture := &streamOutcomeCapture{}
	c, rec := modelTraceChatContext(capture)
	resp := &http.Response{Header: http.Header{"x-request-id": []string{"rid_chat_complete"}}, Body: io.NopCloser(strings.NewReader(strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_chat_1","type":"message","role":"assistant","content":[],"model":"claude-test","usage":{"input_tokens":2}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")))}

	result, err := (&GatewayService{}).handleCCStreamingFromAnthropic(resp, c, "gpt-client", "claude-test", nil, time.Now(), false)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), "hello")
	require.Contains(t, rec.Body.String(), "data: [DONE]")
	require.True(t, capture.started)
	require.Len(t, capture.outcomes, 1)
	require.Equal(t, recording.StreamCompleted, capture.outcomes[0].Status)
	require.NoError(t, capture.outcomes[0].Err)
}

func TestModelTraceChatConversionSSEUpstreamError(t *testing.T) {
	capture := &streamOutcomeCapture{}
	c, rec := modelTraceChatContext(capture)
	upstreamErr := errors.New("chat conversion upstream reset")
	resp := &http.Response{Header: http.Header{"x-request-id": []string{"rid_chat_error"}}, Body: &modelTracePartialReadCloser{
		data: []byte(strings.Join([]string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_chat_1","type":"message","role":"assistant","content":[],"model":"claude-test","usage":{"input_tokens":2}}}`,
			``,
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
			``,
		}, "\n")),
		err: upstreamErr,
	}}

	result, err := (&GatewayService{}).handleCCStreamingFromAnthropic(resp, c, "gpt-client", "claude-test", nil, time.Now(), false)
	require.NoError(t, err, "existing Chat response semantics stay fail-open after partial output")
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), "partial")
	require.True(t, capture.started)
	require.Len(t, capture.outcomes, 1)
	require.Equal(t, recording.StreamError, capture.outcomes[0].Status)
	require.Equal(t, "upstream_read", capture.outcomes[0].ErrorStage)
	require.ErrorIs(t, capture.outcomes[0].Err, upstreamErr)
}

func TestModelTraceChatConversionSSECancelled(t *testing.T) {
	capture := &streamOutcomeCapture{}
	c, _ := modelTraceChatContext(capture)
	cancelled, cancel := context.WithCancel(c.Request.Context())
	cancel()
	c.Request = c.Request.WithContext(cancelled)
	resp := &http.Response{Header: http.Header{"x-request-id": []string{"rid_chat_cancelled"}}, Body: io.NopCloser(strings.NewReader(""))}

	result, err := (&GatewayService{}).handleCCStreamingFromAnthropic(resp, c, "gpt-client", "claude-test", nil, time.Now(), false)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, capture.started)
	require.Len(t, capture.outcomes, 1)
	require.Equal(t, recording.StreamCancelled, capture.outcomes[0].Status)
	require.Equal(t, "upstream_read", capture.outcomes[0].ErrorStage)
	require.ErrorIs(t, capture.outcomes[0].Err, context.Canceled)
}

func modelTraceResponsesContext(capture *streamOutcomeCapture) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	ctx := recording.WithRecorder(context.Background(), capture)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	return c, rec
}

func modelTraceChatContext(capture *streamOutcomeCapture) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	ctx := recording.WithRecorder(context.Background(), capture)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	return c, rec
}

type streamOutcomeCapture struct {
	started  bool
	outcomes []recording.StreamOutcome
}

func (c *streamOutcomeCapture) BeginAttempt(recording.AttemptMetadata, []byte) recording.Attempt {
	return modelTraceNoopAttempt{}
}
func (c *streamOutcomeCapture) BeginStream() { c.started = true }
func (c *streamOutcomeCapture) EndStream(outcome recording.StreamOutcome) {
	c.outcomes = append(c.outcomes, outcome)
}

type modelTraceNoopAttempt struct{}

func (modelTraceNoopAttempt) ObserveResponse(_ int, body io.ReadCloser) io.ReadCloser { return body }
func (modelTraceNoopAttempt) End(recording.AttemptResult)                             {}

type modelTracePartialReadCloser struct {
	data []byte
	err  error
	done bool
}

func (r *modelTracePartialReadCloser) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	if !r.done {
		r.done = true
		return 0, r.err
	}
	return 0, io.EOF
}
func (r *modelTracePartialReadCloser) Close() error { return nil }
