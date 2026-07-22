package modeltrace

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"
)

const redactedValue = "[REDACTED]"

var (
	textSecretPattern  = regexp.MustCompile(`(?i)["']([A-Za-z0-9_.-]*(?:authorization|proxy[_-]?authorization|api[_-]?key|access[_-]?token|refresh[_-]?token|id[_-]?token|auth[_-]?token|bearer[_-]?token|password|passwd|client[_-]?secret|private[_-]?key|secret|credential|set[_-]?cookie|session[_-]?cookie|cookie))["']\s*:\s*(?:"[^"]*"|'[^']*'|[^\s,;]+)`)
	authorizationLine  = regexp.MustCompile(`(?im)\b(proxy-authorization|authorization)\s*:\s*[^\r\n]*`)
	cookieLine         = regexp.MustCompile(`(?im)\b(set-cookie|cookie)\s*:\s*[^\r\n]*`)
	dataURLPattern     = regexp.MustCompile(`(?i)data:([A-Za-z0-9.+-]+/[A-Za-z0-9.+-]+);base64,([A-Za-z0-9_+/=-]+)`)
	absoluteURLPattern = regexp.MustCompile(`(?i)(?:[a-z][a-z0-9+.-]*://|(?:^|[^a-z0-9])//)[^\s"'<>\\]+`)
	camelCaseBoundary  = regexp.MustCompile(`([a-z0-9])([A-Z])`)
)

type capturePolicy struct {
	mediaMaxBytes       int
	captureMediaContent bool
}

// captureModelContent returns a deterministic, bounded trace representation.
// originalBytes may exceed len(raw) when the capture reader retained only a prefix.
func captureModelContent(raw []byte, originalBytes, limit int, policy capturePolicy) string {
	if originalBytes < len(raw) {
		originalBytes = len(raw)
	}
	if len(raw) == 0 {
		if originalBytes > 0 {
			return truncationMarker(originalBytes, 0)
		}
		return ""
	}

	source := raw
	if len(source) > limit {
		source = validUTF8Prefix(source, limit)
	}
	content := sanitizeStructuredContent(source, policy)
	sourceTruncated := originalBytes > len(source)
	if !sourceTruncated && len(content) <= limit {
		return string(content)
	}

	captured := validUTF8Prefix(content, limit)
	return string(captured) + truncationMarker(originalBytes, len(captured))
}

// captureModelContentWithType applies protocol-aware sanitization before the
// common bounded text/JSON capture. Multipart bodies are never treated as
// unstructured text because doing so could export raw uploaded files.
func captureModelContentWithType(raw []byte, originalBytes, limit int, contentType string, policy capturePolicy) string {
	trimmedContentType := strings.TrimSpace(contentType)
	declaredType := strings.ToLower(strings.TrimSpace(strings.SplitN(trimmedContentType, ";", 2)[0]))
	mediaType, params, err := mime.ParseMediaType(trimmedContentType)
	if err != nil {
		if strings.EqualFold(declaredType, "application/x-www-form-urlencoded") {
			return sanitizeFormURLEncoded(raw, originalBytes, limit, policy)
		}
		if !strings.HasPrefix(declaredType, "multipart/") {
			return captureModelContent(raw, originalBytes, limit, policy)
		}
		summary := summarizeMultipartContent(nil, originalBytes, declaredType, "", policy)
		return captureModelContent(summary, len(summary), limit, policy)
	}
	if strings.EqualFold(declaredType, "application/x-www-form-urlencoded") {
		return sanitizeFormURLEncoded(raw, originalBytes, limit, policy)
	}
	if !strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		return captureModelContent(raw, originalBytes, limit, policy)
	}
	summary := summarizeMultipartContent(raw, originalBytes, mediaType, params["boundary"], policy)
	return captureModelContent(summary, len(summary), limit, policy)
}

