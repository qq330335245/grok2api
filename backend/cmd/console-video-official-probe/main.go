// console-video-official-probe runs offline Console-upstream video probes using
// the same SSO+DPoP request stack as grok2api, but with official-docs JSON shapes
// (including multi reference_images / grok-imagine-video-1.5) that the gateway
// currently rejects locally.
//
// This binary must NOT be deployed into the production grok2api container.
// It only needs read-only access to exported SSO tokens + local materials.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

type accountFile struct {
	Accounts []exportedAccount `json:"accounts"`
}

type exportedAccount struct {
	ID       uint64 `json:"id"`
	Name     string `json:"name"`
	SSOToken string `json:"sso_token"`
}

type testCase struct {
	Name    string
	Body    []byte
	Timeout time.Duration
}

func main() {
	accountsPath := flag.String("accounts", "accounts.json", "exported console SSO accounts JSON")
	materials := flag.String("materials", "materials", "materials root with case1/case2/case3")
	baseURL := flag.String("base-url", "https://console.x.ai", "Console base URL")
	duration := flag.Int("duration", 6, "video duration seconds")
	ratio := flag.String("aspect-ratio", "9:16", "aspect_ratio")
	resolution := flag.String("resolution", "720p", "resolution")
	only := flag.String("only", "", "run only this test name substring")
	pollEvery := flag.Duration("poll-every", 3*time.Second, "poll interval")
	timeout := flag.Duration("timeout", 12*time.Minute, "per-test timeout")
	flag.Parse()

	raw, err := os.ReadFile(*accountsPath)
	must(err)
	var file accountFile
	must(json.Unmarshal(raw, &file))
	if len(file.Accounts) == 0 {
		fail("no accounts in %s", *accountsPath)
	}

	// Ephemeral cipher only for this probe process (SSO tokens in accounts.json are plaintext).
	// Adapter still Encrypt/Decrypts credential fields; production grok2api cipher/DB are not used at runtime.
	key := base64.StdEncoding.EncodeToString(bytes32())
	cipher, err := security.NewCipher(key)
	must(err)
	proxyURL := strings.TrimSpace(envOr("PROBE_PROXY_URL", "http://127.0.0.1:7890"))
	encProxy, err := cipher.Encrypt(proxyURL)
	must(err)
	repo := &fixedProxyEgressRepo{node: egressdomain.Node{
		ID: 1, Name: "probe-mihomo", Scope: egressdomain.ScopeConsole, Enabled: true, Health: 1,
		EncryptedProxyURL: encProxy, UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	}}
	manager := egress.NewManager(repo, cipher)
	adapter := console.NewAdapter(console.Config{BaseURL: *baseURL, TimeoutSeconds: 90}, manager, cipher, nil)
	fmt.Printf("probe_proxy=%s base_url=%s (production grok2api untouched)\n", proxyURL, *baseURL)

	case1Prompt := readPrompt(filepath.Join(*materials, "case1", "prompt.txt"))
	case2Prompt := readPrompt(filepath.Join(*materials, "case2", "prompt.txt"))
	case3Prompt := readPrompt(filepath.Join(*materials, "case3", "prompt.txt"))
	refs := mustDataURLs(
		filepath.Join(*materials, "case1", "ref1.png"),
		filepath.Join(*materials, "case1", "ref2.png"),
		filepath.Join(*materials, "case1", "ref3.png"),
		filepath.Join(*materials, "case1", "ref4.png"),
	)
	firstFrame := mustDataURL(firstExisting(
		filepath.Join(*materials, "case3", "03ab6a6f-ad44-4541-bf0b-46bbfa8d2edf.jpg"),
		filepath.Join(*materials, "case3"),
	))

	tests := []testCase{}
	add := func(name, model, prompt, image string, reference []string) {
		body, err := console.MarshalOfficialVideoBody(model, prompt, *ratio, *resolution, *duration, image, reference)
		must(err)
		tests = append(tests, testCase{Name: name, Body: body, Timeout: *timeout})
	}

	// 1) multi reference_images — official R2V shape (grok2api Console currently blocks this locally)
	add("R2V-multi-ref/grok-imagine-video", "grok-imagine-video", case1Prompt, "", refs)
	add("R2V-multi-ref/grok-imagine-video-1.5", "grok-imagine-video-1.5", case1Prompt, "", refs)
	// 2) text-to-video 1.5
	add("T2V/grok-imagine-video-1.5", "grok-imagine-video-1.5", case2Prompt, "", nil)
	// 3) first-frame I2V 1.5
	add("I2V-first-frame/grok-imagine-video-1.5", "grok-imagine-video-1.5", case3Prompt, firstFrame, nil)

	accountIdx := 0
	nextCred := func() account.Credential {
		if accountIdx >= len(file.Accounts) {
			fail("ran out of accounts (%d) before finishing tests", len(file.Accounts))
		}
		item := file.Accounts[accountIdx]
		accountIdx++
		enc, err := cipher.Encrypt(strings.TrimSpace(item.SSOToken))
		must(err)
		fmt.Printf("\n== using account #%d id=%d name=%s ==\n", accountIdx, item.ID, item.Name)
		return account.Credential{
			ID: item.ID, Name: item.Name, Provider: account.ProviderConsole,
			AuthType: account.AuthTypeSSO, EncryptedAccessToken: enc, Enabled: true,
		}
	}

	ctx := context.Background()
	results := make([]map[string]any, 0, len(tests))
	for _, tc := range tests {
		if *only != "" && !strings.Contains(tc.Name, *only) {
			continue
		}
		cred := nextCred()
		fmt.Printf("\n-------- TEST %s --------\n", tc.Name)
		fmt.Printf("request_body_bytes=%d\n", len(tc.Body))
		// print body without huge data urls
		fmt.Printf("request_shape=%s\n", summarizeBody(tc.Body))

		start := time.Now()
		reqID, createRaw, err := adapter.OfficialVideoCreate(ctx, cred, tc.Body)
		entry := map[string]any{
			"test": tc.Name, "account_id": cred.ID, "account_name": cred.Name,
			"create_ms": time.Since(start).Milliseconds(),
		}
		if err != nil {
			entry["phase"] = "create"
			entry["ok"] = false
			entry["error"] = err.Error()
			entry["create_raw"] = trim(string(createRaw), 500)
			fmt.Printf("CREATE FAIL: %v\nraw: %s\n", err, trim(string(createRaw), 500))
			results = append(results, entry)
			continue
		}
		entry["request_id"] = reqID
		fmt.Printf("CREATE OK request_id=%s raw=%s\n", reqID, trim(string(createRaw), 300))

		waitCtx, cancel := context.WithTimeout(ctx, tc.Timeout)
		pollStart := time.Now()
		// manual poll loop to honor poll-every flag
		var finalRaw []byte
		var videoURL string
		var pollErr error
		ticker := time.NewTicker(*pollEvery)
		deadline := time.Now().Add(tc.Timeout)
	loop:
		for {
			finalRaw, done, url, err := adapter.OfficialVideoPoll(waitCtx, cred, reqID)
			if err != nil {
				pollErr = err
				break
			}
			if done {
				videoURL = url
				break
			}
			fmt.Printf("  polling... %s\n", trim(string(finalRaw), 160))
			if time.Now().After(deadline) {
				pollErr = fmt.Errorf("timeout")
				break
			}
			select {
			case <-waitCtx.Done():
				pollErr = waitCtx.Err()
				break loop
			case <-ticker.C:
			}
		}
		ticker.Stop()
		cancel()
		entry["poll_ms"] = time.Since(pollStart).Milliseconds()
		entry["total_ms"] = time.Since(start).Milliseconds()
		if pollErr != nil {
			entry["phase"] = "poll"
			entry["ok"] = false
			entry["error"] = pollErr.Error()
			entry["poll_raw"] = trim(string(finalRaw), 500)
			fmt.Printf("POLL FAIL: %v\nraw: %s\n", pollErr, trim(string(finalRaw), 500))
		} else {
			entry["ok"] = true
			entry["video_url_host"] = hostOf(videoURL)
			entry["poll_raw"] = trim(string(finalRaw), 300)
			fmt.Printf("DONE video_host=%s\n", hostOf(videoURL))
		}
		results = append(results, entry)
	}

	out, _ := json.MarshalIndent(results, "", "  ")
	fmt.Printf("\n===== SUMMARY JSON =====\n%s\n", out)
	_ = os.WriteFile("probe-results.json", out, 0o600)
	fmt.Println("wrote probe-results.json")
}

