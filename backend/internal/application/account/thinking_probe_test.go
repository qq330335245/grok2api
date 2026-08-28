package account

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestStickyProbeIdentityUsesPlusSuffix(t *testing.T) {
	got := stickyProbeIdentity(accountdomain.Credential{ID: 9, Provider: accountdomain.ProviderBuild, EgressIdentity: "sso_abc"}, 1)
	if got != "sso_abc+1" {
		t.Fatalf("identity=%q", got)
	}
	got = stickyProbeIdentity(accountdomain.Credential{ID: 9, Provider: accountdomain.ProviderBuild}, 2)
	if got != "grok_build_9+2" {
		t.Fatalf("fallback identity=%q", got)
	}
}

func TestScanThinkingSSEGoldStandard(t *testing.T) {
	thinking := strings.Join([]string{
		`event: response.reasoning_text.delta`,
		`data: {"type":"response.reasoning_text.delta","delta":"17*19=323"}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"323"}`,
		``,
	}, "\n")
	if scanThinkingSSE(strings.NewReader(thinking)) != probeThinking {
		t.Fatal("responses reasoning delta must count as thinking")
	}
	chat := `data: {"choices":[{"delta":{"reasoning_content":"step"}}]}` + "\n\n"
	if scanThinkingSSE(strings.NewReader(chat)) != probeThinking {
		t.Fatal("chat reasoning_content must count as thinking")
	}
	missing := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		``,
	}, "\n")
	if scanThinkingSSE(strings.NewReader(missing)) != probeMissingThinking {
		t.Fatal("content without thinking must be a miss")
	}
	if scanThinkingSSE(strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")) != probeInconclusive {
		t.Fatal("empty completed stream must be inconclusive")
	}
}

type scriptedThinkingAdapter struct {
	mu    sync.Mutex
	calls []string
	sse   []string
}

func (a *scriptedThinkingAdapter) Provider() accountdomain.Provider {
	return accountdomain.ProviderBuild
}
func (a *scriptedThinkingAdapter) ForwardResponse(_ context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, request.Credential.EgressIdentity)
	body := ""
	if len(a.sse) > 0 {
		body = a.sse[0]
		if len(a.sse) > 1 {
			a.sse = a.sse[1:]
		}
	}
	return &provider.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
	}, nil
}

func TestInspectBuildBotRiskDoesNotNeedSSO(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "bot-risk.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	accessToken, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(database)
	account, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "probe", UserID: "user-probe",
		SourceKey: "bot-risk-ok", EncryptedAccessToken: accessToken, EgressIdentity: "sticky_acc",
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, Priority: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	account.EgressIdentity = "sticky_acc"
	thinking := "data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"ok\"}\n\n"
	missing := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"
	adapter := &scriptedThinkingAdapter{sse: []string{thinking}}
	service := NewService(repo, nil, nil, nil, provider.NewRegistry(adapter), cipher, nil)
	item := service.inspectBuildBotRisk(ctx, account)
	if item.Outcome != BuildDetectOutcomeOK || item.BotFlagSource != 0 {
		t.Fatalf("thinking hit: %#v", item)
	}
	if len(adapter.calls) != 1 || adapter.calls[0] != "sticky_acc+1" {
		t.Fatalf("calls=%v", adapter.calls)
	}
	latest, err := repo.Get(ctx, account.ID)
	if err != nil || latest.BuildBotFlagSource != 0 || latest.EncryptedSSOToken != "" {
		t.Fatalf("persisted=%#v err=%v", latest, err)
	}

	adapter.calls = nil
	adapter.sse = []string{missing, missing}
	item = service.inspectBuildBotRisk(ctx, account)
	if item.Outcome != BuildDetectOutcomeFlagged || item.BotFlagSource != 2 {
		t.Fatalf("all miss: %#v", item)
	}
	if len(adapter.calls) != 2 || adapter.calls[0] != "sticky_acc+1" || adapter.calls[1] != "sticky_acc+2" {
		t.Fatalf("miss calls=%v", adapter.calls)
	}
	latest, err = repo.Get(ctx, account.ID)
	if err != nil || latest.BuildBotFlagSource != 2 || latest.BuildBotFlagOrigin != accountdomain.BuildBotFlagOriginPage {
		t.Fatalf("flagged persist=%#v err=%v", latest, err)
	}

	adapter.calls = nil
	adapter.sse = []string{missing, thinking}
	item = service.inspectBuildBotRisk(ctx, account)
	if item.Outcome != BuildDetectOutcomeOK || item.BotFlagSource != 0 {
		t.Fatalf("second-hit: %#v", item)
	}
	if len(adapter.calls) != 2 {
		t.Fatalf("want two attempts, got %v", adapter.calls)
	}

	adapter.calls = nil
	adapter.sse = []string{"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"}
	item = service.inspectBuildBotRisk(ctx, account)
	if item.Outcome != BuildDetectOutcomeFailed {
		t.Fatalf("empty stream must fail closed: %#v", item)
	}
	if len(adapter.calls) != 1 {
		t.Fatalf("inconclusive should stop without a second miss: %v", adapter.calls)
	}
}
