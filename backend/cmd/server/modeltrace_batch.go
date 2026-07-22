package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/modeltrace"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type batchImageTraceRecorder struct {
	manager *modeltrace.Manager
}

func provideBatchImageTraceRecorder(manager *modeltrace.Manager) service.BatchImageTraceRecorder {
	return &batchImageTraceRecorder{manager: manager}
}

func (r *batchImageTraceRecorder) RecordBatchImageResult(_ context.Context, job *service.BatchImageJob, result service.BatchImageTraceResult) {
	if r == nil || r.manager == nil || job == nil || job.TraceContinuation == nil {
		return
	}
	apiKeyID := int64(0)
	if job.APIKeyID != nil {
		apiKeyID = *job.APIKeyID
	}
	identity := servermiddleware.ResolvedIdentity{UserID: job.UserID, APIKeyID: apiKeyID}
	continuation := *job.TraceContinuation
	record := func(status, itemID string, imageCount int, providerState, errorStage, errorCode string) {
		var traceErr error
		if status == service.BatchImageJobStatusFailed || status == service.BatchImageJobStatusCancelled {
			message := strings.TrimSpace(errorCode)
			if message == "" {
				message = status
			}
			traceErr = errors.New(message)
		}
		output, _ := json.Marshal(struct {
			Status        string `json:"status"`
			ImageCount    int    `json:"image_count,omitempty"`
			ProviderState string `json:"provider_state,omitempty"`
			ErrorStage    string `json:"error_stage,omitempty"`
			ErrorCode     string `json:"error_code,omitempty"`
		}{
			Status:        status,
			ImageCount:    imageCount,
			ProviderState: strings.TrimSpace(providerState),
			ErrorStage:    strings.TrimSpace(errorStage),
			ErrorCode:     strings.TrimSpace(errorCode),
		})
		execution := r.manager.StartAsyncExecution(context.Background(), continuation, modeltrace.AsyncExecutionMetadata{
			Identity:  identity,
			TaskID:    job.BatchID,
			ItemID:    itemID,
			Model:     job.Model,
			Operation: "image.generation",
		}, nil)
		execution.End(status, output, traceErr)
	}

	if len(result.Items) == 0 {
		status := strings.TrimSpace(result.Status)
		if status == "" {
			return
		}
		record(status, "", 0, result.ProviderState, result.ErrorStage, result.ErrorCode)
		return
	}
	for _, item := range result.Items {
		status := "completed"
		errorCode := ""
		if item.Status != service.BatchImageItemStatusSuccess {
			status = "failed"
			if item.ErrorCode != nil {
				errorCode = strings.TrimSpace(*item.ErrorCode)
			}
			if errorCode == "" {
				errorCode = "BATCH_IMAGE_ITEM_FAILED"
			}
		}
		record(status, item.CustomID, item.ImageCount, result.ProviderState, result.ErrorStage, errorCode)
	}
}
