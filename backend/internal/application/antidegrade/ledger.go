package antidegrade

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	reasonDirtyIP    = "dirty_ip"
	reasonFarm       = "farm"
	reasonDensity    = "density"
	reasonQuarantine = "k_ip_no_streamed_thinking"
	priorWeight      = 5.0
	maxWindowHits    = 256
	maxEvents        = 80
	maxFailedIPs     = 16
	accountFailTTL   = time.Hour
)

type accountHit struct {
	AccountID uint64    `json:"accountId"`
	At        time.Time `json:"at"`
}

type qualityEvent struct {
	Success   bool      `json:"success"`
	AccountID uint64    `json:"accountId"`
	At        time.Time `json:"at"`
}

type ipState struct {
	ExitIP                string         `json:"exitIp"`
	NodeIDs               []uint64       `json:"nodeIds,omitempty"`
	Window                []accountHit   `json:"window,omitempty"`
	Events                []qualityEvent `json:"events,omitempty"`
	CooldownUntil         time.Time      `json:"cooldownUntil,omitempty"`
	CooldownReason        string         `json:"cooldownReason,omitempty"`
	OperatorOverrideUntil time.Time      `json:"operatorOverrideUntil,omitempty"`
}

type accountFail struct {
	IPs              []ipFail  `json:"ips,omitempty"`
	QuarantineUntil  time.Time `json:"quarantineUntil,omitempty"`
	QuarantineReason string    `json:"quarantineReason,omitempty"`
}

type ipFail struct {
	ExitIP string    `json:"exitIp"`
	At     time.Time `json:"at"`
}

type snapshot struct {
	IPs      map[string]*ipState    `json:"ips"`
	Accounts map[uint64]accountFail `json:"accounts"`
}

type ledger struct {
	mu    sync.Mutex
	path  string
	now   func() time.Time
	state snapshot
	dirty bool
}

func newLedger(path string) *ledger {
	return &ledger{
		path: path,
		now:  time.Now,
		state: snapshot{
			IPs:      map[string]*ipState{},
			Accounts: map[uint64]accountFail{},
		},
	}
}

