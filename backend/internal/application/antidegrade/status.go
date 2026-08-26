package antidegrade

import (
	"context"
	"sort"
	"strings"
	"time"
)

const maxStatusEvents = 80

// Status is the admin snapshot of ExitIP load, quarantines, and recent events.
type Status struct {
	Config      Config
	IPs         []IPStatus
	Quarantined []AccountStatus
	Events      []EventStatus
}

type IPStatus struct {
	ExitIP                string
	NodeIDs               []uint64
	NodeNames             []string
	AccountIDs            []uint64
	AccountCount          int
	AccountLimit          int
	Cooling               bool
	CooldownUntil         time.Time
	CooldownReason        string
	OperatorOverrideUntil time.Time
	Score                 float64
}

type AccountStatus struct {
	ID               uint64
	FailedIPs        []string
	QuarantineUntil  time.Time
	QuarantineReason string
}

type EventStatus struct {
	At        time.Time
	Success   bool
	AccountID uint64
	ExitIP    string
}

func (c *Controller) Snapshot(ctx context.Context) Status {
	if c == nil {
		return Status{}
	}
	cfg := c.config().Normalize()
	now := c.ledger.now()
	nodes, _ := c.lookup(ctx)
	nodeName := map[uint64]string{}
	for _, node := range nodes {
		nodeName[node.ID] = node.Name
	}

	c.ledger.mu.Lock()
	defer c.ledger.mu.Unlock()

	ips := make([]IPStatus, 0, len(c.ledger.state.IPs))
	events := make([]EventStatus, 0)
	for _, key := range sortedKeys(c.ledger.state.IPs) {
		state := c.ledger.state.IPs[key]
		if state == nil {
			continue
		}
		accountIDs := c.ledger.windowAccounts(state, cfg.DensityWindow, now)
		cooldownActive := !state.CooldownUntil.IsZero() && now.Before(state.CooldownUntil)
		overrideActive := !state.OperatorOverrideUntil.IsZero() && now.Before(state.OperatorOverrideUntil)
		if len(accountIDs) == 0 && !cooldownActive && !overrideActive {
			continue
		}
		names := make([]string, 0, len(state.NodeIDs))
		for _, id := range state.NodeIDs {
			if name := strings.TrimSpace(nodeName[id]); name != "" {
				names = append(names, name)
			}
		}
		ips = append(ips, IPStatus{
			ExitIP:                state.ExitIP,
			NodeIDs:               append([]uint64(nil), state.NodeIDs...),
			NodeNames:             names,
			AccountIDs:            accountIDs,
			AccountCount:          len(accountIDs),
			AccountLimit:          cfg.DensityMaxAccounts,
			Cooling:               cooldownActive && !overrideActive,
			CooldownUntil:         state.CooldownUntil,
			CooldownReason:        state.CooldownReason,
			OperatorOverrideUntil: state.OperatorOverrideUntil,
			Score:                 c.ledger.score(state, cfg.ScorePrior),
		})
		for _, event := range state.Events {
			events = append(events, EventStatus{At: event.At, Success: event.Success, AccountID: event.AccountID, ExitIP: state.ExitIP})
		}
	}
	sort.Slice(ips, func(i, j int) bool {
		if ips[i].Cooling != ips[j].Cooling {
			return ips[i].Cooling
		}
		if ips[i].AccountCount != ips[j].AccountCount {
			return ips[i].AccountCount > ips[j].AccountCount
		}
		return ips[i].ExitIP < ips[j].ExitIP
	})
	sort.Slice(events, func(i, j int) bool { return events[i].At.After(events[j].At) })
	if len(events) > maxStatusEvents {
		events = events[:maxStatusEvents]
	}

	quarantined := make([]AccountStatus, 0)
	for _, id := range sortedAccountIDs(c.ledger.state.Accounts) {
		current := c.ledger.state.Accounts[id]
		if !c.ledger.accountQuarantined(id, now) {
			continue
		}
		failed := make([]string, 0, len(current.IPs))
		for _, item := range current.IPs {
			if item.ExitIP != "" {
				failed = append(failed, item.ExitIP)
			}
		}
		quarantined = append(quarantined, AccountStatus{
			ID: id, FailedIPs: failed, QuarantineUntil: current.QuarantineUntil, QuarantineReason: current.QuarantineReason,
		})
	}
	return Status{Config: cfg, IPs: ips, Quarantined: quarantined, Events: events}
}

func (c *Controller) ClearIP(_ context.Context, exitIP string) {
	if c == nil {
		return
	}
	key := strings.TrimSpace(exitIP)
	if key == "" {
		return
	}
	cfg := c.config()
	now := c.ledger.now()
	c.ledger.mu.Lock()
	if _, ok := c.ledger.state.IPs[key]; !ok {
		c.ledger.mu.Unlock()
		return
	}
	c.ledger.clearCooldown(key, cfg.OperatorOverride, now)
	c.ledger.mu.Unlock()
	_ = c.ledger.persist()
	c.logger.Info("antidegrade_manual_open", "exit_ip", key)
}

func sortedAccountIDs(values map[uint64]accountFail) []uint64 {
	ids := make([]uint64, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
