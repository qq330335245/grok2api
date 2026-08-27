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

var (
	ErrNoEligibleExitIP   = errors.New("没有可用的干净出口 IP")
	ErrAccountQuarantined = errors.New("账号因缺少推理被隔离")
)

// Node is a scheduling snapshot of one egress node.
type Node struct {
	ID      uint64
	Enabled bool
	ExitIP  string
	Name    string
	Scope   string
}

type NodeSource interface {
	ListBuildNodes(context.Context) ([]Node, error)
}

type AccountDisabler interface {
	Disable(context.Context, uint64) error
}

// PageBotFlagInspector reads grok.com homepage botFlagSource from the Build account's own SSO.
type PageBotFlagInspector interface {
	Inspect(ctx context.Context, credential accountdomain.Credential) (int, error)
}

// Controller is the request-path ExitIP ledger: density, cooldown, score, delayed botflag=2.
type Controller struct {
	mu        sync.Mutex
	cfg       Config
	ledger    *ledger
	nodes     NodeSource
	accounts  AccountDisabler
	inspector PageBotFlagInspector
	logger    *slog.Logger
	rng       *rand.Rand
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

func (c *Controller) SetPageInspector(inspector PageBotFlagInspector) {
	if c == nil {
		return
	}
	c.inspector = inspector
}

func (c *Controller) Enabled() bool { return c != nil && c.cfg.Enabled }
func (c *Controller) Enforce() bool { return c != nil && c.cfg.Enforce() }
func (c *Controller) AppliesTo(provider accountdomain.Provider) bool {
	return c != nil && c.config().AppliesTo(provider)
}
func (c *Controller) ActiveFor(provider accountdomain.Provider) bool {
	return c.Enforce() && c.AppliesTo(provider)
}
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

func (c *Controller) AccountQuarantined(accountID uint64) bool {
	if c == nil || accountID == 0 {
		return false
	}
	now := c.ledger.now()
	c.ledger.mu.Lock()
	defer c.ledger.mu.Unlock()
	return c.ledger.accountQuarantined(accountID, now)
}

func (c *Controller) ClearAccount(_ context.Context, accountID uint64) {
	if c == nil || accountID == 0 {
		return
	}
	c.ledger.mu.Lock()
	c.ledger.clearAccountQuarantine(accountID)
	c.ledger.mu.Unlock()
	_ = c.ledger.persist()
	c.logger.Info("antidegrade_account_quarantine_cleared", "account_id", accountID)
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

func hasRealExitIP(node Node) bool {
	key := exitKey(node)
	return key != "" && !strings.HasPrefix(key, "node:")
}

func nodePlaceholder(nodeID uint64) string {
	if nodeID == 0 {
		return ""
	}
	return fmt.Sprintf("node:%d", nodeID)
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

func scopeForProvider(provider accountdomain.Provider) string {
	switch provider {
	case accountdomain.ProviderConsole:
		return "grok_console"
	case accountdomain.ProviderWeb:
		return "grok_web"
	case accountdomain.ProviderBuild, "":
		return "grok_build"
	default:
		return ""
	}
}

func filterNodes(nodes []Node, provider accountdomain.Provider) []Node {
	want := scopeForProvider(provider)
	if want == "" {
		return nodes
	}
	filtered := make([]Node, 0, len(nodes))
	for _, node := range nodes {
		if node.Scope == "" || node.Scope == want {
			filtered = append(filtered, node)
		}
	}
	return filtered
}

func (c *Controller) nodeCoolingLocked(node Node, now time.Time) bool {
	keys := make([]string, 0, 3)
	if key := exitKey(node); key != "" {
		keys = append(keys, key)
	}
	if placeholder := nodePlaceholder(node.ID); placeholder != "" {
		keys = append(keys, placeholder)
	}
	if ip := c.ledger.lastIPForNode(node.ID); ip != "" {
		keys = append(keys, ip)
	}
	seen := map[string]struct{}{}
	for _, key := range keys {
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if c.ledger.cooling(c.ledger.state.IPs[key], now) {
			return true
		}
	}
	return false
}

func (c *Controller) allowed(cfg Config, node Node, accountID uint64, excluded map[uint64]bool, now time.Time) bool {
	if node.ID == 0 || !node.Enabled || excluded[node.ID] || !hasRealExitIP(node) {
		return false
	}
	key := exitKey(node)
	c.ledger.mu.Lock()
	defer c.ledger.mu.Unlock()
	c.ledger.rememberNode(key, node.ID)
	if c.nodeCoolingLocked(node, now) {
		return false
	}
	state := c.ledger.ip(key)
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
		if node.ID == 0 || !node.Enabled || excluded[node.ID] || !hasRealExitIP(node) {
			continue
		}
		key := exitKey(node)
		c.ledger.rememberNode(key, node.ID)
		if c.nodeCoolingLocked(node, now) {
			continue
		}
		state := c.ledger.ip(key)
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
	if c == nil || !c.ActiveFor(credential.Provider) {
		return 0, nil
	}
	if c.AccountQuarantined(credential.ID) {
		return 0, ErrAccountQuarantined
	}
	cfg := c.config()
	nodes, err := c.lookup(ctx)
	if err != nil {
		return 0, err
	}
	nodes = filterNodes(nodes, credential.Provider)
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

func (c *Controller) resolveKeyLocked(nodeID uint64, exitIP string) string {
	key := strings.TrimSpace(exitIP)
	if key == "" || strings.HasPrefix(key, "node:") {
		if remembered := c.ledger.lastIPForNode(nodeID); remembered != "" {
			return remembered
		}
		if nodeID != 0 {
			return nodePlaceholder(nodeID)
		}
	}
	return key
}

func (c *Controller) coolDirtyLocked(key string, nodeID uint64, until time.Time) {
	c.ledger.cool(key, reasonDirtyIP, until)
	c.ledger.rememberNode(key, nodeID)
	// Only stamp the node:N key when that is the identity we actually cooled.
	// Cooling a real IP must not also freeze node:N, or lifting the IP later
	// would still exclude the node.
}

func (c *Controller) OnSuccess(accountID, nodeID uint64, exitIP string) {
	if c == nil || !c.Enabled() || nodeID == 0 && exitIP == "" {
		return
	}
	cfg := c.config()
	now := c.ledger.now()
	c.ledger.mu.Lock()
	key := c.resolveKeyLocked(nodeID, exitIP)
	c.ledger.rememberNode(key, nodeID)
	c.ledger.noteWindow(key, accountID, cfg.DensityWindow, now)
	c.ledger.recordEvent(key, accountID, true, now)
	c.ledger.mu.Unlock()
	_ = c.ledger.persist()
}

func (c *Controller) OnIdleStream(credential accountdomain.Credential, nodeID uint64, exitIP string) {
	if c == nil || !c.Enabled() || !c.AppliesTo(credential.Provider) {
		return
	}
	cfg := c.config()
	now := c.ledger.now()
	c.ledger.mu.Lock()
	key := c.resolveKeyLocked(nodeID, exitIP)
	if key == "" {
		c.ledger.mu.Unlock()
		return
	}
	c.ledger.rememberNode(key, nodeID)
	c.ledger.noteWindow(key, credential.ID, cfg.DensityWindow, now)
	if cfg.Enforce() {
		c.coolDirtyLocked(key, nodeID, now.Add(cfg.DirtyIPCooldown))
	}
	c.ledger.mu.Unlock()
	_ = c.ledger.persist()
	c.logger.Info("antidegrade_idle_stream", "account_id", credential.ID, "node_id", nodeID, "exit_ip", key)
}

func (c *Controller) OnMissingThinking(ctx context.Context, credential accountdomain.Credential, nodeID uint64, exitIP string) {
	if c == nil || !c.Enabled() || !c.AppliesTo(credential.Provider) {
		return
	}
	cfg := c.config()
	now := c.ledger.now()
	c.ledger.mu.Lock()
	key := c.resolveKeyLocked(nodeID, exitIP)
	if key == "" {
		c.ledger.mu.Unlock()
		return
	}
	already := c.ledger.accountQuarantined(credential.ID, now)
	c.ledger.rememberNode(key, nodeID)
	c.ledger.noteWindow(key, credential.ID, cfg.DensityWindow, now)
	c.ledger.recordEvent(key, credential.ID, false, now)
	distinct := 0
	promoted := false
	lifted := []string(nil)
	if cfg.Enforce() {
		if already {
			distinct = c.ledger.distinctFailCount(credential.ID, now)
		} else {
			c.coolDirtyLocked(key, nodeID, now.Add(cfg.DirtyIPCooldown))
			distinct = c.ledger.noteAccountFail(credential.ID, key, now)
			if distinct >= cfg.AccountIPFailThreshold {
				lifted = c.ledger.quarantineAccount(credential.ID, now.Add(cfg.AccountQuarantineTTL), reasonQuarantine)
				promoted = true
			}
		}
	}
	c.ledger.mu.Unlock()
	_ = c.ledger.persist()
	c.logger.Info("antidegrade_missing_thinking", "account_id", credential.ID, "node_id", nodeID, "exit_ip", key, "distinct_fail_ips", distinct, "quarantined", already || promoted, "lifted_ips", lifted)
	if !promoted {
		return
	}
	// SSO is optional and must not block quarantine. botFlag=2 may still
	// upgrade the soft isolation into a permanent disable.
	c.maybeBanAccount(ctx, credential, key, now)
}

func (c *Controller) maybeBanAccount(ctx context.Context, credential accountdomain.Credential, lastIP string, now time.Time) {
	if c.inspector != nil {
		go c.inspectAndMaybeBan(credential, lastIP, now)
		return
	}
	c.banIfSourceTwo(ctx, credential, lastIP, now, credential.BuildBotFlagSource)
}

func (c *Controller) inspectAndMaybeBan(credential accountdomain.Credential, lastIP string, now time.Time) {
	if c == nil || c.inspector == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	source, err := c.inspector.Inspect(ctx, credential)
	if err != nil {
		c.logger.Warn("antidegrade_botflag_inspect_failed", "account_id", credential.ID, "error", err)
		return
	}
	c.banIfSourceTwo(ctx, credential, lastIP, now, source)
}

func (c *Controller) banIfSourceTwo(ctx context.Context, credential accountdomain.Credential, lastIP string, now time.Time, source int) {
	if source != 2 {
		c.logger.Info("antidegrade_botflag_skip", "account_id", credential.ID, "bot_flag_source", source, "reason", "unclassified_or_not_flag_2")
		return
	}
	c.banAccount(ctx, credential, lastIP, now, "page_botflag_2")
}

func (c *Controller) banAccount(ctx context.Context, credential accountdomain.Credential, lastIP string, now time.Time, reason string) {
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
	c.logger.Info("antidegrade_account_banned", "account_id", credential.ID, "reason", reason, "exit_ip", lastIP)
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
	var real string
	if nodes, err := c.lookup(ctx); err == nil {
		if node, ok := c.nodeByID(nodes, nodeID); ok {
			if key := exitKey(node); key != "" && !strings.HasPrefix(key, "node:") {
				real = key
			}
		}
	}
	c.ledger.mu.Lock()
	if real != "" {
		c.ledger.mergeIP(fmt.Sprintf("node:%d", nodeID), real, nodeID)
		c.ledger.rememberNode(real, nodeID)
		c.ledger.mu.Unlock()
		_ = c.ledger.persist()
		return real
	}
	remembered := c.ledger.lastIPForNode(nodeID)
	c.ledger.mu.Unlock()
	if remembered != "" {
		return remembered
	}
	return fmt.Sprintf("node:%d", nodeID)
}
