package modeltrace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/modeltrace/recording"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ResponsesWSTurnMetadata contains facts fixed when one accepted response.create
// frame starts. A connection request ID is correlation metadata, never a Session ID.
type ResponsesWSTurnMetadata struct {
	Identity            servermiddleware.ResolvedIdentity
	ConnectionRequestID string
	TurnRequestID       string
	TurnIndex           int
	Path                string
	Model               string
	SessionID           string
}

// ResponsesWSTurn owns one independent root Trace inside a longer WebSocket
// connection. Its methods only copy bounded prefixes and never export inline.
type ResponsesWSTurn struct {
	span       trace.Span
	generation *GenerationSnapshot
	recorder   *traceRecorder
	metadata   ResponsesWSTurnMetadata
	input      []byte
	inputBytes int
	policy     capturePolicy
	limit      int

	mu          sync.Mutex
	output      bytes.Buffer
	outputBytes int
	endOnce     sync.Once
}

// StartResponsesWSTurn starts a new root for one accepted response.create. A
// disabled manager returns nil without allocating capture buffers.
func (m *Manager) StartResponsesWSTurn(parent context.Context, metadata ResponsesWSTurnMetadata, input []byte) *ResponsesWSTurn {
	generation := m.Acquire()
	if !generation.Enabled() {
		generation.Release()
		return nil
	}
	if parent == nil {
		parent = context.Background()
	}
	cfg := generation.Config()
	tracer := generation.Tracer()
	ctx, span := tracer.Start(parent, rootSpanName,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithNewRoot(),
	)
	policy := capturePolicy{
		mediaMaxBytes:       cfg.MediaMaxBytes,
		captureMediaContent: cfg.CaptureMediaContent,
	}
	inputBytes := len(input)
	if cfg.PromptMaxBytes <= 0 {
		input = nil
	} else if len(input) > cfg.PromptMaxBytes {
		input = input[:cfg.PromptMaxBytes]
	}
	recorder := newTraceRecorder(ctx, tracer, metadata.Identity, cfg.PromptMaxBytes, cfg.ResponseMaxBytes, policy, generation)
	recorder.ctx = recording.WithRecorder(ctx, recorder)
	recorder.BeginStream()
	turn := &ResponsesWSTurn{
		span: span, recorder: recorder, metadata: metadata, generation: generation,
		input: bytes.Clone(input), inputBytes: inputBytes, policy: policy, limit: cfg.ResponseMaxBytes,
	}
	if turn.limit > 0 {
		turn.output.Grow(min(turn.limit, 4096))
	}
	return turn
}

// Context carries this turn's attempt recorder for upstream instrumentation.
func (t *ResponsesWSTurn) Context() context.Context {
	if t == nil || t.recorder == nil {
		return context.Background()
	}
	return t.recorder.ctx
}

// ObserveClientWrite records only frames successfully written to the client.
func (t *ResponsesWSTurn) ObserveClientWrite(payload []byte, writeErr error) {
	if t == nil {
		return
	}
	written := 0
	if writeErr == nil {
		written = len(payload)
		t.mu.Lock()
		t.outputBytes += written
		if t.limit > 0 && t.output.Len() < t.limit {
			remaining := t.limit - t.output.Len()
			if remaining > written {
				remaining = written
			}
			_, _ = t.output.Write(payload[:remaining])
		}
		t.mu.Unlock()
	}
	t.recorder.observeClientWrite(written, writeErr)
}

// End finalizes this turn exactly once.
func (t *ResponsesWSTurn) End(status, errorStage string, err error) {
	if t == nil {
		return
	}
	t.endOnce.Do(func() {
		defer t.generation.Release()
		t.recorder.EndStream(recording.StreamOutcome{Status: recording.StreamStatus(status), ErrorStage: errorStage, Err: err})
		t.recorder.FinishRequest()
		stream := t.recorder.streamSnapshot(false)
		if stream.status == "" {
			stream.status = streamStatusCompleted
		}

		t.mu.Lock()
		output := bytes.Clone(t.output.Bytes())
		outputBytes := t.outputBytes
		t.mu.Unlock()

		metadataJSON, _ := json.Marshal(map[string]any{
			"connection_request_id": t.metadata.ConnectionRequestID,
			"turn_request_id":       t.metadata.TurnRequestID,
			"turn_index":            t.metadata.TurnIndex,
			"api_key_id":            t.metadata.Identity.APIKeyID,
			"user_id":               t.metadata.Identity.UserID,
			"group_id":              t.metadata.Identity.GroupID,
		})
		attrs := []attribute.KeyValue{
			attribute.String("langfuse.trace.name", rootSpanName),
			attribute.String("langfuse.observation.input", captureModelContent(t.input, t.inputBytes, t.recorder.promptMaxBytes, t.policy)),
			attribute.String("langfuse.observation.output", captureModelContent(output, outputBytes, t.limit, t.policy)),
			attribute.String("langfuse.trace.metadata", string(metadataJSON)),
			attribute.String("langfuse.trace.metadata.connection_request_id", scrubURLsInString(t.metadata.ConnectionRequestID)),
			attribute.String("langfuse.trace.metadata.turn_request_id", scrubURLsInString(t.metadata.TurnRequestID)),
			attribute.Int("langfuse.trace.metadata.turn_index", t.metadata.TurnIndex),
			attribute.String("http.request.method", http.MethodGet),
			attribute.String("url.path", t.metadata.Path),
			attribute.String(streamStatusAttribute, stream.status),
		}
		if t.metadata.Model != "" {
			attrs = append(attrs, attribute.String("gen_ai.request.model", scrubURLsInString(t.metadata.Model)))
		}
		if t.metadata.Identity.UserID > 0 {
			attrs = append(attrs, attribute.String("langfuse.user.id", strconv.FormatInt(t.metadata.Identity.UserID, 10)))
		}
		if t.metadata.Identity.APIKeyID > 0 {
			attrs = append(attrs, attribute.Int64("langfuse.trace.metadata.api_key_id", t.metadata.Identity.APIKeyID))
		}
		if t.metadata.Identity.GroupID > 0 {
			attrs = append(attrs, attribute.Int64("langfuse.trace.metadata.group_id", t.metadata.Identity.GroupID))
		}
		if t.metadata.SessionID != "" {
			attrs = append(attrs, attribute.String("langfuse.session.id", scrubURLsInString(t.metadata.SessionID)))
		}
		if stream.firstOutputMs != nil {
			attrs = append(attrs, attribute.Int64(firstOutputMsAttribute, *stream.firstOutputMs))
		}
		if stream.errorStage != "" {
			attrs = append(attrs, attribute.String(streamErrorStageAttribute, stream.errorStage))
		}
		if stream.errorType != "" {
			attrs = append(attrs, attribute.String("error.type", stream.errorType))
		}
		t.span.SetAttributes(attrs...)
		if stream.status == streamStatusCompleted {
			t.span.SetStatus(codes.Ok, "")
		} else {
			t.span.SetStatus(codes.Error, fmt.Sprintf("websocket turn %s", stream.status))
		}
		t.span.End()
	})
}
