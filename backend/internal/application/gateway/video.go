package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
)

const (
	videoJobTimeout          = 2 * time.Hour
	videoJobLease            = videoJobTimeout + 5*time.Minute
	videoJobRecoveryInterval = 30 * time.Second
	videoOutputAttempts      = 3
	// Base64 物化会同时持有原图和编码后字符串，单独限流避免高 mediaConcurrency 放大内存峰值。
	videoInputMaterializeConcurrency = 4
	videoInputJSONBaseBytes          = int64(len(`{"image_urls":[]}`))
)

// VideoInputFileReference 将本地临时 file_id 编码为只在 Gateway 内部解释的引用。
func VideoInputFileReference(fileID string) string {
	return media.InputReference(fileID)
}

type VideoInput struct {
	RequestID   string
	ClientKey   clientkey.Key
	PublicModel string
	// Mode is generate (default), edit, or extend.
	Mode        string
	Prompt      string
	Duration    int
	AspectRatio string
	Resolution  string
	// FirstFrameURL maps to official image-to-video `image` (at most one).
	FirstFrameURL string
	// ReferenceURLs maps to official `reference_images` only.
	ReferenceURLs []string
	// SourceVideoURL maps to official edit/extend `video.url`.
	SourceVideoURL string
}

func normalizeVideoMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", provider.VideoModeGenerate:
		return provider.VideoModeGenerate
	case provider.VideoModeEdit:
		return provider.VideoModeEdit
	case provider.VideoModeExtend:
		return provider.VideoModeExtend
	default:
		return strings.ToLower(strings.TrimSpace(mode))
	}
}

func (s *Service) CreateVideo(ctx context.Context, input VideoInput) (job media.Job, err error) {
	if s.mediaJobs == nil || s.mediaQueue == nil {
		return media.Job{}, fmt.Errorf("视频任务服务未配置")
	}
	mode := normalizeVideoMode(input.Mode)
	defer func() {
		if err != nil && s.logger != nil {
			s.logger.Warn("video_create_failed", "mode", mode, "model", input.PublicModel, "error", err)
		}
	}()
	firstFrame := strings.TrimSpace(input.FirstFrameURL)
	references := trimVideoURLList(input.ReferenceURLs)
	sourceVideo := strings.TrimSpace(input.SourceVideoURL)
	switch mode {
	case provider.VideoModeGenerate:
		if firstFrame != "" && len(references) > 0 {
			return media.Job{}, invalidRequestf("image 与 reference_images 不能同时使用")
		}
		if sourceVideo != "" {
			return media.Job{}, invalidRequestf("视频生成不支持 video 输入，请使用 /v1/videos/edits 或 /v1/videos/extensions")
		}
		if len(input.Prompt) > 100000 || (len(input.Prompt) == 0 && firstFrame == "" && len(references) == 0) {
			return media.Job{}, invalidRequestf("文本生视频必须提供 prompt；图片生视频可以省略 prompt")
		}
	case provider.VideoModeEdit, provider.VideoModeExtend:
		if firstFrame != "" || len(references) > 0 {
			return media.Job{}, invalidRequestf("视频编辑/扩展不支持 image 或 reference_images")
		}
		if sourceVideo == "" {
			return media.Job{}, invalidRequestf("视频编辑/扩展必须提供 video")
		}
		if strings.TrimSpace(input.Prompt) == "" || len(input.Prompt) > 100000 {
			return media.Job{}, invalidRequestf("视频编辑/扩展必须提供有效 prompt")
		}
		if mode == provider.VideoModeExtend && (input.Duration < 2 || input.Duration > 10) {
			return media.Job{}, invalidRequestf("视频扩展 duration 必须在 2 到 10 秒之间")
		}
	default:
		return media.Job{}, invalidRequestf("不支持的视频模式: %s", mode)
	}
	allRefs := references
	if firstFrame != "" {
		allRefs = []string{firstFrame}
	}
	if sourceVideo != "" {
		allRefs = append(allRefs, sourceVideo)
	}
	if err := s.validateVideoInputReferences(ctx, allRefs); err != nil {
		return media.Job{}, err
	}
	inputJSON, err := encodeVideoJobInput(videoJobInput{
		Mode: mode, FirstFrameURL: firstFrame, ReferenceURLs: references, SourceVideoURL: sourceVideo,
	})
	if err != nil {
		return media.Job{}, err
	}
	routes, err := s.models.GetByPublicIDCandidates(ctx, input.PublicModel)
	if err != nil {
		return media.Job{}, ErrModelNotFound
	}
	route, err := s.selectMediaRoute(routes, input.ClientKey, model.CapabilityVideo, func(providerValue account.Provider) bool {
		if _, ok := s.providers.Videos(providerValue); !ok {
			return false
		}
		// Official edit/extend are Console-only in this release.
		if mode == provider.VideoModeEdit || mode == provider.VideoModeExtend {
			return providerValue == account.ProviderConsole
		}
		return true
	})
	if err != nil {
		return media.Job{}, err
	}
	if err := s.checkLedgerReady(); err != nil {
		return media.Job{}, err
	}
	externalModel := model.ExternalPublicID(route.Provider, route.PublicID)
	quotaMode := s.providers.QuotaMode(route.Provider, route.UpstreamModel)
	lease, err := s.selector.AcquireForKey(ctx, route.Provider, route.ID, route.UpstreamModel, quotaMode, "", nil, false, input.ClientKey.AccountScope())
	if err != nil {
		return media.Job{}, fmt.Errorf("%w: %w", ErrNoAvailableAccount, err)
	}
	accountID := lease.Credential.ID
	lease.Release()
	token, err := security.NewOpaqueToken(18)
	if err != nil {
		return media.Job{}, err
	}
	now := time.Now().UTC()
	// media_jobs CHECKs require seconds∈[1,15] and non-empty size/quality.
	// Edit has no client duration/ratio/resolution; extend has duration only.
	seconds := input.Duration
	if seconds < 1 || seconds > 15 {
		seconds = 1 // placeholder for edit (and any out-of-range); not used for billing when ≤0 was estimated
	}
	size := strings.TrimSpace(input.AspectRatio)
	if size == "" {
		size = "source"
	}
	quality := strings.TrimSpace(input.Resolution)
	if quality == "" {
		quality = "source"
	}
	job = media.Job{
		ID: "video_" + token, RequestID: input.RequestID,
		ClientKeyID: input.ClientKey.ID, ClientKeyName: input.ClientKey.Name,
		AccountID: accountID, AccountName: lease.Credential.Name,
		Provider: string(route.Provider), Model: externalModel, ModelRouteID: route.ID, UpstreamModel: model.DisplayUpstreamModel(route.Provider, route.UpstreamModel), Prompt: input.Prompt,
		Seconds: seconds, Size: size, Quality: quality,
		Status: media.StatusQueued, Progress: 0, InputJSON: inputJSON, InputImageCount: len(allRefs), CreatedAt: now, UpdatedAt: now,
	}
	reserved := false
	if pricing, ok := audit.EstimateOfficialVideoCost(externalModel, input.Resolution, input.Duration); ok {
		reserved, err = s.clientKeys.ReserveBilling(ctx, input.ClientKey, "video_usage_"+job.ID, pricing.CostInUSDTicks, mediaBillingReservationTTL)
		if err != nil {
			return media.Job{}, err
		}
	}
	if err = s.mediaJobs.CreateMediaJob(ctx, job); err != nil {
		if reserved {
			s.cancelBillingReservation("video_usage_" + job.ID)
		}
		return media.Job{}, err
	}
	if !s.enqueueVideoJob(job.ID) {
		s.logger.Warn("video_job_queue_full", "job_id", job.ID)
	}
	return job, nil
}

