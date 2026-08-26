package antidegrade

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

var ErrNoEligibleExitIP = errors.New("没有可用的干净出口 IP")

// Node is a scheduling snapshot of one Build egress node.
type Node struct {
	ID      uint64
	Enabled bool
	ExitIP  string
	Name    string
}

type NodeSource interface {
	ListBuildNodes(context.Context) ([]Node, error)
}

type AccountDisabler interface {
	Disable(context.Context, uint64) error
}

// Controller is the request-path ExitIP ledger: density, cooldown, score, delayed botflag=2.
type Controller struct {
	mu       sync.Mutex
	cfg      Config
	ledger   *ledger
	nodes    NodeSource
	accounts AccountDisabler
	logger   *slog.Logger
	rng      *rand.Rand
}

func New(cfg Config, nodes NodeSource, accounts AccountDisabler, logger *slog.Logger) *Controller {
	cfg = cfg.Normalize()
	if logger == nil {
		logger = slog.Default()
	}
	controller := &Controller{
		cfg:      cfg,
		ledger:   newLedger(cfg.StateFile),
		nodes:    nodes,
		accounts: accounts,
		logger:   logger,
		rng:      rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	if err := controller.ledger.load(); err != nil {
		logger.Warn("antidegrade_ledger_load_failed", "path", cfg.StateFile, "error", err)
	}
	return controller
}

func (c *Controller) Enabled() bool { return c != nil && c.cfg.Enabled }
func (c *Controller) Enforce() bool { return c != nil && c.cfg.Enforce() }
func (c *Controller) MaxIPRetries() int {
	if c == nil {
		return 0
	}
	return c.cfg.Normalize().MaxIPRetries
}
func (c *Controller) ThinkingMinOutput() int64 {
	if c == nil {
		return 32
	}
	return c.cfg.Normalize().ThinkingMinOutput
}

func (c *Controller) Update(cfg Config) {
	if c == nil {
		return
	}
	cfg = cfg.Normalize()
	c.mu.Lock()
	c.cfg = cfg
	if c.ledger.path != cfg.StateFile {
		c.ledger.path = cfg.StateFile
	}
	c.mu.Unlock()
}

func (c *Controller) config() Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

func exitKey(node Node) string {
	ip := strings.TrimSpace(node.ExitIP)
	if ip != "" {
		return ip
	}
	if node.ID == 0 {
		return ""
	}
	return fmt.Sprintf("node:%d", node.ID)
}

func (c *Controller) lookup(ctx context.Context) ([]Node, error) {
	if c.nodes == nil {
		return nil, nil
	}
	return c.nodes.ListBuildNodes(ctx)
}

func (c *Controller) nodeByID(nodes []Node, id uint64) (Node, bool) {
	for _, node := range nodes {
		if node.ID == id {
			return node, true
		}
	}
	return Node{}, false
}

func (c *Controller) allowed(cfg Config, node Node, accountID uint64, excluded map[uint64]bool, now time.Time) bool {
	if node.ID == 0 || !node.Enabled || excluded[node.ID] {
		return false
	}
	key := exitKey(node)
	if key == "" {
		return false
	}
	c.ledger.mu.Lock()
	defer c.ledger.mu.Unlock()
	c.ledger.rememberNode(key, node.ID)
	state := c.ledger.ip(key)
	if c.ledger.cooling(state, now) {
		return false
	}
	if c.ledger.accountInWindow(state, accountID, cfg.DensityWindow, now) {
		return true
	}
	return c.ledger.densityCount(state, cfg.DensityWindow, now) < cfg.DensityMaxAccounts
}

func (c *Controller) pick(cfg Config, nodes []Node, accountID, preferred uint64, excluded map[uint64]bool, now time.Time) (uint64, error) {
	type candidate struct {
		node  Node
		score float64
	}
	var options []candidate
	c.ledger.mu.Lock()
	for _, node := range nodes {
		if node.ID == 0 || !node.Enabled || excluded[node.ID] {
			continue
		}
		key := exitKey(node)
		if key == "" {
			continue
		}
		c.ledger.rememberNode(key, node.ID)
		state := c.ledger.ip(key)
		if c.ledger.cooling(state, now) {
			continue
		}
		if !c.ledger.accountInWindow(state, accountID, cfg.DensityWindow, now) && c.ledger.densityCount(state, cfg.DensityWindow, now) >= cfg.DensityMaxAccounts {
			continue
		}
		options = append(options, candidate{node: node, score: c.ledger.score(state, cfg.ScorePrior)})
	}
	c.ledger.mu.Unlock()
	if len(options) == 0 {
		return 0, ErrNoEligibleExitIP
	}
	if preferred != 0 {
		for _, option := range options {
			if option.node.ID == preferred {
				return preferred, nil
			}
		}
	}
	if cfg.ExploreRatio > 0 && c.rng.Float64() < cfg.ExploreRatio {
		return options[c.rng.Intn(len(options))].node.ID, nil
	}
	best := options[0]
	for _, option := range options[1:] {
		if option.score > best.score {
			best = option
		}
	}
	return best.node.ID, nil
}

// Admit returns a ForcedEgress node ID. Zero means "use the account binding".
func (c *Controller) Admit(ctx context.Context, credential accountdomain.Credential, excluded map[uint64]bool) (uint64, error) {
	if c == nil || !c.Enforce() {
		return 0, nil
	}
	cfg := c.config()
	nodes, err := c.lookup(ctx)
	if err != nil {
		return 0, err
	}
	now := c.ledger.now()
	preferred := credential.EgressNodeID
	if preferred != 0 {
		if node, ok := c.nodeByID(nodes, preferred); ok && c.allowed(cfg, node, credential.ID, excluded, now) {
			return 0, nil
		}
	}
	picked, err := c.pick(cfg, nodes, credential.ID, 0, excluded, now)
	if err != nil {
		return 0, err
	}
	if picked == preferred {
		return 0, nil
	}
	return picked, nil
}

func (c *Controller) OnSuccess(accountID, nodeID uint64, exitIP string) {
	if c == nil || !c.Enabled() || nodeID == 0 && exitIP == "" {
		return
	}
	cfg := c.config()
	key := strings.TrimSpace(exitIP)
	if key == "" {
		key = fmt.Sprintf("node:%d", nodeID)
	}
	now := c.ledger.now()
	c.ledger.mu.Lock()
	c.ledger.rememberNode(key, nodeID)
	c.ledger.noteWindow(key, accountID, cfg.DensityWindow, now)
	c.ledger.recordEvent(key, accountID, true, now)
	c.ledger.mu.Unlock()
	_ = c.ledger.persist()
}

func (c *Controller) OnMissingThinking(ctx context.Context, credential accountdomain.Credential, nodeID uint64, exitIP string) {
	if c == nil || !c.Enabled() {
		return
	}
	cfg := c.config()
	key := strings.TrimSpace(exitIP)
	if key == "" && nodeID != 0 {
		key = fmt.Sprintf("node:%d", nodeID)
	}
	if key == "" {
		return
	}
	now := c.ledger.now()
	c.ledger.mu.Lock()
	c.ledger.rememberNode(key, nodeID)
	c.ledger.noteWindow(key, credential.ID, cfg.DensityWindow, now)
	c.ledger.recordEvent(key, credential.ID, false, now)
	distinct := 0
	if cfg.Enforce() {
		c.ledger.cool(key, reasonDirtyIP, now.Add(cfg.DirtyIPCooldown))
		distinct = c.ledger.noteAccountFail(credential.ID, key, now)
	}
	c.ledger.mu.Unlock()
	_ = c.ledger.persist()
	c.logger.Info("antidegrade_missing_thinking", "account_id", credential.ID, "node_id", nodeID, "exit_ip", key, "distinct_fail_ips", distinct)
	if !cfg.Enforce() || distinct < cfg.AccountIPFailThreshold {
		return
	}
	c.maybeBanAccount(ctx, credential, key, now)
}

func (c *Controller) maybeBanAccount(ctx context.Context, credential accountdomain.Credential, lastIP string, now time.Time) {
	if credential.BuildBotFlagSource != 2 {
		c.logger.Info("antidegrade_botflag_skip", "account_id", credential.ID, "bot_flag_source", credential.BuildBotFlagSource, "reason", "not_flag_2")
		return
	}
	cfg := c.config()
	c.ledger.mu.Lock()
	c.ledger.cool(lastIP, reasonFarm, now.Add(cfg.FarmIPCooldown))
	c.ledger.mu.Unlock()
	_ = c.ledger.persist()
	if c.accounts == nil {
		c.logger.Warn("antidegrade_account_ban_skipped", "account_id", credential.ID, "reason", "no_disabler")
		return
	}
	if err := c.accounts.Disable(ctx, credential.ID); err != nil {
		c.logger.Error("antidegrade_account_ban_failed", "account_id", credential.ID, "error", err)
		return
	}
	c.logger.Info("antidegrade_account_banned", "account_id", credential.ID, "bot_flag_source", 2, "exit_ip", lastIP)
}

func (c *Controller) ClearForNode(ctx context.Context, nodeID uint64) {
	if c == nil || nodeID == 0 {
		return
	}
	cfg := c.config()
	now := c.ledger.now()
	if nodes, err := c.lookup(ctx); err == nil {
		if node, ok := c.nodeByID(nodes, nodeID); ok {
			key := exitKey(node)
			if key != "" {
				c.ledger.mu.Lock()
				c.ledger.rememberNode(key, nodeID)
				c.ledger.clearCooldown(key, cfg.OperatorOverride, now)
				c.ledger.mu.Unlock()
				_ = c.ledger.persist()
				c.logger.Info("antidegrade_manual_open", "node_id", nodeID, "exit_ip", key)
				return
			}
		}
	}
	c.ledger.mu.Lock()
	c.ledger.clearCooldownForNode(nodeID, cfg.OperatorOverride, now)
	c.ledger.mu.Unlock()
	_ = c.ledger.persist()
	c.logger.Info("antidegrade_manual_open", "node_id", nodeID)
}

func (c *Controller) ExitIP(ctx context.Context, nodeID uint64) string {
	if c == nil || nodeID == 0 {
		return ""
	}
	nodes, err := c.lookup(ctx)
	if err != nil {
		return fmt.Sprintf("node:%d", nodeID)
	}
	node, ok := c.nodeByID(nodes, nodeID)
	if !ok {
		return fmt.Sprintf("node:%d", nodeID)
	}
	return exitKey(node)
}
