package repository

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/modeltrace/recording"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestHTTPUpstreamModelTraceAttempt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.JSONEq(t, `{"model":"gpt-upstream","input":"wire input"}`, string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","model":"gpt-upstream","output":"wire output"}`))
	}))
	defer server.Close()

	capture := &httpAttemptCapture{}
	ctx := recording.WithRecorder(context.Background(), capture)
	upstream := NewHTTPUpstream(nil)
	for range 2 {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-upstream","input":"wire input"}`)))
		require.NoError(t, err)
		resp, err := upstream.Do(req, "", 29, 1)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.JSONEq(t, `{"id":"resp_1","model":"gpt-upstream","output":"wire output"}`, string(body))
	}

	capture.mu.Lock()
	defer capture.mu.Unlock()
	require.Len(t, capture.attempts, 2, "every actual retry/send must have its own attempt")
	for _, attempt := range capture.attempts {
		require.Equal(t, int64(29), attempt.metadata.AccountID)
		require.Equal(t, "gpt-upstream", attempt.metadata.UpstreamModel)
		require.Equal(t, "responses", attempt.metadata.Operation)
		require.Equal(t, server.URL+"/v1/responses", attempt.metadata.Endpoint)
		require.JSONEq(t, `{"model":"gpt-upstream","input":"wire input"}`, string(attempt.input))
		require.Equal(t, http.StatusOK, attempt.statusCode)
		require.JSONEq(t, `{"id":"resp_1","model":"gpt-upstream","output":"wire output"}`, string(attempt.output))
		require.NoError(t, attempt.result.Err)
	}
}

func TestHTTPUpstreamModelTraceUsesExplicitProfileForCustomEndpoint(t *testing.T) {
	capture := &httpAttemptCapture{}
	ctx := recording.WithRecorder(context.Background(), capture)
	ctx = service.WithHTTPUpstreamProfile(ctx, service.HTTPUpstreamProfileOpenAI)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://llm.example/v1/images/edits", bytes.NewReader([]byte("multipart-safe-snapshot")))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=trace-boundary")
	client := httpClientWithModelTrace(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewBufferString(`{"ok":true}`)),
			Request:    req,
		}, nil
	})}, 29)
	resp, err := client.Do(req)
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	capture.mu.Lock()
	defer capture.mu.Unlock()
	require.Len(t, capture.attempts, 1)
	metadata := capture.attempts[0].metadata
	require.Equal(t, "openai", metadata.Provider)
	require.Equal(t, "edits", metadata.Operation)
	require.Equal(t, "multipart/form-data; boundary=trace-boundary", metadata.ContentType)
}

func TestHTTPUpstreamModelTraceAttemptWithTLSPath(t *testing.T) {
	capture := &httpAttemptCapture{}
	ctx := recording.WithRecorder(context.Background(), capture)
	requestBody := []byte(`{"model":"claude-mapped"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.example/v1/messages", bytes.NewReader(requestBody))
	require.NoError(t, err)

	profile := &tlsfingerprint.Profile{Name: "test-profile"}
	upstream, ok := NewHTTPUpstream(nil).(*httpUpstreamService)
	require.True(t, ok)
	entry, err := upstream.getClientEntryWithTLS("", 31, 1, profile, service.HTTPUpstreamProfileDefault, false, false)
	require.NoError(t, err)
	entry.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewBufferString(`{"ok":true}`)),
			Request:    req,
		}, nil
	})

	resp, err := upstream.DoWithTLS(req, "", 31, 1, profile)
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	capture.mu.Lock()
	defer capture.mu.Unlock()
	require.Len(t, capture.attempts, 1)
	require.Equal(t, int64(31), capture.attempts[0].metadata.AccountID)
	require.Equal(t, "https://upstream.example/v1/messages", capture.attempts[0].metadata.Endpoint)
	require.Equal(t, http.StatusOK, capture.attempts[0].statusCode)
}

func TestHTTPUpstreamModelTraceAttemptRecordsGrokAccessDeniedFallbackSends(t *testing.T) {
	capture := &httpAttemptCapture{}
	ctx := recording.WithRecorder(context.Background(), capture)
	requestBody := []byte(`{"model":"grok-4","input":"wire input"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+grokCLIProxyHost+"/v1/responses", bytes.NewReader(requestBody))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")

	var sends int
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		sends++
		if sends == 1 {
			require.Equal(t, grokCLIProxyHost, req.URL.Hostname())
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewBufferString(`{"code":"permission_denied","error":"Access denied"}`)),
				Request:    req,
			}, nil
		}
		require.Equal(t, grokOfficialAPIHost, req.URL.Hostname())
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewBufferString(`{"id":"resp_fallback"}`)),
			Request:    req,
		}, nil
	})
	client := httpClientWithModelTrace(&http.Client{Transport: base}, 29)
	client = httpClientWithGrokAccessDeniedFallback(client)
	resp, err := client.Do(req)
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, 2, sends)

	capture.mu.Lock()
	defer capture.mu.Unlock()
	require.Len(t, capture.attempts, 2)
	require.Equal(t, "https://"+grokCLIProxyHost+"/v1/responses", capture.attempts[0].metadata.Endpoint)
	require.Equal(t, http.StatusForbidden, capture.attempts[0].statusCode)
	require.Equal(t, "https://"+grokOfficialAPIHost+"/v1/responses", capture.attempts[1].metadata.Endpoint)
	require.Equal(t, http.StatusOK, capture.attempts[1].statusCode)
}