func (s *Service) GetVideo(ctx context.Context, id string, key clientkey.Key) (media.Job, error) {
	if s.mediaJobs == nil {
		return media.Job{}, ErrResponseNotFound
	}
	job, err := s.mediaJobs.GetMediaJob(ctx, id, key.ID)
	if err != nil {
		return media.Job{}, ErrResponseNotFound
	}
	return job, nil
}

func (s *Service) OpenVideoContent(ctx context.Context, id string, key clientkey.Key) (io.ReadCloser, string, int64, error) {
	job, err := s.GetVideo(ctx, id, key)
	if err != nil {
		return nil, "", 0, err
	}
	if job.Status != media.StatusCompleted {
		return nil, "", 0, fmt.Errorf("视频内容尚未可用")
	}
	// 本地资产优先：XAI ZDR 上传完成后不经公网回环下载。
	if job.ResultAssetID != "" && s.mediaAssets != nil {
		asset, body, openErr := s.mediaAssets.OpenVideo(ctx, job.ResultAssetID)
		if openErr == nil {
			return body, asset.MIMEType, asset.SizeBytes, nil
		}
	}
	if job.UpstreamURL == "" {
		return nil, "", 0, fmt.Errorf("视频内容尚未可用")
	}
	adapter, ok := s.providers.Videos(account.Provider(job.Provider))
	if !ok {
		return nil, "", 0, ErrResponseAccountUnavailable
	}
	downloader, ok := adapter.(provider.VideoContentDownloader)
	if !ok || s.selector == nil || s.selector.accounts == nil || s.accounts == nil {
		return nil, "", 0, ErrResponseAccountUnavailable
	}
	credential, err := s.selector.accounts.Get(ctx, job.AccountID)
	if err != nil {
		return nil, "", 0, ErrResponseAccountUnavailable
	}
	credential, err = s.accounts.EnsureCredential(ctx, credential, false)
	if err != nil {
		return nil, "", 0, ErrResponseAccountUnavailable
	}
	return downloader.DownloadVideo(ctx, credential, job.UpstreamURL)
}

func (s *Service) RecoverVideoJobs(ctx context.Context) error {
	if s.mediaJobs == nil {
		return nil
	}
	usageErr := s.reconcileVideoUsage(ctx)
	values, err := s.mediaJobs.ListRecoverableMediaJobs(ctx, 1000)
	if err != nil {
		return errors.Join(usageErr, err)
	}
	for _, job := range values {
		if !s.enqueueVideoJob(job.ID) {
			break
		}
	}
	return usageErr
}

