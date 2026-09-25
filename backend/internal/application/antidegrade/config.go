package antidegrade

import (
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

const (
	ModeObserve = "observe"
	ModeEnforce = "enforce"
)

// Config is the in-process ExitIP anti-degrade policy.
type Config struct {
	Enabled bool
	Mode    string
	// Providers is the channel allowlist. Empty normalizes to grok_build so
	// Console/Web are not held or quarantined until explicitly opted in.
	Providers []string
	// DisabledModels skips anti-degrade for those public model IDs.
	// Empty means every model is on.
	DisabledModels         []string
	ThinkingMinOutput      int64
	DensityWindow          time.Duration
	DensityMaxAccounts     int
	DirtyIPCooldown        time.Duration
	FarmIPCooldown         time.Duration
	MaxIPRetries           int
	AccountIPFailThreshold int
	AccountQuarantineTTL   time.Duration
	ScorePrior             float64
	ExploreRatio           float64
	OperatorOverride       time.Duration
	StateFile              string
}

func (c Config) Normalize() Config {
	if c.Mode != ModeObserve {
		c.Mode = ModeEnforce
	}
	c.Providers = normalizeProviders(c.Providers)
	c.DisabledModels = normalizeDisabledModels(c.DisabledModels)
	if c.ThinkingMinOutput <= 0 {
		c.ThinkingMinOutput = 32
	}
	if c.DensityWindow <= 0 {
		c.DensityWindow = 15 * time.Minute
	}
	if c.DensityMaxAccounts <= 0 {
		c.DensityMaxAccounts = 5
	}
	if c.DirtyIPCooldown <= 0 {
		c.DirtyIPCooldown = 30 * time.Minute
	}
	if c.FarmIPCooldown <= 0 {
		c.FarmIPCooldown = 6 * time.Hour
	}
	if c.AccountIPFailThreshold <= 0 {
		c.AccountIPFailThreshold = 2
	}
	if c.MaxIPRetries <= 0 {
		c.MaxIPRetries = 3
	}
	if c.MaxIPRetries < c.AccountIPFailThreshold {
		c.MaxIPRetries = c.AccountIPFailThreshold
	}
	if c.AccountQuarantineTTL <= 0 {
		c.AccountQuarantineTTL = 2 * time.Hour
	}
	if c.ScorePrior <= 0 || c.ScorePrior > 1 {
		c.ScorePrior = 0.7
	}
	if c.ExploreRatio < 0 {
		c.ExploreRatio = 0
	}
	if c.ExploreRatio > 0.5 {
		c.ExploreRatio = 0.5
	}
	if c.OperatorOverride <= 0 {
		c.OperatorOverride = 10 * time.Minute
	}
	if c.StateFile == "" {
		c.StateFile = "data/antidegrade-exitip.json"
	}
	return c
}

func (c Config) Enforce() bool {
	return c.Enabled && c.Mode == ModeEnforce
}

func (c Config) AppliesTo(provider accountdomain.Provider) bool {
	want := strings.TrimSpace(string(provider))
	if want == "" {
		want = string(accountdomain.ProviderBuild)
	}
	for _, item := range c.Normalize().Providers {
		if item == want {
			return true
		}
	}
	return false
}

// AppliesToModel reports whether this public model is covered.
// An empty disable list covers every model, including ones added later.
// An empty model id stays covered so an unclassified request is not silently skipped.
func (c Config) AppliesToModel(publicModel string) bool {
	want := normalizeModelID(publicModel)
	if want == "" {
		return true
	}
	for _, item := range c.Normalize().DisabledModels {
		if item == want {
			return false
		}
	}
	return true
}

func normalizeModelID(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeDisabledModels(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		item := normalizeModelID(value)
		if item == "" {
			continue
		}
		if _, dup := seen[item]; dup {
			continue
		}
		seen[item] = struct{}{}
		result = append(result, item)
	}
	if len(result) == 0 {
		return []string{}
	}
	return result
}

var allowedAntiDegradeProviders = map[string]struct{}{
	string(accountdomain.ProviderBuild):   {},
	string(accountdomain.ProviderConsole): {},
	string(accountdomain.ProviderWeb):     {},
}

func normalizeProviders(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		item := strings.ToLower(strings.TrimSpace(value))
		if _, ok := allowedAntiDegradeProviders[item]; !ok {
			continue
		}
		if _, dup := seen[item]; dup {
			continue
		}
		seen[item] = struct{}{}
		result = append(result, item)
	}
	if len(result) == 0 {
		return []string{string(accountdomain.ProviderBuild)}
	}
	return result
}
