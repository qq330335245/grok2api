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
	defaultMaxFiles        = 50
	maxRequestBody   int64 = 4 << 20
)

// Config controls on-disk request/response dumps for Hold and degrade debugging.
type Config struct {
	Enabled   bool
	Directory string
	MaxBytes  int64
	MaxFiles  int
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
	if cfg.MaxFiles <= 0 {
		cfg.MaxFiles = defaultMaxFiles
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
	session.writeStringLocked(fmt.Sprintf("=== REQUEST INFO ===\nTimestamp: %s\nRequest-ID: %s\nMethod: %s\nPath: %s\nOperation: %s\nModel: %s\nStreaming: %v\nClient-Key: %s\n\n",
		time.Now().Format(time.RFC3339Nano), meta.RequestID, meta.Method, meta.Path, meta.Operation, meta.Model, meta.Streaming, meta.ClientKeyName))
	r.prune(cfg.Directory, cfg.MaxFiles)
	return session
}

func (r *Recorder) prune(dir string, maxFiles int) {
	if r == nil || maxFiles <= 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type item struct {
		path string
		mod  time.Time
	}
	logs := make([]item, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".log") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		logs = append(logs, item{path: filepath.Join(dir, entry.Name()), mod: info.ModTime()})
	}
	if len(logs) <= maxFiles {
		return
	}
	sort.Slice(logs, func(i, j int) bool { return logs[i].mod.After(logs[j].mod) })
	for _, old := range logs[maxFiles:] {
		if removeErr := os.Remove(old.path); removeErr != nil && r.logger != nil {
			r.logger.Warn("traffic_log_prune_failed", "path", old.path, "error", removeErr)
		}
	}
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

// Session is one request dump. Public methods are safe for concurrent Tee reads.
type Session struct {
	mu        sync.Mutex
	file      *os.File
	buf       *bufio.Writer
	max       int64
	n         int64
	truncated bool
	afterHold bool
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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeStringLocked("=== HEADERS ===\n")
	for _, key := range sortedHeaderKeys(header) {
		for _, value := range header.Values(key) {
			s.writeStringLocked(key + ": " + redactHeaderValue(key, value) + "\n")
		}
	}
	s.writeStringLocked("\n")
}

func (s *Session) WriteRequestBody(body []byte) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	redacted := redactJSON(body)
	original := len(redacted)
	s.writeStringLocked(fmt.Sprintf("=== REQUEST BODY (%d bytes) ===\n", original))
	limit := s.remainingLocked()
	if limit > maxRequestBody {
		limit = maxRequestBody
	}
	bodyTruncated := int64(original) > limit
	if bodyTruncated {
		redacted = redacted[:int(limit)]
	}
	s.writeRawLocked(redacted)
	if bodyTruncated {
		s.writeStringLocked(fmt.Sprintf("\n=== REQUEST BODY TRUNCATED (dumped %d of %d bytes) ===\n\n", len(redacted), original))
		return
	}
	if !bytes.HasSuffix(bytes.TrimSpace(body), []byte("\n")) {
		s.writeStringLocked("\n")
	}
	s.writeStringLocked("\n")
}

func (s *Session) BeginAttempt(accountID uint64, accountName string, nodeID uint64, status int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempt++
	s.afterHold = false
	s.writeStringLocked(fmt.Sprintf("=== UPSTREAM ATTEMPT %d ===\nTimestamp: %s\nAccount-ID: %d\nAccount: %s\nEgress-Node-ID: %d\nHTTP-Status: %d\n\n=== UPSTREAM BODY ===\n",
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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeStringLocked(fmt.Sprintf("\n=== HOLD ===\nTimestamp: %s\nVerdict: %s\nStreamed-Thinking: %v\nOutput-Tokens: %d\nReasoning-Tokens: %d\n\n",
		time.Now().Format(time.RFC3339Nano), verdict, streamedThinking, outputTokens, reasoningTokens))
	s.afterHold = true
}

func (s *Session) WriteNote(format string, args ...any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeStringLocked("=== NOTE ===\n" + fmt.Sprintf(format, args...) + "\n\n")
}

func (s *Session) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.truncated {
		_, _ = s.buf.Write([]byte("\n=== TRUNCATED ===\n"))
	}
	_ = s.buf.Flush()
	_ = s.file.Close()
	if s.log != nil {
		s.log.Info("traffic_log_written", "path", s.path, "bytes", s.n, "truncated", s.truncated)
	}
}

func (s *Session) writeStringLocked(value string) {
	s.writeRawLocked([]byte(value))
}

func (s *Session) remainingLocked() int64 {
	left := s.max - s.n
	if left < 0 {
		return 0
	}
	return left
}

func (s *Session) writeRawLocked(data []byte) {
	if s == nil || s.closed || len(data) == 0 || s.truncated {
		return
	}
	limit := s.remainingLocked()
	if limit <= 0 {
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

func (s *Session) writeUpstream(data []byte) {
	if s == nil || len(data) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.afterHold {
		s.afterHold = false
		s.writeStringLocked("=== UPSTREAM BODY CONTINUED ===\n")
	}
	s.writeRawLocked(data)
}

type teeBody struct {
	src  io.ReadCloser
	sess *Session
}

func (t *teeBody) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 {
		t.sess.writeUpstream(p[:n])
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
	switch k {
	case "sso", "password", "secret", "authorization", "cookie", "set-cookie",
		"api_key", "apikey", "x-api-key", "access_token", "refresh_token", "id_token",
		"client_secret", "private_key":
		return true
	default:
		if k == "max_tokens" || k == "tokens" || strings.HasSuffix(k, "_tokens") {
			return false
		}
		return strings.Contains(k, "password") || strings.Contains(k, "secret") ||
			strings.Contains(k, "authorization") || strings.Contains(k, "cookie") ||
			strings.Contains(k, "api_key") || strings.Contains(k, "apikey") ||
			(strings.Contains(k, "token") && !strings.Contains(k, "tokenizer"))
	}
}
