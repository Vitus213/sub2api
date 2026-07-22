package modeltrace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/modeltrace/recording"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var errUpstreamResponseIncomplete = errors.New("upstream response closed before EOF")

type traceRecorder struct {
	ctx                context.Context
	tracer             trace.Tracer
	identity           servermiddleware.ResolvedIdentity
	generation         *GenerationSnapshot
	promptMaxBytes     int
	responseMaxBytes   int
	capturePolicy      capturePolicy
	nextAttempt        atomic.Uint64
	streamStartedAt    time.Time
	streamMu           sync.Mutex
	stream             streamState
	attemptMu          sync.Mutex
	successfulAttempts []*traceAttempt
	pendingUsage       *recording.UsageFacts
	requestFinished    bool
	expectedUsageTasks int
}

func newTraceRecorder(
	ctx context.Context,
	tracer trace.Tracer,
	identity servermiddleware.ResolvedIdentity,
	promptMaxBytes int,
	responseMaxBytes int,
	capturePolicy capturePolicy,
	generation *GenerationSnapshot,
) *traceRecorder {
	return &traceRecorder{
		ctx: ctx, tracer: tracer, identity: identity,
		promptMaxBytes: promptMaxBytes, responseMaxBytes: responseMaxBytes,
		capturePolicy: capturePolicy, generation: generation, streamStartedAt: time.Now(),
	}
}

func (r *traceRecorder) TraceInputLimit() int {
	if r == nil {
		return 0
	}
	return r.promptMaxBytes
}

func (r *traceRecorder) TraceContinuation() recording.TraceContinuation {
	if r == nil || r.generation == nil {
		return recording.TraceContinuation{}
	}
	spanContext := trace.SpanContextFromContext(r.ctx)
	if !spanContext.IsValid() {
		return recording.TraceContinuation{}
	}
	return recording.TraceContinuation{
		TraceID:               spanContext.TraceID().String(),
		SpanID:                spanContext.SpanID().String(),
		TraceFlags:            byte(spanContext.TraceFlags()),
		TraceState:            spanContext.TraceState().String(),
		GenerationFingerprint: r.generation.Fingerprint(),
	}
}

func (r *traceRecorder) DetachRecorder(base context.Context) (context.Context, func()) {
	if r == nil || r.generation == nil {
		return base, func() {}
	}
	retained := r.generation.Retain()
	r.attemptMu.Lock()
	r.expectedUsageTasks++
	r.attemptMu.Unlock()
	var releaseOnce sync.Once
	return recording.WithRecorder(base, r), func() {
		releaseOnce.Do(func() {
			r.finishExpectedUsageTask()
			retained.Release()
		})
	}
}

// FinishRequest closes successful attempts immediately when no usage task was
// announced. Detached usage tasks keep the matching Generation open until they
// either report final facts or release after error, panic, cancellation, or drop.
func (r *traceRecorder) FinishRequest() {
	if r == nil {
		return
	}
	r.attemptMu.Lock()
	r.requestFinished = true
	var attempts []*traceAttempt
	if r.expectedUsageTasks == 0 {
		attempts = r.takeSuccessfulAttemptsLocked()
		r.pendingUsage = nil
	}
	r.attemptMu.Unlock()
	finishTraceAttempts(attempts)
}

func (r *traceRecorder) finishExpectedUsageTask() {
	if r == nil {
		return
	}
	r.attemptMu.Lock()
	if r.expectedUsageTasks > 0 {
		r.expectedUsageTasks--
	}
	var attempts []*traceAttempt
	if r.expectedUsageTasks == 0 {
		attempts = r.takeSuccessfulAttemptsLocked()
		r.pendingUsage = nil
	}
	r.attemptMu.Unlock()
	finishTraceAttempts(attempts)
}

func (r *traceRecorder) takeSuccessfulAttemptsLocked() []*traceAttempt {
	attempts := r.successfulAttempts
	r.successfulAttempts = nil
	return attempts
}

func finishTraceAttempts(attempts []*traceAttempt) {
	for _, attempt := range attempts {
		attempt.finish(nil)
	}
}

