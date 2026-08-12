// Fetches completed Console video jobs by request_id and saves MP4 files.
// Uses the same SSO+DPoP stack as grok2api; does not touch production service.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

type jobSpec struct {
	Name      string `json:"name"`
	AccountID uint64 `json:"account_id"`
	RequestID string `json:"request_id"`
}

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: console-video-fetch <accounts.json> <jobs.json> <outdir>")
		os.Exit(2)
	}
	accountsPath, jobsPath, outDir := os.Args[1], os.Args[2], os.Args[3]
	must(os.MkdirAll(outDir, 0o755))

	var af accountFile
	must(json.Unmarshal(read(accountsPath), &af))
	var jobs []jobSpec
	must(json.Unmarshal(read(jobsPath), &jobs))

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
	adapter := console.NewAdapter(console.Config{BaseURL: envOr("PROBE_BASE_URL", "https://console.x.ai"), TimeoutSeconds: 90}, manager, cipher, nil)

	byID := map[uint64]account.Credential{}
	for _, a := range af.Accounts {
		enc, err := cipher.Encrypt(strings.TrimSpace(a.SSOToken))
		must(err)
		byID[a.ID] = account.Credential{
			ID: a.ID, Name: a.Name, Provider: account.ProviderConsole,
			AuthType: account.AuthTypeSSO, EncryptedAccessToken: enc, Enabled: true,
		}
	}

	ctx := context.Background()
	for _, job := range jobs {
		cred, ok := byID[job.AccountID]
		if !ok {
			fmt.Printf("SKIP %s missing account %d\n", job.Name, job.AccountID)
			continue
		}
		fmt.Printf("FETCH %s account=%d request_id=%s\n", job.Name, job.AccountID, job.RequestID)
		raw, done, videoURL, err := adapter.OfficialVideoPoll(ctx, cred, job.RequestID)
		if err != nil {
			fmt.Printf("  poll error: %v raw=%s\n", err, trim(string(raw), 200))
			continue
		}
		if !done || videoURL == "" {
			fmt.Printf("  not ready raw=%s\n", trim(string(raw), 200))
			continue
		}
		fmt.Printf("  url_host=%s\n", hostOf(videoURL))
		// Prefer authenticated DownloadVideo path (Console asset egress).
		body, contentType, _, err := adapter.DownloadVideo(ctx, cred, videoURL)
		filename := sanitize(job.Name) + ".mp4"
		outPath := filepath.Join(outDir, filename)
		if err != nil {
			fmt.Printf("  DownloadVideo failed (%v); try plain GET via proxy\n", err)
			if err2 := downloadPlain(proxyURL, videoURL, outPath); err2 != nil {
				fmt.Printf("  plain GET failed: %v\n", err2)
				continue
			}
		} else {
			 defer body.Close()
			f, err := os.Create(outPath)
			must(err)
			n, copyErr := io.Copy(f, body)
			_ = f.Close()
			if copyErr != nil {
				fmt.Printf("  save failed: %v\n", copyErr)
				continue
			}
			fmt.Printf("  saved %s bytes=%d content_type=%s\n", outPath, n, contentType)
			continue
		}
		fi, _ := os.Stat(outPath)
		if fi != nil {
			fmt.Printf("  saved %s bytes=%d\n", outPath, fi.Size())
		}
	}
}

func downloadPlain(proxyURL, videoURL, outPath string) error {
	client := &http.Client{Timeout: 3 * time.Minute}
	// Rely on HTTPS_PROXY env if set; also honor explicit proxy via Transport if needed.
	_ = proxyURL
	req, err := http.NewRequest(http.MethodGet, videoURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func read(path string) []byte {
	b, err := os.ReadFile(path)
	must(err)
	return b
}
func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func envOr(k, d string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return d
}
func bytes32() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i + 1)
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
func sanitize(name string) string {
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, name)
	return name
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
