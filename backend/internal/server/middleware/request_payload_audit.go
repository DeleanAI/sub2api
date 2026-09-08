package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// RequestPayloadAudit captures only authenticated gateway mutations. The
// content is bounded and redacted before it leaves process memory.
func RequestPayloadAudit(audit *service.RequestPayloadAuditService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if audit == nil || !audit.Enabled() || !shouldCapturePayload(c.Request.Method) {
			c.Next()
			return
		}

		apiKey, ok := GetAPIKeyFromContext(c)
		if !ok || apiKey == nil || apiKey.ID <= 0 || c.Request == nil {
			c.Next()
			return
		}
		clientRequestID, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string)
		if strings.TrimSpace(clientRequestID) == "" {
			c.Next()
			return
		}

		requestLimit, responseLimit := auditCaptureLimits(audit)
		requestCapture := newPayloadCapture(requestLimit)
		responseCapture := newPayloadCapture(responseLimit)
		c.Request.Body = io.NopCloser(io.TeeReader(c.Request.Body, requestCapture))
		c.Writer = &payloadAuditResponseWriter{ResponseWriter: c.Writer, capture: responseCapture}
		c.Request = c.Request.WithContext(service.ContextWithRequestPayloadAudit(c.Request.Context(), audit))

		c.Next()

		requestBody, requestOmitted := capturedText(c.GetHeader("Content-Type"), requestCapture)
		responseBody, responseOmitted := capturedText(c.Writer.Header().Get("Content-Type"), responseCapture)
		metadata := map[string]any{
			"method":                   c.Request.Method,
			"path":                     c.Request.URL.Path,
			"query":                    sanitizeQuery(c.Request.URL.Query()),
			"request_content_type":     c.GetHeader("Content-Type"),
			"response_content_type":    c.Writer.Header().Get("Content-Type"),
			"status_code":              c.Writer.Status(),
			"request_bytes":            requestCapture.totalBytes(),
			"response_bytes":           responseCapture.totalBytes(),
			"request_truncated":        requestCapture.wasTruncated(),
			"response_truncated":       responseCapture.wasTruncated(),
			"request_content_omitted":  requestOmitted,
			"response_content_omitted": responseOmitted,
			"captured_at":              time.Now().UTC().Format(time.RFC3339Nano),
		}
		capture := service.RequestPayloadAuditCapture{
			ClientRequestID: clientRequestID,
			APIKeyID:        apiKey.ID,
			RequestBody:     redactPayload(requestBody),
			ResponseBody:    redactPayload(responseBody),
			Metadata:        metadata,
		}

		_ = audit.EnqueueCapture(capture)
	}
}

func shouldCapturePayload(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// Request content receives most of max_entry_mb. Keep the middleware
// independent from config by deriving the limits through the service.
func auditCaptureLimits(audit *service.RequestPayloadAuditService) (int64, int64) {
	return audit.CaptureLimits()
}

type payloadAuditResponseWriter struct {
	gin.ResponseWriter
	capture *payloadCapture
}

func (w *payloadAuditResponseWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if n > 0 {
		_, _ = w.capture.Write(data[:n])
	}
	return n, err
}

func (w *payloadAuditResponseWriter) WriteString(data string) (int, error) {
	n, err := w.ResponseWriter.WriteString(data)
	if n > 0 {
		_, _ = w.capture.Write([]byte(data[:n]))
	}
	return n, err
}

type payloadCapture struct {
	mu        sync.Mutex
	maxBytes  int64
	data      bytes.Buffer
	total     int64
	truncated bool
}

func newPayloadCapture(maxBytes int64) *payloadCapture {
	return &payloadCapture{maxBytes: maxBytes}
}

func (c *payloadCapture) Write(data []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total += int64(len(data))
	remaining := c.maxBytes - int64(c.data.Len())
	if remaining <= 0 {
		if len(data) > 0 {
			c.truncated = true
		}
		return len(data), nil
	}
	if int64(len(data)) > remaining {
		_, _ = c.data.Write(data[:remaining])
		c.truncated = true
		return len(data), nil
	}
	_, _ = c.data.Write(data)
	return len(data), nil
}

func (c *payloadCapture) snapshot() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.data.Bytes()...)
}

func (c *payloadCapture) totalBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

func (c *payloadCapture) wasTruncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

