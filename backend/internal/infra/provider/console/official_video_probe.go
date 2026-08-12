package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// OfficialVideoCreate posts an arbitrary JSON body to Console
// POST /v1/videos/generations using the same SSO+DPoP+egress stack as GenerateVideo.
// Intended for offline capability probes; does not enforce grok2api's local field limits.
func (a *Adapter) OfficialVideoCreate(ctx context.Context, credential account.Credential, body []byte) (requestID string, raw []byte, err error) {
	return a.OfficialVideoPost(ctx, credential, "/videos/generations", body)
}

// OfficialVideoPost posts body to an arbitrary Console /v1 video path
// (generations / edits / extensions) with the same auth stack.
func (a *Adapter) OfficialVideoPost(ctx context.Context, credential account.Credential, path string, body []byte) (requestID string, raw []byte, err error) {
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return "", nil, err
	}
	lease, err := a.egress.AcquireCredential(ctx, egressdomain.ScopeConsole, credential)
	if err != nil {
		return "", nil, err
	}
	defer lease.Release()
	endpoint := consoleV1Endpoint(a.config().BaseURL, path)
	raw, err = a.doConsoleVideoJSON(ctx, credential, token, lease, http.MethodPost, endpoint, body)
	if err != nil {
		return "", raw, err
	}
	requestID, err = parseConsoleVideoCreate(raw)
	return requestID, raw, err
}

// OfficialVideoPoll GETs /v1/videos/{id} with the same auth stack.
func (a *Adapter) OfficialVideoPoll(ctx context.Context, credential account.Credential, requestID string) (raw []byte, done bool, videoURL string, err error) {
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return nil, false, "", err
	}
	lease, err := a.egress.AcquireCredential(ctx, egressdomain.ScopeConsole, credential)
	if err != nil {
		return nil, false, "", err
	}
	defer lease.Release()
	endpoint := consoleV1Endpoint(a.config().BaseURL, "/videos/"+url.PathEscape(strings.TrimSpace(requestID)))
	raw, err = a.doConsoleVideoJSON(ctx, credential, token, lease, http.MethodGet, endpoint, nil)
	if err != nil {
		return raw, false, "", err
	}
	result, finished, parseErr := parseConsoleVideoStatus(raw, nil)
	if parseErr != nil {
		// still return raw body for diagnostics
		return raw, false, "", parseErr
	}
	if finished {
		return raw, true, result.URL, nil
	}
	return raw, false, "", nil
}

// OfficialVideoWait polls until done/failed or timeout.
func (a *Adapter) OfficialVideoWait(ctx context.Context, credential account.Credential, requestID string, every time.Duration, timeout time.Duration) (raw []byte, videoURL string, err error) {
	if every <= 0 {
		every = consoleVideoPollEvery
	}
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		var finished bool
		raw, finished, videoURL, err = a.OfficialVideoPoll(ctx, credential, requestID)
		if err != nil {
			return raw, "", err
		}
		if finished {
			return raw, videoURL, nil
		}
		if time.Now().After(deadline) {
			return raw, "", fmt.Errorf("poll timeout after %s; last body=%s", timeout, trimProbeBody(raw))
		}
		select {
		case <-ctx.Done():
			return raw, "", ctx.Err()
		case <-ticker.C:
		}
	}
}

// MarshalOfficialVideoBody builds an official-docs-shaped create payload.
// imageURL and referenceImageURLs are mutually exclusive at the call site when mimicking docs.
func MarshalOfficialVideoBody(model, prompt, aspectRatio, resolution string, duration int, imageURL string, referenceImageURLs []string) ([]byte, error) {
	payload := map[string]any{
		"model": strings.TrimSpace(model),
	}
	if duration > 0 {
		payload["duration"] = duration
	}
	if p := strings.TrimSpace(prompt); p != "" {
		payload["prompt"] = p
	}
	if r := strings.TrimSpace(aspectRatio); r != "" {
		payload["aspect_ratio"] = r
	}
	if r := strings.TrimSpace(resolution); r != "" {
		payload["resolution"] = r
	}
	if u := strings.TrimSpace(imageURL); u != "" {
		payload["image"] = map[string]any{"url": u}
	}
	if len(referenceImageURLs) > 0 {
		refs := make([]map[string]any, 0, len(referenceImageURLs))
		for _, raw := range referenceImageURLs {
			u := strings.TrimSpace(raw)
			if u == "" {
				continue
			}
			refs = append(refs, map[string]any{"url": u})
		}
		if len(refs) > 0 {
			payload["reference_images"] = refs
		}
	}
	return json.Marshal(payload)
}

func trimProbeBody(raw []byte) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	if len(s) > 240 {
		return s[:240] + "…"
	}
	return s
}
