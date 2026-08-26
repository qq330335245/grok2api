package app

import (
	"context"
	"log/slog"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/application/antidegrade"
	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func antiDegradeRuntime(value config.AntiDegradeConfig) antidegrade.Config {
	return antidegrade.Config{
		Enabled:                value.Enabled,
		Mode:                   value.Mode,
		ThinkingMinOutput:      int64(value.ThinkingMinOutput),
		DensityWindow:          value.DensityWindow.Value(),
		DensityMaxAccounts:     value.DensityMaxAccounts,
		DirtyIPCooldown:        value.DirtyIPCooldown.Value(),
		FarmIPCooldown:         value.FarmIPCooldown.Value(),
		MaxIPRetries:           value.MaxIPRetries,
		AccountIPFailThreshold: value.AccountIPFailThreshold,
		ScorePrior:             value.ScorePrior,
		ExploreRatio:           value.ExploreRatio,
		OperatorOverride:       value.OperatorOverride.Value(),
		StateFile:              value.StateFile,
	}.Normalize()
}

type antiDegradeNodes struct {
	svc *egressapp.Service
}

func (n antiDegradeNodes) ListBuildNodes(ctx context.Context) ([]antidegrade.Node, error) {
	values, err := n.svc.ListAll(ctx, egressdomain.ScopeBuild, repository.SortQuery{})
	if err != nil {
		return nil, err
	}
	result := make([]antidegrade.Node, 0, len(values))
	for _, value := range values {
		result = append(result, antidegrade.Node{
			ID: value.ID, Enabled: value.Enabled, ExitIP: value.ExitIP, Name: value.Name,
		})
	}
	return result, nil
}

type antiDegradeAccounts struct {
	svc *accountapp.Service
}

func (a antiDegradeAccounts) Disable(ctx context.Context, id uint64) error {
	enabled := false
	_, err := a.svc.Update(ctx, id, accountapp.UpdateInput{Enabled: &enabled})
	return err
}

func newAntiDegradeController(cfg config.AntiDegradeConfig, egress *egressapp.Service, accounts *accountapp.Service, logger *slog.Logger) *antidegrade.Controller {
	return antidegrade.New(antiDegradeRuntime(cfg), antiDegradeNodes{svc: egress}, antiDegradeAccounts{svc: accounts}, logger)
}
