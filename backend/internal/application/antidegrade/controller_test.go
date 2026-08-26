package antidegrade

import (
	"context"
	"errors"
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
