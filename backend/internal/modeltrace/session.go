package modeltrace

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// ExtractLangfuseSessionID accepts only explicit client-owned conversation
// identifiers. Cache, routing, content-derived and generated upstream keys are
// deliberately outside this allowlist.
func ExtractLangfuseSessionID(body []byte, headers http.Header, grokRoute bool) string {
	if len(body) > 0 && gjson.ValidBytes(body) {
		for _, path := range []string{
			"session_id",
			"conversation_id",
			"metadata.session_id",
			"metadata.user_id.session_id",
		} {
			value := gjson.GetBytes(body, path)
			if !value.Exists() || value.Type != gjson.String {
				continue
			}
			if sessionID := strings.TrimSpace(value.String()); sessionID != "" {
				return sessionID
			}
		}
	}
	if grokRoute && headers != nil {
		return strings.TrimSpace(headers.Get("x-grok-conv-id"))
	}
	return ""
}