// RunVideoWorkers 使用固定 Worker 处理持久化任务，避免突发请求按任务创建无界 goroutine。
func (s *Service) RunVideoWorkers(ctx context.Context) {
	if s.mediaQueue == nil || s.mediaWorker <= 0 {
		return
	}
	var workers sync.WaitGroup
	workers.Add(s.mediaWorker)
	for range s.mediaWorker {
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case id := <-s.mediaQueue:
					err := batch.Do(ctx, func(workCtx context.Context) error {
						s.processVideoJob(workCtx, id)
						return nil
					})
					s.mediaMu.Lock()
					delete(s.mediaQueued, id)
					s.mediaMu.Unlock()
					if err != nil && ctx.Err() == nil {
						if panicErr, ok := err.(*batch.PanicError); ok {
							s.logger.Error("video_worker_panicked", "job_id", id, "error", panicErr, "stack", string(panicErr.Stack))
						} else {
							s.logger.Error("video_worker_failed", "job_id", id, "error", err)
						}
					}
				}
			}
		}()
	}
	workers.Wait()
}

func (s *Service) enqueueVideoJob(id string) bool {
	if id == "" || s.mediaQueue == nil {
		return false
	}
	s.mediaMu.Lock()
	if _, exists := s.mediaQueued[id]; exists {
		s.mediaMu.Unlock()
		return true
	}
	s.mediaQueued[id] = struct{}{}
	s.mediaMu.Unlock()
	select {
	case s.mediaQueue <- id:
		return true
	default:
		s.mediaMu.Lock()
		delete(s.mediaQueued, id)
		s.mediaMu.Unlock()
		full := s.mediaQueueFull.Add(1)
		if s.logger != nil && (full == 1 || full%100 == 0) {
			s.logger.Warn("video_queue_full", "count", full, "queued", len(s.mediaQueue), "capacity", cap(s.mediaQueue))
		}
		return false
	}
}

func (s *Service) processVideoJob(ctx context.Context, id string) {
	job, claimed, err := s.claimVideoJob(ctx, id)
	if err != nil {
		s.logger.Warn("video_job_claim_failed", "job_id", id, "error", err)
		return
	}
	if !claimed {
		return
	}
	var route model.Route
	if job.ModelRouteID != 0 {
		route, err = s.models.Get(ctx, job.ModelRouteID)
	} else {
		route, err = s.models.GetByPublicID(ctx, job.Model)
	}
	if err != nil {
		s.failVideoJob(ctx, job, "model_not_found", errors.New("模型路由不存在"))
		return
	}
	s.runVideoJob(ctx, job, route)
}

// RunVideoRecovery 周期认领新建后未启动或执行实例失联后的媒体任务。
func (s *Service) RunVideoRecovery(ctx context.Context) {
	ticker := time.NewTicker(videoJobRecoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.RecoverVideoJobs(ctx); err != nil {
				s.logger.Warn("video_job_recovery_failed", "error", err)
			}
		}
	}
}

func (s *Service) claimVideoJob(ctx context.Context, id string) (media.Job, bool, error) {
	now := time.Now().UTC()
	claimToken, err := security.NewOpaqueToken(18)
	if err != nil {
		return media.Job{}, false, err
	}
	return s.mediaJobs.TryClaimMediaJob(ctx, id, now, now.Add(videoJobLease), claimToken)
}

