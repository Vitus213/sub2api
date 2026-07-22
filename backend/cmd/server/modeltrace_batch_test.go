package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/modeltrace"
	"github.com/Wei-Shaw/sub2api/internal/modeltrace/recording"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestBatchImageTraceRecorder_JobTerminalsHaveTaskMetadataWithoutItemOrUsage(t *testing.T) {
	manager, exportedSpans := newBatchTraceTestManager(t)
	recorder := provideBatchImageTraceRecorder(manager)
	continuation := batchTraceTestContinuation()

	recorder.RecordBatchImageResult(context.Background(), &service.BatchImageJob{
		BatchID: "batch-failed", Model: "imagen-test", TraceContinuation: &continuation,
	}, service.BatchImageTraceResult{
		Status: service.BatchImageJobStatusFailed, ProviderState: "JOB_STATE_FAILED",
		ErrorStage: "provider", ErrorCode: "BAD_PROMPT",
	})
	recorder.RecordBatchImageResult(context.Background(), &service.BatchImageJob{
		BatchID: "batch-cancelled", Model: "imagen-test", TraceContinuation: &continuation,
	}, service.BatchImageTraceResult{
		Status: service.BatchImageJobStatusCancelled, ProviderState: "JOB_STATE_CANCELLED",
		ErrorStage: "provider", ErrorCode: "PROVIDER_BATCH_CANCELLED",
	})

	spans := exportedSpans()
	require.Len(t, spans, 2)
	assertBatchTraceJobTerminal(t, spanForBatchTask(t, spans, "batch-failed"), "failed", "JOB_STATE_FAILED", "provider", "BAD_PROMPT")
	assertBatchTraceJobTerminal(t, spanForBatchTask(t, spans, "batch-cancelled"), "cancelled", "JOB_STATE_CANCELLED", "provider", "PROVIDER_BATCH_CANCELLED")
}

func TestBatchImageTraceRecorder_SuccessfulIndexEmitsItemResults(t *testing.T) {
	manager, exportedSpans := newBatchTraceTestManager(t)
	recorder := provideBatchImageTraceRecorder(manager)
	continuation := batchTraceTestContinuation()
	errorCode := "SAFETY_BLOCKED"

	recorder.RecordBatchImageResult(context.Background(), &service.BatchImageJob{
		BatchID: "batch-items", Model: "imagen-test", TraceContinuation: &continuation,
	}, service.BatchImageTraceResult{
		ProviderState: "JOB_STATE_SUCCEEDED",
		Items: []service.CreateBatchImageItemParams{
			{CustomID: "item-ok", Status: service.BatchImageItemStatusSuccess, ImageCount: 1},
			{CustomID: "item-failed", Status: service.BatchImageItemStatusFailed, ErrorCode: &errorCode},
		},
	})

	spans := exportedSpans()
	require.Len(t, spans, 2)
	seen := make(map[string]string, 2)
	for _, span := range spans {
		attrs := batchTraceAttributes(span)
		var metadata map[string]string
		require.NoError(t, json.Unmarshal([]byte(batchTraceStringAttribute(t, attrs, "langfuse.observation.metadata")), &metadata))
		require.Equal(t, "batch-items", metadata["task_id"])
		itemID := metadata["item_id"]
		require.NotEmpty(t, itemID)
		var output map[string]any
		require.NoError(t, json.Unmarshal([]byte(batchTraceStringAttribute(t, attrs, "langfuse.observation.output")), &output))
		require.Equal(t, "JOB_STATE_SUCCEEDED", output["provider_state"])
		seen[itemID], _ = output["status"].(string)
	}
	require.Equal(t, map[string]string{"item-ok": "completed", "item-failed": "failed"}, seen)
}

func newBatchTraceTestManager(t *testing.T) (*modeltrace.Manager, func() []*tracepb.Span) {
	t.Helper()
	var mu sync.Mutex
	var requests []*collectortracepb.ExportTraceServiceRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		request := &collectortracepb.ExportTraceServiceRequest{}
		if err := proto.Unmarshal(body, request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, request)
		mu.Unlock()
		response, _ := proto.Marshal(&collectortracepb.ExportTraceServiceResponse{})
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(response)
	}))
	t.Cleanup(server.Close)

	manager, err := modeltrace.NewManager(context.Background(), config.ModelTracingConfig{
		Enabled: true, Endpoint: server.URL + "/api/public/otel",
		PublicKey: "public", SecretKey: "secret",
		PromptMaxBytes: 4096, ResponseMaxBytes: 4096, MediaMaxBytes: 4096,
	})
	require.NoError(t, err)

	return manager, func() []*tracepb.Span {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, manager.Shutdown(ctx))
		mu.Lock()
		defer mu.Unlock()
		var spans []*tracepb.Span
		for _, request := range requests {
			for _, resourceSpans := range request.ResourceSpans {
				for _, scopeSpans := range resourceSpans.ScopeSpans {
					spans = append(spans, scopeSpans.Spans...)
				}
			}
		}
		return spans
	}
}

func batchTraceTestContinuation() recording.TraceContinuation {
	return recording.TraceContinuation{
		TraceID: strings.Repeat("1", 32), SpanID: strings.Repeat("2", 16), TraceFlags: 1,
		GenerationFingerprint: strings.Repeat("3", 64),
	}
}

func spanForBatchTask(t *testing.T, spans []*tracepb.Span, taskID string) *tracepb.Span {
	t.Helper()
	for _, span := range spans {
		if batchTraceStringAttribute(t, batchTraceAttributes(span), "langfuse.trace.metadata.task_id") == taskID {
			return span
		}
	}
	t.Fatalf("span for task %q not found", taskID)
	return nil
}

func assertBatchTraceJobTerminal(t *testing.T, span *tracepb.Span, status, providerState, errorStage, errorCode string) {
	t.Helper()
	attrs := batchTraceAttributes(span)
	require.Equal(t, tracepb.Status_STATUS_CODE_ERROR, span.Status.Code)
	require.Equal(t, status, batchTraceStringAttribute(t, attrs, "modeltrace.async.status"))
	var metadata map[string]string
	require.NoError(t, json.Unmarshal([]byte(batchTraceStringAttribute(t, attrs, "langfuse.observation.metadata")), &metadata))
	require.NotEmpty(t, metadata["task_id"])
	require.NotContains(t, metadata, "item_id")
	var output map[string]any
	require.NoError(t, json.Unmarshal([]byte(batchTraceStringAttribute(t, attrs, "langfuse.observation.output")), &output))
	require.Equal(t, status, output["status"])
	require.Equal(t, providerState, output["provider_state"])
	require.Equal(t, errorStage, output["error_stage"])
	require.Equal(t, errorCode, output["error_code"])
	require.NotContains(t, output, "image_count")
	for key := range attrs {
		require.NotContains(t, key, "usage")
		require.NotContains(t, key, "cost")
	}
}

func batchTraceAttributes(span *tracepb.Span) map[string]*commonpb.AnyValue {
	attrs := make(map[string]*commonpb.AnyValue, len(span.Attributes))
	for _, attr := range span.Attributes {
		attrs[attr.Key] = attr.Value
	}
	return attrs
}

func batchTraceStringAttribute(t *testing.T, attrs map[string]*commonpb.AnyValue, key string) string {
	t.Helper()
	value, ok := attrs[key]
	if !ok {
		return ""
	}
	return value.GetStringValue()
}
