package trafficlog

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	session.WriteRequestBody([]byte(`{"model":"grok-4.6","sso":"jwt-secret-value","messages":[{"role":"user","content":"hi"}]}`))
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
	matches, _ := filepath.Glob(filepath.Join(dir, "responses-*-req_1.log"))
	if len(matches) != 1 {
		t.Fatalf("files=%v", matches)
	}
}
