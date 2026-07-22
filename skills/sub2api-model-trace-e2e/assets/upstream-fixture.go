package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

const successSSE = "event: message_start\n" +
	"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_e2e_attempt\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-e2e\",\"stop_reason\":\"\",\"usage\":{\"input_tokens\":7}},\"access_token\":\"e2e-upstream-success-secret\"}\n\n" +
	"event: content_block_start\n" +
	"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"e2e upstream success\"}}\n\n" +
	"event: message_delta\n" +
	"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
	"event: message_stop\n" +
	"data: {\"type\":\"message_stop\"}\n\n"

type counters struct {
	fail, ok, hold, image, otlpOK, otlpError, otlpSlow             atomic.Int64
	batchAuthOK, batchAuthFail, batchUpload, batchCreate, batchGet atomic.Int64
	batchMetadata, batchDownload                                   atomic.Int64
}

var stats counters

type holdGate struct {
	sync.Mutex
	ready   chan struct{}
	release chan struct{}
}

var gate = holdGate{ready: make(chan struct{}, 1), release: make(chan struct{})}

type batchFixture struct {
	sync.Mutex
	keys []string
}

var batch batchFixture

type protocolFixture struct {
	sync.Mutex
	calls map[string]int64
}

var protocols = protocolFixture{calls: make(map[string]int64)}

func (p *protocolFixture) record(name string) {
	p.Lock()
	defer p.Unlock()
	p.calls[name]++
}

func (p *protocolFixture) snapshot() map[string]int64 {
	p.Lock()
	defer p.Unlock()
	out := make(map[string]int64, len(p.calls))
	for key, value := range p.calls {
		out[key] = value
	}
	return out
}
func readJSONBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "cannot read request", http.StatusBadRequest)
		return nil, false
	}
	if strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") && !json.Valid(body) {
		http.Error(w, "invalid JSON request", http.StatusBadRequest)
		return nil, false
	}
	return body, true
}

func writeJSON(w http.ResponseWriter, requestID, payload string) {
	w.Header().Set("Content-Type", "application/json")
	if requestID != "" {
		w.Header().Set("X-Request-Id", requestID)
	}
	_, _ = fmt.Fprint(w, payload)
}

func writeOpenAIResponsesSSE(w http.ResponseWriter, requestID string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Request-Id", requestID)
	_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"response_id\":\"resp_e2e_matrix\",\"delta\":\"matrix response\"}\n\n")
	_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_e2e_matrix\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-e2e-upstream\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"matrix response\"}]}],\"usage\":{\"input_tokens\":13,\"output_tokens\":5,\"total_tokens\":18}}}\n\n")
}

func writeGeminiSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Request-Id", "req_e2e_gemini_stream")
	_, _ = fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"matrix gemini stream\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":17,\"candidatesTokenCount\":4,\"totalTokenCount\":21},\"modelVersion\":\"gemini-e2e-upstream\"}\n\n")
}

func requireBearer(w http.ResponseWriter, r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return true
	}
	http.Error(w, "missing bearer token", http.StatusUnauthorized)
	return false
}

func requireAnthropicKey(w http.ResponseWriter, r *http.Request) bool {
	if strings.TrimSpace(r.Header.Get("x-api-key")) != "" {
		return true
	}
	http.Error(w, "missing anthropic key", http.StatusUnauthorized)
	return false
}

func (g *holdGate) reset() {
	g.Lock()
	defer g.Unlock()
	g.ready = make(chan struct{}, 1)
	g.release = make(chan struct{})
}

func (g *holdGate) snapshot() (<-chan struct{}, chan<- struct{}) {
	g.Lock()
	defer g.Unlock()
	return g.release, g.ready
}

func (g *holdGate) releaseCurrent() {
	g.Lock()
	defer g.Unlock()
	select {
	case <-g.release:
	default:
		close(g.release)
	}
}

func (b *batchFixture) replaceKeys(keys []string) {
	b.Lock()
	defer b.Unlock()
	b.keys = append([]string(nil), keys...)
}

func (b *batchFixture) keyCount() int {
	b.Lock()
	defer b.Unlock()
	return len(b.keys)
}