func summarizeMultipartContent(raw []byte, originalBytes int, contentType, boundary string, policy capturePolicy) []byte {
	if originalBytes < len(raw) {
		originalBytes = len(raw)
	}
	summary := map[string]any{
		"content_type": contentType,
		"body_bytes":   originalBytes,
		"media_count":  0,
	}
	if strings.TrimSpace(boundary) == "" {
		summary["content"] = "[MULTIPART OMITTED]"
		return marshalMultipartSummary(summary)
	}

	reader := multipart.NewReader(bytes.NewReader(raw), boundary)
	fields := make(map[string]any)
	media := make([]map[string]any, 0)
	truncated := originalBytes > len(raw)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			truncated = true
			break
		}
		name := strings.TrimSpace(part.FormName())
		if multipartPartIsMedia(part, name) {
			descriptor, complete := summarizeMultipartMedia(part, name, policy)
			media = append(media, descriptor)
			if !complete {
				truncated = true
			}
			continue
		}
		value, readErr := io.ReadAll(part)
		if readErr != nil {
			truncated = true
		}
		appendMultipartField(fields, name, string(validUTF8Prefix(value, len(value))))
	}

	summary["fields"] = sanitizeJSONValue(fields, "", "", policy)
	summary["media"] = media
	summary["media_count"] = len(media)
	if truncated {
		summary["truncated"] = true
	}
	return marshalMultipartSummary(summary)
}

func multipartPartIsMedia(part *multipart.Part, name string) bool {
	if part == nil {
		return false
	}
	if strings.TrimSpace(part.FileName()) != "" {
		return true
	}
	contentType, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	if strings.HasPrefix(contentType, "image/") || strings.HasPrefix(contentType, "audio/") ||
		strings.HasPrefix(contentType, "video/") || contentType == "application/octet-stream" ||
		contentType == "application/pdf" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "image", "images", "mask", "audio", "video", "file", "document":
		return true
	default:
		return false
	}
}

func summarizeMultipartMedia(part *multipart.Part, name string, policy capturePolicy) (map[string]any, bool) {
	contentType, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
	contentType = normalizeMediaType(contentType, "application/octet-stream")
	hasher := sha256.New()
	var captured bytes.Buffer
	captureLimit := 0
	if policy.captureMediaContent && policy.mediaMaxBytes > 0 {
		captureLimit = policy.mediaMaxBytes
	}
	buffer := make([]byte, 32*1024)
	total := 0
	complete := true
	for {
		n, err := part.Read(buffer)
		if n > 0 {
			_, _ = hasher.Write(buffer[:n])
			total += n
			if captured.Len() < captureLimit {
				remaining := captureLimit - captured.Len()
				if remaining > n {
					remaining = n
				}
				_, _ = captured.Write(buffer[:remaining])
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			complete = false
			break
		}
	}
	descriptor := map[string]any{
		"field":       name,
		"media_type":  contentType,
		"bytes":       total,
		"fingerprint": "sha256:" + hex.EncodeToString(hasher.Sum(nil)),
	}
	if !complete {
		descriptor["truncated"] = true
	}
	if policy.captureMediaContent {
		descriptor["captured_bytes"] = captured.Len()
		descriptor["content_base64"] = base64.StdEncoding.EncodeToString(captured.Bytes())
		if captured.Len() < total {
			descriptor["truncated"] = true
		}
	}
	return descriptor, complete
}

func appendMultipartField(fields map[string]any, name, value string) {
	if name == "" {
		name = "unnamed"
	}
	previous, exists := fields[name]
	if !exists {
		fields[name] = value
		return
	}
	switch typed := previous.(type) {
	case []any:
		fields[name] = append(typed, value)
	default:
		fields[name] = []any{previous, value}
	}
}

func marshalMultipartSummary(summary map[string]any) []byte {
	encoded, err := json.Marshal(summary)
	if err != nil {
		return []byte(`{"content":"[MULTIPART OMITTED]"}`)
	}
	return encoded
}

func sanitizeStructuredContent(raw []byte, policy capturePolicy) []byte {
	if needsStructuredSanitization(raw) {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err == nil {
			var trailing any
			if err := decoder.Decode(&trailing); errors.Is(err, io.EOF) {
				original := value
				sanitized := sanitizeJSONValue(value, "", "", policy)
				if reflect.DeepEqual(original, sanitized) {
					return raw
				}
				if encoded, marshalErr := json.Marshal(sanitized); marshalErr == nil {
					return encoded
				}
			}
		}
	}
	// Always run the unstructured sanitizer as fallback. This ensures
	// plain-text and non-JSON content (text/plain responses, SSE, etc.)
	// still gets URL/credential scrubbing, not just content that matched
	// structured sanitization triggers.
	return []byte(sanitizeUnstructuredText(string(validUTF8Prefix(raw, len(raw))), policy))
}

func needsStructuredSanitization(raw []byte) bool {
	// Root JSON objects/arrays may carry credentials in URL string values
	// under non-secret keys. Always run the structured sanitizer so the
	// JSON decoder normalizes escape sequences (\u002f, \/) into literal
	// characters the URL pattern can match. This check runs first so a
	// truncated JSON body missing its closing quote still gets sanitized.
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		return true
	}
	for i := 0; i < len(raw); i++ {
		quote := raw[i]
		if quote != '"' && quote != '\'' {
			continue
		}
		remaining := raw[i+1:]
		end := bytes.IndexByte(remaining, quote)
		if end < 0 {
			return bytesHasFoldPrefix(remaining, []byte("data:"))
		}
		token := remaining[:end]
		if bytesHasFoldPrefix(token, []byte("data:")) {
			return true
		}
		if len(token) <= 128 {
			key := string(token)
			if isSecretKey(key) || isMediaPayloadKey(key, "image") {
				return true
			}
		}
		i += end + 1
	}
	return bytesContainsFold(raw, []byte("authorization:")) ||
		bytesContainsFold(raw, []byte("proxy-authorization:")) ||
		bytesContainsFold(raw, []byte("cookie:")) ||
		bytesContainsFold(raw, []byte("set-cookie:"))
}

