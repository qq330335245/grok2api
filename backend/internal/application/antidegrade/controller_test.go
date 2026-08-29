package antidegrade

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

type staticNodes []Node

func (s staticNodes) ListBuildNodes(context.Context) ([]Node, error) { return s, nil }

type banRecorder struct{ ids []uint64 }

func (b *banRecorder) Disable(_ context.Context, id uint64) error {
	b.ids = append(b.ids, id)
	return nil
}

func TestAdmitSkipsCooledIPAndKeepsSameAccountBindingWhenHealthy(t *testing.T) {
	nodes := staticNodes{
		{ID: 1, Enabled: true, ExitIP: "203.0.113.1"},
		{ID: 2, Enabled: true, ExitIP: "203.0.113.2"},
	}
	controller := New(Config{Enabled: true, Mode: ModeEnforce, StateFile: t.TempDir() + "/l.json"}, nodes, nil, nil)
	controller.ledger.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	cred := accountdomain.Credential{ID: 10, EgressNodeID: 1}
	got, err := controller.Admit(context.Background(), cred, nil)
	if err != nil || got != 0 {
		t.Fatalf("healthy binding override=%d err=%v", got, err)
	}
	controller.OnMissingThinking(context.Background(), cred, 1, "203.0.113.1")
	got, err = controller.Admit(context.Background(), cred, map[uint64]bool{1: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("retry node=%d, want 2", got)
	}
}

func TestMissingThinkingBansOnlyOnBotFlagTwoAfterDistinctIPs(t *testing.T) {
	nodes := staticNodes{{ID: 1, Enabled: true, ExitIP: "10.0.0.1"}, {ID: 2, Enabled: true, ExitIP: "10.0.0.2"}}
	bans := &banRecorder{}
	controller := New(Config{Enabled: true, Mode: ModeEnforce, AccountIPFailThreshold: 2, StateFile: t.TempDir() + "/l.json"}, nodes, bans, nil)
	cred := accountdomain.Credential{ID: 7, BuildBotFlagSource: 2}
	controller.OnMissingThinking(context.Background(), cred, 1, "10.0.0.1")
	if len(bans.ids) != 0 {
		t.Fatal("should not ban on first IP")
	}
	controller.OnMissingThinking(context.Background(), cred, 2, "10.0.0.2")
	if len(bans.ids) != 1 || bans.ids[0] != 7 {
		t.Fatalf("bans=%v", bans.ids)
	}
}

func TestMissingThinkingDoesNotBanUnclassifiedAccount(t *testing.T) {
	bans := &banRecorder{}
	controller := New(Config{Enabled: true, Mode: ModeEnforce, AccountIPFailThreshold: 2, StateFile: t.TempDir() + "/l.json"}, nil, bans, nil)
	cred := accountdomain.Credential{ID: 8, BuildBotFlagSource: 0}
	controller.OnMissingThinking(context.Background(), cred, 1, "10.0.0.1")
	controller.OnMissingThinking(context.Background(), cred, 2, "10.0.0.2")
	if len(bans.ids) != 0 {
		t.Fatalf("banned unclassified account: %v", bans.ids)
	}
	if !controller.AccountQuarantined(8) {
		t.Fatal("unclassified account must still be quarantined after K IPs")
	}
}

func TestMissingThinkingQuarantinesAndKeepsFirstIPCool(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nodes := staticNodes{
		{ID: 1, Enabled: true, ExitIP: "10.0.0.1"},
		{ID: 2, Enabled: true, ExitIP: "10.0.0.2"},
	}
	controller := New(Config{
		Enabled: true, Mode: ModeEnforce, AccountIPFailThreshold: 2,
		DirtyIPCooldown: 30 * time.Minute, AccountQuarantineTTL: 2 * time.Hour,
		StateFile: t.TempDir() + "/l.json",
	}, nodes, nil, nil)
	controller.ledger.now = func() time.Time { return now }
	cred := accountdomain.Credential{ID: 7, EgressNodeID: 1}
	controller.OnMissingThinking(context.Background(), cred, 1, "10.0.0.1")
	if controller.AccountQuarantined(7) {
		t.Fatal("must not quarantine on first IP")
	}
	controller.OnMissingThinking(context.Background(), cred, 2, "10.0.0.2")
	if !controller.AccountQuarantined(7) {
		t.Fatal("expected quarantine after K IPs")
	}
	controller.ledger.mu.Lock()
	firstCool := controller.ledger.cooling(controller.ledger.ip("10.0.0.1"), now)
	secondCool := controller.ledger.cooling(controller.ledger.ip("10.0.0.2"), now)
	controller.ledger.mu.Unlock()
	if !firstCool {
		t.Fatal("first failed IP must stay cooled")
	}
	if secondCool {
		t.Fatal("later failed IPs must be lifted after account quarantine")
	}
	if _, err := controller.Admit(context.Background(), cred, nil); !errors.Is(err, ErrAccountQuarantined) {
		t.Fatalf("admit quarantined account err=%v", err)
	}
	other := accountdomain.Credential{ID: 9, EgressNodeID: 2}
	got, err := controller.Admit(context.Background(), other, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("other account should keep healthy binding, override=%d", got)
	}
	controller.OnMissingThinking(context.Background(), cred, 2, "10.0.0.2")
	controller.ledger.mu.Lock()
	if controller.ledger.cooling(controller.ledger.ip("10.0.0.2"), now) {
		controller.ledger.mu.Unlock()
		t.Fatal("already-quarantined account must not cool more IPs")
	}
	controller.ledger.mu.Unlock()
	controller.ClearAccount(context.Background(), 7)
	if controller.AccountQuarantined(7) {
		t.Fatal("clear-cooldown must lift quarantine")
	}
	controller.OnMissingThinking(context.Background(), cred, 1, "10.0.0.1")
	if controller.AccountQuarantined(7) {
		t.Fatal("clearing quarantine must reset fail-exit count")
	}
}

func TestSameIPMissesDoNotQuarantine(t *testing.T) {
	nodes := staticNodes{{ID: 1, Enabled: true, ExitIP: "10.0.0.1"}, {ID: 2, Enabled: true, ExitIP: "10.0.0.2"}}
	controller := New(Config{Enabled: true, Mode: ModeEnforce, AccountIPFailThreshold: 2, StateFile: t.TempDir() + "/l.json"}, nodes, nil, nil)
	cred := accountdomain.Credential{ID: 11, Provider: accountdomain.ProviderBuild}
	controller.OnMissingThinking(context.Background(), cred, 1, "10.0.0.1")
	controller.OnMissingThinking(context.Background(), cred, 1, "10.0.0.1")
	if controller.AccountQuarantined(11) {
		t.Fatal("same ExitIP misses must not quarantine the account")
	}
}

func TestThinkingSuccessResetsConsecutiveMisses(t *testing.T) {
	nodes := staticNodes{{ID: 1, Enabled: true, ExitIP: "10.0.0.1"}, {ID: 2, Enabled: true, ExitIP: "10.0.0.2"}}
	controller := New(Config{Enabled: true, Mode: ModeEnforce, AccountIPFailThreshold: 2, StateFile: t.TempDir() + "/l.json"}, nodes, nil, nil)
	cred := accountdomain.Credential{ID: 12, Provider: accountdomain.ProviderBuild}
	controller.OnMissingThinking(context.Background(), cred, 1, "10.0.0.1")
	controller.OnSuccess(12, 2, "10.0.0.2")
	controller.OnMissingThinking(context.Background(), cred, 2, "10.0.0.2")
	if controller.AccountQuarantined(12) {
		t.Fatal("a streamed-thinking success must reset the consecutive streak")
	}
}

func TestRecidivismLengthensQuarantineAfterRelease(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nodes := staticNodes{{ID: 1, Enabled: true, ExitIP: "10.0.0.1"}, {ID: 2, Enabled: true, ExitIP: "10.0.0.2"}}
	controller := New(Config{
		Enabled: true, Mode: ModeEnforce, AccountIPFailThreshold: 2,
		AccountQuarantineTTL: 2 * time.Hour, StateFile: t.TempDir() + "/l.json",
	}, nodes, nil, nil)
	controller.ledger.now = func() time.Time { return now }
	cred := accountdomain.Credential{ID: 13, Provider: accountdomain.ProviderBuild}
	controller.OnMissingThinking(context.Background(), cred, 1, "10.0.0.1")
	controller.OnMissingThinking(context.Background(), cred, 2, "10.0.0.2")
	firstUntil := controller.ledger.state.Accounts[13].QuarantineUntil
	if !firstUntil.Equal(now.Add(2 * time.Hour)) {
		t.Fatalf("first quarantine until %s", firstUntil)
	}
	controller.ClearAccount(context.Background(), 13)
	if controller.ledger.state.Accounts[13].Recidivism != 1 {
		t.Fatalf("dossier recidivism=%d", controller.ledger.state.Accounts[13].Recidivism)
	}
	controller.OnMissingThinking(context.Background(), cred, 1, "10.0.0.1")
	if controller.AccountQuarantined(13) {
		t.Fatal("cleared consecutive streak must start over")
	}
	controller.OnMissingThinking(context.Background(), cred, 2, "10.0.0.2")
	secondUntil := controller.ledger.state.Accounts[13].QuarantineUntil
	if !secondUntil.Equal(now.Add(4 * time.Hour)) {
		t.Fatalf("second quarantine until %s, want +4h", secondUntil)
	}
}

func TestExitIPRemembersLastProbeWhenNodeIPEmpty(t *testing.T) {
	nodes := staticNodes{{ID: 9, Enabled: true, ExitIP: "51.75.118.79"}}
	controller := New(Config{Enabled: true, Mode: ModeEnforce, StateFile: t.TempDir() + "/l.json"}, nodes, nil, nil)
	controller.OnMissingThinking(context.Background(), accountdomain.Credential{ID: 7}, 9, controller.ExitIP(context.Background(), 9))
	controller.nodes = staticNodes{{ID: 9, Enabled: true, ExitIP: ""}}
	if got := controller.ExitIP(context.Background(), 9); got != "51.75.118.79" {
		t.Fatalf("ExitIP=%q, want remembered 51.75.118.79", got)
	}
}

func TestSnapshotShowsWindowLoadAndClearIP(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nodes := staticNodes{
		{ID: 1, Enabled: true, ExitIP: "10.0.0.1", Name: "node-a"},
		{ID: 2, Enabled: true, ExitIP: "10.0.0.2", Name: "node-b"},
	}
	controller := New(Config{
		Enabled: true, Mode: ModeEnforce, DensityWindow: 15 * time.Minute, DensityMaxAccounts: 5,
		DirtyIPCooldown: 30 * time.Minute, AccountIPFailThreshold: 2, StateFile: t.TempDir() + "/l.json",
	}, nodes, nil, nil)
	controller.ledger.now = func() time.Time { return now }
	first := accountdomain.Credential{ID: 7}
	second := accountdomain.Credential{ID: 8}
	controller.OnMissingThinking(context.Background(), first, 1, "10.0.0.1")
	controller.OnSuccess(8, 1, "10.0.0.1")
	_ = second
	snapshot := controller.Snapshot(context.Background())
	if len(snapshot.IPs) != 1 {
		t.Fatalf("ips=%d", len(snapshot.IPs))
	}
	if snapshot.IPs[0].AccountCount != 2 || snapshot.IPs[0].AccountLimit != 5 || !snapshot.IPs[0].Cooling {
		t.Fatalf("ip=%#v", snapshot.IPs[0])
	}
	controller.ClearIP(context.Background(), "10.0.0.1")
	snapshot = controller.Snapshot(context.Background())
	if snapshot.IPs[0].Cooling {
		t.Fatal("cleared IP should not be cooling")
	}
}

func TestExitIPResolvesConsoleNodeAndRekeysPlaceholder(t *testing.T) {
	nodes := staticNodes{
		{ID: 3, Enabled: true, ExitIP: "104.28.215.68", Name: "console 001", Scope: "grok_console"},
		{ID: 7, Enabled: true, ExitIP: "147.135.213.131", Name: "katabump 001", Scope: "grok_build"},
	}
	controller := New(Config{Enabled: true, Mode: ModeEnforce, StateFile: t.TempDir() + "/l.json"}, nodes, nil, nil)
	controller.OnSuccess(53, 3, "node:3")
	if got := controller.ExitIP(context.Background(), 3); got != "104.28.215.68" {
		t.Fatalf("ExitIP=%q", got)
	}
	snapshot := controller.Snapshot(context.Background())
	if len(snapshot.IPs) != 1 || snapshot.IPs[0].ExitIP != "104.28.215.68" {
		t.Fatalf("ips=%#v", snapshot.IPs)
	}
	if len(snapshot.IPs[0].NodeNames) != 1 || snapshot.IPs[0].NodeNames[0] != "console 001" {
		t.Fatalf("names=%v", snapshot.IPs[0].NodeNames)
	}
	controller.ledger.mu.Lock()
	_, placeholder := controller.ledger.state.IPs["node:3"]
	controller.ledger.mu.Unlock()
	if placeholder {
		t.Fatal("placeholder node:3 should be merged into the real ExitIP")
	}
}

func TestSharedExitNodeCoolsAccountIPWithoutBlockingOthers(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nodes := staticNodes{
		{ID: 82, Enabled: true, ExitIP: "2001:db8::probe", Name: "ipv6-sticky", Scope: "grok_build", SharedExit: true},
		{ID: 1, Enabled: true, ExitIP: "203.0.113.1", Name: "fixed", Scope: "grok_build"},
	}
	controller := New(Config{
		Enabled: true, Mode: ModeEnforce, DensityMaxAccounts: 1, DirtyIPCooldown: 30 * time.Minute,
		StateFile: t.TempDir() + "/l.json",
	}, nodes, nil, nil)
	controller.ledger.now = func() time.Time { return now }
	degraded := accountdomain.Credential{ID: 41, Provider: accountdomain.ProviderBuild, EgressNodeID: 82}
	healthy := accountdomain.Credential{ID: 42, Provider: accountdomain.ProviderBuild, EgressNodeID: 82}
	controller.OnMissingThinking(context.Background(), degraded, 82, "2001:db8::aaaa")

	got, err := controller.Admit(context.Background(), healthy, nil)
	if err != nil {
		t.Fatalf("healthy account must still use the sticky node, err=%v", err)
	}
	if got != 0 {
		t.Fatalf("healthy binding override=%d, want 0 (keep node 82)", got)
	}
	got, err = controller.Admit(context.Background(), degraded, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("degraded account must leave the cooled sticky IP, override=%d want 1", got)
	}
	snapshot := controller.Snapshot(context.Background())
	if len(snapshot.IPs) == 0 {
		t.Fatal("IP load must still list sticky occupancy")
	}
	for _, ip := range snapshot.IPs {
		if ip.ExitIP == "2001:db8::probe" {
			t.Fatalf("probe identity must not appear in IP load: %#v", ip)
		}
		if ip.ExitIP == "2001:db8::aaaa" && !ip.Cooling {
			t.Fatalf("account ExitIP should be cooling: %#v", ip)
		}
	}
}

func TestAppliesToDefaultsToBuildOnly(t *testing.T) {
	controller := New(Config{Enabled: true, Mode: ModeEnforce, StateFile: t.TempDir() + "/l.json"}, nil, nil, nil)
	if !controller.AppliesTo(accountdomain.ProviderBuild) || !controller.ActiveFor(accountdomain.ProviderBuild) {
		t.Fatal("build must be in the default allowlist")
	}
	if controller.AppliesTo(accountdomain.ProviderConsole) || controller.ActiveFor(accountdomain.ProviderConsole) {
		t.Fatal("console must be ignored until opted in")
	}
	controller.OnMissingThinking(context.Background(), accountdomain.Credential{ID: 53, Provider: accountdomain.ProviderConsole}, 3, "104.28.215.68")
	if controller.AccountQuarantined(53) {
		t.Fatal("console missing-thinking must not quarantine")
	}
	controller.ledger.mu.Lock()
	_, recorded := controller.ledger.state.IPs["104.28.215.68"]
	controller.ledger.mu.Unlock()
	if recorded {
		t.Fatal("console missing-thinking must not write the ExitIP ledger")
	}
	controller.Update(Config{Enabled: true, Mode: ModeEnforce, Providers: []string{"grok_build", "grok_console"}, StateFile: controller.config().StateFile})
	if !controller.ActiveFor(accountdomain.ProviderConsole) {
		t.Fatal("opting in grok_console must activate the channel")
	}
}

func TestAdmitKeepsBuildRetryOnBuildNodes(t *testing.T) {
	nodes := staticNodes{
		{ID: 3, Enabled: true, ExitIP: "104.28.215.68", Name: "console 001", Scope: "grok_console"},
		{ID: 7, Enabled: true, ExitIP: "147.135.213.131", Name: "katabump 001", Scope: "grok_build"},
	}
	controller := New(Config{Enabled: true, Mode: ModeEnforce, DensityMaxAccounts: 5, StateFile: t.TempDir() + "/l.json"}, nodes, nil, nil)
	picked, err := controller.Admit(context.Background(), accountdomain.Credential{ID: 48, Provider: accountdomain.ProviderBuild}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if picked != 7 {
		t.Fatalf("picked=%d, want build node 7", picked)
	}
}

func TestAdmitSkipsNodesWithoutRealExitIP(t *testing.T) {
	nodes := staticNodes{
		{ID: 214, Enabled: true, ExitIP: "", Name: "us-02"},
		{ID: 7, Enabled: true, ExitIP: "147.135.213.131", Name: "katabump"},
	}
	controller := New(Config{Enabled: true, Mode: ModeEnforce, DensityMaxAccounts: 5, StateFile: t.TempDir() + "/l.json"}, nodes, nil, nil)
	picked, err := controller.Admit(context.Background(), accountdomain.Credential{ID: 1, Provider: accountdomain.ProviderBuild}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if picked != 7 {
		t.Fatalf("picked=%d, want node with real ExitIP", picked)
	}
}

func TestCooldownPlaceholderExcludesNodeEvenAfterIPAppears(t *testing.T) {
	nodes := staticNodes{{ID: 214, Enabled: true, ExitIP: "198.51.100.9"}}
	controller := New(Config{Enabled: true, Mode: ModeEnforce, DirtyIPCooldown: 30 * time.Minute, StateFile: t.TempDir() + "/l.json"}, nodes, nil, nil)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	controller.ledger.now = func() time.Time { return now }
	cred := accountdomain.Credential{ID: 9, Provider: accountdomain.ProviderBuild}
	controller.OnMissingThinking(context.Background(), cred, 214, "node:214")
	if _, err := controller.Admit(context.Background(), accountdomain.Credential{ID: 10, Provider: accountdomain.ProviderBuild}, nil); !errors.Is(err, ErrNoEligibleExitIP) {
		t.Fatalf("cooled node must stay ineligible, err=%v", err)
	}
}

func TestIdleStreamCoolsIPWithoutCountingTowardK(t *testing.T) {
	nodes := staticNodes{
		{ID: 1, Enabled: true, ExitIP: "10.0.0.1"},
		{ID: 2, Enabled: true, ExitIP: "10.0.0.2"},
	}
	controller := New(Config{Enabled: true, Mode: ModeEnforce, AccountIPFailThreshold: 2, DirtyIPCooldown: 30 * time.Minute, StateFile: t.TempDir() + "/l.json"}, nodes, nil, nil)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	controller.ledger.now = func() time.Time { return now }
	cred := accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}
	controller.OnIdleStream(cred, 1, "10.0.0.1")
	controller.OnIdleStream(cred, 2, "10.0.0.2")
	if controller.AccountQuarantined(7) {
		t.Fatal("idle streams must not quarantine")
	}
	controller.OnMissingThinking(context.Background(), cred, 1, "10.0.0.1")
	if controller.AccountQuarantined(7) {
		t.Fatal("one withhold plus idles must not reach K=2")
	}
	controller.OnMissingThinking(context.Background(), cred, 2, "10.0.0.2")
	if !controller.AccountQuarantined(7) {
		t.Fatal("two withholds must quarantine")
	}
}

func TestProxyFailureCoolsIPWithoutQuarantine(t *testing.T) {
	nodes := staticNodes{
		{ID: 1, Enabled: true, ExitIP: "10.0.0.1"},
		{ID: 2, Enabled: true, ExitIP: "10.0.0.2"},
	}
	controller := New(Config{Enabled: true, Mode: ModeEnforce, AccountIPFailThreshold: 2, StateFile: t.TempDir() + "/l.json"}, nodes, nil, nil)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	controller.ledger.now = func() time.Time { return now }
	cred := accountdomain.Credential{ID: 9, Provider: accountdomain.ProviderBuild}
	controller.OnProxyFailure(cred, 1, "10.0.0.1")
	controller.OnProxyFailure(cred, 2, "10.0.0.2")
	if controller.AccountQuarantined(9) {
		t.Fatal("proxy failures must not quarantine")
	}
	if _, err := controller.Admit(context.Background(), cred, nil); !errors.Is(err, ErrNoEligibleExitIP) {
		t.Fatalf("proxy-down IPs should be ineligible: %v", err)
	}
	controller.ledger.now = func() time.Time { return now.Add(3 * time.Minute) }
	if _, err := controller.Admit(context.Background(), cred, nil); err != nil {
		t.Fatalf("proxy-down cooldown must expire: %v", err)
	}
}

func densityOf(controller *Controller, ip string, window time.Duration, now time.Time) int {
	controller.ledger.mu.Lock()
	defer controller.ledger.mu.Unlock()
	return controller.ledger.densityCount(controller.ledger.ip(ip), window, now)
}

func TestAdmitOccupiesDensityBeforeRequestCompletes(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nodes := staticNodes{
		{ID: 1, Enabled: true, ExitIP: "10.0.0.1"},
		{ID: 2, Enabled: true, ExitIP: "10.0.0.2"},
	}
	window := 15 * time.Minute
	controller := New(Config{
		Enabled: true, Mode: ModeEnforce, DensityWindow: window, DensityMaxAccounts: 1,
		StateFile: t.TempDir() + "/l.json",
	}, nodes, nil, nil)
	controller.ledger.now = func() time.Time { return now }

	first, err := controller.Admit(context.Background(), accountdomain.Credential{ID: 1, Provider: accountdomain.ProviderBuild}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := controller.Admit(context.Background(), accountdomain.Credential{ID: 2, Provider: accountdomain.ProviderBuild}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first == 0 || second == 0 || first == second {
		t.Fatalf("unbound accounts must land on different IPs, first=%d second=%d", first, second)
	}
	if densityOf(controller, "10.0.0.1", window, now) != 1 || densityOf(controller, "10.0.0.2", window, now) != 1 {
		t.Fatalf("admit must occupy both IPs before completion, d1=%d d2=%d", densityOf(controller, "10.0.0.1", window, now), densityOf(controller, "10.0.0.2", window, now))
	}
	if _, err := controller.Admit(context.Background(), accountdomain.Credential{ID: 3, Provider: accountdomain.ProviderBuild}, nil); !errors.Is(err, ErrNoEligibleExitIP) {
		t.Fatalf("all IPs at cap must 503, err=%v", err)
	}
}

func TestAdmitSameAccountDoesNotConsumeSecondDensitySlot(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nodes := staticNodes{{ID: 1, Enabled: true, ExitIP: "10.0.0.1"}}
	window := 15 * time.Minute
	controller := New(Config{
		Enabled: true, Mode: ModeEnforce, DensityWindow: window, DensityMaxAccounts: 1,
		StateFile: t.TempDir() + "/l.json",
	}, nodes, nil, nil)
	controller.ledger.now = func() time.Time { return now }
	cred := accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild, EgressNodeID: 1}
	if got, err := controller.Admit(context.Background(), cred, nil); err != nil || got != 0 {
		t.Fatalf("first binding admit override=%d err=%v", got, err)
	}
	if got, err := controller.Admit(context.Background(), cred, nil); err != nil || got != 0 {
		t.Fatalf("same account must keep binding, override=%d err=%v", got, err)
	}
	if got := densityOf(controller, "10.0.0.1", window, now); got != 1 {
		t.Fatalf("density=%d, want 1", got)
	}
}

func TestAdmitBoundNodeFullFailsOverWithoutUnbinding(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nodes := staticNodes{
		{ID: 1, Enabled: true, ExitIP: "10.0.0.1"},
		{ID: 2, Enabled: true, ExitIP: "10.0.0.2"},
	}
	window := 15 * time.Minute
	controller := New(Config{
		Enabled: true, Mode: ModeEnforce, DensityWindow: window, DensityMaxAccounts: 1,
		StateFile: t.TempDir() + "/l.json",
	}, nodes, nil, nil)
	controller.ledger.now = func() time.Time { return now }
	owner := accountdomain.Credential{ID: 1, Provider: accountdomain.ProviderBuild, EgressNodeID: 1}
	if got, err := controller.Admit(context.Background(), owner, nil); err != nil || got != 0 {
		t.Fatalf("owner binding override=%d err=%v", got, err)
	}
	other := accountdomain.Credential{ID: 2, Provider: accountdomain.ProviderBuild, EgressNodeID: 1}
	got, err := controller.Admit(context.Background(), other, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("full bound IP must fail over for this request, override=%d", got)
	}
	if owner.EgressNodeID != 1 || other.EgressNodeID != 1 {
		t.Fatal("admit must not rewrite stored bindings")
	}
}

func TestAdmitConcurrentAccountsRespectDensityCap(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nodes := staticNodes{{ID: 1, Enabled: true, ExitIP: "10.0.0.1"}}
	window := 15 * time.Minute
	controller := New(Config{
		Enabled: true, Mode: ModeEnforce, DensityWindow: window, DensityMaxAccounts: 1,
		StateFile: t.TempDir() + "/l.json",
	}, nodes, nil, nil)
	controller.ledger.now = func() time.Time { return now }

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = controller.Admit(context.Background(), accountdomain.Credential{ID: uint64(i + 1), Provider: accountdomain.ProviderBuild}, nil)
		}(i)
	}
	wg.Wait()
	accepted, rejected := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrNoEligibleExitIP):
			rejected++
		default:
			t.Fatalf("unexpected admit error: %v", err)
		}
	}
	if accepted != 1 || rejected != 7 {
		t.Fatalf("accepted=%d rejected=%d, want 1 and 7", accepted, rejected)
	}
	if got := densityOf(controller, "10.0.0.1", window, now); got != 1 {
		t.Fatalf("density=%d, want 1", got)
	}
}
