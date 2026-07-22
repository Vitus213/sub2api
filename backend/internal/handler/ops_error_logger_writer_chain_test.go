package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type opsNestedResponseWriter struct {
	gin.ResponseWriter
}

func (w *opsNestedResponseWriter) UnwrapResponseWriter() gin.ResponseWriter {
	return w.ResponseWriter
}

func TestOpsErrorLoggerMiddlewareRestoresThroughNestedResponseWriter(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	observedStatus := 0
	router.Use(func(c *gin.Context) {
		c.Next()
		observedStatus = c.Writer.Status()
	})
	router.Use(OpsErrorLoggerMiddleware(nil))
	router.Use(func(c *gin.Context) {
		c.Writer = &opsNestedResponseWriter{ResponseWriter: c.Writer}
		c.Next()
	})
	router.GET("/nested-writer", func(c *gin.Context) {
		c.JSON(http.StatusForbidden, gin.H{"error": "denied"})
	})

	recorder := httptest.NewRecorder()
	require.NotPanics(t, func() {
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/nested-writer", nil))
	})
	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.Equal(t, http.StatusForbidden, observedStatus)
	require.JSONEq(t, `{"error":"denied"}`, recorder.Body.String())
}
