package service

import (
	"context"
	"io"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/modeltrace/recording"
	"github.com/stretchr/testify/require"
)

func TestModelTraceUsageAndCostMatchesUsageLog(t *testing.T) {
	durationMs := 321
	firstTokenMs := 45
	upstreamModel := "gpt-upstream"
	usageLog := &UsageLog{
		UserID: 73, APIKeyID: 71, AccountID: 29, RequestID: "usage-request-1",
		Model: "gpt-client", UpstreamModel: &upstreamModel,
		InputTokens: 11, OutputTokens: 7, CacheCreationTokens: 2, CacheReadTokens: 3,
		InputCost: 0.11, OutputCost: 0.07, CacheCreationCost: 0.02, CacheReadCost: 0.03,
		ImageInputTokens: 5, ImageOutputTokens: 13, ImageInputCost: 0.05, ImageOutputCost: 0.13,
		TotalCost: 0.23, ActualCost: 0.19, DurationMs: &durationMs, FirstTokenMs: &firstTokenMs,
	}
	capture := &usageFactsCapture{}
	ctx := recording.WithRecorder(context.Background(), capture)

	recordModelTraceUsage(ctx, usageLog)

	require.Len(t, capture.facts, 1)
	require.Equal(t, recording.UsageFacts{
		Known: true, RequestID: "usage-request-1", Model: "gpt-upstream", AccountID: 29,
		InputTokens: 11, OutputTokens: 7, CacheCreationTokens: 2, CacheReadTokens: 3,
		InputCost: 0.11, OutputCost: 0.07, CacheCreationCost: 0.02, CacheReadCost: 0.03,
		ImageInputTokens: 5, ImageOutputTokens: 13, ImageInputCost: 0.05, ImageOutputCost: 0.13,
		TotalCost: 0.23, ActualCost: 0.19, DurationMs: &durationMs, FirstTokenMs: &firstTokenMs,
	}, capture.facts[0])
}

func TestModelTraceUsageWithoutUpstreamModelDoesNotUseClientModel(t *testing.T) {
	usageLog := &UsageLog{Model: "gpt-client", AccountID: 29, InputTokens: 7, OutputTokens: 3}
	capture := &usageFactsCapture{}
	recordModelTraceUsage(recording.WithRecorder(context.Background(), capture), usageLog)

	require.Len(t, capture.facts, 1)
	require.Empty(t, capture.facts[0].Model)
	require.Equal(t, int64(29), capture.facts[0].AccountID)
}

func TestModelTraceUnknownUsageDoesNotReport(t *testing.T) {
	capture := &usageFactsCapture{}
	ctx := recording.WithRecorder(context.Background(), capture)
	recordModelTraceUsage(ctx, nil)
	require.Empty(t, capture.facts)
}

type usageFactsCapture struct {
	facts []recording.UsageFacts
}

func (c *usageFactsCapture) BeginAttempt(recording.AttemptMetadata, []byte) recording.Attempt {
	return usageNoopAttempt{}
}

func (c *usageFactsCapture) RecordUsage(facts recording.UsageFacts) {
	c.facts = append(c.facts, facts)
}

type usageNoopAttempt struct{}

func (usageNoopAttempt) ObserveResponse(_ int, body io.ReadCloser) io.ReadCloser { return body }
func (usageNoopAttempt) End(recording.AttemptResult)                             {}
