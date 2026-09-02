package account

import (
	"context"
	"errors"
	"fmt"
	"sync"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
)

// DetectBuildBotFlagsWithProgress 对 Build 号做号级风控确认：同一号用粘性
// {account}+n 换口打思考请求。不走 grok-4.5 探活，不改启用状态，不需要 SSO。
func (s *Service) DetectBuildBotFlagsWithProgress(ctx context.Context, ids []uint64, all bool, progress BatchProgressObserver, itemObserver BuildDetectItemObserver) (int, int, error) {
	if all == (len(ids) > 0) {
		return 0, 0, invalidInput("必须明确选择全部账号或提供非空账号 ID")
	}
	var err error
	if all {
		ids, err = s.accounts.ListEnabledCredentialRefreshAccountIDs(ctx, accountdomain.ProviderBuild, false)
		if err != nil {
			return 0, 0, err
		}
	} else {
		ids, err = normalizeBatchIDs(ids)
		if err != nil {
			return 0, 0, err
		}
	}
	if len(ids) == 0 {
		return 0, 0, nil
	}
	pool := s.detectPool
	if pool == nil {
		pool = s.syncPool
	}
	if progress != nil {
		if err := progress(0, len(ids)); err != nil {
			return 0, 0, err
		}
	}
	var observerMu sync.Mutex
	var progressErr error
	completed := 0
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	summary, err := batch.ForEachObserved(runCtx, ids, batch.Options{Workers: pool.Limit(), Pool: pool}, func(workCtx context.Context, id uint64) (BuildDetectItemResult, error) {
		item := s.detectBuildPageBotFlag(workCtx, id)
		if itemObserver != nil {
			if notifyErr := itemObserver(item); notifyErr != nil {
				return item, notifyErr
			}
		}
		if item.Outcome == BuildDetectOutcomeOK || item.Outcome == BuildDetectOutcomeFlagged {
			return item, nil
		}
		if item.Reason != "" {
			return item, fmt.Errorf("%s", item.Reason)
		}
		return item, fmt.Errorf("风控检测失败")
	}, func(index int, result batch.Result[BuildDetectItemResult]) {
		observerMu.Lock()
		defer observerMu.Unlock()
		completed++
		if progress != nil {
			if notifyErr := progress(completed, len(ids)); notifyErr != nil && progressErr == nil {
				progressErr = notifyErr
				cancel()
			}
		}
	})
	s.logBatchSummary("build_botflag_detect", pool, summary, err)
	return summary.Succeeded, summary.Failed, errors.Join(err, progressErr)
}

// InspectAndPersistPageBotFlag 供反降智路径在失败出口达阈值后做号级确认。
func (s *Service) InspectAndPersistPageBotFlag(ctx context.Context, credential accountdomain.Credential) (int, error) {
	item := s.inspectBuildPageBotFlag(ctx, credential)
	if item.Outcome != BuildDetectOutcomeOK && item.Outcome != BuildDetectOutcomeFlagged {
		if item.Reason != "" {
			return 0, fmt.Errorf("%s", item.Reason)
		}
		return 0, fmt.Errorf("风控检测失败")
	}
	return item.BotFlagSource, nil
}

func (s *Service) detectBuildPageBotFlag(ctx context.Context, id uint64) BuildDetectItemResult {
	item := BuildDetectItemResult{AccountID: id, Outcome: BuildDetectOutcomeFailed}
	value, err := s.accounts.Get(ctx, id)
	if err != nil {
		item.Reason = mapRepositoryError(err).Error()
		return item
	}
	return s.inspectBuildPageBotFlag(ctx, value)
}

func (s *Service) inspectBuildPageBotFlag(ctx context.Context, value accountdomain.Credential) BuildDetectItemResult {
	return s.inspectBuildBotRisk(ctx, value)
}

func (s *Service) persistPageBotFlag(ctx context.Context, value accountdomain.Credential, source int) error {
	if source != 1 && source != 2 {
		source = 0
	}
	latest, err := s.accounts.Get(ctx, value.ID)
	if err != nil {
		return err
	}
	latest.BuildBotFlagSource = source
	latest.BuildBotFlagOrigin = accountdomain.BuildBotFlagOriginPage
	if _, err := s.accounts.Update(ctx, latest); err != nil {
		return err
	}
	if err := s.propagatePageBotFlagToLinked(ctx, latest, source); err != nil {
		return err
	}
	s.invalidateBuildBotFlagCache()
	return nil
}

func (s *Service) propagatePageBotFlagToLinked(ctx context.Context, value accountdomain.Credential, source int) error {
	if s.accounts == nil || !value.Provider.IsValid() {
		return nil
	}
	targets := make([]accountdomain.Provider, 0, 2)
	for _, provider := range accountdomain.Providers() {
		if provider != value.Provider {
			targets = append(targets, provider)
		}
	}
	resolution, err := s.accounts.ResolveLinkedDeleteIDs(ctx, value.Provider, []uint64{value.ID}, targets)
	if err != nil {
		return err
	}
	for id := range resolution.PeerProviders {
		peer, getErr := s.accounts.Get(ctx, id)
		if getErr != nil {
			return getErr
		}
		peer.BuildBotFlagSource = source
		peer.BuildBotFlagOrigin = accountdomain.BuildBotFlagOriginPage
		if _, updateErr := s.accounts.Update(ctx, peer); updateErr != nil {
			return updateErr
		}
	}
	return nil
}