func (r *traceRecorder) RecordAsyncSubmission(taskID string, itemIDs []string) {
	if r == nil {
		return
	}
	span := trace.SpanFromContext(r.ctx)
	attrs := []attribute.KeyValue{attribute.Int("modeltrace.async.item_count", len(itemIDs))}
	if taskID != "" {
		attrs = append(attrs, attribute.String("langfuse.trace.metadata.task_id", scrubURLsInString(taskID)))
	}
	if len(itemIDs) > 0 {
		scrubbedIDs := make([]string, len(itemIDs))
		for i, id := range itemIDs {
			scrubbedIDs[i] = scrubURLsInString(id)
		}
		attrs = append(attrs, attribute.StringSlice("langfuse.trace.metadata.item_ids", scrubbedIDs))
	}
	span.SetAttributes(attrs...)
}

func (r *traceRecorder) BeginAttempt(metadata recording.AttemptMetadata, input []byte) recording.Attempt {
	index := r.nextAttempt.Add(1)
	name := fmt.Sprintf("upstream.attempt.%d", index)
	_, span := r.tracer.Start(r.ctx, name, trace.WithSpanKind(trace.SpanKindClient))

	observationMetadata := attemptObservationMetadata{
		AttemptIndex: index,
		AccountID:    metadata.AccountID,
		Provider:     metadata.Provider,
		ClientModel:  scrubURLsInString(metadata.ClientModel),
		Endpoint:     sanitizeAttemptEndpoint(metadata.Endpoint),
		APIKeyID:     r.identity.APIKeyID,
		UserID:       r.identity.UserID,
		GroupID:      r.identity.GroupID,
	}
	metadataJSON, _ := json.Marshal(observationMetadata)
	attrs := []attribute.KeyValue{
		attribute.String("langfuse.observation.type", "generation"),
		attribute.String("langfuse.observation.name", name),
		attribute.String("langfuse.observation.input", captureModelContentWithType(input, len(input), r.promptMaxBytes, metadata.ContentType, r.capturePolicy)),
		attribute.String("langfuse.observation.metadata", string(metadataJSON)),
		attribute.Int64("modeltrace.attempt.index", int64(index)),
	}
	if metadata.Operation != "" {
		attrs = append(attrs, attribute.String("gen_ai.operation.name", metadata.Operation))
	}
	if metadata.Provider != "" {
		attrs = append(attrs, attribute.String("gen_ai.provider.name", metadata.Provider))
	}
	if metadata.UpstreamModel != "" {
		model := scrubURLsInString(metadata.UpstreamModel)
		attrs = append(attrs,
			attribute.String("gen_ai.request.model", model),
			attribute.String("langfuse.observation.model.name", model),
		)
	}
	if metadata.AccountID > 0 {
		attrs = append(attrs, attribute.Int64("modeltrace.account.id", metadata.AccountID))
	}
	if observationMetadata.Endpoint != "" {
		attrs = append(attrs, attribute.String("server.address", observationMetadata.Endpoint))
	}
	span.SetAttributes(attrs...)

	return &traceAttempt{
		ctx:              r.ctx,
		span:             span,
		responseMaxBytes: r.responseMaxBytes,
		capturePolicy:    r.capturePolicy,
		streamRecorder:   r,
		accountID:        metadata.AccountID,
		upstreamModel:    metadata.UpstreamModel,
	}
}

func (r *traceRecorder) RecordUsage(facts recording.UsageFacts) {
	if r == nil || !facts.Known {
		return
	}
	r.attemptMu.Lock()
	index := r.matchSuccessfulAttemptLocked(facts)
	var attempt *traceAttempt
	if index >= 0 {
		attempt = r.successfulAttempts[index]
		r.successfulAttempts = append(r.successfulAttempts[:index], r.successfulAttempts[index+1:]...)
	} else if !r.requestFinished || r.expectedUsageTasks > 0 {
		copyFacts := facts
		r.pendingUsage = &copyFacts
	}
	r.attemptMu.Unlock()
	if attempt != nil {
		attempt.finish(&facts)
	}
}