func (s *Service) runVideoJob(parent context.Context, job media.Job, route model.Route) {
	ctx, cancel := context.WithTimeout(parent, videoJobTimeout)
	defer cancel()
	ctx, egressTrace := infraegress.WithTrace(ctx)
	startedAt := time.Now()
	job.Progress = max(job.Progress, 1)
	job.UpdatedAt = time.Now().UTC()
	if err := s.mediaJobs.UpdateMediaJob(ctx, job); err != nil {
		s.logger.Warn("video_job_progress_write_failed", "job_id", job.ID, "error", err)
	}
	jobInput := decodeVideoJobInput(job.InputJSON)
	inputReferences := jobInput.allURLs()
	releaseInputSlot, err := s.acquireVideoInputSlot(ctx, inputReferences)
	if err != nil {
		s.deferVideoJob(parent, job)
		return
	}
	defer releaseInputSlot()
	// 视频任务创建时已持久化账号归属；恢复只能重新获取原账号，禁止因后续
	// 轮询或结果处理失败切换到其他账号。
	quotaMode := s.providers.QuotaMode(route.Provider, route.UpstreamModel)
	lease, err := s.selector.AcquirePinned(ctx, route.Provider, job.AccountID, route.ID, route.UpstreamModel, quotaMode, true)
	if err != nil {
		if parent.Err() != nil {
			s.deferVideoJob(parent, job)
			return
		}
		s.failVideoJob(parent, job, "account_unavailable", err)
		return
	}
	defer lease.Release()
	credential, err := s.accounts.EnsureCredential(ctx, lease.Credential, false)
	if err != nil {
		s.failVideoJob(parent, job, "account_unavailable", err)
		return
	}
	lease.Credential = credential
	adapter, ok := s.providers.Videos(route.Provider)
	if !ok {
		s.failVideoJob(parent, job, "provider_unavailable", ErrNoAvailableAccount)
		return
	}
	resolved, err := s.resolveVideoJobInput(ctx, jobInput)
	if err != nil {
		s.failVideoJob(parent, job, "input_unavailable", err)
		return
	}
	lastProgress := job.Progress
	result, err := adapter.GenerateVideo(ctx, provider.VideoRequest{
		Credential: lease.Credential, Billing: lease.Billing, JobID: job.ID, Mode: resolved.Mode, Prompt: job.Prompt, Duration: job.Seconds, AspectRatio: job.Size, Resolution: job.Quality,
		FirstFrameURL: resolved.FirstFrameURL, ReferenceURLs: resolved.ReferenceURLs, SourceVideoURL: resolved.SourceVideoURL, UpstreamModel: route.UpstreamModel,
		Progress: func(value int) {
			value = min(99, max(1, value))
			if value-lastProgress < 5 {
				return
			}
			lastProgress = value
			job.Progress, job.UpdatedAt = value, time.Now().UTC()
			leaseUntil := job.UpdatedAt.Add(videoJobLease)
			job.LeaseUntil = &leaseUntil
			updateCtx, updateCancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = s.mediaJobs.UpdateMediaJob(updateCtx, job)
			updateCancel()
		},
	})
	// Provider 已消费请求体，尽早释放 Base64 物化名额和大字符串。
	resolved = videoJobInput{}
	releaseInputSlot()
	if err == nil && result.AssetID == "" && result.URL != "" {
		result, err = s.persistRemoteVideo(ctx, job.ID, adapter, lease.Credential, result)
	}
	if err != nil {
		if parent.Err() != nil {
			s.deferVideoJob(parent, job)
			return
		}
		failureCtx, failureCancel := context.WithTimeout(context.Background(), finalizationTimeout)
		failureHandled := false
		if errors.Is(err, provider.ErrUnauthorized) {
			if lease.Credential.AuthType == account.AuthTypeSSO {
				s.markSSOCredentialRejected(failureCtx, lease.Credential, fmt.Sprintf("%s SSO credential rejected", lease.Credential.Provider))
			}
			failureHandled = true
		} else if status, ok := provider.ErrorHTTPStatus(err); ok {
			switch {
			case status == http.StatusUnauthorized && lease.Credential.AuthType == account.AuthTypeSSO:
				s.markSSOCredentialRejected(failureCtx, lease.Credential, fmt.Sprintf("%s SSO credential rejected", lease.Credential.Provider))
				failureHandled = true
			case status == http.StatusForbidden && s.providers.RetryForbiddenAsEgress(lease.Credential.Provider):
				// Web Provider 已对 anti-bot 403 降低出口健康并重建浏览器会话；
				// 视频请求已提交，不能换号重试，也不能误伤账号池。
				// 符合资格的 Build 主地址 403 由 Adapter 尝试 XAI，不在此禁用账号。
				failureHandled = true
			case status == http.StatusForbidden && lease.Credential.Provider == account.ProviderBuild:
				if !account.IsBuildSuper(lease.Credential, lease.Billing) {
					// 非 Super 的 403 按账号级故障处理；auto 模式不会因此回退 XAI。
					s.selector.MarkFailure(failureCtx, lease.Credential, status, 0)
				}
				// Super（Billing paid 或 entitlement）的 403 保持服务级处理。
				failureHandled = true
			case (status == http.StatusPaymentRequired || status == http.StatusTooManyRequests) && lease.QuotaMode != "":
				exhausted, reconcileErr := s.accounts.ReconcileRateLimit(failureCtx, lease.Credential.ID, lease.QuotaMode, 0)
				s.selector.MarkQuotaStateChanged(lease.Credential.Provider, lease.Credential.ID)
				if reconcileErr != nil || !exhausted {
					s.selector.MarkFailure(failureCtx, lease.Credential, status, 0)
				}
				failureHandled = true
			case status >= http.StatusInternalServerError:
				// 5xx 是 Provider 服务级故障，不应让某个账号退出号池。
				failureHandled = true
			default:
				s.selector.MarkFailure(failureCtx, lease.Credential, status, 0)
				failureHandled = true
			}
		}
		if !failureHandled && !provider.IsMediaPostProcessingError(err) {
			s.selector.MarkFailure(failureCtx, lease.Credential, 0, 0)
		}
		failureCancel()
		applyMediaJobEgress(&job, egressTrace, route.Provider)
		s.logVideoGenerationFailure(job, lease.Credential, err)
		failureCode, publicErr := classifyVideoGenerationFailure(err)
		s.failVideoJob(parent, job, failureCode, publicErr)
		return
	}
	now := time.Now().UTC()
	job.Status, job.Progress, job.UpstreamURL, job.ContentType = media.StatusCompleted, 100, result.URL, result.ContentType
	// 成功终态必须清空历史错误字段，避免管理端/恢复路径把中间失败文案当成最终结果。
	job.ErrorCode, job.ErrorMessage = "", ""
	if result.AssetID != "" {
		job.ResultAssetID = result.AssetID
	}
	applyMediaJobEgress(&job, egressTrace, route.Provider)
	job.LeaseUntil, job.UpdatedAt, job.CompletedAt = nil, now, &now
	if err := s.persistVideoJobWithRetry(parent, job); err != nil {
		s.logger.Error("video_job_terminal_write_failed", "job_id", job.ID, "error", err)
		return
	}
	s.selector.MarkSuccess(context.Background(), lease.Credential)
	if lease.QuotaMode != "" && lease.QuotaMode != "weekly" {
		quotaCtx, quotaCancel := context.WithTimeout(context.Background(), accountStateWriteTimeout)
		updated, quotaErr := s.accounts.DecrementQuota(quotaCtx, job.AccountID, lease.QuotaMode, 1)
		quotaCancel()
		if quotaErr != nil {
			s.logger.Warn("video_quota_decrement_failed", "provider", route.Provider, "account_id", job.AccountID, "mode", lease.QuotaMode, "error", quotaErr)
		} else if updated {
			s.selector.ConsumeQuota(route.Provider, job.AccountID, lease.QuotaMode, 1)
		}
	}
	if err := s.recordVideoAudit(context.Background(), job, time.Since(startedAt).Milliseconds()); err != nil {
		s.logger.Error("video_usage_record_failed", "job_id", job.ID, "event_id", "video_usage_"+job.ID, "error", err)
	}
	if quotaKind, _ := s.providers.QuotaKind(route.Provider); quotaKind == provider.QuotaRemoteWindow && lease.QuotaMode != "" {
		s.accounts.QueueQuotaRefresh(job.AccountID, lease.QuotaMode)
	}
	// 输入回收放在账号状态、计费和审计收尾之后，存储抖动不得延迟关键终态逻辑。
	s.releaseVideoInputs(job)
}

