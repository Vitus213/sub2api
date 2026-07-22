package handler

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/modeltrace"
	"github.com/Wei-Shaw/sub2api/internal/modeltrace/recording"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

type openAIWSTraceTurns struct {
	handler     *OpenAIGatewayHandler
	parent      context.Context
	identity    servermiddleware.ResolvedIdentity
	connection  string
	path        string
	mu          sync.Mutex
	activeTurns map[int]*modeltrace.ResponsesWSTurn
}

func newOpenAIWSTraceTurns(h *OpenAIGatewayHandler, c *gin.Context, apiKey *service.APIKey, subject servermiddleware.AuthSubject) *openAIWSTraceTurns {
	parent := context.Background()
	path := ""
	connection := ""
	if c != nil && c.Request != nil {
		parent = c.Request.Context()
		path = c.Request.URL.Path
		connection, _ = parent.Value(ctxkey.ClientRequestID).(string)
	}
	groupID := int64(0)
	if apiKey != nil {
		if apiKey.Group != nil {
			groupID = apiKey.Group.ID
		} else if apiKey.GroupID != nil {
			groupID = *apiKey.GroupID
		}
	}
	apiKeyID := int64(0)
	if apiKey != nil {
		apiKeyID = apiKey.ID
	}
	return &openAIWSTraceTurns{
		handler: h, parent: parent, connection: strings.TrimSpace(connection), path: path,
		identity:    servermiddleware.ResolvedIdentity{APIKeyID: apiKeyID, UserID: subject.UserID, GroupID: groupID},
		activeTurns: make(map[int]*modeltrace.ResponsesWSTurn),
	}
}

func isOpenAIWSResponseCreateFrame(payload []byte) bool {
	return gjson.ValidBytes(payload) && strings.TrimSpace(gjson.GetBytes(payload, "type").String()) == "response.create"
}

func (t *openAIWSTraceTurns) start(turn int, payload []byte, model string) {
	if t == nil || t.handler == nil || turn <= 0 {
		return
	}
	if !isOpenAIWSResponseCreateFrame(payload) {
		return
	}
	manager := t.handler.modelTraceManager.Load()
	if manager == nil {
		return
	}
	turnTrace := manager.StartResponsesWSTurn(t.parent, modeltrace.ResponsesWSTurnMetadata{
		Identity:            t.identity,
		ConnectionRequestID: t.connection,
		TurnRequestID:       uuid.NewString(),
		TurnIndex:           turn,
		Path:                t.path,
		Model:               strings.TrimSpace(model),
		SessionID:           modeltrace.ExtractLangfuseSessionID(payload, nil, false),
	}, payload)
	if turnTrace == nil {
		return
	}
	t.mu.Lock()
	if existing := t.activeTurns[turn]; existing != nil {
		existing.End(string(recording.StreamError), "duplicate_turn", errors.New("duplicate websocket turn"))
	}
	t.activeTurns[turn] = turnTrace
	t.mu.Unlock()
}

func (t *openAIWSTraceTurns) context(turn int) context.Context {
	if t == nil {
		return context.Background()
	}
	t.mu.Lock()
	turnTrace := t.activeTurns[turn]
	t.mu.Unlock()
	if turnTrace != nil {
		return turnTrace.Context()
	}
	return t.parent
}

func (t *openAIWSTraceTurns) observeClientWrite(turn int, payload []byte, writeErr error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	turnTrace := t.activeTurns[turn]
	t.mu.Unlock()
	if turnTrace != nil {
		turnTrace.ObserveClientWrite(payload, writeErr)
	}
}

func (t *openAIWSTraceTurns) finish(turn int, result *service.OpenAIForwardResult, turnErr error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	turnTrace := t.activeTurns[turn]
	delete(t.activeTurns, turn)
	t.mu.Unlock()
	if turnTrace == nil {
		return
	}
	status, stage, terminalErr := classifyOpenAIWSTraceOutcome(result, turnErr)
	turnTrace.End(status, stage, terminalErr)
}

func (t *openAIWSTraceTurns) finishOpen() {
	if t == nil {
		return
	}
	t.mu.Lock()
	open := t.activeTurns
	t.activeTurns = make(map[int]*modeltrace.ResponsesWSTurn)
	t.mu.Unlock()
	for _, turnTrace := range open {
		turnTrace.End(string(recording.StreamError), "connection_close", errors.New("websocket connection closed before turn completion"))
	}
}

func classifyOpenAIWSTraceOutcome(result *service.OpenAIForwardResult, turnErr error) (string, string, error) {
	if turnErr != nil {
		if errors.Is(turnErr, context.Canceled) {
			return string(recording.StreamCancelled), "turn_cancelled", turnErr
		}
		return string(recording.StreamError), "turn", turnErr
	}
	if result == nil {
		err := errors.New("websocket turn completed without result")
		return string(recording.StreamError), "turn_result", err
	}
	terminal := strings.ToLower(strings.TrimSpace(result.UpstreamTerminalEvent))
	switch {
	case strings.Contains(terminal, "cancel"):
		return string(recording.StreamCancelled), "upstream_terminal", nil
	case strings.Contains(terminal, "fail"), strings.Contains(terminal, "error"), strings.Contains(terminal, "incomplete"):
		return string(recording.StreamError), "upstream_terminal", errors.New("websocket upstream terminal failure")
	default:
		return string(recording.StreamCompleted), "", nil
	}
}
