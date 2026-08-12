// Offline probe for Console video edit + extend using official API shapes.
// Does not modify production grok2api.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type accountFile struct {
	Accounts []struct {
		ID       uint64 `json:"id"`
		Name     string `json:"name"`
		SSOToken string `json:"sso_token"`
	} `json:"accounts"`
}

func main() {
	accountsPath := flag.String("accounts", "accounts.json", "exported console SSO accounts")
	sourceVideo := flag.String("video", "", "source mp4 path (required)")
	outDir := flag.String("out", "videos-edit-extend", "output directory")
	baseURL := flag.String("base-url", "https://console.x.ai", "Console base URL")
	model := flag.String("model", "grok-imagine-video", "video model")
	editPrompt := flag.String("edit-prompt", "给画面加上轻微的电影感色彩和慢镜头氛围，保持主体动作不变", "edit prompt")
	extendPrompt := flag.String("extend-prompt", "镜头继续推进，动作自然延伸，保持同一场景与角色", "extend prompt")
	extendDuration := flag.Int("extend-duration", 6, "extension segment seconds (2-10)")
	pollEvery := flag.Duration("poll-every", 5*time.Second, "poll interval")
	timeout := flag.Duration("timeout", 12*time.Minute, "per-test timeout")
	only := flag.String("only", "", "edit | extend | empty=both")
	flag.Parse()
	if strings.TrimSpace(*sourceVideo) == "" {
		fail("-video is required")
	}
	must(os.MkdirAll(*outDir, 0o755))

	var af accountFile
	must(json.Unmarshal(mustRead(*accountsPath), &af))
	if len(af.Accounts) == 0 {
		fail("no accounts")
	}

	videoBytes := mustRead(*sourceVideo)
	videoDataURL := "data:video/mp4;base64," + base64.StdEncoding.EncodeToString(videoBytes)
	fmt.Printf("source_video=%s bytes=%d data_url_len=%d\n", *sourceVideo, len(videoBytes), len(videoDataURL))

	key := base64.StdEncoding.EncodeToString(bytes32())
	cipher, err := security.NewCipher(key)
	must(err)
	proxyURL := envOr("PROBE_PROXY_URL", "http://127.0.0.1:7890")
	encProxy, err := cipher.Encrypt(proxyURL)
	must(err)
	repo := &fixedProxyEgressRepo{node: egressdomain.Node{
		ID: 1, Name: "probe-mihomo", Scope: egressdomain.ScopeConsole, Enabled: true, Health: 1,
		EncryptedProxyURL: encProxy,
		UserAgent:         "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	}}
	manager := egress.NewManager(repo, cipher)
	adapter := console.NewAdapter(console.Config{BaseURL: *baseURL, TimeoutSeconds: 120}, manager, cipher, nil)
	fmt.Printf("probe_proxy=%s base_url=%s model=%s\n", proxyURL, *baseURL, *model)

	type job struct {
		Name string
		Path string
		Body []byte
	}
	var jobs []job
	if *only == "" || *only == "edit" {
		body, _ := json.Marshal(map[string]any{
			"model":  *model,
			"prompt": *editPrompt,
			"video":  map[string]any{"url": videoDataURL},
		})
		jobs = append(jobs, job{Name: "video-edit", Path: "/videos/edits", Body: body})
	}
	if *only == "" || *only == "extend" {
		body, _ := json.Marshal(map[string]any{
			"model":    *model,
			"prompt":   *extendPrompt,
			"duration": *extendDuration,
			"video":    map[string]any{"url": videoDataURL},
		})
		jobs = append(jobs, job{Name: "video-extend", Path: "/videos/extensions", Body: body})
	}

	ctx := context.Background()
	results := []map[string]any{}
	accIdx := 0
	nextCred := func() account.Credential {
		if accIdx >= len(af.Accounts) {
			fail("out of accounts")
		}
		a := af.Accounts[accIdx]
		accIdx++
		enc, err := cipher.Encrypt(strings.TrimSpace(a.SSOToken))
		must(err)
		fmt.Printf("\n== account #%d id=%d name=%s ==\n", accIdx, a.ID, a.Name)
		return account.Credential{
			ID: a.ID, Name: a.Name, Provider: account.ProviderConsole,
			AuthType: account.AuthTypeSSO, EncryptedAccessToken: enc, Enabled: true,
		}
	}

	for _, j := range jobs {
		cred := nextCred()
		fmt.Printf("\n-------- TEST %s path=%s --------\n", j.Name, j.Path)
		fmt.Printf("body_bytes=%d shape=%s\n", len(j.Body), summarizeBody(j.Body))
		start := time.Now()
		reqID, createRaw, err := adapter.OfficialVideoPost(ctx, cred, j.Path, j.Body)
		entry := map[string]any{
			"test": j.Name, "path": j.Path, "account_id": cred.ID, "account_name": cred.Name,
			"create_ms": time.Since(start).Milliseconds(), "model": *model,
		}
		if err != nil {
			entry["ok"] = false
			entry["phase"] = "create"
			entry["error"] = err.Error()
			entry["create_raw"] = trim(string(createRaw), 500)
			fmt.Printf("CREATE FAIL: %v\nraw=%s\n", err, trim(string(createRaw), 500))
			results = append(results, entry)
			continue
		}
		entry["request_id"] = reqID
		fmt.Printf("CREATE OK request_id=%s\n", reqID)

		pollStart := time.Now()
		var finalRaw []byte
		var videoURL string
		var pollErr error
		deadline := time.Now().Add(*timeout)
		ticker := time.NewTicker(*pollEvery)
		for {
			finalRaw, done, url, err := adapter.OfficialVideoPoll(ctx, cred, reqID)
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
			<-ticker.C
		}
		ticker.Stop()
		entry["poll_ms"] = time.Since(pollStart).Milliseconds()
		entry["total_ms"] = time.Since(start).Milliseconds()
		if pollErr != nil {
			entry["ok"] = false
			entry["phase"] = "poll"
			entry["error"] = pollErr.Error()
			entry["poll_raw"] = trim(string(finalRaw), 500)
			fmt.Printf("POLL FAIL: %v\n", pollErr)
			results = append(results, entry)
			continue
		}
		entry["ok"] = true
		entry["video_url_host"] = hostOf(videoURL)
		fmt.Printf("DONE host=%s\n", hostOf(videoURL))

		// download
		outPath := filepath.Join(*outDir, j.Name+".mp4")
		body, ctype, _, err := adapter.DownloadVideo(ctx, cred, videoURL)
		if err != nil {
			entry["download_error"] = err.Error()
			fmt.Printf("DOWNLOAD FAIL: %v\n", err)
		} else {
			func() {
				defer body.Close()
				f, err := os.Create(outPath)
				if err != nil {
					entry["download_error"] = err.Error()
					return
				}
				defer f.Close()
				n, err := io.Copy(f, body)
				if err != nil {
					entry["download_error"] = err.Error()
					return
				}
				entry["saved"] = outPath
				entry["saved_bytes"] = n
				entry["content_type"] = ctype
				fmt.Printf("SAVED %s bytes=%d type=%s\n", outPath, n, ctype)
			}()
		}
		results = append(results, entry)
	}

	out, _ := json.MarshalIndent(results, "", "  ")
	fmt.Printf("\n===== SUMMARY =====\n%s\n", out)
	_ = os.WriteFile(filepath.Join(*outDir, "probe-results.json"), out, 0o600)
}

func summarizeBody(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return trim(string(body), 200)
	}
	if v, ok := m["video"].(map[string]any); ok {
		if u, _ := v["url"].(string); strings.HasPrefix(u, "data:") {
			v["url"] = fmt.Sprintf("dataURL(len=%d)", len(u))
		}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func mustRead(path string) []byte {
	b, err := os.ReadFile(path)
	must(err)
	return b
}
func must(err error) {
	if err != nil {
		fail("%v", err)
	}
}
func fail(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...); os.Exit(1) }
func envOr(k, d string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return d
}
func bytes32() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i + 3)
	}
	return b
}
func trim(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
func hostOf(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if j := strings.IndexAny(rest, "/?"); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	return raw
}

type fixedProxyEgressRepo struct{ node egressdomain.Node }

func (r *fixedProxyEgressRepo) ListEgressNodes(_ context.Context, scope egressdomain.Scope, _ repository.SortQuery) ([]egressdomain.Node, error) {
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