func (s *Service) acquireVideoInputSlot(ctx context.Context, references []string) (func(), error) {
	hasLocalInput := false
	for _, reference := range references {
		if strings.HasPrefix(reference, media.InputReferencePrefix) {
			hasLocalInput = true
			break
		}
	}
	if !hasLocalInput || s.mediaInputSlots == nil {
		return func() {}, nil
	}
	select {
	case s.mediaInputSlots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-s.mediaInputSlots }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Service) validateVideoInputReferences(ctx context.Context, references []string) error {
	estimatedBytes := videoInputJSONBaseBytes
	for _, reference := range references {
		fileID, local := media.ParseInputReference(reference)
		if !strings.HasPrefix(reference, media.InputReferencePrefix) {
			if !addVideoReferenceBytes(&estimatedBytes, int64(len(reference))) {
				return ErrVideoInputTooLarge
			}
			continue
		}
		if s.mediaAssets == nil || !local {
			return ErrVideoInputUnavailable
		}
		asset, body, err := s.mediaAssets.OpenInputImage(ctx, fileID)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrVideoInputUnavailable, err)
		}
		_ = body.Close()
		if !addVideoReferenceBytes(&estimatedBytes, materializedVideoReferenceBytes(asset.MIMEType, asset.SizeBytes)) {
			return ErrVideoInputTooLarge
		}
	}
	return nil
}

func (s *Service) resolveVideoInputReferences(ctx context.Context, references []string) ([]string, error) {
	resolved := make([]string, 0, len(references))
	estimatedBytes := videoInputJSONBaseBytes
	for _, reference := range references {
		fileID, local := media.ParseInputReference(reference)
		if !strings.HasPrefix(reference, media.InputReferencePrefix) {
			if !addVideoReferenceBytes(&estimatedBytes, int64(len(reference))) {
				return nil, ErrVideoInputTooLarge
			}
			resolved = append(resolved, reference)
			continue
		}
		if s.mediaAssets == nil || !local {
			return nil, ErrVideoInputUnavailable
		}
		asset, body, err := s.mediaAssets.OpenInputImage(ctx, fileID)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrVideoInputUnavailable, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(body, media.MaxInputJSONBytes+1))
		closeErr := body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("读取视频临时输入: %w", readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("关闭视频临时输入: %w", closeErr)
		}
		if len(data) == 0 || len(data) > media.MaxInputJSONBytes {
			return nil, ErrVideoInputTooLarge
		}
		if !addVideoReferenceBytes(&estimatedBytes, materializedVideoReferenceBytes(asset.MIMEType, int64(len(data)))) {
			return nil, ErrVideoInputTooLarge
		}
		resolved = append(resolved, "data:"+asset.MIMEType+";base64,"+base64.StdEncoding.EncodeToString(data))
	}
	return resolved, nil
}

func materializedVideoReferenceBytes(mimeType string, sizeBytes int64) int64 {
	if sizeBytes <= 0 || sizeBytes > int64(media.MaxInputJSONBytes) {
		return -1
	}
	return int64(len("data:")+len(mimeType)+len(";base64,")) + ((sizeBytes+2)/3)*4
}

func addVideoReferenceBytes(total *int64, referenceBytes int64) bool {
	// 两个引号加一个逗号是保守的单元 JSON 开销（首项不需要逗号）。
	const jsonElementOverhead = 3
	addition := referenceBytes + jsonElementOverhead
	limit := int64(media.MaxInputJSONBytes)
	if referenceBytes < 0 || addition < 0 || *total > limit-addition {
		return false
	}
	*total += addition
	return true
}

