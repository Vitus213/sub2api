package middleware

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const identityEstablishedHookContextKey = "identity_established_hook"

// ResolvedIdentity 是身份建立通知的中立载荷。零值表示对应身份未知。
type ResolvedIdentity struct {
	APIKeyID int64
	UserID   int64
	GroupID  int64
}

// IdentityEstablishedHook 在 API Key 身份成功解析后接收一次中立身份通知。
// Hook 仅用于旁路观察，不得影响鉴权结果。
type IdentityEstablishedHook func(*gin.Context, ResolvedIdentity)

// SetIdentityEstablishedHook 为当前请求安装身份建立通知回调。
// nil Context 不执行任何操作；nil Hook 等同于未安装回调。
func SetIdentityEstablishedHook(c *gin.Context, hook IdentityEstablishedHook) {
	if c == nil {
		return
	}
	c.Set(identityEstablishedHookContextKey, hook)
}

func notifyIdentityEstablished(c *gin.Context, identity ResolvedIdentity) {
	if c == nil {
		return
	}
	value, exists := c.Get(identityEstablishedHookContextKey)
	if !exists {
		return
	}
	hook, ok := value.(IdentityEstablishedHook)
	if !ok || hook == nil {
		return
	}

	// 观察边界必须 fail-open：回调故障不能改变现有鉴权状态或响应。
	defer func() {
		_ = recover()
	}()
	hook(c, identity)
}

func resolvedIdentityFromAPIKey(apiKey *service.APIKey) ResolvedIdentity {
	if apiKey == nil {
		return ResolvedIdentity{}
	}

	identity := ResolvedIdentity{APIKeyID: apiKey.ID}
	if apiKey.User != nil {
		identity.UserID = apiKey.User.ID
	}
	if apiKey.Group != nil {
		identity.GroupID = apiKey.Group.ID
	}
	return identity
}