func (r *traceRecorder) deferSuccessfulAttempt(attempt *traceAttempt) {
	if r == nil || attempt == nil {
		return
	}
	r.attemptMu.Lock()
	var facts *recording.UsageFacts
	if r.pendingUsage != nil && usageMatchesAttempt(*r.pendingUsage, attempt) {
		copyFacts := *r.pendingUsage
		facts = &copyFacts
		r.pendingUsage = nil
	} else if r.requestFinished && r.expectedUsageTasks == 0 {
		// The request ended without announcing a usage task. Do not leave a
		// successful Generation open waiting for facts that cannot arrive.
	} else {
		r.successfulAttempts = append(r.successfulAttempts, attempt)
		r.attemptMu.Unlock()
		return
	}
	r.attemptMu.Unlock()
	attempt.finish(facts)
}

func (r *traceRecorder) matchSuccessfulAttemptLocked(facts recording.UsageFacts) int {
	for index := len(r.successfulAttempts) - 1; index >= 0; index-- {
		if usageMatchesAttempt(facts, r.successfulAttempts[index]) {
			return index
		}
	}
	return -1
}

func usageMatchesAttempt(facts recording.UsageFacts, attempt *traceAttempt) bool {
	if attempt == nil {
		return false
	}
	if facts.AccountID > 0 && attempt.accountID > 0 && facts.AccountID != attempt.accountID {
		return false
	}
	if strings.TrimSpace(facts.Model) != "" && strings.TrimSpace(attempt.upstreamModel) != "" &&
		strings.TrimSpace(facts.Model) != strings.TrimSpace(attempt.upstreamModel) {
		return false
	}
	return true
}

func (r *traceRecorder) usageAttributes(facts recording.UsageFacts) []attribute.KeyValue {
	totalTokens := facts.InputTokens + facts.OutputTokens + facts.CacheCreationTokens + facts.CacheReadTokens
	usageDetails := map[string]int{
		"input": facts.InputTokens, "output": facts.OutputTokens,
		"cache_creation_input_tokens": facts.CacheCreationTokens,
		"cache_read_input_tokens":     facts.CacheReadTokens,
		"total":                       totalTokens,
	}
	if facts.ImageInputTokens != 0 {
		usageDetails["image_input_tokens"] = facts.ImageInputTokens
	}
	if facts.ImageOutputTokens != 0 {
		usageDetails["image_output_tokens"] = facts.ImageOutputTokens
	}
	usageJSON, _ := json.Marshal(usageDetails)
	costDetails := map[string]float64{
		"input": facts.InputCost, "output": facts.OutputCost,
		"cache_creation": facts.CacheCreationCost, "cache_read": facts.CacheReadCost,
		"total": facts.TotalCost, "actual": facts.ActualCost,
	}
	if facts.ImageInputCost != 0 {
		costDetails["image_input"] = facts.ImageInputCost
	}
	if facts.ImageOutputCost != 0 {
		costDetails["image_output"] = facts.ImageOutputCost
	}
	costJSON, _ := json.Marshal(costDetails)
	attrs := []attribute.KeyValue{
		attribute.Int("gen_ai.usage.input_tokens", facts.InputTokens),
		attribute.Int("gen_ai.usage.output_tokens", facts.OutputTokens),
		attribute.Int("gen_ai.usage.total_tokens", totalTokens),
		attribute.String("langfuse.observation.usage_details", string(usageJSON)),
		attribute.String("langfuse.observation.cost_details", string(costJSON)),
	}
	if facts.RequestID != "" {
		attrs = append(attrs, attribute.String("gen_ai.response.id", scrubURLsInString(facts.RequestID)))
	}
	if facts.Model != "" {
		model := scrubURLsInString(facts.Model)
		attrs = append(attrs,
			attribute.String("gen_ai.response.model", model),
			attribute.String("langfuse.observation.model.name", model),
		)
	}
	if facts.AccountID > 0 {
		attrs = append(attrs, attribute.Int64("modeltrace.account.id", facts.AccountID))
	}
	if facts.DurationMs != nil {
		attrs = append(attrs, attribute.Int("modeltrace.duration_ms", *facts.DurationMs))
	}
	if facts.FirstTokenMs != nil {
		attrs = append(attrs, attribute.Int(firstOutputMsAttribute, *facts.FirstTokenMs))
	}
	if r.identity.UserID > 0 {
		attrs = append(attrs, attribute.String("langfuse.user.id", fmt.Sprintf("%d", r.identity.UserID)))
	}
	if r.identity.APIKeyID > 0 {
		attrs = append(attrs, attribute.Int64("langfuse.trace.metadata.api_key_id", r.identity.APIKeyID))
	}
	if r.identity.GroupID > 0 {
		attrs = append(attrs, attribute.Int64("langfuse.trace.metadata.group_id", r.identity.GroupID))
	}
	return attrs
}