// persistRemoteVideo 只重试已经生成的视频结果下载与本地归档，不重新调用生成接口，
// 且所有尝试固定使用创建任务的同一凭据。
func (s *Service) persistRemoteVideo(ctx context.Context, jobID string, adapter provider.VideoAdapter, credential account.Credential, result provider.VideoResult) (provider.VideoResult, error) {
	if s.mediaAssets == nil {
		return result, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, errors.New("视频媒体存储未配置"))
	}
	downloader, ok := adapter.(provider.VideoContentDownloader)
	if !ok {
		return result, provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, errors.New("Provider 不支持视频内容下载"))
	}
	var lastErr error
	for attempt := 0; attempt < videoOutputAttempts; attempt++ {
		body, contentType, _, downloadErr := downloader.DownloadVideo(ctx, credential, result.URL)
		if downloadErr != nil {
			lastErr = provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, downloadErr)
		} else {
			asset, saveErr := s.mediaAssets.SaveVideo(ctx, jobID, contentType, body)
			_ = body.Close()
			if saveErr == nil {
				result.AssetID = asset.ID
				result.ContentType = asset.MIMEType
				return result, nil
			}
			lastErr = provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, saveErr)
		}
		if ctx.Err() != nil || attempt+1 >= videoOutputAttempts {
			break
		}
		if waitErr := waitVideoOutputRetry(ctx, attempt); waitErr != nil {
			return result, waitErr
		}
	}
	return result, lastErr
}

func waitVideoOutputRetry(ctx context.Context, attempt int) error {
	delays := [...]time.Duration{200 * time.Millisecond, 750 * time.Millisecond}
	timer := time.NewTimer(delays[min(attempt, len(delays)-1)])
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *Service) reconcileVideoUsage(ctx context.Context) error {
	jobs, err := s.mediaJobs.ListUnrecordedTerminalMediaJobs(ctx, 200)
	if err != nil {
		return err
	}
	var result error
	for _, job := range jobs {
		durationMS := int64(0)
		if job.CompletedAt != nil {
			durationMS = max(int64(0), job.CompletedAt.Sub(job.CreatedAt).Milliseconds())
		}
		if err := s.recordVideoAudit(ctx, job, durationMS); err != nil {
			result = firstError(result, fmt.Errorf("任务 %s: %w", job.ID, err))
		}
	}
	return result
}

func (s *Service) recordVideoAudit(ctx context.Context, job media.Job, durationMS int64) error {
	var accountID *uint64
	if job.AccountID > 0 {
		value := job.AccountID
		accountID = &value
	}
	createdAt := time.Now().UTC()
	if job.CompletedAt != nil && !job.CompletedAt.IsZero() {
		createdAt = job.CompletedAt.UTC()
	}
	statusCode := http.StatusOK
	if job.Status == media.StatusFailed {
		statusCode = http.StatusBadGateway
		switch job.ErrorCode {
		case "account_unavailable", "provider_unavailable":
			statusCode = http.StatusServiceUnavailable
		case "model_not_found":
			statusCode = http.StatusNotFound
		}
	}
	record := audit.Record{
		EventID: "video_usage_" + job.ID, RequestID: job.RequestID, ClientKeyID: job.ClientKeyID, ClientKeyName: job.ClientKeyName,
		ModelRouteID: job.ModelRouteID, ModelPublicID: job.Model, ModelUpstreamModel: job.UpstreamModel,
		Provider: job.Provider, Operation: audit.OperationVideo, UsageSource: audit.UsageSourceNone,
		AccountID: accountID, AccountName: job.AccountName, StatusCode: statusCode, ErrorCode: job.ErrorCode,
		EgressNodeID: job.EgressNodeID, EgressNodeName: job.EgressNodeName, EgressScope: job.EgressScope, EgressMode: audit.EgressMode(job.EgressMode),
		MediaInputImages: int64(job.InputImageCount),
		DurationMS:       durationMS, CreatedAt: createdAt,
	}
	if job.Status == media.StatusCompleted {
		record.MediaOutputSeconds = int64(max(0, job.Seconds))
	}
	if pricing, ok := audit.EstimateOfficialVideoCost(job.Model, job.Quality, job.Seconds); ok && job.Status == media.StatusCompleted {
		record.EstimatedCostInUSDTicks = pricing.CostInUSDTicks
		record.PricingModel = pricing.Model
		record.PricingVersion = audit.OfficialPricingAsOf
	}
	if durable, ok := s.audits.(interface {
		CreateDurable(context.Context, audit.Record) error
	}); ok {
		if err := durable.CreateDurable(ctx, record); err != nil {
			return err
		}
	} else if err := s.audits.Create(ctx, record); err != nil {
		return err
	}
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	return s.mediaJobs.MarkMediaJobUsageRecorded(markCtx, job.ID, time.Now().UTC())
}

// videoJobInput is persisted in media_jobs.input_json.
// Legacy jobs only have image_urls; new jobs prefer mode + first_frame_url / reference_urls / source_video_url.
type videoJobInput struct {
	Mode           string
	FirstFrameURL  string
	ReferenceURLs  []string
	SourceVideoURL string
}

func (in videoJobInput) allURLs() []string {
	out := make([]string, 0, 2+len(in.ReferenceURLs))
	if strings.TrimSpace(in.FirstFrameURL) != "" {
		out = append(out, strings.TrimSpace(in.FirstFrameURL))
	}
	for _, u := range in.ReferenceURLs {
		if v := strings.TrimSpace(u); v != "" {
			out = append(out, v)
		}
	}
	if v := strings.TrimSpace(in.SourceVideoURL); v != "" {
		out = append(out, v)
	}
	return out
}