func (l *ledger) load() error {
	if l.path == "" {
		return nil
	}
	data, err := os.ReadFile(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var next snapshot
	if err := json.Unmarshal(data, &next); err != nil {
		return err
	}
	if next.IPs == nil {
		next.IPs = map[string]*ipState{}
	}
	if next.Accounts == nil {
		next.Accounts = map[uint64]accountFail{}
	}
	l.mu.Lock()
	l.state = next
	l.dirty = false
	l.mu.Unlock()
	return nil
}

func (l *ledger) persist() error {
	l.mu.Lock()
	if !l.dirty || l.path == "" {
		l.mu.Unlock()
		return nil
	}
	data, err := json.MarshalIndent(l.state, "", "  ")
	if err != nil {
		l.mu.Unlock()
		return err
	}
	l.dirty = false
	path := l.path
	l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".antidegrade-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func (l *ledger) ip(key string) *ipState {
	current := l.state.IPs[key]
	if current == nil {
		current = &ipState{ExitIP: key}
		l.state.IPs[key] = current
	}
	return current
}

func (l *ledger) rememberNode(key string, nodeID uint64) {
	if nodeID == 0 {
		return
	}
	state := l.ip(key)
	for _, existing := range state.NodeIDs {
		if existing == nodeID {
			return
		}
	}
	state.NodeIDs = append(state.NodeIDs, nodeID)
	l.dirty = true
}

func (l *ledger) densityCount(state *ipState, window time.Duration, now time.Time) int {
	cutoff := now.Add(-window)
	seen := map[uint64]struct{}{}
	kept := state.Window[:0]
	for _, hit := range state.Window {
		if hit.At.Before(cutoff) || hit.AccountID == 0 {
			continue
		}
		kept = append(kept, hit)
		seen[hit.AccountID] = struct{}{}
	}
	state.Window = kept
	return len(seen)
}

func (l *ledger) windowAccounts(state *ipState, window time.Duration, now time.Time) []uint64 {
	if state == nil {
		return nil
	}
	cutoff := now.Add(-window)
	seen := map[uint64]struct{}{}
	ids := make([]uint64, 0)
	for _, hit := range state.Window {
		if hit.At.Before(cutoff) || hit.AccountID == 0 {
			continue
		}
		if _, ok := seen[hit.AccountID]; ok {
			continue
		}
		seen[hit.AccountID] = struct{}{}
		ids = append(ids, hit.AccountID)
	}
	return ids
}

func (l *ledger) accountInWindow(state *ipState, accountID uint64, window time.Duration, now time.Time) bool {
	cutoff := now.Add(-window)
	for _, hit := range state.Window {
		if hit.AccountID == accountID && !hit.At.Before(cutoff) {
			return true
		}
	}
	return false
}

func (l *ledger) noteWindow(key string, accountID uint64, window time.Duration, now time.Time) {
	if accountID == 0 {
		return
	}
	state := l.ip(key)
	if l.accountInWindow(state, accountID, window, now) {
		return
	}
	state.Window = append(state.Window, accountHit{AccountID: accountID, At: now})
	if len(state.Window) > maxWindowHits {
		state.Window = state.Window[len(state.Window)-maxWindowHits:]
	}
	l.dirty = true
}

func (l *ledger) cooling(state *ipState, now time.Time) bool {
	if state == nil {
		return false
	}
	if !state.OperatorOverrideUntil.IsZero() && now.Before(state.OperatorOverrideUntil) {
		return false
	}
	return !state.CooldownUntil.IsZero() && now.Before(state.CooldownUntil)
}

func (l *ledger) cool(key, reason string, until time.Time) {
	state := l.ip(key)
	if until.After(state.CooldownUntil) {
		state.CooldownUntil = until
		state.CooldownReason = reason
		l.dirty = true
	}
}

func (l *ledger) liftCooldown(key string) {
	if key == "" {
		return
	}
	state, ok := l.state.IPs[key]
	if !ok || state == nil {
		return
	}
	state.CooldownUntil = time.Time{}
	state.CooldownReason = ""
	l.dirty = true
}

func (l *ledger) accountQuarantined(accountID uint64, now time.Time) bool {
	if accountID == 0 {
		return false
	}
	current, ok := l.state.Accounts[accountID]
	if !ok {
		return false
	}
	return !current.QuarantineUntil.IsZero() && now.Before(current.QuarantineUntil)
}

func (l *ledger) quarantineAccount(accountID uint64, until time.Time, reason string) []string {
	if accountID == 0 {
		return nil
	}
	current := l.state.Accounts[accountID]
	current.QuarantineUntil = until
	current.QuarantineReason = reason
	lifted := make([]string, 0)
	// Keep the first failed ExitIP cooled (may still be a dirty IP). Lift the
	// later IPs that were only used to prove the failure follows the account.
	for i, item := range current.IPs {
		if i == 0 || item.ExitIP == "" {
			continue
		}
		l.liftCooldown(item.ExitIP)
		lifted = append(lifted, item.ExitIP)
	}
	l.state.Accounts[accountID] = current
	l.dirty = true
	return lifted
}

func (l *ledger) clearAccountQuarantine(accountID uint64) {
	if accountID == 0 {
		return
	}
	current, ok := l.state.Accounts[accountID]
	if !ok {
		return
	}
	current.QuarantineUntil = time.Time{}
	current.QuarantineReason = ""
	current.IPs = nil
	l.state.Accounts[accountID] = current
	l.dirty = true
}

func parseNodeKey(key string) (uint64, bool) {
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(key, "node:") {
		return 0, false
	}
	id, err := strconv.ParseUint(strings.TrimPrefix(key, "node:"), 10, 64)
	return id, err == nil && id != 0
}

func (l *ledger) lastIPForNode(nodeID uint64) string {
	if nodeID == 0 {
		return ""
	}
	for _, key := range sortedKeys(l.state.IPs) {
		if strings.HasPrefix(key, "node:") {
			continue
		}
		state := l.state.IPs[key]
		if state == nil {
			continue
		}
		for _, id := range state.NodeIDs {
			if id != nodeID {
				continue
			}
			if ip := strings.TrimSpace(state.ExitIP); ip != "" && !strings.HasPrefix(ip, "node:") {
				return ip
			}
			if !strings.HasPrefix(key, "node:") {
				return key
			}
		}
	}
	return ""
}

func (l *ledger) rekeyFromNodes(nodes []Node) {
	byID := make(map[uint64]Node, len(nodes))
	for _, node := range nodes {
		if node.ID != 0 {
			byID[node.ID] = node
		}
	}
	for _, key := range sortedKeys(l.state.IPs) {
		state := l.state.IPs[key]
		if state == nil {
			continue
		}
		ids := append([]uint64(nil), state.NodeIDs...)
		if id, ok := parseNodeKey(key); ok {
			ids = append(ids, id)
		}
		for _, id := range ids {
			node, ok := byID[id]
			if !ok {
				continue
			}
			ip := strings.TrimSpace(node.ExitIP)
			if ip == "" || strings.HasPrefix(ip, "node:") || ip == key {
				continue
			}
			l.mergeIP(key, ip, id)
			break
		}
	}
}

func (l *ledger) mergeIP(from, to string, nodeID uint64) {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" || to == "" || from == to {
		return
	}
	src := l.state.IPs[from]
	if src == nil {
		l.rememberNode(to, nodeID)
		return
	}
	dst := l.ip(to)
	for _, id := range src.NodeIDs {
		already := false
		for _, existing := range dst.NodeIDs {
			if existing == id {
				already = true
				break
			}
		}
		if !already {
			dst.NodeIDs = append(dst.NodeIDs, id)
		}
	}
	if nodeID != 0 {
		already := false
		for _, existing := range dst.NodeIDs {
			if existing == nodeID {
				already = true
				break
			}
		}
		if !already {
			dst.NodeIDs = append(dst.NodeIDs, nodeID)
		}
	}
	dst.Window = append(dst.Window, src.Window...)
	dst.Events = append(dst.Events, src.Events...)
	if src.CooldownUntil.After(dst.CooldownUntil) {
		dst.CooldownUntil = src.CooldownUntil
		dst.CooldownReason = src.CooldownReason
	}
	if src.OperatorOverrideUntil.After(dst.OperatorOverrideUntil) {
		dst.OperatorOverrideUntil = src.OperatorOverrideUntil
	}
	delete(l.state.IPs, from)
	for id, acc := range l.state.Accounts {
		changed := false
		for i, item := range acc.IPs {
			if item.ExitIP == from {
				acc.IPs[i].ExitIP = to
				changed = true
			}
		}
		if changed {
			l.state.Accounts[id] = acc
		}
	}
	l.dirty = true
}

