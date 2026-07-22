package routes

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/modeltrace"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// RegisterGatewayRoutes 注册 API 网关路由（Claude/OpenAI/Gemini 兼容）
func RegisterGatewayRoutes(
	r *gin.Engine,
	h *handler.Handlers,
	apiKeyAuth middleware.APIKeyAuthMiddleware,
	apiKeyService *service.APIKeyService,
	subscriptionService *service.SubscriptionService,
	opsService *service.OpsService,
	settingService *service.SettingService,
	cfg *config.Config,
	modelTraceManagers ...*modeltrace.Manager,
) {
	bodyLimit := middleware.RequestBodyLimit(cfg.Gateway.MaxBodySize)
	textBodyLimit := middleware.RequestBodyLimit(cfg.Gateway.TextMaxBodySize)
	clientRequestID := middleware.ClientRequestID()
	opsErrorLogger := handler.OpsErrorLoggerMiddleware(opsService)
	endpointNorm := handler.InboundEndpointMiddleware()
	var modelTrace *modeltrace.Manager
	if len(modelTraceManagers) > 0 {
		modelTrace = modelTraceManagers[0]
	}
	if h != nil && h.OpenAIGateway != nil {
		h.OpenAIGateway.SetModelTraceManager(modelTrace)
	}
	modelTraceCandidate := modelTrace.CandidateMiddleware()
	modelTraceDeferred := modelTrace.DeferredCandidateMiddleware()

	// 未分组 Key 拦截中间件（按协议格式区分错误响应）
	requireGroupAnthropic := middleware.RequireGroupAssignment(settingService, middleware.AnthropicErrorWriter)
	requireGroupGoogle := middleware.RequireGroupAssignment(settingService, middleware.GoogleErrorWriter)

	isOpenAIResponsesCompatibleGatewayPlatform := func(c *gin.Context) bool {
		switch getGroupPlatform(c) {
		case service.PlatformOpenAI, service.PlatformGrok:
			return true
		default:
			return false
		}
	}
	isOpenAIGatewayPlatform := func(c *gin.Context) bool {
		return getGroupPlatform(c) == service.PlatformOpenAI
	}
	countTokensHandler := func(c *gin.Context) {
		switch getGroupPlatform(c) {
		case service.PlatformOpenAI:
			h.OpenAIGateway.CountTokens(c)
		case service.PlatformGrok:
			// Grok token counting is a local estimate and must not create a model Trace.
			h.OpenAIGateway.GrokCountTokens(c)
		case service.PlatformAntigravity:
			// Antigravity rejects this endpoint locally without an upstream model call.
			h.Gateway.CountTokens(c)
		default:
			h.Gateway.CountTokens(c)
		}
	}
	modelsHandler := func(c *gin.Context) {
		if isOpenAIGatewayPlatform(c) && c.Query("client_version") != "" {
			h.OpenAIGateway.CodexModels(c)
			return
		}
		h.Gateway.Models(c)
	}
	imagesHandler := func(c *gin.Context) {
		switch getGroupPlatform(c) {
		case service.PlatformOpenAI:
			h.OpenAIGateway.Images(c)
		case service.PlatformGrok:
			h.OpenAIGateway.GrokImages(c)
		default:
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
			c.JSON(http.StatusNotFound, gin.H{
				"error": gin.H{
					"type":    "not_found_error",
					"message": "Images API is not supported for this platform",
				},
			})
		}
	}
	videoGenerationHandler := func(c *gin.Context) {
		if getGroupPlatform(c) == service.PlatformGrok {
			h.OpenAIGateway.GrokVideoGeneration(c)
			return
		}
		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{
				"type":    "not_found_error",
				"message": "Videos API is not supported for this platform",
			},
		})
	}
	videoStatusHandler := func(c *gin.Context) {
		if getGroupPlatform(c) == service.PlatformGrok {
			h.OpenAIGateway.GrokVideoStatus(c)
			return
		}
		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{
				"type":    "not_found_error",
				"message": "Videos API is not supported for this platform",
			},
		})
	}
	videoContentHandler := func(c *gin.Context) {
		if getGroupPlatform(c) == service.PlatformGrok {
			h.OpenAIGateway.GrokVideoContent(c)
			return
		}
		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{
				"type":    "not_found_error",
				"message": "Videos API is not supported for this platform",
			},
		})
	}
	videoEditHandler := func(c *gin.Context) {
		if getGroupPlatform(c) == service.PlatformGrok {
			h.OpenAIGateway.GrokVideoEdit(c)
			return
		}
		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"type": "not_found_error", "message": "Videos API is not supported for this platform"}})
	}
	videoExtensionHandler := func(c *gin.Context) {
		if getGroupPlatform(c) == service.PlatformGrok {
			h.OpenAIGateway.GrokVideoExtension(c)
			return
		}
		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"type": "not_found_error", "message": "Videos API is not supported for this platform"}})
	}
	messagesHandler := func(c *gin.Context) {
		if isOpenAIResponsesCompatibleGatewayPlatform(c) {
			h.OpenAIGateway.Messages(c)
			return
		}
		h.Gateway.Messages(c)
	}
	responsesHandler := func(c *gin.Context) {
		if isOpenAIResponsesCompatibleGatewayPlatform(c) {
			h.OpenAIGateway.Responses(c)
			return
		}
		h.Gateway.Responses(c)
	}
	responsesWebSocketHandler := func(c *gin.Context) {
		h.OpenAIGateway.ResponsesWebSocket(c)
	}
	chatCompletionsHandler := func(c *gin.Context) {
		if isOpenAIResponsesCompatibleGatewayPlatform(c) {
			h.OpenAIGateway.ChatCompletions(c)
			return
		}
		h.Gateway.ChatCompletions(c)
	}
	embeddingsHandler := func(c *gin.Context) {
		if getGroupPlatform(c) != service.PlatformOpenAI {
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
			c.JSON(http.StatusNotFound, gin.H{
				"error": gin.H{
					"type":    "not_found_error",
					"message": "Embeddings API is not supported for this platform",
				},
			})
			return
		}
		h.OpenAIGateway.Embeddings(c)
	}

	apiKeyAuthHandler := gin.HandlerFunc(apiKeyAuth)
	googleAPIKeyAuth := middleware.APIKeyAuthWithSubscriptionGoogle(apiKeyService, subscriptionService, cfg)

	// API 网关（Claude/OpenAI 兼容）。模型执行与控制面使用显式、互斥的中间件链。
	// OpsErrorLogger owns a pooled writer, so it must wrap Candidate; Candidate must
	// still wrap API-key auth to observe the identity-resolution hook.
	// Billing historically runs after API-key auth but before the group-assignment guard.
	r.GET("/v1/sub2api/billing", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, apiKeyAuthHandler, h.Gateway.KeyBillingInfo)

	{
		gateway := r.Group("/v1", clientRequestID, opsErrorLogger, modelTraceCandidate)

		// Model execution candidates.
		gateway.POST("/messages", bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, messagesHandler)
		gateway.POST("/responses", bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, responsesHandler)
		gateway.POST("/responses/*subpath", bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, responsesHandler)
		gateway.POST("/alpha/search", bodyLimit, textBodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, h.OpenAIGateway.AlphaSearch)
		gateway.POST("/chat/completions", bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, chatCompletionsHandler)
		gateway.POST("/embeddings", bodyLimit, textBodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, embeddingsHandler)
		gateway.POST("/images/generations", bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, imagesHandler)
		gateway.POST("/images/edits", bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, imagesHandler)
		gateway.POST("/images/generations/async", bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, h.AsyncImage.Submit)
		gateway.POST("/images/edits/async", bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, h.AsyncImage.Submit)
		gateway.POST("/images/batches", bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, h.BatchImage.Submit)
		gateway.POST("/videos/generations", bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, videoGenerationHandler)
		gateway.POST("/videos/edits", bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, videoEditHandler)
		gateway.POST("/videos/extensions", bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, videoExtensionHandler)
	}
	r.POST("/v1/messages/count_tokens", clientRequestID, opsErrorLogger, modelTraceDeferred, bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, countTokensHandler)
	{
		// Control-plane routes retain their previous middleware order and never install Candidate.
		gateway := r.Group("/v1", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic)
		gateway.GET("/models", modelsHandler)
		gateway.GET("/usage", h.Gateway.Usage)
		gateway.GET("/responses", responsesWebSocketHandler)
		gateway.GET("/images/tasks/:task_id", h.AsyncImage.Get)
		gateway.GET("/images/batches", h.BatchImage.List)
		gateway.GET("/images/batches/models", h.BatchImage.Models)
		gateway.GET("/images/batches/:id", h.BatchImage.Get)
		gateway.GET("/images/batches/:id/items", h.BatchImage.Items)
		gateway.GET("/images/batches/:id/items/:custom_id/content", h.BatchImage.ItemContent)
		gateway.GET("/images/batches/:id/download", h.BatchImage.Download)
		gateway.POST("/images/batches/:id/cancel", h.BatchImage.Cancel)
		gateway.DELETE("/images/batches/:id", h.BatchImage.DeleteRecord)
		gateway.DELETE("/images/batches/:id/outputs", h.BatchImage.DeleteOutputs)
		gateway.GET("/videos/:request_id", videoStatusHandler)
		gateway.GET("/videos/:request_id/content", videoContentHandler)
	}

	// Gemini native API compatibility layer.
	{
		gemini := r.Group("/v1beta", clientRequestID, opsErrorLogger, modelTraceCandidate, bodyLimit, endpointNorm, googleAPIKeyAuth, requireGroupGoogle)
		// Gin treats ":" as a param marker, but Gemini uses "{model}:{action}" in the same segment.
		gemini.POST("/models/*modelAction", h.Gateway.GeminiV1BetaModels)
	}
	{
		gemini := r.Group("/v1beta", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, googleAPIKeyAuth, requireGroupGoogle)
		gemini.GET("/models", h.Gateway.GeminiV1BetaListModels)
		gemini.GET("/models/:model", h.Gateway.GeminiV1BetaGetModel)
	}

	// Root aliases: candidates install Candidate before the applicable body limit and auth chain.
	r.POST("/responses", clientRequestID, opsErrorLogger, modelTraceCandidate, bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, responsesHandler)
	r.POST("/responses/*subpath", clientRequestID, opsErrorLogger, modelTraceCandidate, bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, responsesHandler)
	r.POST("/alpha/search", clientRequestID, opsErrorLogger, modelTraceCandidate, textBodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, h.OpenAIGateway.AlphaSearch)
	r.POST("/chat/completions", clientRequestID, opsErrorLogger, modelTraceCandidate, bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, chatCompletionsHandler)
	r.POST("/embeddings", clientRequestID, opsErrorLogger, modelTraceCandidate, textBodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, embeddingsHandler)
	r.POST("/images/generations", clientRequestID, opsErrorLogger, modelTraceCandidate, bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, imagesHandler)
	r.POST("/images/edits", clientRequestID, opsErrorLogger, modelTraceCandidate, bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, imagesHandler)
	r.POST("/images/generations/async", clientRequestID, opsErrorLogger, modelTraceCandidate, bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, h.AsyncImage.Submit)
	r.POST("/images/edits/async", clientRequestID, opsErrorLogger, modelTraceCandidate, bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, h.AsyncImage.Submit)
	r.POST("/videos/generations", clientRequestID, opsErrorLogger, modelTraceCandidate, bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, videoGenerationHandler)
	r.POST("/videos/edits", clientRequestID, opsErrorLogger, modelTraceCandidate, bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, videoEditHandler)
	r.POST("/videos/extensions", clientRequestID, opsErrorLogger, modelTraceCandidate, bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, videoExtensionHandler)
	r.GET("/responses", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, responsesWebSocketHandler)
	r.GET("/models", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, modelsHandler)
	r.POST("/messages/count_tokens", clientRequestID, opsErrorLogger, modelTraceDeferred, bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, countTokensHandler)
	r.GET("/images/tasks/:task_id", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, h.AsyncImage.Get)
	r.GET("/videos/:request_id", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, videoStatusHandler)
	r.GET("/videos/:request_id/content", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, videoContentHandler)

	// Codex direct aliases.
	{
		codexDirect := r.Group("/backend-api/codex", clientRequestID, opsErrorLogger, modelTraceCandidate)
		codexDirect.POST("/responses", bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, responsesHandler)
		codexDirect.POST("/responses/*subpath", bodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, responsesHandler)
		codexDirect.POST("/alpha/search", bodyLimit, textBodyLimit, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic, h.OpenAIGateway.AlphaSearch)
	}
	{
		codexDirect := r.Group("/backend-api/codex", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, apiKeyAuthHandler, requireGroupAnthropic)
		codexDirect.GET("/responses", responsesWebSocketHandler)
		codexDirect.GET("/models", h.OpenAIGateway.CodexModels)
	}

	// Antigravity model list remains a control-plane route with its historical chain.
	r.GET("/antigravity/models", apiKeyAuthHandler, requireGroupAnthropic, h.Gateway.AntigravityModels)

	// Antigravity Anthropic-compatible routes.
	{
		antigravityV1 := r.Group("/antigravity/v1", clientRequestID, opsErrorLogger, modelTraceCandidate, bodyLimit, endpointNorm, middleware.ForcePlatform(service.PlatformAntigravity), apiKeyAuthHandler, requireGroupAnthropic)
		antigravityV1.POST("/messages", h.Gateway.Messages)
	}
	r.POST("/antigravity/v1/messages/count_tokens", clientRequestID, opsErrorLogger, modelTraceDeferred, bodyLimit, endpointNorm, middleware.ForcePlatform(service.PlatformAntigravity), apiKeyAuthHandler, requireGroupAnthropic, countTokensHandler)
	{
		antigravityV1 := r.Group("/antigravity/v1", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, middleware.ForcePlatform(service.PlatformAntigravity), apiKeyAuthHandler, requireGroupAnthropic)
		antigravityV1.GET("/models", h.Gateway.AntigravityModels)
		antigravityV1.GET("/usage", h.Gateway.Usage)
	}

	// Antigravity Gemini-compatible routes.
	{
		antigravityV1Beta := r.Group("/antigravity/v1beta", clientRequestID, opsErrorLogger, modelTraceCandidate, bodyLimit, endpointNorm, middleware.ForcePlatform(service.PlatformAntigravity), googleAPIKeyAuth, requireGroupGoogle)
		antigravityV1Beta.POST("/models/*modelAction", h.Gateway.GeminiV1BetaModels)
	}
	{
		antigravityV1Beta := r.Group("/antigravity/v1beta", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, middleware.ForcePlatform(service.PlatformAntigravity), googleAPIKeyAuth, requireGroupGoogle)
		antigravityV1Beta.GET("/models", h.Gateway.GeminiV1BetaListModels)
		antigravityV1Beta.GET("/models/:model", h.Gateway.GeminiV1BetaGetModel)
	}

}

// getGroupPlatform extracts the group platform from the API Key stored in context.
func getGroupPlatform(c *gin.Context) string {
	apiKey, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || apiKey.Group == nil {
		return ""
	}
	return apiKey.Group.Platform
}
