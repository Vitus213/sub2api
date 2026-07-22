package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/modeltrace"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"

	"github.com/gin-gonic/gin"
)

// RegisterModelTracingRoutes mounts the minimal admin configuration surface.
// The surrounding admin group supplies authentication and audit middleware.
func RegisterModelTracingRoutes(admin *gin.RouterGroup) {
	group := admin.Group("/model-tracing")
	group.GET("/config", dispatchModelTracingAdmin(func(h *modeltrace.AdminHandler, c *gin.Context) {
		h.GetConfig(c)
	}))
	group.PUT("/config", dispatchModelTracingAdmin(func(h *modeltrace.AdminHandler, c *gin.Context) {
		h.UpdateConfig(c)
	}))
}

func dispatchModelTracingAdmin(action func(*modeltrace.AdminHandler, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		handler := modeltrace.DefaultAdminHandler()
		if handler == nil {
			response.Error(c, 503, "model tracing config is unavailable")
			return
		}
		action(handler, c)
	}
}
