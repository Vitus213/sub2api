// Package recording defines the dependency-neutral model-attempt contract used
// by gateway services. The OTel implementation lives in the parent modeltrace
// package so service code never depends on an exporter or SDK.
package recording

import (
	"context"
	"io"
)

type contextKey struct{}
type attemptSourceContextKey struct{}
type deferredActivatorContextKey struct{}

// TraceContinuation is the only tracing state persisted with an asynchronous
// task. It contains W3C identifiers and a one-way generation fingerprint; no
// exporter endpoint, credential, prompt, or response is included.
type TraceContinuation struct {
	TraceID               string `json:"trace_id"`
	SpanID                string `json:"span_id"`
	TraceFlags            byte   `json:"trace_flags"`
	TraceState            string `json:"trace_state,omitempty"`
	GenerationFingerprint string `json:"generation_fingerprint"`
}

func (c TraceContinuation) Valid() bool {
	return len(c.TraceID) == 32 && len(c.SpanID) == 16 && len(c.GenerationFingerprint) == 64
}

type continuationProvider interface {
	TraceContinuation() TraceContinuation
}

// ContinuationFromContext snapshots safe async continuation metadata.
func ContinuationFromContext(ctx context.Context) (TraceContinuation, bool) {
	if ctx == nil {
		return TraceContinuation{}, false
	}
	provider, ok := ctx.Value(contextKey{}).(continuationProvider)
	if !ok || provider == nil {
		return TraceContinuation{}, false
	}
	continuation := provider.TraceContinuation()
	return continuation, continuation.Valid()
}

type recorderDetacher interface {
	DetachRecorder(context.Context) (context.Context, func())
}

// Detach preserves only the recorder and pins its immutable generation for a
// bounded background task. The returned release must be called exactly once.
func Detach(parent, base context.Context) (context.Context, func()) {
	if base == nil {
		base = context.Background()
	}
	if parent == nil {
		return base, func() {}
	}
	if detacher, ok := parent.Value(contextKey{}).(recorderDetacher); ok && detacher != nil {
		return detacher.DetachRecorder(base)
	}
	return Propagate(parent, base), func() {}
}

type asyncSubmissionReporter interface {
	RecordAsyncSubmission(taskID string, itemIDs []string)
}

// RecordAsyncSubmission attaches stable task/item correlation to the submit Trace.
func RecordAsyncSubmission(ctx context.Context, taskID string, itemIDs []string) {
	if ctx == nil {
		return
	}
	if reporter, ok := ctx.Value(contextKey{}).(asyncSubmissionReporter); ok && reporter != nil {
		reporter.RecordAsyncSubmission(taskID, itemIDs)
	}
}

// WithDeferredActivator attaches a source-time activation callback without
// allocating a Trace. The callback returns the context containing the recorder
// created at the first confirmed upstream send.
func WithDeferredActivator(ctx context.Context, activate func() context.Context) context.Context {
	if ctx == nil || activate == nil {
		return ctx
	}
	return context.WithValue(ctx, deferredActivatorContextKey{}, activate)
}

// ActivateDeferred starts a deferred model Trace at the actual-send boundary.
// It is a no-op for ordinary requests and local-only control paths.
func ActivateDeferred(ctx context.Context) context.Context {
	if ctx == nil {
		return ctx
	}
	activate, _ := ctx.Value(deferredActivatorContextKey{}).(func() context.Context)
	if activate == nil {
		return ctx
	}
	if activated := activate(); activated != nil {
		return activated
	}
	return ctx
}

type inputLimitReporter interface {
	TraceInputLimit() int
}

// InputLimit returns the maximum request bytes worth cloning at the transport
// boundary. ok=false means no model recorder is attached.
func InputLimit(ctx context.Context) (limit int, ok bool) {
	if ctx == nil {
		return 0, false
	}
	recorder := ctx.Value(contextKey{})
	if recorder == nil {
		return 0, false
	}
	if limited, ok := recorder.(inputLimitReporter); ok {
		return limited.TraceInputLimit(), true
	}
	return 1 << 20, true
}

// AttemptSource carries protocol-aware metadata to the actual RoundTrip
// boundary. Retry, fallback, and redirect sends inherit it and therefore each
// create a distinct Attempt while retaining the richer adapter metadata.
type AttemptSource struct {
	Metadata AttemptMetadata
	Input    []byte
}

func WithAttemptSource(ctx context.Context, source AttemptSource) context.Context {
	if ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, attemptSourceContextKey{}, source)
}