func summarizeBody(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return trim(string(body), 200)
	}
	if img, ok := m["image"].(map[string]any); ok {
		if u, _ := img["url"].(string); strings.HasPrefix(u, "data:") {
			img["url"] = fmt.Sprintf("dataURL(len=%d)", len(u))
		}
	}
	if refs, ok := m["reference_images"].([]any); ok {
		for i, ref := range refs {
			rm, _ := ref.(map[string]any)
			if rm == nil {
				continue
			}
			if u, _ := rm["url"].(string); strings.HasPrefix(u, "data:") {
				rm["url"] = fmt.Sprintf("dataURL(len=%d)", len(u))
				refs[i] = rm
			}
		}
		m["reference_images"] = refs
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func readPrompt(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		fail("read prompt %s: %v", path, err)
	}
	return strings.TrimSpace(string(b))
}

func mustDataURLs(paths ...string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, mustDataURL(p))
	}
	return out
}

func mustDataURL(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		// if directory, pick first image
		entries, dirErr := os.ReadDir(path)
		if dirErr != nil {
			fail("read %s: %v", path, err)
		}
		for _, e := range entries {
			name := strings.ToLower(e.Name())
			if strings.HasSuffix(name, ".png") || strings.HasSuffix(name, ".jpg") || strings.HasSuffix(name, ".jpeg") || strings.HasSuffix(name, ".webp") {
				return mustDataURL(filepath.Join(path, e.Name()))
			}
		}
		fail("no image in %s", path)
	}
	mime := "image/png"
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		mime = "image/jpeg"
	case strings.HasSuffix(lower, ".webp"):
		mime = "image/webp"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
}