func TestHTTPUpstreamModelTraceSourceHookRedirectRecordsEachSend(t *testing.T) {
	requestBody := []byte(`{"model":"claude-mapped","messages":[{"role":"user","content":"hello"}]}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/first":
			http.Redirect(w, r, "/second", http.StatusTemporaryRedirect)
		case "/second":
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.Equal(t, requestBody, body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_redirect"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	capture := &httpAttemptCapture{}
	ctx := recording.WithRecorder(context.Background(), capture)
	ctx = recording.WithAttemptSource(ctx, recording.AttemptSource{
		Metadata: recording.AttemptMetadata{
			Provider: "anthropic", Operation: "chat", ClientModel: "gpt-client",
			UpstreamModel: "claude-mapped", AccountID: 29,
		},
		Input: requestBody,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/first", bytes.NewReader(requestBody))
	require.NoError(t, err)
	client := httpClientWithModelTrace(&http.Client{}, 29)
	resp, err := client.Do(req)
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	capture.mu.Lock()
	defer capture.mu.Unlock()
	require.Len(t, capture.attempts, 2, "307 redirect performs two physical sends")
	require.Equal(t, server.URL+"/first", capture.attempts[0].metadata.Endpoint)
	require.Equal(t, http.StatusTemporaryRedirect, capture.attempts[0].statusCode)
	require.Equal(t, server.URL+"/second", capture.attempts[1].metadata.Endpoint)
	require.Equal(t, http.StatusOK, capture.attempts[1].statusCode)
	for _, attempt := range capture.attempts {
		require.Equal(t, "anthropic", attempt.metadata.Provider)
		require.Equal(t, "chat", attempt.metadata.Operation)
		require.Equal(t, "gpt-client", attempt.metadata.ClientModel)
		require.Equal(t, "claude-mapped", attempt.metadata.UpstreamModel)
		require.Equal(t, requestBody, attempt.input)
	}
}

func TestHTTPModelTraceURLMetadataProtocolMatrix(t *testing.T) {
	cases := []struct {
		name, target, operation, provider, model string
	}{
		{name: "Responses", target: "https://api.openai.com/v1/responses", operation: "responses", provider: "openai"},
		{name: "Anthropic Messages", target: "https://api.anthropic.com/v1/messages", operation: "messages", provider: "anthropic"},
		{name: "Gemini generateContent", target: "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-pro:generateContent", operation: "generateContent", provider: "gemini", model: "gemini-2.5-pro"},
		{name: "Gemini streamGenerateContent", target: "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse", operation: "streamGenerateContent", provider: "gemini", model: "gemini-2.5-flash"},
		{name: "Gemini countTokens", target: "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:countTokens", operation: "countTokens", provider: "gemini", model: "gemini-2.5-flash"},
		{name: "Embeddings", target: "https://api.openai.com/v1/embeddings", operation: "embeddings", provider: "openai"},
		{name: "Image generation", target: "https://api.openai.com/v1/images/generations", operation: "generations", provider: "openai"},
		{name: "Video edit", target: "https://api.x.ai/v1/videos/edits", operation: "edits", provider: "grok"},
		{name: "Bedrock stream", target: "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-5-sonnet/invoke-with-response-stream", operation: "invoke-with-response-stream", provider: "bedrock", model: "anthropic.claude-3-5-sonnet"},
		{name: "Custom image generation", target: "https://llm.example/v1/images/generations", operation: "generations", provider: "openai"},
		{name: "Custom image edit", target: "https://llm.example/v1/images/edits", operation: "edits", provider: "openai"},
		{name: "Custom video extension", target: "https://llm.example/v1/videos/extensions", operation: "extensions", provider: "grok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, tc.target, nil)
			require.NoError(t, err)
			operation, provider, model := httpModelTraceURLMetadata(req.URL)
			require.Equal(t, tc.operation, operation)
			require.Equal(t, tc.provider, provider)
			require.Equal(t, tc.model, model)
		})
	}
}

type httpAttemptCapture struct {
	mu       sync.Mutex
	attempts []*httpAttemptCaptureItem
}

type httpAttemptCaptureItem struct {
	metadata   recording.AttemptMetadata
	input      []byte
	statusCode int
	output     []byte
	result     recording.AttemptResult
}

func (c *httpAttemptCapture) BeginAttempt(metadata recording.AttemptMetadata, input []byte) recording.Attempt {
	item := &httpAttemptCaptureItem{metadata: metadata, input: bytes.Clone(input)}
	c.mu.Lock()
	c.attempts = append(c.attempts, item)
	c.mu.Unlock()
	return item
}

func (a *httpAttemptCaptureItem) ObserveResponse(statusCode int, body io.ReadCloser) io.ReadCloser {
	a.statusCode = statusCode
	return &httpAttemptCaptureBody{ReadCloser: body, item: a}
}

func (a *httpAttemptCaptureItem) End(result recording.AttemptResult) { a.result = result }

type httpAttemptCaptureBody struct {
	io.ReadCloser
	item *httpAttemptCaptureItem
}

func (b *httpAttemptCaptureBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.item.output = append(b.item.output, p[:n]...)
	return n, err
}
