package modeltrace

import (
	"log/slog"
	"sync/atomic"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// AdminHandler exposes the runtime model tracing configuration to administrators.
type AdminHandler struct {
	manager *ConfigManager
}

func NewAdminHandler(manager *ConfigManager) *AdminHandler {
	return &AdminHandler{manager: manager}
}

func (h *AdminHandler) GetConfig(c *gin.Context) {
	if _, ok := requireModelTraceAdmin(c); !ok {
		return
	}
	if h == nil || h.manager == nil {
		response.Error(c, 503, "model tracing config is unavailable")
		return
	}
	response.Success(c, h.manager.GetConfig(c.Request.Context()))
}

func (h *AdminHandler) UpdateConfig(c *gin.Context) {
	subject, ok := requireModelTraceAdmin(c)
	if !ok {
		return
	}
	if h == nil || h.manager == nil {
		response.Error(c, 503, "model tracing config is unavailable")
		return
	}
	var request UpdateConfigRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		response.BadRequest(c, "invalid model tracing config request")
		return
	}
	updated, err := h.manager.Save(c.Request.Context(), request, subject.UserID)
	if response.ErrorFrom(c, err) {
		return
	}
	slog.Info("model tracing config updated",
		"audit", true,
		"user_id", subject.UserID,
		"config_version", updated.ConfigVersion,
		"enabled", updated.Enabled,
	)
	response.Success(c, updated)
}

func requireModelTraceAdmin(c *gin.Context) (middleware.AuthSubject, bool) {
	role, ok := middleware.GetUserRoleFromContext(c)
	if !ok {
		response.Unauthorized(c, "Unauthorized")
		return middleware.AuthSubject{}, false
	}
	if role != service.RoleAdmin {
		response.Forbidden(c, "Admin access required")
		return middleware.AuthSubject{}, false
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Unauthorized(c, "Unauthorized")
		return middleware.AuthSubject{}, false
	}
	return subject, true
}

var defaultAdminHandler atomic.Pointer[AdminHandler]

// InstallDefaultConfigManager connects the route helper to the process config manager.
func InstallDefaultConfigManager(manager *ConfigManager) {
	if manager == nil {
		defaultAdminHandler.Store(nil)
		return
	}
	defaultAdminHandler.Store(NewAdminHandler(manager))
}

// DefaultAdminHandler returns the currently installed process handler, if any.
func DefaultAdminHandler() *AdminHandler {
	return defaultAdminHandler.Load()
}