func AttemptSourceFromContext(ctx context.Context) (AttemptSource, bool) {
	if ctx == nil {
		return AttemptSource{}, false
	}
	source, ok := ctx.Value(attemptSourceContextKey{}).(AttemptSource)
	return source, ok
}

// AttemptMetadata contains facts fixed for one actual upstream send.
type AttemptMetadata struct {
	Provider      string
	Operation     string
	ClientModel   string
	UpstreamModel string
	Endpoint      string
	AccountID     int64
	ContentType   string
}

// UsageFacts is copied from the final UsageLog fact object. Known=false means
// the request never obtained valid usage and all token/cost attributes remain absent.
type UsageFacts struct {
	Known               bool
	RequestID           string
	Model               string
	AccountID           int64
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
	ImageInputTokens    int
	ImageOutputTokens   int
	InputCost           float64
	OutputCost          float64
	CacheCreationCost   float64
	CacheReadCost       float64
	TotalCost           float64
	ActualCost          float64
	ImageInputCost      float64
	ImageOutputCost     float64
	DurationMs          *int
	FirstTokenMs        *int
}

type usageReporter interface {
	RecordUsage(UsageFacts)
}

// RecordUsage reports final token and cost facts without affecting billing.
func RecordUsage(ctx context.Context, facts UsageFacts) {
	if ctx == nil || !facts.Known {
		return
	}
	if reporter, ok := ctx.Value(contextKey{}).(usageReporter); ok && reporter != nil {
		reporter.RecordUsage(facts)
	}
}

// Propagate preserves only the request recorder across a bounded background
// context. It does not copy cancellation, deadlines, baggage or arbitrary values.
func Propagate(parent, base context.Context) context.Context {
	if parent == nil || base == nil {
		return base
	}
	recorder := parent.Value(contextKey{})
	if recorder == nil {
		return base
	}
	return context.WithValue(base, contextKey{}, recorder)
}

// AttemptResult completes an attempt when the caller already owns the response
// bytes or the send failed before a response body existed.
type AttemptResult struct {
	Output     []byte
	HTTPStatus int
	Err        error
}

// Recorder starts one observation immediately before each actual upstream send.
type Recorder interface {
	BeginAttempt(AttemptMetadata, []byte) Attempt
}

// Attempt records the result of one actual upstream send.
type Attempt interface {
	// ObserveResponse returns a transparent body wrapper. It completes the
	// attempt at EOF, read failure, or Close and never reads ahead.
	ObserveResponse(statusCode int, body io.ReadCloser) io.ReadCloser
	End(AttemptResult)
}

// StreamStatus classifies the terminal state of one streaming model request.
type StreamStatus string

const (
	StreamCompleted          StreamStatus = "completed"
	StreamCancelled          StreamStatus = "cancelled"
	StreamClientDisconnected StreamStatus = "client_disconnected"
	StreamError              StreamStatus = "stream_error"
)

// StreamOutcome contains only terminal facts owned by the streaming path.
type StreamOutcome struct {
	Status     StreamStatus
	ErrorStage string
	Err        error
}

type streamReporter interface {
	BeginStream()
	EndStream(StreamOutcome)
}

// BeginStream marks an identified request as streaming without changing its I/O.
func BeginStream(ctx context.Context) {
	if reporter := streamReporterFromContext(ctx); reporter != nil {
		reporter.BeginStream()
	}
}

// EndStream records a terminal streaming outcome. It is a no-op when tracing
// is disabled and never performs export work synchronously.
func EndStream(ctx context.Context, outcome StreamOutcome) {
	if reporter := streamReporterFromContext(ctx); reporter != nil {
		reporter.EndStream(outcome)
	}
}

func streamReporterFromContext(ctx context.Context) streamReporter {
	if ctx == nil {
		return nil
	}
	reporter, _ := ctx.Value(contextKey{}).(streamReporter)
	return reporter
}

// WithRecorder attaches the request-scoped implementation after identity has
// established the root trace.
func WithRecorder(ctx context.Context, recorder Recorder) context.Context {
	if ctx == nil || recorder == nil {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, recorder)
}

// BeginAttempt is a no-op unless an identified model request installed a
// recorder. Call it only immediately before a real upstream send.
func BeginAttempt(ctx context.Context, metadata AttemptMetadata, input []byte) Attempt {
	if ctx != nil {
		if recorder, ok := ctx.Value(contextKey{}).(Recorder); ok && recorder != nil {
			return recorder.BeginAttempt(metadata, input)
		}
	}
	return noopAttempt{}
}

type noopAttempt struct{}

func (noopAttempt) ObserveResponse(_ int, body io.ReadCloser) io.ReadCloser { return body }
func (noopAttempt) End(AttemptResult)                                       {}
