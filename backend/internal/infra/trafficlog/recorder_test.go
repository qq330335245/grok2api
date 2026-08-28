package trafficlog

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartDisabledReturnsNil(t *testing.T) {
	t.Parallel()
	rec := New(Config{Enabled: false, Directory: t.TempDir()}, nil)
	if rec.Start(SessionMeta{RequestID: "abc"}) != nil {
		t.Fatal("disabled recorder must not open files")
	}
}

func TestSessionRedactsSecretsAndCapturesBody(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rec := New(Config{Enabled: true, Directory: dir, MaxBytes: 1 << 20}, nil)
	session := rec.Start(SessionMeta{RequestID: "req_1", Method: "POST", Path: "/v1/responses", Operation: "responses", Model: "grok-4.6", Streaming: true, ClientKeyName: "test-key"})
	if session == nil {
		t.Fatal("enabled recorder must start a session")
	}
	header := make(http.Header)
	header.Set("Authorization", "Bearer super-secret-token")
	header.Set("Content-Type", "application/json")
	session.WriteHeaders(header)
	session.WriteRequestBody([]byte(`{"model":"grok-4.6","sso":"jwt-secret-value","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`))
	session.BeginAttempt(48, "mortar", 12, 200)
	teed := session.Tee(io.NopCloser(strings.NewReader("data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"plan\"}\n\n")))
	got, err := io.ReadAll(teed)
	if err != nil {
		t.Fatal(err)
	}
	_ = teed.Close()
	session.WriteHold("withhold", false, 21, 0)
	session.Close()

	raw, err := os.ReadFile(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "Bearer s…oken") && !strings.Contains(text, "…") {
		t.Fatalf("authorization must be redacted: %s", text)
	}
	if strings.Contains(text, "super-secret-token") || strings.Contains(text, "jwt-secret-value") {
		t.Fatalf("secrets leaked: %s", text)
	}
	if !strings.Contains(text, "reasoning_text.delta") || !strings.Contains(string(got), "plan") {
		t.Fatalf("upstream body missing: %s", text)
	}
		if !strings.Contains(text, "Verdict: withhold") {
		t.Fatalf("hold section missing: %s", text)
	}
	if !strings.Contains(text, `"max_tokens":4096`) {
		t.Fatalf("max_tokens must be preserved: %s", text)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "responses-*-req_1.log"))
	if len(matches) != 1 {
		t.Fatalf("files=%v", matches)
	}
}

func TestLargeRequestBodyStillCapturesUpstream(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rec := New(Config{Enabled: true, Directory: dir, MaxBytes: 8 << 20}, nil)
	session := rec.Start(SessionMeta{RequestID: "req_big", Operation: "responses"})
	if session == nil {
		t.Fatal("start")
	}
	body := bytes.Repeat([]byte("a"), int(maxRequestBody)+2048)
	session.WriteRequestBody(body)
	session.BeginAttempt(1, "acct", 2, 200)
	upstream := "data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"plan\"}\n\n"
	teed := session.Tee(io.NopCloser(strings.NewReader(upstream)))
	if _, err := io.ReadAll(teed); err != nil {
		t.Fatal(err)
	}
	_ = teed.Close()
	session.WriteHold("deliver", true, 12, 8)
	more := session.Tee(io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")))
	if _, err := io.ReadAll(more); err != nil {
		t.Fatal(err)
	}
	_ = more.Close()
	session.Close()

	raw, err := os.ReadFile(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "=== REQUEST BODY TRUNCATED") {
		t.Fatalf("request truncation marker missing: %s", text[len(text)-200:])
	}
	if !strings.Contains(text, "=== UPSTREAM ATTEMPT 1 ===") || !strings.Contains(text, "reasoning_text.delta") {
		t.Fatalf("upstream body dropped after large request: %s", text[len(text)-400:])
	}
	if !strings.Contains(text, "Verdict: deliver") {
		t.Fatalf("hold dropped after large request: %s", text[len(text)-400:])
	}
	if !strings.Contains(text, "=== UPSTREAM BODY CONTINUED ===") || !strings.Contains(text, "output_text.delta") {
		t.Fatalf("post-hold stream missing: %s", text[len(text)-400:])
	}
}

func TestSessionWritesTruncatedMarker(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rec := New(Config{Enabled: true, Directory: dir, MaxBytes: 256}, nil)
	session := rec.Start(SessionMeta{RequestID: "req_cap", Operation: "responses"})
	if session == nil {
		t.Fatal("start")
	}
	session.WriteRequestBody([]byte(`{"hello":"world"}`))
	session.BeginAttempt(1, "acct", 2, 200)
	teed := session.Tee(io.NopCloser(strings.NewReader(strings.Repeat("data: x\n", 64))))
	_, _ = io.ReadAll(teed)
	_ = teed.Close()
	session.Close()

	raw, err := os.ReadFile(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "=== TRUNCATED ===") {
		t.Fatalf("file-cap marker missing: %s", raw)
	}
}

func TestStartPrunesOldestFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rec := New(Config{Enabled: true, Directory: dir, MaxFiles: 2}, nil)
	for i := 1; i <= 4; i++ {
		session := rec.Start(SessionMeta{RequestID: fmt.Sprintf("req_%d", i), Operation: "responses"})
		if session == nil {
			t.Fatalf("start %d", i)
		}
		session.Close()
		time.Sleep(5 * time.Millisecond)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("kept %d files, want 2: %v", len(matches), matches)
	}
}