func bytesContainsFold(value, needle []byte) bool {
	for len(value) >= len(needle) {
		if bytes.EqualFold(value[:len(needle)], needle) {
			return true
		}
		value = value[1:]
	}
	return false
}

func bytesHasFoldPrefix(value, prefix []byte) bool {
	return len(value) >= len(prefix) && bytes.EqualFold(value[:len(prefix)], prefix)
}

func sanitizeJSONValue(value any, key, parentType string, policy capturePolicy) any {
	if isSecretKey(key) {
		return redactedValue
	}
	if isMediaPayloadKey(key, parentType) {
		return summarizeMedia(value, key, parentType, policy)
	}

	switch typed := value.(type) {
	case map[string]any:
		typeName, _ := typed["type"].(string)
		result := make(map[string]any, len(typed))
		for childKey, childValue := range typed {
			result[childKey] = sanitizeJSONValue(childValue, childKey, typeName, policy)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for i, child := range typed {
			result[i] = sanitizeJSONValue(child, key, parentType, policy)
		}
		return result
	case string:
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(typed)), "data:") {
			return summarizeMedia(typed, key, parentType, policy)
		}
		return scrubURLsInString(typed)
	default:
		return value
	}
}

func isSecretKey(key string) bool {
	normalized := camelCaseBoundary.ReplaceAllString(strings.TrimSpace(key), `${1}_${2}`)
	normalized = strings.ToLower(normalized)
	normalized = strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(normalized)
	switch normalized {
	case "authorization", "proxy_authorization", "api_key", "apikey", "x_api_key",
		"token", "access_token", "refresh_token", "id_token", "auth_token", "bearer_token",
		"authorization_token", "password", "passwd", "secret", "secret_key", "api_secret",
		"client_secret", "private_key", "secret_access_key", "access_key_id", "access_key",
		"cookie", "set_cookie", "session_cookie", "credential", "credentials":
		return true
	}
	for _, suffix := range []string{
		"_api_key", "_access_token", "_refresh_token", "_id_token", "_auth_token",
		"_bearer_token", "_password", "_client_secret", "_private_key", "_credential",
		"_secret_key", "_api_secret", "_secret_access_key", "_access_key_id", "_access_key",
		"_authorization_token",
	} {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return false
}

func isMediaPayloadKey(key, parentType string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	parentType = strings.ToLower(strings.TrimSpace(parentType))
	switch key {
	case "image_url", "input_image", "input_audio", "video_url", "input_video",
		"file_url", "document_url", "input_file", "file_data", "b64_json", "image_data":
		return true
	case "source", "data", "image", "audio":
		return isMediaType(parentType)
	default:
		return false
	}
}

func isMediaType(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.Contains(value, "image") || strings.Contains(value, "audio") ||
		strings.Contains(value, "video") || strings.Contains(value, "file") || strings.Contains(value, "document")
}

func summarizeMedia(value any, key, parentType string, policy capturePolicy) any {
	mediaType := inferMediaType(key, parentType)
	var payload string

	switch typed := value.(type) {
	case string:
		payload = typed
	case map[string]any:
		for _, field := range []string{"media_type", "mime_type", "format"} {
			if candidate, ok := typed[field].(string); ok && strings.TrimSpace(candidate) != "" {
				mediaType = normalizeMediaType(candidate, mediaType)
				break
			}
		}
		for _, field := range []string{"url", "data", "image_url", "video_url", "file_url", "document_url", "file_data", "b64_json"} {
			if candidate, ok := typed[field].(string); ok && strings.TrimSpace(candidate) != "" {
				payload = candidate
				break
			}
		}
	}

	if payload == "" {
		return map[string]any{"media_type": mediaType, "content": "[MEDIA OMITTED]"}
	}
	if parsedType, decoded, ok := decodeDataURL(payload); ok {
		return mediaDescriptor(parsedType, decoded, policy)
	}
	if parsed, err := url.Parse(payload); err == nil && parsed.IsAbs() && parsed.Host != "" {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.Fragment = ""
		normalized := parsed.String()
		digest := sha256.Sum256([]byte(normalized))
		return map[string]any{
			"media_type":  mediaType,
			"url":         normalized,
			"fingerprint": "sha256:" + hex.EncodeToString(digest[:]),
		}
	}
	decoded, err := decodeBase64(payload)
	if err != nil {
		digest := sha256.Sum256([]byte(payload))
		return map[string]any{
			"media_type":   mediaType,
			"approx_bytes": approximateBase64Bytes(payload),
			"fingerprint":  "sha256:" + hex.EncodeToString(digest[:]),
		}
	}
	return mediaDescriptor(mediaType, decoded, policy)
}

func mediaDescriptor(mediaType string, decoded []byte, policy capturePolicy) map[string]any {
	digest := sha256.Sum256(decoded)
	result := map[string]any{
		"media_type":   mediaType,
		"approx_bytes": len(decoded),
		"fingerprint":  "sha256:" + hex.EncodeToString(digest[:]),
	}
	if !policy.captureMediaContent {
		return result
	}
	limit := policy.mediaMaxBytes
	if limit < 0 {
		limit = 0
	}
	captured := decoded
	if len(captured) > limit {
		captured = captured[:limit]
		result["truncated"] = true
	}
	result["captured_bytes"] = len(captured)
	result["content_base64"] = base64.StdEncoding.EncodeToString(captured)
	return result
}

func decodeDataURL(value string) (string, []byte, bool) {
	trimmed := strings.TrimSpace(value)
	comma := strings.IndexByte(trimmed, ',')
	if comma <= len("data:") || !strings.HasPrefix(strings.ToLower(trimmed), "data:") {
		return "", nil, false
	}
	header := trimmed[len("data:"):comma]
	parts := strings.Split(header, ";")
	if len(parts) < 2 || !strings.EqualFold(parts[len(parts)-1], "base64") {
		return "", nil, false
	}
	decoded, err := decodeBase64(trimmed[comma+1:])
	if err != nil {
		return "", nil, false
	}
	return normalizeMediaType(parts[0], "application/octet-stream"), decoded, true
}

func decodeBase64(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if decoded, err := encoding.DecodeString(value); err == nil {
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("invalid base64 media payload")
}

func inferMediaType(key, parentType string) string {
	combined := strings.ToLower(key + " " + parentType)
	switch {
	case strings.Contains(combined, "image") || strings.Contains(combined, "b64_json"):
		return "image/*"
	case strings.Contains(combined, "audio"):
		return "audio/*"
	case strings.Contains(combined, "video"):
		return "video/*"
	default:
		return "application/octet-stream"
	}
}

func normalizeMediaType(value, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if strings.Contains(value, "/") {
		return value
	}
	switch value {
	case "png", "jpeg", "jpg", "gif", "webp":
		return "image/" + strings.ReplaceAll(value, "jpg", "jpeg")
	case "wav", "mpeg", "mp3", "ogg":
		return "audio/" + strings.ReplaceAll(value, "mp3", "mpeg")
	default:
		return fallback
	}
}

func approximateBase64Bytes(value string) int {
	value = strings.TrimRight(strings.TrimSpace(value), "=")
	return len(value) * 3 / 4
}

func sanitizeUnstructuredText(value string, policy capturePolicy) string {
	// Normalize JSON escape sequences (\u002f → /, \/ → /) so that URL and
	value = strings.ReplaceAll(value, `\u002f`, "/")
	value = strings.ReplaceAll(value, `\u002F`, "/")
	value = strings.ReplaceAll(value, `\/`, "/")
	value = strings.ReplaceAll(value, `\u003a`, ":")
	value = strings.ReplaceAll(value, `\u003A`, ":")
	value = strings.ReplaceAll(value, `\u003f`, "?")
	value = strings.ReplaceAll(value, `\u003F`, "?")
	value = strings.ReplaceAll(value, `\u0023`, "#")
	value = strings.ReplaceAll(value, `\u0040`, "@")
	lines := strings.SplitAfter(value, "\n")
	for i, line := range lines {
		// Scrub URLs, auth headers, and cookies on ALL lines including SSE
		// data: lines — credential material must never enter Trace.
		line = absoluteURLPattern.ReplaceAllStringFunc(line, sanitizeCapturedURL)
		line = authorizationLine.ReplaceAllStringFunc(line, redactLineValue)
		line = cookieLine.ReplaceAllStringFunc(line, redactLineValue)
		line = textSecretPattern.ReplaceAllStringFunc(line, func(match string) string {
			separator := strings.IndexByte(match, ':')
			if separator < 0 {
				return redactedValue
			}
			return match[:separator+1] + redactedValue
		})
		line = dataURLPattern.ReplaceAllStringFunc(line, func(match string) string {
			mediaType, decoded, ok := decodeDataURL(match)
			if !ok {
				return "[MEDIA OMITTED]"
			}
			descriptor := mediaDescriptor(mediaType, decoded, policy)
			encoded, _ := json.Marshal(descriptor)
			return string(encoded)
		})
		lines[i] = line
	}
	return strings.Join(lines, "")
}

func sanitizeCapturedURL(raw string) string {
	// The regex may include a leading boundary character (space, quote, etc.)
	// before a network-path reference. Separate it so url.Parse sees a clean
	// URL, then reattach it after sanitization.
	prefix := ""
	urlPart := raw
	for len(urlPart) > 0 && !isURLStart(urlPart[0]) {
		prefix += string(urlPart[0])
		urlPart = urlPart[1:]
	}
	if urlPart == "" {
		return raw
	}
	parsed, err := url.Parse(urlPart)
	if err != nil {
		return prefix + "[URL OMITTED]"
	}
	hasAuthMaterial := parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != ""
	// If the URL carries no userinfo, query, or fragment, there is nothing
	// to scrub — preserve it for observability (e.g. file:///path, which
	// legitimately has an empty host). Only strip auth material when it
	// exists.
	if !hasAuthMaterial {
		return raw
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	// Reconstruct the cleaned URL. For hostless URIs (file:///path) where
	// auth material existed (rare but possible: file://user@host/path),
	// rebuild from scheme. For network-path refs (//host) with no scheme,
	// url.String() produces //host/path correctly.
	cleaned := parsed.String()
	if strings.HasPrefix(cleaned, "://") {
		// Scheme was empty (network-path ref). Rebuild as //host/path.
		cleaned = "//" + parsed.Host + parsed.Path
	}
	return prefix + cleaned
}

func isURLStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '/'
}

// scrubURLsInString strips credentials, query parameters and fragments from
// every absolute URL embedded in a free-form string value. This closes the
// gap where a JSON field such as {"url":"https://user:pass@host?token=x"}
// bypasses sanitization because "url" is neither a secret key nor a media
// payload key.
func scrubURLsInString(value string) string {
	return absoluteURLPattern.ReplaceAllStringFunc(value, sanitizeCapturedURL)
}

// sanitizeTraceError scrubs an error message before it is written to an OTLP
// span status or recorded as an exception event. Transport errors may embed
// request URLs (including userinfo credentials), proxy-auth headers or
// upstream response bodies; the OpenSpec tracing requirement mandates that
// authentication material never enters any Trace field, including errors.
func sanitizeTraceError(msg string) string {
	if msg == "" {
		return ""
	}
	// Unescape JSON Unicode escapes for URL punctuation so the URL pattern
	// can match escaped URLs in error messages.
	msg = strings.ReplaceAll(msg, `\u002f`, "/")
	msg = strings.ReplaceAll(msg, `\u002F`, "/")
	msg = strings.ReplaceAll(msg, `\/`, "/")
	msg = strings.ReplaceAll(msg, `\u003a`, ":")
	msg = strings.ReplaceAll(msg, `\u003A`, ":")
	msg = strings.ReplaceAll(msg, `\u003f`, "?")
	msg = strings.ReplaceAll(msg, `\u003F`, "?")
	msg = strings.ReplaceAll(msg, `\u0023`, "#")
	msg = strings.ReplaceAll(msg, `\u0040`, "@")
	scrubbed := absoluteURLPattern.ReplaceAllStringFunc(msg, sanitizeCapturedURL)
	scrubbed = authorizationLine.ReplaceAllStringFunc(scrubbed, redactLineValue)
	scrubbed = cookieLine.ReplaceAllStringFunc(scrubbed, redactLineValue)
	scrubbed = textSecretPattern.ReplaceAllStringFunc(scrubbed, func(match string) string {
		separator := strings.IndexByte(match, ':')
		if separator < 0 {
			return redactedValue
		}
		return match[:separator+1] + redactedValue
	})
	const maxErrorBytes = 512
	return string(validUTF8Prefix([]byte(scrubbed), maxErrorBytes))
}

// sanitizeFormURLEncosed parses an application/x-www-form-urlencoded body,
// redacts values whose field name is a known secret key, and bounds the
// result through the common capture path. Without this, a form body such as
// "api_key=client-secret&model=gpt-4" bypasses redaction because it has no
// JSON quoting or colon-delimited headers that the regex sanitizers match.
func sanitizeFormURLEncoded(raw []byte, originalBytes, limit int, policy capturePolicy) string {
	values, err := url.ParseQuery(string(raw))
	if err != nil {
		// ParseQuery rejects some malformed forms (e.g. raw `;` separators).
		// Fall back to conservative redaction of form-style key=value pairs
		// so secret fields are never exported raw even on parse failure.
		return boundCapture([]byte(redactFormKV(string(raw))), originalBytes, limit)
	}
	for field := range values {
		if isSecretKey(field) {
			values[field] = []string{redactedValue}
		} else {
			// Non-secret field values may still contain URLs with
			// embedded credentials (e.g. url=https://user:pass@host).
			// ParseQuery already URL-decoded the values, so scrub them.
			scrubbed := make([]string, len(values[field]))
			for i, v := range values[field] {
				scrubbed[i] = scrubURLsInString(v)
			}
			values[field] = scrubbed
		}
	}
	encoded := values.Encode()
	return boundCapture([]byte(encoded), originalBytes, limit)
}

// redactFormKV conservatively redacts values of secret-named keys in a
// form-style "key=value&key2=value2" body when url.ParseQuery fails. It
// splits on & and = and replaces the value of any field whose name matches
// isSecretKey with [REDACTED]. This ensures malformed form bodies cannot
// leak credentials through the ParseQuery fallback path.
func redactFormKV(body string) string {
	// Split on both & and ; since ParseQuery treats both as separators and
	// the failure path may receive bodies using either delimiter.
	normalized := strings.ReplaceAll(body, ";", "&")
	pairs := strings.Split(normalized, "&")
	for i, pair := range pairs {
		eq := strings.IndexByte(pair, '=')
		if eq < 0 {
			continue
		}
		rawKey := strings.TrimSpace(pair[:eq])
		decodedKey, decErr := url.QueryUnescape(rawKey)
		if decErr != nil {
			decodedKey = rawKey
		}
		if isSecretKey(decodedKey) {
			pairs[i] = rawKey + "=" + redactedValue
		} else {
			// Non-secret values may contain URLs with credentials.
			// Unescape the value to match, then re-encode loosely.
			rawValue := pair[eq+1:]
			decodedValue, decVErr := url.QueryUnescape(rawValue)
			if decVErr != nil {
				decodedValue = rawValue
			}
			pairs[i] = rawKey + "=" + scrubURLsInString(decodedValue)
		}
	}
	return strings.Join(pairs, "&")
}

// boundCapture applies the common byte-limit and truncation-marker logic to
// already-sanitized content.
func boundCapture(source []byte, originalBytes, limit int) string {
	if originalBytes < len(source) {
		originalBytes = len(source)
	}
	if len(source) == 0 {
		if originalBytes > 0 {
			return truncationMarker(originalBytes, 0)
		}
		return ""
	}
	if len(source) > limit {
		source = validUTF8Prefix(source, limit)
		return string(source) + truncationMarker(originalBytes, len(source))
	}
	if originalBytes > len(source) {
		return string(source) + truncationMarker(originalBytes, len(source))
	}
	return string(source)
}

func redactLineValue(match string) string {
	separator := strings.IndexByte(match, ':')
	if separator < 0 {
		return redactedValue
	}
	return match[:separator+1] + redactedValue
}

func validUTF8Prefix(value []byte, limit int) []byte {
	if limit < 0 {
		limit = 0
	}
	if len(value) <= limit {
		return value
	}
	prefix := value[:limit]
	for len(prefix) > 0 && !utf8.Valid(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix
}

func truncationMarker(originalBytes, capturedBytes int) string {
	return fmt.Sprintf("[truncated:original_bytes=%d,captured_bytes=%d]", originalBytes, capturedBytes)
}
