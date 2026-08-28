package trafficlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	defaultDirectory       = "./data/traffic-logs"
	defaultMaxBytes  int64 = 8 << 20
	maxRequestBody   int64 = 512 << 10
)

// Config controls on-disk request/response dumps for Hold and degrade debugging.
type Config struct {
	Enabled   bool
	Directory string
	MaxBytes  int64
}

// Recorder is a process-wide, hot-updatable dump switch.
type Recorder struct {
	mu     sync.RWMutex
	cfg    Config
	logger *slog.Logger
}

func New(cfg Config, logger *slog.Logger) *Recorder {
	if logger == nil {
		logger = slog.Default()
	}
	return &Recorder{cfg: normalize(cfg), logger: logger}
}

func normalize(cfg Config) Config {
	if strings.TrimSpace(cfg.Directory) == "" {
		cfg.Directory = defaultDirectory
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultMaxBytes
	}
	return cfg
}

func (r *Recorder) Update(cfg Config) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.cfg = normalize(cfg)
	r.mu.Unlock()
}

func (r *Recorder) snapshot() Config {
	if r == nil {
		return Config{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg
}

// Start opens a dump file when enabled. The caller must Close the session.
func (r *Recorder) Start(meta SessionMeta) *Session {
	cfg := r.snapshot()
	if r == nil || !cfg.Enabled {
		return nil
	}
	if err := os.MkdirAll(cfg.Directory, 0o700); err != nil {
		r.logger.Warn("traffic_log_mkdir_failed", "dir", cfg.Directory, "error", err)
		return nil
	}
	name := filename(meta)
	path := filepath.Join(cfg.Directory, name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		r.logger.Warn("traffic_log_open_failed", "path", path, "error", err)
		return nil
	}
	session := &Session{
		file: file,
		buf:  bufio.NewWriterSize(file, 64<<10),
		max:  cfg.MaxBytes,
		path: path,
		log:  r.logger,
	}
	session.writeString(fmt.Sprintf("=== REQUEST INFO ===\nTimestamp: %s\nRequest-ID: %s\nMethod: %s\nPath: %s\nOperation: %s\nModel: %s\nStreaming: %v\nClient-Key: %s\n\n",
		time.Now().Format(time.RFC3339Nano), meta.RequestID, meta.Method, meta.Path, meta.Operation, meta.Model, meta.Streaming, meta.ClientKeyName))
	return session
}

type SessionMeta struct {
	RequestID     string
	Method        string
	Path          string
	Operation     string
	Model         string
	Streaming     bool
	ClientKeyName string
}

// Session is one request dump. Not safe for concurrent writers.
type Session struct {
	file      *os.File
	buf       *bufio.Writer
	max       int64
	n         int64
	truncated bool
	path      string
	log       *slog.Logger
	attempt   int
	closed    bool
}

func (s *Session) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

func (s *Session) WriteHeaders(header http.Header) {
	if s == nil {
		return
	}
	s.writeString("=== HEADERS ===\n")
	for _, key := range sortedHeaderKeys(header) {
		for _, value := range header.Values(key) {
			s.writeString(key + ": " + redactHeaderValue(key, value) + "\n")
		}
	}
	s.writeString("\n")
}

func (s *Session) WriteRequestBody(body []byte) {
	if s == nil {
		return
	}
	s.writeString("=== REQUEST BODY ===\n")
	s.writeLimited(redactJSON(body), maxRequestBody)
	if !bytes.HasSuffix(bytes.TrimSpace(body), []byte("\n")) {
		s.writeString("\n")
	}
	s.writeString("\n")
}

func (s *Session) BeginAttempt(accountID uint64, accountName string, nodeID uint64, status int) {
	if s == nil {
		return
	}
	s.attempt++
	s.writeString(fmt.Sprintf("=== UPSTREAM ATTEMPT %d ===\nTimestamp: %s\nAccount-ID: %d\nAccount: %s\nEgress-Node-ID: %d\nHTTP-Status: %d\n\n=== UPSTREAM BODY ===\n",
		s.attempt, time.Now().Format(time.RFC3339Nano), accountID, accountName, nodeID, status))
}

func (s *Session) Tee(body io.ReadCloser) io.ReadCloser {
	if s == nil || body == nil {
		return body
	}
	return &teeBody{src: body, sess: s}
}

func (s *Session) WriteHold(verdict string, streamedThinking bool, outputTokens, reasoningTokens int64) {
	if s == nil {
		return
	}
	s.writeString(fmt.Sprintf("\n=== HOLD ===\nTimestamp: %s\nVerdict: %s\nStreamed-Thinking: %v\nOutput-Tokens: %d\nReasoning-Tokens: %d\n\n",
		time.Now().Format(time.RFC3339Nano), verdict, streamedThinking, outputTokens, reasoningTokens))
}

func (s *Session) WriteNote(format string, args ...any) {
	if s == nil {
		return
	}
	s.writeString("=== NOTE ===\n" + fmt.Sprintf(format, args...) + "\n\n")
}

func (s *Session) Close() {
	if s == nil || s.closed {
		return
	}
	s.closed = true
	if s.truncated {
		s.writeString("\n=== TRUNCATED ===\n")
	}
	_ = s.buf.Flush()
	_ = s.file.Close()
	if s.log != nil {
		s.log.Info("traffic_log_written", "path", s.path, "bytes", s.n, "truncated", s.truncated)
	}
}

func (s *Session) writeString(value string) {
	s.writeLimited([]byte(value), s.remaining())
}

func (s *Session) remaining() int64 {
	left := s.max - s.n
	if left < 0 {
		return 0
	}
	return left
}

func (s *Session) writeLimited(data []byte, limit int64) {
	if s == nil || s.closed || len(data) == 0 || s.truncated {
		return
	}
	if limit <= 0 || s.n >= s.max {
		s.truncated = true
		return
	}
	if int64(len(data)) > limit {
		data = data[:limit]
		s.truncated = true
	}
	n, err := s.buf.Write(data)
	s.n += int64(n)
	if err != nil {
		s.truncated = true
	}
}

type teeBody struct {
	src  io.ReadCloser
	sess *Session
}

func (t *teeBody) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 {
		t.sess.writeLimited(p[:n], t.sess.remaining())
	}
	return n, err
}

func (t *teeBody) Close() error {
	return t.src.Close()
}

func filename(meta SessionMeta) string {
	op := sanitizeFilePart(meta.Operation)
	if op == "" {
		op = "request"
	}
	id := sanitizeFilePart(meta.RequestID)
	if id == "" {
		id = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%s-%s-%s.log", op, time.Now().Format("20060102T150405"), id)
}

func sanitizeFilePart(value string) string {
	var b strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func sortedHeaderKeys(header http.Header) []string {
	keys := make([]string, 0, len(header))
	for key := range header {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return strings.ToLower(keys[i]) < strings.ToLower(keys[j]) })
	return keys
}

func redactHeaderValue(key, value string) string {
	switch strings.ToLower(key) {
	case "authorization", "cookie", "set-cookie", "x-api-key", "x-auth-token", "proxy-authorization":
		return maskSecret(value)
	default:
		return value
	}
}

func maskSecret(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= 8 {
		return "…"
	}
	return value[:3] + "…" + value[len(value)-4:]
}

func redactJSON(body []byte) []byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return body
	}
	var payload any
	if json.Unmarshal(trimmed, &payload) != nil {
		return body
	}
	redactValue(payload)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}

func redactValue(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if sensitiveKey(key) {
				if text, ok := child.(string); ok {
					typed[key] = maskSecret(text)
				} else {
					typed[key] = "…"
				}
				continue
			}
			redactValue(child)
		}
	case []any:
		for _, child := range typed {
			redactValue(child)
		}
	}
}

func sensitiveKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	switch {
	case strings.Contains(k, "token"), strings.Contains(k, "secret"), strings.Contains(k, "password"),
		strings.Contains(k, "authorization"), strings.Contains(k, "cookie"), k == "sso", strings.Contains(k, "sso_"),
		strings.Contains(k, "api_key"), strings.Contains(k, "apikey"):
		return true
	default:
		return false
	}
}
