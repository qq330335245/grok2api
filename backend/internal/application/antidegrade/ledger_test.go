package antidegrade

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDensityCountsDistinctAccountsInWindow(t *testing.T) {
	ledger := newLedger("")
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	ledger.now = func() time.Time { return now }
	key := "203.0.113.8"
	ledger.noteWindow(key, 1, 15*time.Minute, now)
	ledger.noteWindow(key, 1, 15*time.Minute, now.Add(time.Minute))
	ledger.noteWindow(key, 2, 15*time.Minute, now.Add(2*time.Minute))
	if got := ledger.densityCount(ledger.ip(key), 15*time.Minute, now.Add(3*time.Minute)); got != 2 {
		t.Fatalf("density = %d, want 2", got)
	}
	if got := ledger.densityCount(ledger.ip(key), 15*time.Minute, now.Add(20*time.Minute)); got != 0 {
		t.Fatalf("expired density = %d", got)
	}
}

func TestCoolingHonorsOperatorOverride(t *testing.T) {
	ledger := newLedger("")
	now := time.Now().UTC()
	key := "198.51.100.2"
	ledger.cool(key, reasonDirtyIP, now.Add(30*time.Minute))
	if !ledger.cooling(ledger.ip(key), now) {
		t.Fatal("expected cooling")
	}
	ledger.clearCooldown(key, 10*time.Minute, now)
	if ledger.cooling(ledger.ip(key), now) {
		t.Fatal("operator override should skip cooling")
	}
	if ledger.cooling(ledger.ip(key), now.Add(11*time.Minute)) {
		t.Fatal("override expired and cooldown was cleared")
	}
}

func TestAccountFailCountsDistinctIPs(t *testing.T) {
	ledger := newLedger("")
	now := time.Now().UTC()
	if got := ledger.noteAccountFail(9, "1.1.1.1", now); got != 1 {
		t.Fatalf("first = %d", got)
	}
	if got := ledger.noteAccountFail(9, "1.1.1.1", now.Add(time.Minute)); got != 1 {
		t.Fatalf("same ip = %d", got)
	}
	if got := ledger.noteAccountFail(9, "8.8.8.8", now.Add(2*time.Minute)); got != 2 {
		t.Fatalf("second ip = %d", got)
	}
}

func TestLedgerRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	first := newLedger(path)
	now := time.Now().UTC()
	first.noteWindow("203.0.113.9", 4, 15*time.Minute, now)
	first.cool("203.0.113.9", reasonDirtyIP, now.Add(time.Hour))
	if err := first.persist(); err != nil {
		t.Fatal(err)
	}
	second := newLedger(path)
	if err := second.load(); err != nil {
		t.Fatal(err)
	}
	if !second.cooling(second.ip("203.0.113.9"), now.Add(time.Minute)) {
		t.Fatal("expected persisted cooldown")
	}
}
