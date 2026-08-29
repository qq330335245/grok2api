package account

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
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

func TestFormatProbeTargetIncludesNodeAndExitIP(t *testing.T) {
	got := formatProbeTarget(BotRiskProbeAttempt{Identity: "acc+1", NodeName: "katabump 008", ExitIP: "51.75.118.171"})
	if got != "acc+1 katabump 008 51.75.118.171" {
		t.Fatalf("target=%q", got)
	}
	if formatProbeTarget(BotRiskProbeAttempt{Identity: "acc+2"}) != "acc+2" {
		t.Fatal("identity-only target")
	}
}

func TestFormatFailedProbeReasonIncludesStatusAndDetail(t *testing.T) {
	got := formatFailedProbeReason(BotRiskProbeAttempt{
		Identity: "acc+1", NodeName: "阿里云ipv6动态粘性", ExitIP: "2a11:6c7::1", Status: 403, Detail: "access denied",
	})
	if !strings.Contains(got, "acc+1") || !strings.Contains(got, "HTTP 403") || !strings.Contains(got, "access denied") {
		t.Fatalf("reason=%q", got)
	}
}

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
	if scanThinkingSSE(strings.NewReader(thinking)).verdict != probeThinking {
		t.Fatal("responses reasoning delta must count as thinking")
	}
	chat := `data: {"choices":[{"delta":{"reasoning_content":"step"}}]}` + "\n\n"
	if scanThinkingSSE(strings.NewReader(chat)).verdict != probeThinking {
		t.Fatal("chat reasoning_content must count as thinking")
	}
	missing := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		``,
	}, "\n")
	if scanThinkingSSE(strings.NewReader(missing)).verdict != probeMissingThinking {
		t.Fatal("content without thinking must be a miss")
	}
	if scanThinkingSSE(strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")).verdict != probeInconclusive {
		t.Fatal("empty completed stream must be inconclusive")
	}
	objectDelta := `data: {"type":"response.reasoning_text.delta","delta":{"text":"nope"}}` + "\n\n"
	if scanThinkingSSE(strings.NewReader(objectDelta)).verdict != probeInconclusive {
		t.Fatal("non-string reasoning delta must not count as thinking")
	}
}

type scriptedThinkingAdapter struct {
	mu      sync.Mutex
	calls   []string
	sse     []string
	status  int
	errBody []byte
	fail    error
}

func (a *scriptedThinkingAdapter) Provider() accountdomain.Provider {
	return accountdomain.ProviderBuild
}
func (a *scriptedThinkingAdapter) ForwardResponse(_ context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, request.Credential.EgressIdentity)
	if a.fail != nil {
		return nil, a.fail
	}
	if a.status != 0 && a.status != http.StatusOK {
		body := a.errBody
		if body == nil {
			body = []byte(`{}`)
		}
		return &provider.Response{
			StatusCode: a.status,
			Body:       io.NopCloser(bytes.NewReader(body)),
		}, nil
	}
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
	if !strings.Contains(item.Reason, "sticky_acc+1") {
		t.Fatalf("reason=%q", item.Reason)
	}
	if len(item.Attempts) != 1 || item.Attempts[0].Identity != "sticky_acc+1" || item.Attempts[0].Verdict != "thinking" {
		t.Fatalf("attempts=%#v", item.Attempts)
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
	if !strings.Contains(item.Reason, "sticky_acc+1") || !strings.Contains(item.Reason, "sticky_acc+2") {
		t.Fatalf("flagged reason=%q", item.Reason)
	}
	if len(item.Attempts) != 2 || item.Attempts[0].Identity != "sticky_acc+1" || item.Attempts[1].Identity != "sticky_acc+2" {
		t.Fatalf("flagged attempts=%#v", item.Attempts)
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
	if !strings.Contains(item.Reason, "空流") {
		t.Fatalf("empty stream reason=%q", item.Reason)
	}
	if len(adapter.calls) != 1 {
		t.Fatalf("inconclusive should stop without a second miss: %v", adapter.calls)
	}
}

func TestInspectBuildBotRiskQuotaExhausted(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "bot-risk-quota.db"))
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
		Provider: accountdomain.ProviderBuild, Name: "quota", UserID: "user-quota",
		SourceKey: "bot-risk-quota", EncryptedAccessToken: accessToken,
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, Priority: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	account.EgressIdentity = "sticky_acc"
	adapter := &scriptedThinkingAdapter{
		status:  http.StatusTooManyRequests,
		errBody: []byte(`{"code":"subscription:free-usage-exhausted","error":"tokens (actual/limit): 10/10"}`),
	}
	service := NewService(repo, nil, nil, nil, provider.NewRegistry(adapter), cipher, nil)
	item := service.inspectBuildBotRisk(ctx, account)
	if item.Outcome != BuildDetectOutcomeFailed || item.BotFlagSource != 0 || !strings.Contains(item.Reason, "额度已满") {
		t.Fatalf("quota item=%#v", item)
	}
	if len(item.Attempts) != 1 || item.Attempts[0].Identity != "sticky_acc+1" {
		t.Fatalf("quota attempts=%#v", item.Attempts)
	}
	if len(adapter.calls) != 1 {
		t.Fatalf("quota should stop after first attempt: %v", adapter.calls)
	}
	recovery, err := repo.GetQuotaRecovery(ctx, account.ID)
	if err != nil || recovery.Kind != accountdomain.QuotaRecoveryKindFree || recovery.Status != accountdomain.QuotaRecoveryStatusExhausted {
		t.Fatalf("quota recovery=%#v err=%v", recovery, err)
	}
	latest, err := repo.Get(ctx, account.ID)
	if err != nil || latest.BuildBotFlagSource == 2 {
		t.Fatalf("quota must not persist botflag=2: %#v err=%v", latest, err)
	}
}

func TestInspectBuildBotRiskSurfacesTransportAndHTTPFailures(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "bot-risk-fail.db"))
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
		Provider: accountdomain.ProviderBuild, Name: "fail", UserID: "user-fail",
		SourceKey: "bot-risk-fail", EncryptedAccessToken: accessToken,
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, Priority: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	account.EgressIdentity = "sticky_acc"

	adapter := &scriptedThinkingAdapter{fail: errors.New("socks connect tcp 127.0.0.1:10800: i/o timeout")}
	service := NewService(repo, nil, nil, nil, provider.NewRegistry(adapter), cipher, nil)
	item := service.inspectBuildBotRisk(ctx, account)
	if item.Outcome != BuildDetectOutcomeFailed || item.BotFlagSource != 0 {
		t.Fatalf("transport item=%#v", item)
	}
	if !strings.Contains(item.Reason, "i/o timeout") {
		t.Fatalf("transport reason=%q", item.Reason)
	}
	if len(item.Attempts) != 1 || !strings.Contains(item.Attempts[0].Detail, "i/o timeout") {
		t.Fatalf("transport attempts=%#v", item.Attempts)
	}

	adapter.fail = nil
	adapter.status = http.StatusForbidden
	adapter.errBody = []byte(`{"error":{"code":7,"message":"This page is out of date. Reload to continue."}}`)
	item = service.inspectBuildBotRisk(ctx, account)
	if item.Outcome != BuildDetectOutcomeFailed || item.HTTPStatus != http.StatusForbidden {
		t.Fatalf("http item=%#v", item)
	}
	if !strings.Contains(item.Reason, "HTTP 403") || !strings.Contains(item.Reason, "out of date") {
		t.Fatalf("http reason=%q", item.Reason)
	}
}
