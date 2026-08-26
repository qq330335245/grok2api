package antidegrade

import (
	"context"
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

func TestMissingThinkingDoesNotBanCleanBotFlag(t *testing.T) {
	bans := &banRecorder{}
	controller := New(Config{Enabled: true, Mode: ModeEnforce, AccountIPFailThreshold: 2, StateFile: t.TempDir() + "/l.json"}, nil, bans, nil)
	cred := accountdomain.Credential{ID: 8, BuildBotFlagSource: 0}
	controller.OnMissingThinking(context.Background(), cred, 1, "10.0.0.1")
	controller.OnMissingThinking(context.Background(), cred, 2, "10.0.0.2")
	if len(bans.ids) != 0 {
		t.Fatalf("banned clean account: %v", bans.ids)
	}
}