func capturedText(contentType string, capture *payloadCapture) (string, bool) {
	if capture == nil {
		return "", true
	}
	data := capture.snapshot()
	if len(data) == 0 {
		return "", false
	}
	if !isTextContent(contentType, data) {
		return "", true
	}
	return string(data), false
}

func isTextContent(contentType string, sample []byte) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err == nil {
		mediaType = strings.ToLower(mediaType)
		if strings.HasPrefix(mediaType, "text/") || mediaType == "application/json" ||
			strings.HasSuffix(mediaType, "+json") || mediaType == "application/x-ndjson" ||
			mediaType == "application/xml" || strings.HasSuffix(mediaType, "+xml") ||
			mediaType == "application/graphql-response+json" {
			return true
		}
		if mediaType != "" {
			return false
		}
	}
	return bytes.IndexByte(sample, 0) == -1 && len(strings.TrimSpace(string(sample))) > 0
}

func sanitizeQuery(values url.Values) map[string][]string {
	if len(values) == 0 {
		return map[string][]string{}
	}
	out := make(map[string][]string, len(values))
	for key, values := range values {
		if isSensitiveField(key) {
			out[key] = []string{"[REDACTED]"}
			continue
		}
		out[key] = append([]string(nil), values...)
	}
	return out
}

func redactPayload(value string) string {
	if strings.TrimSpace(value) == "" {
		return value
	}
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()
	var parsed any
	if err := decoder.Decode(&parsed); err == nil {
		var extra any
		if err := decoder.Decode(&extra); err == io.EOF {
			redactJSONValue(parsed)
			if redacted, err := json.Marshal(parsed); err == nil {
				return string(redacted)
			}
		}
	}
	return redactTextFields(value)
}

func redactJSONValue(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if isSensitiveField(key) {
				typed[key] = "[REDACTED]"
				continue
			}
			redactJSONValue(child)
		}
	case []any:
		for _, child := range typed {
			redactJSONValue(child)
		}
	}
}

// credentialWords 是"这个词出现在字段名里就说明它承载凭据"的唯一声明。
var credentialWords = map[string]bool{
	"authorization": true, "auth": true, "credential": true, "credentials": true,
	"key": true, "apikey": true, "secret": true, "token": true,
	"password": true, "passwd": true, "pwd": true,
	"cookie": true, "signature": true, "sign": true, "session": true,
}

// isSensitiveField 判断字段/查询参数名是否承载凭据。
//
// 按"词"判断而不是按后缀：原来的 strings.HasSuffix(normalized, "key") 会把 monkey、
// keyboard 这类正常字段一起打码，而审计记录被过度打码就失去了排障价值。反过来，
// 原来的名单里没有裸 "key"，于是 Gemini 的 ?key=AIza… 明文进了 metadata.query，
// 并由用户端和管理端两个接口原样返回。
//
// 分词同时处理下划线/中划线/点分隔与 camelCase：api_key、access-key、apiKey、
// sessionKey 都会被切出 "key" 这个词，而 monkey 只有 "monkey" 一个词。
func isSensitiveField(key string) bool {
	for _, word := range splitFieldNameWords(key) {
		if credentialWords[word] {
			return true
		}
	}
	return false
}

// splitFieldNameWords 把字段名切成小写词：按 _ - . 空格分隔，并在 camelCase 边界断开。
func splitFieldNameWords(key string) []string {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	var words []string
	var current []rune
	flush := func() {
		if len(current) > 0 {
			words = append(words, strings.ToLower(string(current)))
			current = current[:0]
		}
	}
	runes := []rune(key)
	for i, r := range runes {
		switch {
		case r == '_' || r == '-' || r == '.' || r == ' ':
			flush()
		case unicode.IsUpper(r) && i > 0 && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])):
			// camelCase 边界：apiKey -> api | Key
			flush()
			current = append(current, r)
		default:
			current = append(current, r)
		}
	}
	flush()
	return words
}

var sensitiveTextFieldPattern = regexp.MustCompile(`(?i)(["']?(?:authorization|api[_-]?key|access[_-]?token|refresh[_-]?token|id[_-]?token|token|secret|password|cookie|credential)["']?\s*[:=]\s*)("(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^,\s}\]]+)`)

func redactTextFields(value string) string {
	return sensitiveTextFieldPattern.ReplaceAllString(value, "${1}[REDACTED]")
}