func trimVideoURLList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if v := strings.TrimSpace(value); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func encodeVideoJobInput(input videoJobInput) (string, error) {
	mode := normalizeVideoMode(input.Mode)
	first := strings.TrimSpace(input.FirstFrameURL)
	refs := trimVideoURLList(input.ReferenceURLs)
	source := strings.TrimSpace(input.SourceVideoURL)
	// Prefer compact structured form. Also emit legacy image_urls as the flat union so
	// older release paths that only read image_urls still free temporary inputs.
	// When only references are present (common R2V / legacy generate), store the flat list alone
	// to avoid doubling large base64 payloads.
	if mode == provider.VideoModeGenerate && first == "" && source == "" {
		return encodeVideoInput(refs)
	}
	payload := map[string]any{}
	if mode != provider.VideoModeGenerate {
		payload["mode"] = mode
	}
	if first != "" {
		payload["first_frame_url"] = first
	}
	if len(refs) > 0 {
		payload["reference_urls"] = refs
	}
	if source != "" {
		payload["source_video_url"] = source
	}
	if all := input.allURLs(); len(all) > 0 {
		payload["image_urls"] = all
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("编码视频输入: %w", err)
	}
	if len(data) > media.MaxInputJSONBytes {
		return "", ErrVideoInputTooLarge
	}
	return string(data), nil
}

// encodeVideoInput stores the historical flat image_urls list (size-limit tests + legacy jobs).
func encodeVideoInput(referenceURLs []string) (string, error) {
	data, err := json.Marshal(map[string][]string{"image_urls": referenceURLs})
	if err != nil {
		return "", fmt.Errorf("编码视频输入: %w", err)
	}
	if len(data) > media.MaxInputJSONBytes {
		return "", ErrVideoInputTooLarge
	}
	return string(data), nil
}

func decodeVideoJobInput(value string) videoJobInput {
	var raw struct {
		Mode           string   `json:"mode"`
		FirstFrameURL  string   `json:"first_frame_url"`
		ReferenceURLs  []string `json:"reference_urls"`
		SourceVideoURL string   `json:"source_video_url"`
		ImageURLs      []string `json:"image_urls"`
	}
	_ = json.Unmarshal([]byte(value), &raw)
	mode := normalizeVideoMode(raw.Mode)
	first := strings.TrimSpace(raw.FirstFrameURL)
	refs := trimVideoURLList(raw.ReferenceURLs)
	source := strings.TrimSpace(raw.SourceVideoURL)
	if first != "" || len(refs) > 0 || source != "" || mode != provider.VideoModeGenerate {
		return videoJobInput{Mode: mode, FirstFrameURL: first, ReferenceURLs: refs, SourceVideoURL: source}
	}
	// Legacy: only image_urls. Preserve prior multi-ref Web behavior by treating them as references.
	// Console GenerateVideo still maps a lone legacy URL to image when FirstFrame is empty and len==1.
	return videoJobInput{Mode: provider.VideoModeGenerate, ReferenceURLs: trimVideoURLList(raw.ImageURLs)}
}

func decodeVideoInput(value string) []string {
	return decodeVideoJobInput(value).allURLs()
}

func (s *Service) resolveVideoJobInput(ctx context.Context, input videoJobInput) (videoJobInput, error) {
	mode := normalizeVideoMode(input.Mode)
	all := input.allURLs()
	resolvedAll, err := s.resolveVideoInputReferences(ctx, all)
	if err != nil {
		return videoJobInput{}, err
	}
	out := videoJobInput{Mode: mode}
	if len(resolvedAll) == 0 {
		return out, nil
	}
	hasFirst := strings.TrimSpace(input.FirstFrameURL) != ""
	hasSource := strings.TrimSpace(input.SourceVideoURL) != ""
	refCount := len(trimVideoURLList(input.ReferenceURLs))
	// Structured jobs preserve slot order: first frame, references, source video.
	if hasFirst || refCount > 0 || hasSource {
		idx := 0
		if hasFirst {
			out.FirstFrameURL = resolvedAll[idx]
			idx++
		}
		if refCount > 0 {
			end := min(idx+refCount, len(resolvedAll))
			out.ReferenceURLs = append([]string(nil), resolvedAll[idx:end]...)
			idx = end
		}
		if hasSource && idx < len(resolvedAll) {
			out.SourceVideoURL = resolvedAll[idx]
		}
		return out, nil
	}
	// Legacy flat image_urls → references for generate.
	out.ReferenceURLs = append([]string(nil), resolvedAll...)
	return out, nil
}