const (
	streamStatusCompleted          = "completed"
	streamStatusCancelled          = "cancelled"
	streamStatusClientDisconnected = "client_disconnected"
	streamStatusStreamError        = "stream_error"
	streamStatusAttribute          = "modeltrace.stream.status"
	streamErrorStageAttribute      = "modeltrace.stream.error_stage"
	firstOutputMsAttribute         = "gen_ai.client.first_byte.duration_ms"
)

type streamState struct {
	firstOutputMs *int64
	status        string
	errorStage    string
	errorType     string
	started       bool
}

func (r *traceRecorder) BeginStream() {
	if r == nil {
		return
	}
	r.streamMu.Lock()
	r.stream.started = true
	r.streamMu.Unlock()
}

func (r *traceRecorder) EndStream(outcome recording.StreamOutcome) {
	if r == nil {
		return
	}
	status := string(outcome.Status)
	if status == "" {
		if errors.Is(outcome.Err, context.Canceled) {
			status = streamStatusCancelled
		} else if outcome.Err != nil {
			status = streamStatusStreamError
		}
	}
	r.streamMu.Lock()
	defer r.streamMu.Unlock()
	r.stream.started = true
	r.setStreamTerminalLocked(status, strings.TrimSpace(outcome.ErrorStage), outcome.Err)
}

func (r *traceRecorder) observeClientWrite(n int, err error) {
	if r == nil {
		return
	}
	r.streamMu.Lock()
	defer r.streamMu.Unlock()
	if n > 0 && r.stream.firstOutputMs == nil {
		value := time.Since(r.streamStartedAt).Milliseconds()
		r.stream.firstOutputMs = &value
	}
	if err != nil {
		r.setStreamTerminalLocked(streamStatusClientDisconnected, "downstream_write", err)
	}
}

func (r *traceRecorder) observeUpstreamStreamError(err error, stage string) {
	if r == nil || err == nil {
		return
	}
	status := streamStatusStreamError
	if errors.Is(err, context.Canceled) {
		status = streamStatusCancelled
	}
	r.streamMu.Lock()
	defer r.streamMu.Unlock()
	r.setStreamTerminalLocked(status, stage, err)
}

func (r *traceRecorder) setStreamTerminalLocked(status, stage string, err error) {
	if r.stream.status == streamStatusClientDisconnected {
		return
	}
	if status != streamStatusClientDisconnected && r.stream.status != "" {
		return
	}
	r.stream.status = status
	r.stream.errorStage = stage
	if err != nil {
		r.stream.errorType = fmt.Sprintf("%T", err)
	}
}

func (r *traceRecorder) streamSnapshot(complete bool) streamState {
	if r == nil {
		return streamState{}
	}
	r.streamMu.Lock()
	defer r.streamMu.Unlock()
	result := r.stream
	if complete && result.status == "" {
		result.status = streamStatusCompleted
	}
	if result.firstOutputMs != nil {
		value := *result.firstOutputMs
		result.firstOutputMs = &value
	}
	return result
}

type attemptObservationMetadata struct {
	AttemptIndex uint64 `json:"attempt_index"`
	AccountID    int64  `json:"account_id,omitempty"`
	Provider     string `json:"provider,omitempty"`
	ClientModel  string `json:"client_model,omitempty"`
	Endpoint     string `json:"endpoint,omitempty"`
	APIKeyID     int64  `json:"api_key_id,omitempty"`
	UserID       int64  `json:"user_id,omitempty"`
	GroupID      int64  `json:"group_id,omitempty"`
}

type traceAttempt struct {
	ctx              context.Context
	span             trace.Span
	responseMaxBytes int
	capturePolicy    capturePolicy
	streamRecorder   *traceRecorder
	accountID        int64
	upstreamModel    string
	spanEndOnce      sync.Once
	endOnce          sync.Once
}