func firstExisting(paths ...string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return paths[0]
}

func hostOf(raw string) string {
	if raw == "" {
		return ""
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if j := strings.IndexAny(rest, "/?"); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	return raw
}

func trim(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func must(err error) {
	if err != nil {
		fail("%v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// fixedProxyEgressRepo exposes one in-memory Console egress node (local mihomo).
// It never writes to production DB / grok2api.
type fixedProxyEgressRepo struct{ node egressdomain.Node }

func (r *fixedProxyEgressRepo) ListEgressNodes(_ context.Context, scope egressdomain.Scope, _ repository.SortQuery) ([]egressdomain.Node, error) {
	if scope != egressdomain.ScopeConsole && scope != egressdomain.ScopeConsoleAsset && scope != egressdomain.ScopeWeb {
		return nil, nil
	}
	n := r.node
	n.Scope = scope
	return []egressdomain.Node{n}, nil
}
func (r *fixedProxyEgressRepo) GetEgressNode(_ context.Context, id uint64) (egressdomain.Node, error) {
	if id != r.node.ID {
		return egressdomain.Node{}, fmt.Errorf("not found")
	}
	return r.node, nil
}
func (*fixedProxyEgressRepo) CreateEgressNode(context.Context, egressdomain.Node) (egressdomain.Node, error) {
	return egressdomain.Node{}, fmt.Errorf("unsupported")
}
func (*fixedProxyEgressRepo) UpdateEgressNode(context.Context, egressdomain.Node) (egressdomain.Node, error) {
	return egressdomain.Node{}, fmt.Errorf("unsupported")
}
func (*fixedProxyEgressRepo) DeleteEgressNode(context.Context, uint64) error { return fmt.Errorf("unsupported") }

func bytes32() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i + 1)
	}
	return b
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
