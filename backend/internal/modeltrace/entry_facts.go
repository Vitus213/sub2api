package modeltrace

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/url"
	"strings"
	"unicode/utf8"
)

const maxEntryFactBytes = 512

type entryFacts struct {
	Protocol    string
	ClientModel string
}

func resolveEntryFacts(path, contentType string, body []byte) entryFacts {
	facts := entryFacts{Protocol: entryProtocol(path)}
	if model := geminiModelFromPath(path); model != "" {
		facts.ClientModel = boundedEntryFact(model)
		return facts
	}
	facts.ClientModel = boundedEntryFact(requestModel(contentType, body))
	return facts
}

func entryProtocol(path string) string {
	switch {
	case strings.Contains(path, "/v1beta/models/"):
		action := ""
		if index := strings.LastIndex(path, ":"); index >= 0 && index+1 < len(path) {
			action = strings.TrimSpace(path[index+1:])
		}
		if action != "" {
			return "gemini." + action
		}
		return "gemini"
	case strings.HasSuffix(path, "/messages/count_tokens"):
		return "anthropic.count_tokens"
	case strings.HasSuffix(path, "/messages"):
		return "anthropic.messages"
	case strings.HasSuffix(path, "/chat/completions"):
		return "openai.chat_completions"
	case strings.HasSuffix(path, "/responses"):
		return "openai.responses"
	case strings.HasSuffix(path, "/embeddings"):
		return "openai.embeddings"
	case strings.HasSuffix(path, "/alpha/search"):
		return "openai.search"
	case strings.HasSuffix(path, "/images/generations/async"):
		return "openai.images.generations.async"
	case strings.HasSuffix(path, "/images/edits/async"):
		return "openai.images.edits.async"
	case strings.HasSuffix(path, "/images/batches"):
		return "openai.images.batches"
	case strings.HasSuffix(path, "/images/generations"):
		return "openai.images.generations"
	case strings.HasSuffix(path, "/images/edits"):
		return "openai.images.edits"
	case strings.HasSuffix(path, "/images/tasks"):
		return "openai.images.tasks"
	case strings.HasSuffix(path, "/videos/generations"):
		return "openai.videos.generations"
	case strings.HasSuffix(path, "/videos/edits"):
		return "openai.videos.edits"
	case strings.HasSuffix(path, "/videos/extensions"):
		return "openai.videos.extensions"
	case strings.HasSuffix(path, "/videos/tasks"):
		return "openai.videos.tasks"
	default:
		return ""
	}
}

func geminiModelFromPath(path string) string {
	const marker = "/v1beta/models/"
	index := strings.Index(path, marker)
	if index < 0 {
		return ""
	}
	modelAction := path[index+len(marker):]
	if colon := strings.IndexByte(modelAction, ':'); colon >= 0 {
		modelAction = modelAction[:colon]
	}
	return strings.TrimSpace(modelAction)
}

func requestModel(contentType string, body []byte) string {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = strings.TrimSpace(strings.Split(contentType, ";")[0])
	}
	switch strings.ToLower(mediaType) {
	case "multipart/form-data":
		return multipartModel(body, params["boundary"])
	case "application/x-www-form-urlencoded":
		values, parseErr := url.ParseQuery(string(body))
		if parseErr == nil {
			return values.Get("model")
		}
		return ""
	default:
		var envelope struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &envelope) == nil {
			return envelope.Model
		}
		return ""
	}
}

func multipartModel(body []byte, boundary string) string {
	if len(body) == 0 || strings.TrimSpace(boundary) == "" {
		return ""
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err != nil {
			return ""
		}
		if part.FormName() != "model" {
			_ = part.Close()
			continue
		}
		value, readErr := io.ReadAll(io.LimitReader(part, maxEntryFactBytes+1))
		_ = part.Close()
		if readErr != nil {
			return ""
		}
		return string(value)
	}
}

func boundedEntryFact(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= maxEntryFactBytes {
		return value
	}
	cut := maxEntryFactBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}