// classifyVideoGenerationFailure maps provider/local failures to stable job error codes and client-safe messages.
func classifyVideoGenerationFailure(err error) (code string, public error) {
	if err == nil {
		return "generation_failed", errors.New("视频生成失败")
	}
	var invalid *InvalidRequestError
	if errors.As(err, &invalid) {
		return "invalid_argument", invalid
	}
	if errors.Is(err, provider.ErrUnauthorized) {
		return "provider_unavailable", errors.New("上游服务暂不可用")
	}
	message := err.Error()
	lower := strings.ToLower(message)
	// Prefer specific, actionable mappings before generic fallbacks.
	if strings.Contains(lower, "auth context expired") || strings.Contains(lower, "auth_context_expired") {
		return "auth_expired", errors.New("Console 鉴权已过期，请重新导入或刷新 Console SSO")
	}
	if status, ok := provider.ErrorHTTPStatus(err); ok {
		switch {
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			// Keep upstream text when present; otherwise a stable auth hint.
			if strings.TrimSpace(message) != "" && !strings.EqualFold(message, provider.ErrUnauthorized.Error()) {
				return "auth_expired", publicVideoClientMessage(err)
			}
			return "auth_expired", errors.New("Console 鉴权失效，请重新导入或刷新 Console SSO")
		case status == http.StatusTooManyRequests:
			return "rate_limited", errors.New("上游视频额度或速率受限，请稍后重试")
		case status == http.StatusPaymentRequired:
			return "quota_exhausted", errors.New("上游视频额度不足")
		case status == http.StatusBadRequest:
			return "invalid_argument", publicVideoClientMessage(err)
		case status >= http.StatusInternalServerError:
			// Surface sanitized upstream detail instead of a fixed "请稍后重试".
			return "generation_failed", publicVideoClientMessage(err)
		}
	}
	switch {
	case strings.Contains(message, "不能同时"), strings.Contains(message, "必须提供"),
		strings.Contains(message, "仅支持"), strings.Contains(message, "不支持"),
		strings.Contains(message, "最多支持"), strings.Contains(message, "必须是"),
		strings.Contains(lower, "invalid"):
		return "invalid_argument", publicVideoClientMessage(err)
	case strings.Contains(lower, "rate"), strings.Contains(message, "额度"), strings.Contains(lower, "quota"),
		strings.Contains(lower, "resource-exhausted"), strings.Contains(lower, "resource_exhausted"):
		return "rate_limited", errors.New("上游视频额度或速率受限，请稍后重试")
	default:
		return "generation_failed", publicVideoClientMessage(err)
	}
}

func publicVideoClientMessage(err error) error {
	if err == nil {
		return errors.New("视频生成失败")
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	// Avoid leaking raw multi-line upstream dumps; keep short actionable text.
	if len(message) > 240 {
		message = message[:240]
	}
	// Soften historical "首图" wording that misleads R2V clients.
	message = strings.ReplaceAll(message, "最多支持 1 张首图", "当前路径最多支持 1 张 image 输入")
	message = strings.ReplaceAll(message, "视频首图", "image（I2V 首帧）")
	return errors.New(message)
}

func (s *Service) failVideoJob(ctx context.Context, job media.Job, code string, err error) {
	now := time.Now().UTC()
	job.Status, job.ErrorCode, job.ErrorMessage = media.StatusFailed, code, err.Error()
	if len(job.ErrorMessage) > 512 {
		job.ErrorMessage = job.ErrorMessage[:512]
	}
	job.LeaseUntil, job.UpdatedAt, job.CompletedAt = nil, now, &now
	if updateErr := s.persistVideoJobWithRetry(ctx, job); updateErr != nil {
		s.logger.Error("video_job_terminal_write_failed", "job_id", job.ID, "error", updateErr)
		return
	}
	if auditErr := s.recordVideoAudit(context.Background(), job, max(int64(0), now.Sub(job.CreatedAt).Milliseconds())); auditErr != nil {
		s.logger.Error("video_usage_record_failed", "job_id", job.ID, "event_id", "video_usage_"+job.ID, "error", auditErr)
	}
	s.cancelBillingReservation("video_usage_" + job.ID)
	s.releaseVideoInputs(job)
}

func (s *Service) releaseVideoInputs(job media.Job) {
	if s.mediaAssets == nil {
		return
	}
	references := decodeVideoJobInput(job.InputJSON).allURLs()
	if len(references) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.mediaAssets.ReleaseInputImages(ctx, references); err != nil {
		logger := s.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("video_input_release_failed", "job_id", job.ID, "error", err)
	}
}

func (s *Service) logVideoGenerationFailure(job media.Job, credential account.Credential, err error) {
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	attributes := []any{
		"job_id", job.ID,
		"request_id", job.RequestID,
		"account_id", credential.ID,
		"provider", credential.Provider,
		"model", job.UpstreamModel,
		"egress_scope", job.EgressScope,
		"egress_mode", job.EgressMode,
		"error", sanitizeDiagnosticText(err.Error(), 512),
	}
	if status, ok := provider.ErrorHTTPStatus(err); ok {
		attributes = append(attributes, "upstream_status", status)
	}
	if job.EgressNodeID != nil {
		attributes = append(attributes, "egress_node_id", *job.EgressNodeID, "egress_node_name", job.EgressNodeName)
	}
	logger.Warn("video_generation_failed", attributes...)
}

func (s *Service) deferVideoJob(ctx context.Context, job media.Job) {
	now := time.Now().UTC()
	leaseUntil := now.Add(5 * time.Minute)
	job.Status = media.StatusInProgress
	job.LeaseUntil = &leaseUntil
	job.UpdatedAt = now
	job.ErrorCode = ""
	job.ErrorMessage = ""
	if err := s.persistVideoJobWithRetry(ctx, job); err != nil {
		s.logger.Error("video_job_defer_write_failed", "job_id", job.ID, "error", err)
	}
}

// persistVideoJobWithRetry 至少执行一次收尾写入；后续退避可被工作进程关闭信号取消。
func (s *Service) persistVideoJobWithRetry(ctx context.Context, job media.Job) error {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		lastErr = s.mediaJobs.UpdateMediaJob(writeCtx, job)
		cancel()
		if lastErr == nil {
			return nil
		}
		if attempt < 3 {
			timer := time.NewTimer(time.Duration(attempt) * 100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return errors.Join(lastErr, ctx.Err())
			case <-timer.C:
			}
		}
	}
	return lastErr
}