func (b *batchFixture) writeOutput(w http.ResponseWriter, mediaCanary string) {
	b.Lock()
	keys := append([]string(nil), b.keys...)
	b.Unlock()
	w.Header().Set("Content-Type", "application/jsonl")
	enc := json.NewEncoder(w)
	for index, key := range keys {
		if index == 0 {
			_ = enc.Encode(map[string]any{
				"key": key,
				"response": map[string]any{"candidates": []any{map[string]any{
					"content": map[string]any{"parts": []any{map[string]any{
						"inlineData": map[string]any{"mimeType": "image/png", "data": base64.StdEncoding.EncodeToString([]byte(mediaCanary))},
					}}},
				}}},
			})
			continue
		}
		_ = enc.Encode(map[string]any{
			"key":   key,
			"error": map[string]any{"code": "SAFETY", "message": "blocked by e2e policy"},
		})
	}
}

func writeSSE(w http.ResponseWriter, requestID string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Request-Id", requestID)
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	for _, frame := range strings.SplitAfter(successSSE, "\n\n") {
		if frame == "" {
			continue
		}
		_, _ = fmt.Fprint(w, frame)
		flusher.Flush()
	}
}

func main() {
	geminiAPIKey := os.Getenv("E2E_GEMINI_API_KEY")
	mediaCanary := os.Getenv("E2E_BATCH_MEDIA_CANARY")
	tlsCert := os.Getenv("E2E_TLS_CERT_FILE")
	tlsKey := os.Getenv("E2E_TLS_KEY_FILE")
	if geminiAPIKey == "" || mediaCanary == "" || tlsCert == "" || tlsKey == "" {
		log.Fatal("fixture requires Gemini credential, media canary, and TLS certificate paths")
	}
	authorized := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("x-goog-api-key") == geminiAPIKey {
			stats.batchAuthOK.Add(1)
			return true
		}
		stats.batchAuthFail.Add(1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		payload := map[string]any{
			"fail": stats.fail.Load(), "ok": stats.ok.Load(), "hold": stats.hold.Load(),
			"image": stats.image.Load(), "otlp_ok": stats.otlpOK.Load(),
			"otlp_error": stats.otlpError.Load(), "otlp_slow": stats.otlpSlow.Load(),
			"batch_auth_ok": stats.batchAuthOK.Load(), "batch_auth_fail": stats.batchAuthFail.Load(),
			"batch_upload": stats.batchUpload.Load(), "batch_create": stats.batchCreate.Load(),
			"batch_get": stats.batchGet.Load(), "batch_metadata": stats.batchMetadata.Load(),
			"batch_download": stats.batchDownload.Load(), "batch_input_items": int64(batch.keyCount()),
			"protocol": protocols.snapshot(),
		}
		_ = json.NewEncoder(w).Encode(payload)
	})
	mux.HandleFunc("/control/hold/reset", func(w http.ResponseWriter, _ *http.Request) {
		gate.reset()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/control/hold/release", func(w http.ResponseWriter, _ *http.Request) {
		gate.releaseCurrent()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/fail/v1/messages", func(w http.ResponseWriter, _ *http.Request) {
		stats.fail.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req_e2e_failed_attempt")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"e2e forced failover"},"api_key":"e2e-upstream-failure-secret"}`)
	})
	mux.HandleFunc("/ok/v1/messages", func(w http.ResponseWriter, _ *http.Request) {
		stats.ok.Add(1)
		writeSSE(w, "req_e2e_success_attempt")
	})
	mux.HandleFunc("/hold/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		stats.hold.Add(1)
		release, ready := gate.snapshot()
		select {
		case ready <- struct{}{}:
		default:
		}
		select {
		case <-release:
			writeSSE(w, "req_e2e_hold_attempt")
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/openai/v1/images/generations", func(w http.ResponseWriter, _ *http.Request) {
		stats.image.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"created":1784690000,"data":[{"b64_json":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=","revised_prompt":"e2e async image"}]}`)
	})
	mux.HandleFunc("/matrix/anthropic/v1/messages/count_tokens", func(w http.ResponseWriter, r *http.Request) {
		if !requireAnthropicKey(w, r) {
			return
		}
		if _, ok := readJSONBody(w, r); !ok {
			return
		}
		protocols.record("anthropic.count_tokens")
		writeJSON(w, "req_e2e_anthropic_count", `{"input_tokens":11}`)
	})
	mux.HandleFunc("/matrix/anthropic/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if !requireAnthropicKey(w, r) {
			return
		}
		body, ok := readJSONBody(w, r)
		if !ok {
			return
		}
		protocols.record("anthropic.messages")
		if strings.Contains(string(body), `"stream":true`) {
			writeSSE(w, "req_e2e_anthropic_stream")
			return
		}
		writeJSON(w, "req_e2e_anthropic", `{"id":"msg_e2e_matrix","type":"message","role":"assistant","model":"claude-e2e-upstream","content":[{"type":"text","text":"matrix anthropic"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":3}}`)
	})
	mux.HandleFunc("/matrix/openai/", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		body, ok := readJSONBody(w, r)
		if !ok {
			return
		}
		switch r.URL.Path {
		case "/matrix/openai/v1/chat/completions":
			protocols.record("openai.chat_completions")
			writeJSON(w, "req_e2e_openai_chat", `{"id":"chatcmpl_e2e_matrix","object":"chat.completion","model":"gpt-e2e-upstream","choices":[{"index":0,"message":{"role":"assistant","content":"matrix chat"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":4,"total_tokens":16}}`)
		case "/matrix/openai/v1/responses":
			protocols.record("openai.responses")
			if strings.Contains(string(body), `"stream":true`) {
				writeOpenAIResponsesSSE(w, "req_e2e_openai_responses_stream")
				return
			}
			writeJSON(w, "req_e2e_openai_responses", `{"id":"resp_e2e_matrix","object":"response","status":"completed","model":"gpt-e2e-upstream","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"matrix response"}]}],"usage":{"input_tokens":13,"output_tokens":5,"total_tokens":18}}`)
		case "/matrix/openai/v1/embeddings":
			protocols.record("openai.embeddings")
			writeJSON(w, "req_e2e_openai_embeddings", `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.125,-0.25,0.5]}],"model":"embed-e2e-upstream","usage":{"prompt_tokens":3,"total_tokens":3}}`)
		case "/matrix/openai/v1/alpha/search":
			protocols.record("openai.search")
			writeJSON(w, "req_e2e_openai_search", `{"results":[{"title":"matrix result","url":"https://example.test/e2e","snippet":"deterministic search"}]}`)
		case "/matrix/openai/v1/responses/input_tokens":
			protocols.record("openai.count_tokens")
			writeJSON(w, "req_e2e_openai_count", `{"object":"response.input_tokens","input_tokens":11}`)
		case "/matrix/openai/v1/images/generations":
			protocols.record("openai.images.generations")
			writeJSON(w, "req_e2e_openai_image_generation", `{"created":1784690000,"data":[{"url":"https://example.test/matrix-generation.png","revised_prompt":"matrix image generation"}]}`)
		case "/matrix/openai/v1/images/edits":
			protocols.record("openai.images.edits")
			writeJSON(w, "req_e2e_openai_image_edit", `{"created":1784690001,"data":[{"url":"https://example.test/matrix-edit.png","revised_prompt":"matrix image edit"}]}`)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/matrix/grok/", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(w, r) {
			return
		}
		if _, ok := readJSONBody(w, r); !ok {
			return
		}
		switch r.URL.Path {
		case "/matrix/grok/v1/images/generations":
			protocols.record("openai.images.generations")
			writeJSON(w, "req_e2e_grok_image_generation", `{"created":1784690002,"data":[{"url":"https://example.test/grok-generation.png"}]}`)
		case "/matrix/grok/v1/images/edits":
			protocols.record("openai.images.edits")
			writeJSON(w, "req_e2e_grok_image_edit", `{"created":1784690003,"data":[{"url":"https://example.test/grok-edit.png"}]}`)
		case "/matrix/grok/v1/videos/generations":
			protocols.record("openai.videos.generations")
			writeJSON(w, "req_e2e_grok_video_generation", `{"request_id":"video_e2e_generation","status":"pending"}`)
		case "/matrix/grok/v1/videos/edits":
			protocols.record("openai.videos.edits")
			writeJSON(w, "req_e2e_grok_video_edit", `{"request_id":"video_e2e_edit","status":"pending"}`)
		case "/matrix/grok/v1/videos/extensions":
			protocols.record("openai.videos.extensions")
			writeJSON(w, "req_e2e_grok_video_extension", `{"request_id":"video_e2e_extension","status":"pending"}`)
		default:
			http.NotFound(w, r)
		}
	})
	geminiHandler := func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSpace(r.Header.Get("x-goog-api-key")) == "" {
			http.Error(w, "missing gemini key", http.StatusUnauthorized)
			return
		}
		if _, ok := readJSONBody(w, r); !ok {
			return
		}
		platform := "gemini"
		if strings.Contains(r.URL.Path, "/antigravity/") {
			platform = "antigravity"
		}
		switch {
		case strings.HasSuffix(r.URL.Path, ":generateContent"):
			protocols.record(platform + ".generateContent")
			writeJSON(w, "req_e2e_"+platform+"_generate", `{"candidates":[{"content":{"role":"model","parts":[{"text":"matrix gemini"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":17,"candidatesTokenCount":4,"totalTokenCount":21},"modelVersion":"gemini-e2e-upstream"}`)
		case strings.HasSuffix(r.URL.Path, ":streamGenerateContent"):
			protocols.record(platform + ".streamGenerateContent")
			writeGeminiSSE(w)
		default:
			http.NotFound(w, r)
		}
	}
	mux.HandleFunc("/matrix/gemini/v1beta/models/", geminiHandler)
	mux.HandleFunc("/matrix/gemini/antigravity/v1beta/models/", geminiHandler)
	mux.HandleFunc("/otlp-ok/v1/traces", func(w http.ResponseWriter, _ *http.Request) {
		stats.otlpOK.Add(1)
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/otlp-500/v1/traces", func(w http.ResponseWriter, _ *http.Request) {
		stats.otlpError.Add(1)
		http.Error(w, "e2e forced OTLP failure", http.StatusInternalServerError)
	})
	mux.HandleFunc("/otlp-slow/v1/traces", func(w http.ResponseWriter, r *http.Request) {
		stats.otlpSlow.Add(1)
		<-r.Context().Done()
		w.WriteHeader(http.StatusGatewayTimeout)
	})
	mux.HandleFunc("/upload/v1beta/files", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		stats.batchUpload.Add(1)
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			http.Error(w, "invalid multipart upload", http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing file", http.StatusBadRequest)
			return
		}
		defer func() { _ = file.Close() }()
		keys := make([]string, 0, 2)
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			var line struct {
				Key string `json:"key"`
			}
			if json.Unmarshal(scanner.Bytes(), &line) == nil && strings.TrimSpace(line.Key) != "" {
				keys = append(keys, strings.TrimSpace(line.Key))
			}
		}
		if scanner.Err() != nil || len(keys) == 0 {
			http.Error(w, "invalid JSONL", http.StatusBadRequest)
			return
		}
		batch.replaceKeys(keys)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"file": map[string]any{
			"name": "files/e2e-input", "displayName": "e2e-input", "uri": "files/e2e-input", "mimeType": "application/jsonl",
		}})
	})
	mux.HandleFunc("/v1beta/models/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ":batchGenerateContent") || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if !authorized(w, r) {
			return
		}
		stats.batchCreate.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "batches/e2e-batch", "state": "JOB_STATE_PENDING"})
	})
	mux.HandleFunc("/v1beta/batches/e2e-batch", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		stats.batchGet.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "batches/e2e-batch", "state": "JOB_STATE_SUCCEEDED",
			"dest": map[string]any{"fileName": "files/e2e-output"},
		})
	})
	mux.HandleFunc("/v1beta/files/e2e-output", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		stats.batchMetadata.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"downloadUri": "https://generativelanguage.googleapis.com/v1beta/files/e2e-output:download",
			"mimeType":    "application/jsonl",
		})
	})
	mux.HandleFunc("/v1beta/files/e2e-output:download", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		stats.batchDownload.Add(1)
		batch.writeOutput(w, mediaCanary)
	})

	errs := make(chan error, 2)
	go func() { errs <- http.ListenAndServe("127.0.0.1:18081", mux) }()
	go func() { errs <- http.ListenAndServeTLS("127.0.0.1:443", tlsCert, tlsKey, mux) }()
	log.Fatal(<-errs)
}