func (a *traceAttempt) ObserveResponse(statusCode int, body io.ReadCloser) io.ReadCloser {
	if body == nil {
		a.End(recording.AttemptResult{HTTPStatus: statusCode})
		return nil
	}
	return &attemptResponseReadCloser{
		ReadCloser: body,
		attempt:    a,
		statusCode: statusCode,
		limit:      a.responseMaxBytes,
	}
}

func (a *traceAttempt) End(result recording.AttemptResult) {
	a.endCaptured(result, len(result.Output))
}

func (a *traceAttempt) endCaptured(result recording.AttemptResult, originalOutputBytes int) {
	a.endOnce.Do(func() {
		attrs := make([]attribute.KeyValue, 0, 3)
		if result.Output != nil {
			output := captureModelContent(result.Output, originalOutputBytes, a.responseMaxBytes, a.capturePolicy)
			attrs = append(attrs, attribute.String("langfuse.observation.output", output))
		}
		if result.HTTPStatus > 0 {
			attrs = append(attrs, attribute.Int("http.response.status_code", result.HTTPStatus))
		}
		if result.Err != nil {
			attrs = append(attrs, attribute.String("error.type", fmt.Sprintf("%T", result.Err)))
		}
		a.span.SetAttributes(attrs...)

		switch {
		case result.Err != nil:
			a.span.SetStatus(codes.Error, sanitizeTraceError(result.Err.Error()))
		case result.HTTPStatus >= http.StatusBadRequest:
			a.span.SetStatus(codes.Error, httpStatusText(result.HTTPStatus))
		default:
			a.span.SetStatus(codes.Ok, "")
		}
		if result.Err == nil && result.HTTPStatus >= http.StatusOK && result.HTTPStatus < http.StatusMultipleChoices && a.streamRecorder != nil {
			a.streamRecorder.deferSuccessfulAttempt(a)
			return
		}
		a.finish(nil)
	})
}

func (a *traceAttempt) finish(facts *recording.UsageFacts) {
	if a == nil {
		return
	}
	a.spanEndOnce.Do(func() {
		if facts != nil && a.streamRecorder != nil {
			a.span.SetAttributes(a.streamRecorder.usageAttributes(*facts)...)
		}
		a.span.End()
	})
}

type attemptResponseReadCloser struct {
	io.ReadCloser
	attempt    *traceAttempt
	statusCode int
	limit      int
	sawEOF     atomic.Bool

	mu    sync.Mutex
	buf   bytes.Buffer
	total int
}

func (r *attemptResponseReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.capture(p[:n])
	}
	if err == io.EOF {
		r.sawEOF.Store(true)
	}
	if err != nil {
		output, total := r.bytesAndTotal()
		result := recording.AttemptResult{Output: output, HTTPStatus: r.statusCode}
		if err != io.EOF {
			result.Err = err
			r.attempt.streamRecorder.observeUpstreamStreamError(err, "upstream_read")
		}
		r.attempt.endCaptured(result, total)
	}
	return n, err
}

func (r *attemptResponseReadCloser) Close() error {
	err := r.ReadCloser.Close()
	output, total := r.bytesAndTotal()
	result := recording.AttemptResult{Output: output, HTTPStatus: r.statusCode, Err: err}
	if result.Err == nil && r.attempt.ctx != nil {
		result.Err = r.attempt.ctx.Err()
	}
	if result.Err == nil && !r.sawEOF.Load() {
		result.Err = errUpstreamResponseIncomplete
	}
	if result.Err != nil && !r.sawEOF.Load() {
		r.attempt.streamRecorder.observeUpstreamStreamError(result.Err, "upstream_close")
	}
	r.attempt.endCaptured(result, total)
	return err
}

func (r *attemptResponseReadCloser) capture(p []byte) {
	if len(p) == 0 {
		return
	}
	r.mu.Lock()
	r.total += len(p)
	defer r.mu.Unlock()
	if r.limit <= 0 || r.buf.Len() >= r.limit {
		return
	}
	remaining := r.limit - r.buf.Len()
	if len(p) > remaining {
		_, _ = r.buf.Write(p[:remaining])
		return
	}
	_, _ = r.buf.Write(p)
}

func (r *attemptResponseReadCloser) bytesAndTotal() ([]byte, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return bytes.Clone(r.buf.Bytes()), r.total
}

func sanitizeAttemptEndpoint(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String()
}
