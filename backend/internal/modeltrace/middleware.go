package modeltrace

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/modeltrace/recording"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const rootSpanName = "model.request"

// CandidateMiddleware marks an explicit model-execution route without doing
// any tracing work until API-key resolution establishes a real identity.
func (m *Manager) CandidateMiddleware() gin.HandlerFunc {
	return m.candidateMiddleware(false)
}

// DeferredCandidateMiddleware records an authenticated identity but delays
// creating a model Trace until ActivateDeferredCandidate confirms that the
// request will execute a model upstream. Local-only/control paths stay no-op.
func (m *Manager) DeferredCandidateMiddleware() gin.HandlerFunc {
	return m.candidateMiddleware(true)
}

const deferredCandidateContextKey = "modeltrace.deferred_candidate"

func (m *Manager) candidateMiddleware(deferred bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		generation := m.Acquire()
		if !generation.Enabled() {
			generation.Release()
			c.Next()
			return
		}

		state := &candidateState{generation: generation}
		if deferred {
			c.Set(deferredCandidateContextKey, state)
			state.prepareDeferred(c)
			c.Request = c.Request.WithContext(recording.WithDeferredActivator(c.Request.Context(), func() context.Context {
				if state.identity.APIKeyID > 0 {
					state.start(c, state.identity)
				}
				return c.Request.Context()
			}))
		}
		middleware.SetIdentityEstablishedHook(c, func(c *gin.Context, identity middleware.ResolvedIdentity) {
			if deferred {
				state.identity = identity
				return
			}
			state.start(c, identity)
		})
		defer func() {
			if recovered := recover(); recovered != nil {
				state.finish(c, http.StatusInternalServerError)
				panic(recovered)
			}
			state.finish(c, 0)
		}()
		c.Next()
	}
}

// ActivateDeferredCandidate starts the deferred Trace exactly once. New source
// hooks should prefer recording.ActivateDeferred at the actual-send boundary;
// this adapter remains for callers that already proved execution in a handler.
func ActivateDeferredCandidate(c *gin.Context) {
	if c == nil || c.Request == nil {
		return
	}
	c.Request = c.Request.WithContext(recording.ActivateDeferred(c.Request.Context()))
}

type candidateState struct {
	generation     *GenerationSnapshot
	startOnce      sync.Once
	started        bool
	span           trace.Span
	identity       middleware.ResolvedIdentity
	requestCapture *requestCaptureReadCloser
	recorder       *traceRecorder
	response       *responseRecorder
	originalWriter gin.ResponseWriter
}

func (s *candidateState) prepareDeferred(c *gin.Context) {
	if s == nil || s.generation == nil || c == nil || c.Request == nil || c.Request.Body == nil {
		return
	}
	cfg := s.generation.Config()
	if cfg.PromptMaxBytes <= 0 {
		return
	}
	s.requestCapture = &requestCaptureReadCloser{
		ReadCloser: c.Request.Body,
		limit:      cfg.PromptMaxBytes,
	}
	c.Request.Body = s.requestCapture
}

func (s *candidateState) start(c *gin.Context, identity middleware.ResolvedIdentity) {
	s.startOnce.Do(func() {
		cfg := s.generation.Config()
		tracer := s.generation.Tracer()
		ctx, span := tracer.Start(c.Request.Context(), rootSpanName,
			trace.WithSpanKind(trace.SpanKindServer),
		)
		recorder := newTraceRecorder(ctx, tracer, identity,
			cfg.PromptMaxBytes, cfg.ResponseMaxBytes,
			capturePolicy{
				mediaMaxBytes:       cfg.MediaMaxBytes,
				captureMediaContent: cfg.CaptureMediaContent,
			},
			s.generation,
		)
		ctx = recording.WithRecorder(ctx, recorder)
		c.Request = c.Request.WithContext(ctx)
		s.span = span
		s.identity = identity
		s.recorder = recorder
		s.started = true

		if s.requestCapture == nil && c.Request.Body != nil && cfg.PromptMaxBytes > 0 {
			s.requestCapture = &requestCaptureReadCloser{
				ReadCloser: c.Request.Body,
				limit:      cfg.PromptMaxBytes,
			}
			c.Request.Body = s.requestCapture
		}

		s.originalWriter = c.Writer
		s.response = &responseRecorder{
			ResponseWriter: c.Writer,
			limit:          cfg.ResponseMaxBytes,
			recorder:       recorder,
		}
		c.Writer = s.response
	})
}

