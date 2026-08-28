package app

import (
	"context"
	"log/slog"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/application/antidegrade"
	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func antiDegradeRuntime(value config.AntiDegradeConfig) antidegrade.Config {
	return antidegrade.Config{
		Enabled:                value.Enabled,
		Mode:                   value.Mode,
		Providers:              append([]string(nil), value.Providers...),
		ThinkingMinOutput:      int64(value.ThinkingMinOutput),
		DensityWindow:          value.DensityWindow.Value(),
		DensityMaxAccounts:     value.DensityMaxAccounts,
		DirtyIPCooldown:        value.DirtyIPCooldown.Value(),
		FarmIPCooldown:         value.FarmIPCooldown.Value(),
		MaxIPRetries:           value.MaxIPRetries,
		AccountIPFailThreshold: value.AccountIPFailThreshold,
		AccountQuarantineTTL:   value.AccountQuarantineTTL.Value(),
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
	values, err := n.svc.ListAll(ctx, "", repository.SortQuery{})
	if err != nil {
		return nil, err
	}
	result := make([]antidegrade.Node, 0, len(values))
	for _, value := range values {
		result = append(result, antidegrade.Node{
			ID: value.ID, Enabled: value.Enabled, ExitIP: value.ExitIP, Name: value.Name, Scope: string(value.Scope),
			SharedExit: value.AccountBoundProxy || value.ProxyPool,
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

type antiDegradeBotFlag struct {
	svc *accountapp.Service
}

func (a antiDegradeBotFlag) Inspect(ctx context.Context, credential accountdomain.Credential) (int, error) {
	return a.svc.InspectAndPersistPageBotFlag(ctx, credential)
}

func newAntiDegradeController(cfg config.AntiDegradeConfig, egress *egressapp.Service, accounts *accountapp.Service, logger *slog.Logger) *antidegrade.Controller {
	controller := antidegrade.New(antiDegradeRuntime(cfg), antiDegradeNodes{svc: egress}, antiDegradeAccounts{svc: accounts}, logger)
	controller.SetPageInspector(antiDegradeBotFlag{svc: accounts})
	return controller
}
