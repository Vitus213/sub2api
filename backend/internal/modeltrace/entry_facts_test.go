package modeltrace

import (
	"bytes"
	"mime/multipart"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveEntryFacts(t *testing.T) {
	t.Run("JSON protocol and model", func(t *testing.T) {
		facts := resolveEntryFacts("/v1/responses", "application/json", []byte(`{"model":"gpt-client"}`))
		require.Equal(t, "openai.responses", facts.Protocol)
		require.Equal(t, "gpt-client", facts.ClientModel)
	})

	t.Run("Gemini model comes from path", func(t *testing.T) {
		facts := resolveEntryFacts("/v1beta/models/gemini-client:streamGenerateContent", "application/json", []byte(`{"model":"ignored"}`))
		require.Equal(t, "gemini.streamGenerateContent", facts.Protocol)
		require.Equal(t, "gemini-client", facts.ClientModel)
	})

	t.Run("multipart model field", func(t *testing.T) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		require.NoError(t, writer.WriteField("model", "gpt-image-client"))
		part, err := writer.CreateFormFile("image", "image.png")
		require.NoError(t, err)
		_, err = part.Write([]byte("binary-image"))
		require.NoError(t, err)
		require.NoError(t, writer.Close())

		facts := resolveEntryFacts("/v1/images/edits", writer.FormDataContentType(), body.Bytes())
		require.Equal(t, "openai.images.edits", facts.Protocol)
		require.Equal(t, "gpt-image-client", facts.ClientModel)
	})

	t.Run("entry facts stay bounded and valid UTF-8", func(t *testing.T) {
		model := strings.Repeat("界", 300)
		facts := resolveEntryFacts("/v1/chat/completions", "application/json", []byte(`{"model":"`+model+`"}`))
		require.Equal(t, "openai.chat_completions", facts.Protocol)
		require.LessOrEqual(t, len(facts.ClientModel), maxEntryFactBytes)
		require.True(t, strings.HasPrefix(model, facts.ClientModel))
		require.NotContains(t, facts.ClientModel, "�")
	})
}