func (s *candidateState) finish(c *gin.Context, statusOverride int) {
	defer s.generation.Release()
	if !s.started {
		return
	}
	if c.Writer == s.response {
		c.Writer = s.originalWriter
	}

	var clientInput []byte
	var clientInputBytes int
	if s.requestCapture != nil {
		clientInput, clientInputBytes = s.requestCapture.bytesAndTotal()
	}
	status := s.response.Status()
	if statusOverride > 0 {
		status = statusOverride
	}
	clientOutput, clientOutputBytes := s.response.bytesAndTotal()
	stream := s.recorder.streamSnapshot(false)
	isStream := stream.started || strings.Contains(strings.ToLower(s.response.Header().Get("Content-Type")), "text/event-stream")
	if isStream && stream.status == "" {
		stream.status = streamStatusCompleted
	}
	cfg := s.generation.Config()
	policy := capturePolicy{
		mediaMaxBytes:       cfg.MediaMaxBytes,
		captureMediaContent: cfg.CaptureMediaContent,
	}
	attrs := []attribute.KeyValue{
		attribute.String("langfuse.trace.name", rootSpanName),
		attribute.String("langfuse.observation.input", captureModelContentWithType(clientInput, clientInputBytes, cfg.PromptMaxBytes, c.GetHeader("Content-Type"), policy)),
		attribute.String("langfuse.observation.output", captureModelContentWithType(clientOutput, clientOutputBytes, cfg.ResponseMaxBytes, s.response.Header().Get("Content-Type"), policy)),
		attribute.String("http.request.method", c.Request.Method),
		attribute.String("url.path", c.Request.URL.Path),
		attribute.Int64("http.response.status_code", int64(status)),
	}
	entry := resolveEntryFacts(c.Request.URL.Path, c.GetHeader("Content-Type"), clientInput)
	traceTags := make([]string, 0, 2)
	if entry.Protocol != "" {
		attrs = append(attrs,
			attribute.String("modeltrace.entry.protocol", entry.Protocol),
			attribute.String("langfuse.trace.metadata.entry_protocol", entry.Protocol),
		)
		traceTags = append(traceTags, "entry_protocol:"+entry.Protocol)
	}
	if entry.ClientModel != "" {
		model := scrubURLsInString(entry.ClientModel)
		attrs = append(attrs,
			attribute.String("modeltrace.client.request.model", model),
			attribute.String("langfuse.trace.metadata.client_model", model),
		)
		traceTags = append(traceTags, "client_model:"+model)
	}
	if len(traceTags) > 0 {
		attrs = append(attrs, attribute.StringSlice("langfuse.trace.tags", traceTags))
	}
	if isStream {
		attrs = append(attrs, attribute.String(streamStatusAttribute, stream.status))
		if stream.firstOutputMs != nil {
			attrs = append(attrs, attribute.Int64(firstOutputMsAttribute, *stream.firstOutputMs))
		}
		if stream.errorStage != "" {
			attrs = append(attrs, attribute.String(streamErrorStageAttribute, stream.errorStage))
		}
		if stream.errorType != "" {
			attrs = append(attrs, attribute.String("error.type", stream.errorType))
		}
	}
	if reqID := scrubURLsInString(clientRequestID(c)); reqID != "" {
		attrs = append(attrs, attribute.String("langfuse.trace.metadata.request_id", reqID))
	}
	if s.identity.UserID > 0 {
		attrs = append(attrs, attribute.String("langfuse.user.id", strconv.FormatInt(s.identity.UserID, 10)))
	}
	if s.identity.APIKeyID > 0 {
		attrs = append(attrs, attribute.Int64("langfuse.trace.metadata.api_key_id", s.identity.APIKeyID))
	}
	if s.identity.GroupID > 0 {
		attrs = append(attrs, attribute.Int64("langfuse.trace.metadata.group_id", s.identity.GroupID))
	}
	if session := scrubURLsInString(extractSession(clientInput, c)); session != "" {
		attrs = append(attrs, attribute.String("langfuse.session.id", session))
	}
	s.span.SetAttributes(attrs...)
	if status >= 400 || (isStream && stream.status != streamStatusCompleted) {
		description := httpStatusText(status)
		if isStream && stream.status != streamStatusCompleted {
			description = stream.status
		}
		s.span.SetStatus(codes.Error, description)
	} else {
		s.span.SetStatus(codes.Ok, "")
	}
	s.recorder.FinishRequest()
	s.span.End()
}

