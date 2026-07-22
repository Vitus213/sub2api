package service

import (
	"flag"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	// Many legacy tests mutate Gin's process-global mode. Keep package-level
	// parallel tests serialized so those setup writes cannot race each other.
	_ = flag.Set("test.parallel", "1")
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
}
