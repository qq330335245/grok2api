package antidegrade

import "time"

const (
	ModeObserve = "observe"
	ModeEnforce = "enforce"
)

// Config is the in-process ExitIP anti-degrade policy.
type Config struct {
	Enabled                bool
	Mode                   string
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