type requestCaptureReadCloser struct {
	io.ReadCloser
	limit int
	buf   bytes.Buffer
	total int
}

func (r *requestCaptureReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.total += n
	if n > 0 && r.buf.Len() < r.limit {
		remaining := r.limit - r.buf.Len()
		if remaining > n {
			remaining = n
		}
		_, _ = r.buf.Write(p[:remaining])
	}
	return n, err
}

func (r *requestCaptureReadCloser) bytesAndTotal() ([]byte, int) {
	return bytes.Clone(r.buf.Bytes()), r.total
}

type responseRecorder struct {
	gin.ResponseWriter
	limit    int
	status   int
	buf      bytes.Buffer
	recorder *traceRecorder
	total    int
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) WriteHeaderNow() {
	if r.status == 0 {
		r.status = 200
	}
	r.ResponseWriter.WriteHeaderNow()
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = 200
	}
	n, err := r.ResponseWriter.Write(p)
	r.capture(p[:n])
	r.recorder.observeClientWrite(n, err)
	return n, err
}

func (r *responseRecorder) WriteString(value string) (int, error) {
	if r.status == 0 {
		r.status = 200
	}
	n, err := r.ResponseWriter.WriteString(value)
	bytesValue := []byte(value)
	if n > len(bytesValue) {
		n = len(bytesValue)
	}
	r.capture(bytesValue[:n])
	r.recorder.observeClientWrite(n, err)
	return n, err
}

func (r *responseRecorder) Status() int {
	if r.status == 0 {
		return 200
	}
	return r.status
}

func (r *responseRecorder) UnwrapResponseWriter() gin.ResponseWriter { return r.ResponseWriter }

func (r *responseRecorder) capture(p []byte) {
	r.total += len(p)
	if r.limit <= 0 || r.buf.Len() >= r.limit {
		return
	}
	remaining := r.limit - r.buf.Len()
	if remaining > len(p) {
		remaining = len(p)
	}
	_, _ = r.buf.Write(p[:remaining])
}

func (r *responseRecorder) bytesAndTotal() ([]byte, int) {
	return bytes.Clone(r.buf.Bytes()), r.total
}

func clientRequestID(c *gin.Context) string {
	if v := strings.TrimSpace(c.GetHeader("X-Client-Request-ID")); v != "" {
		return v
	}
	return ""
}

func extractSession(body []byte, c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ExtractLangfuseSessionID(body, nil, false)
	}
	grokRoute := false
	if apiKey, ok := middleware.GetAPIKeyFromContext(c); ok && apiKey != nil && apiKey.Group != nil {
		grokRoute = strings.EqualFold(strings.TrimSpace(apiKey.Group.Platform), "grok")
	}
	return ExtractLangfuseSessionID(body, c.Request.Header, grokRoute)
}

func httpStatusText(code int) string {
	switch {
	case code >= 500:
		return "server_error"
	case code >= 400:
		return "client_error"
	default:
		return ""
	}
}
