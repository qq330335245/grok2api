package account

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
)

// DetectBuildBotFlagsWithProgress 用 Build 号自带 SSO 拉取 grok.com 首页 botFlagSource。
// 不走 grok-4.5 可用性探测，也不改账号启用状态。
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
		return item, fmt.Errorf("SSO 风控检测失败")
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

// InspectAndPersistPageBotFlag 供反降智路径在 K 个 ExitIP 失败后调用。
func (s *Service) InspectAndPersistPageBotFlag(ctx context.Context, credential accountdomain.Credential) (int, error) {
	item := s.inspectBuildPageBotFlag(ctx, credential)
	if item.Outcome != BuildDetectOutcomeOK && item.Outcome != BuildDetectOutcomeFlagged {
		if item.Reason != "" {
			return 0, fmt.Errorf("%s", item.Reason)
		}
		return 0, fmt.Errorf("SSO 风控检测失败")
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
	item := BuildDetectItemResult{AccountID: value.ID, Name: value.Name, Email: value.Email, Outcome: BuildDetectOutcomeFailed}
	if value.Provider != accountdomain.ProviderBuild {
		item.Reason = "仅 Grok Build 账号支持 SSO 风控检测"
		return item
	}
	if strings.TrimSpace(value.EncryptedSSOToken) == "" {
		item.Reason = "未配置 SSO"
		return item
	}
	ssoToken, err := s.cipher.Decrypt(value.EncryptedSSOToken)
	if err != nil {
		item.Reason = "解密 SSO 失败"
		return item
	}
	inspector, ok := s.providers.HomeBotFlag()
	if !ok {
		item.Reason = "Grok Web 检测能力未注册"
		return item
	}
	source, status, err := inspector.InspectHomeBotFlag(ctx, ssoToken)
	item.HTTPStatus = status
	if err != nil {
		item.Reason = err.Error()
		return item
	}
	if persistErr := s.persistPageBotFlag(ctx, value, source); persistErr != nil {
		item.Reason = persistErr.Error()
		return item
	}
	item.BotFlagSource = source
	item.Reason = fmt.Sprintf("botFlagSource=%d", source)
	if source == 1 || source == 2 {
		item.Outcome = BuildDetectOutcomeFlagged
		return item
	}
	item.Outcome = BuildDetectOutcomeOK
	return item
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
	s.invalidateBuildBotFlagCache()
	return nil
}

