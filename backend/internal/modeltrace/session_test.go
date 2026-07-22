package modeltrace

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLangfuseSessionExtractor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		body   string
		header http.Header
		grok   bool
		want   string
	}{
		{name: "session_id", body: `{"session_id":"session-1"}`, want: "session-1"},
		{name: "conversation_id", body: `{"conversation_id":"conversation-1"}`, want: "conversation-1"},
		{name: "metadata session", body: `{"metadata":{"session_id":"metadata-1"}}`, want: "metadata-1"},
		{name: "structured user id session", body: `{"metadata":{"user_id":{"session_id":"nested-1"}}}`, want: "nested-1"},
		{name: "grok header on grok route", header: http.Header{"X-Grok-Conv-Id": []string{"grok-1"}}, grok: true, want: "grok-1"},
		{name: "grok header rejected on non grok route", header: http.Header{"X-Grok-Conv-Id": []string{"grok-1"}}},
		{name: "prompt cache key excluded", body: `{"prompt_cache_key":"cache-1"}`},
		{name: "sticky hash excluded", body: `{"session_hash":"sticky-1","metadata":{"sticky_hash":"sticky-2"}}`},
		{name: "content fallback excluded", body: `{"input":"same content"}`},
		{name: "generated upstream session excluded", body: `{"metadata":{"upstream_session_id":"generated-1"}}`},
		{name: "non string excluded", body: `{"session_id":42,"metadata":{"session_id":true}}`},
		{name: "invalid json excluded", body: `{`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, ExtractLangfuseSessionID([]byte(tt.body), tt.header, tt.grok))
		})
	}
}
