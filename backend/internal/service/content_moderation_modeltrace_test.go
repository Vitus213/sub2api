package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/modeltrace/recording"
	"github.com/stretchr/testify/require"
)

func TestModelTraceProtocolMatrixContentModerationSourceHook(t *testing.T) {
	const apiKey = "moderation-canary-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer "+apiKey, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"flagged":false,"category_scores":{}}]}`))
	}))
	defer server.Close()

	capture := &moderationTraceCapture{}
	ctx := recording.WithRecorder(context.Background(), capture)
	service := &ContentModerationService{httpClient: server.Client()}
	status := 0
	result, err := service.callModerationOnceWithInput(ctx, &ContentModerationConfig{
		BaseURL: server.URL, Model: "omni-moderation-latest", TimeoutMS: 1000,
	}, apiKey, "business prompt", &status)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, http.StatusOK, status)
	require.Len(t, capture.items, 1)
	item := capture.items[0]
	require.Equal(t, "openai", item.metadata.Provider)
	require.Equal(t, "moderations", item.metadata.Operation)
	require.Equal(t, "omni-moderation-latest", item.metadata.UpstreamModel)
	require.JSONEq(t, `{"model":"omni-moderation-latest","input":"business prompt"}`, string(item.input))
	require.JSONEq(t, `{"results":[{"flagged":false,"category_scores":{}}]}`, string(item.output))
	require.Equal(t, 1, item.endCalls)
	require.NotContains(t, string(item.input)+string(item.output)+item.metadata.Endpoint, apiKey)
}

type moderationTraceCapture struct{ items []*moderationTraceItem }

func (c *moderationTraceCapture) BeginAttempt(metadata recording.AttemptMetadata, input []byte) recording.Attempt {
	item := &moderationTraceItem{metadata: metadata, input: bytes.Clone(input)}
	c.items = append(c.items, item)
	return item
}

type moderationTraceItem struct {
	metadata recording.AttemptMetadata
	input    []byte
	output   []byte
	endCalls int
}

func (i *moderationTraceItem) ObserveResponse(_ int, body io.ReadCloser) io.ReadCloser {
	return &moderationTraceBody{ReadCloser: body, item: i}
}
func (i *moderationTraceItem) End(recording.AttemptResult) { i.endCalls++ }

type moderationTraceBody struct {
	io.ReadCloser
	item *moderationTraceItem
}

func (b *moderationTraceBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.item.output = append(b.item.output, p[:n]...)
	return n, err
}
func (b *moderationTraceBody) Close() error {
	b.item.endCalls++
	return b.ReadCloser.Close()
}