func (l *ledger) clearCooldown(key string, override time.Duration, now time.Time) {
	state := l.ip(key)
	state.CooldownUntil = time.Time{}
	state.CooldownReason = ""
	if override > 0 {
		state.OperatorOverrideUntil = now.Add(override)
	}
	l.dirty = true
}

func (l *ledger) clearCooldownForNode(nodeID uint64, override time.Duration, now time.Time) {
	if nodeID == 0 {
		return
	}
	for key, state := range l.state.IPs {
		for _, id := range state.NodeIDs {
			if id == nodeID {
				l.clearCooldown(key, override, now)
				break
			}
		}
	}
}

func (l *ledger) recordEvent(key string, accountID uint64, success bool, now time.Time) {
	state := l.ip(key)
	state.Events = append(state.Events, qualityEvent{Success: success, AccountID: accountID, At: now})
	if len(state.Events) > maxEvents {
		state.Events = state.Events[len(state.Events)-maxEvents:]
	}
	l.dirty = true
}

func (l *ledger) score(state *ipState, prior float64) float64 {
	if state == nil {
		return prior
	}
	successes := 0.0
	samples := 0.0
	for _, event := range state.Events {
		samples++
		if event.Success {
			successes++
		}
	}
	return (priorWeight*prior + successes) / (priorWeight + samples)
}

func (l *ledger) noteAccountFail(accountID uint64, exitIP string, now time.Time) int {
	if accountID == 0 || exitIP == "" {
		return 0
	}
	current := l.state.Accounts[accountID]
	cutoff := now.Add(-accountFailTTL)
	kept := current.IPs[:0]
	seen := map[string]struct{}{}
	for _, item := range current.IPs {
		if item.At.Before(cutoff) {
			continue
		}
		kept = append(kept, item)
		seen[item.ExitIP] = struct{}{}
	}
	if _, ok := seen[exitIP]; !ok {
		kept = append(kept, ipFail{ExitIP: exitIP, At: now})
		seen[exitIP] = struct{}{}
	}
	if len(kept) > maxFailedIPs {
		kept = kept[len(kept)-maxFailedIPs:]
	}
	current.IPs = kept
	l.state.Accounts[accountID] = current
	l.dirty = true
	return len(seen)
}

func (l *ledger) distinctFailCount(accountID uint64, now time.Time) int {
	current := l.state.Accounts[accountID]
	cutoff := now.Add(-accountFailTTL)
	seen := map[string]struct{}{}
	for _, item := range current.IPs {
		if item.At.Before(cutoff) {
			continue
		}
		seen[item.ExitIP] = struct{}{}
	}
	return len(seen)
}

func sortedKeys(values map[string]*ipState) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
