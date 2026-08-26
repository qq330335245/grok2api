package antidegrade

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/application/antidegrade"
	settingsapp "github.com/chenyme/grok2api/backend/internal/application/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	controller *antidegrade.Controller
	accounts   *accountapp.Service
	settings   *settingsapp.Service
}

func NewHandler(controller *antidegrade.Controller, accounts *accountapp.Service, settings *settingsapp.Service) *Handler {
	return &Handler{controller: controller, accounts: accounts, settings: settings}
}

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/antidegrade/status", h.status)
	router.PUT("/antidegrade/config", h.updateConfig)
	router.POST("/antidegrade/accounts/:id/clear", h.clearAccount)
	router.POST("/antidegrade/ips/clear", h.clearIP)
}

type configDTO struct {
	Enabled                bool    `json:"enabled"`
	Mode                   string  `json:"mode"`
	ThinkingMinOutput      int     `json:"thinkingMinOutput"`
	DensityWindow          string  `json:"densityWindow"`
	DensityMaxAccounts     int     `json:"densityMaxAccounts"`
	DirtyIPCooldown        string  `json:"dirtyIpCooldown"`
	FarmIPCooldown         string  `json:"farmIpCooldown"`
	MaxIPRetries           int     `json:"maxIpRetries"`
	AccountIPFailThreshold int     `json:"failExitThreshold"`
	AccountQuarantineTTL   string  `json:"accountQuarantineTtl"`
	ScorePrior             float64 `json:"scorePrior"`
	ExploreRatio           float64 `json:"exploreRatio"`
	OperatorOverride       string  `json:"operatorOverride"`
}

type accountRefDTO struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type ipDTO struct {
	ExitIP                string          `json:"exitIp"`
	NodeIDs               []string        `json:"nodeIds"`
	NodeNames             []string        `json:"nodeNames"`
	Accounts              []accountRefDTO `json:"accounts"`
	AccountCount          int             `json:"accountCount"`
	AccountLimit          int             `json:"accountLimit"`
	Cooling               bool            `json:"cooling"`
	CooldownUntil         *time.Time      `json:"cooldownUntil,omitempty"`
	CooldownReason        string          `json:"cooldownReason,omitempty"`
	OperatorOverrideUntil *time.Time      `json:"operatorOverrideUntil,omitempty"`
	Score                 float64         `json:"score"`
}

type quarantinedDTO struct {
	ID               string    `json:"id"`
	Name             string    `json:"name,omitempty"`
	FailedIPs        []string  `json:"failedExitIps"`
	QuarantineUntil  time.Time `json:"quarantineUntil"`
	QuarantineReason string    `json:"quarantineReason,omitempty"`
}

type eventDTO struct {
	At          time.Time `json:"at"`
	Success     bool      `json:"success"`
	AccountID   string    `json:"accountId,omitempty"`
	AccountName string    `json:"accountName,omitempty"`
	ExitIP      string    `json:"exitIp"`
}

type statusDTO struct {
	Revision    uint64           `json:"revision,string"`
	Config      configDTO        `json:"config"`
	IPs         []ipDTO          `json:"ips"`
	Quarantined []quarantinedDTO `json:"quarantined"`
	Events      []eventDTO       `json:"events"`
}

type updateConfigRequest struct {
	Revision uint64    `json:"revision,string"`
	Config   configDTO `json:"config"`
}

type clearIPRequest struct {
	ExitIP string `json:"exitIp"`
}

func (h *Handler) status(c *gin.Context) {
	if h.controller == nil {
		response.Error(c, http.StatusServiceUnavailable, "antidegradeUnavailable", "反降智未启用")
		return
	}
	snapshot := h.controller.Snapshot(c.Request.Context())
	names := h.accountNames(c.Request.Context(), collectAccountIDs(snapshot))
	revision := uint64(0)
	if h.settings != nil {
		revision = h.settings.Get().Revision
	}
	response.Success(c, http.StatusOK, statusDTO{
		Revision: revision, Config: configToDTO(snapshot.Config),
		IPs: ipDTOs(snapshot.IPs, names), Quarantined: quarantinedDTOs(snapshot.Quarantined, names),
		Events: eventDTOs(snapshot.Events, names),
	})
}

func (h *Handler) updateConfig(c *gin.Context) {
	if h.controller == nil || h.settings == nil {
		response.Error(c, http.StatusServiceUnavailable, "antidegradeUnavailable", "反降智未启用")
		return
	}
	var request updateConfigRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	input, err := dtoToSettings(request.Config)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", err.Error())
		return
	}
	_, err = h.settings.UpdateAntiDegrade(c.Request.Context(), request.Revision, input)
	if errors.Is(err, settingsapp.ErrConflict) {
		response.Error(c, http.StatusConflict, "settingsConflict", "运行设置已被其他会话更新")
		return
	}
	if errors.Is(err, settingsapp.ErrInvalidInput) {
		response.Error(c, http.StatusBadRequest, "invalidRequest", err.Error())
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "antidegradeConfigFailed", "保存反降智设置失败")
		return
	}
	h.status(c)
}

func (h *Handler) clearAccount(c *gin.Context) {
	if h.controller == nil {
		response.Error(c, http.StatusServiceUnavailable, "antidegradeUnavailable", "反降智未启用")
		return
	}
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "账号 ID 无效")
		return
	}
	h.controller.ClearAccount(c.Request.Context(), id)
	h.status(c)
}

func (h *Handler) clearIP(c *gin.Context) {
	if h.controller == nil {
		response.Error(c, http.StatusServiceUnavailable, "antidegradeUnavailable", "反降智未启用")
		return
	}
	var request clearIPRequest
	if c.ShouldBindJSON(&request) != nil || strings.TrimSpace(request.ExitIP) == "" {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "出口 IP 无效")
		return
	}
	h.controller.ClearIP(c.Request.Context(), request.ExitIP)
	h.status(c)
}

func (h *Handler) accountNames(ctx context.Context, ids []uint64) map[uint64]string {
	names := make(map[uint64]string, len(ids))
	if h.accounts == nil {
		return names
	}
	for _, id := range ids {
		view, err := h.accounts.Get(ctx, id)
		if err != nil {
			continue
		}
		name := strings.TrimSpace(view.Credential.Name)
		if name == "" {
			name = strings.TrimSpace(view.Credential.Email)
		}
		if name != "" {
			names[id] = name
		}
	}
	return names
}

func collectAccountIDs(snapshot antidegrade.Status) []uint64 {
	seen := map[uint64]struct{}{}
	ids := make([]uint64, 0)
	add := func(id uint64) {
		if id == 0 {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for _, ip := range snapshot.IPs {
		for _, id := range ip.AccountIDs {
			add(id)
		}
	}
	for _, account := range snapshot.Quarantined {
		add(account.ID)
	}
	for _, event := range snapshot.Events {
		add(event.AccountID)
	}
	return ids
}

func configToDTO(cfg antidegrade.Config) configDTO {
	cfg = cfg.Normalize()
	return configDTO{
		Enabled: cfg.Enabled, Mode: cfg.Mode, ThinkingMinOutput: int(cfg.ThinkingMinOutput),
		DensityWindow: formatDuration(cfg.DensityWindow), DensityMaxAccounts: cfg.DensityMaxAccounts,
		DirtyIPCooldown: formatDuration(cfg.DirtyIPCooldown), FarmIPCooldown: formatDuration(cfg.FarmIPCooldown),
		MaxIPRetries: cfg.MaxIPRetries, AccountIPFailThreshold: cfg.AccountIPFailThreshold,
		AccountQuarantineTTL: formatDuration(cfg.AccountQuarantineTTL), ScorePrior: cfg.ScorePrior,
		ExploreRatio: cfg.ExploreRatio, OperatorOverride: formatDuration(cfg.OperatorOverride),
	}
}

func dtoToSettings(value configDTO) (settingsapp.AntiDegradeConfig, error) {
	mode := strings.TrimSpace(value.Mode)
	if mode == "" {
		mode = antidegrade.ModeEnforce
	}
	return settingsapp.AntiDegradeConfig{
		Enabled: value.Enabled, Mode: mode, ThinkingMinOutput: value.ThinkingMinOutput,
		DensityWindow: value.DensityWindow, DensityMaxAccounts: value.DensityMaxAccounts,
		DirtyIPCooldown: value.DirtyIPCooldown, FarmIPCooldown: value.FarmIPCooldown,
		MaxIPRetries: value.MaxIPRetries, AccountIPFailThreshold: value.AccountIPFailThreshold,
		AccountQuarantineTTL: value.AccountQuarantineTTL, ScorePrior: value.ScorePrior,
		ExploreRatio: value.ExploreRatio, OperatorOverride: value.OperatorOverride,
	}, nil
}

func ipDTOs(values []antidegrade.IPStatus, names map[uint64]string) []ipDTO {
	result := make([]ipDTO, 0, len(values))
	for _, value := range values {
		nodeIDs := make([]string, 0, len(value.NodeIDs))
		for _, id := range value.NodeIDs {
			nodeIDs = append(nodeIDs, strconv.FormatUint(id, 10))
		}
		result = append(result, ipDTO{
			ExitIP: value.ExitIP, NodeIDs: nodeIDs, NodeNames: value.NodeNames,
			Accounts: accountRefs(value.AccountIDs, names), AccountCount: value.AccountCount, AccountLimit: value.AccountLimit,
			Cooling: value.Cooling, CooldownUntil: optionalTime(value.CooldownUntil), CooldownReason: value.CooldownReason,
			OperatorOverrideUntil: optionalTime(value.OperatorOverrideUntil), Score: value.Score,
		})
	}
	return result
}

func quarantinedDTOs(values []antidegrade.AccountStatus, names map[uint64]string) []quarantinedDTO {
	result := make([]quarantinedDTO, 0, len(values))
	for _, value := range values {
		result = append(result, quarantinedDTO{
			ID: strconv.FormatUint(value.ID, 10), Name: names[value.ID], FailedIPs: value.FailedIPs,
			QuarantineUntil: value.QuarantineUntil, QuarantineReason: value.QuarantineReason,
		})
	}
	return result
}

func eventDTOs(values []antidegrade.EventStatus, names map[uint64]string) []eventDTO {
	result := make([]eventDTO, 0, len(values))
	for _, value := range values {
		accountID := ""
		if value.AccountID != 0 {
			accountID = strconv.FormatUint(value.AccountID, 10)
		}
		result = append(result, eventDTO{
			At: value.At, Success: value.Success, AccountID: accountID, AccountName: names[value.AccountID], ExitIP: value.ExitIP,
		})
	}
	return result
}

func accountRefs(ids []uint64, names map[uint64]string) []accountRefDTO {
	result := make([]accountRefDTO, 0, len(ids))
	for _, id := range ids {
		result = append(result, accountRefDTO{ID: strconv.FormatUint(id, 10), Name: names[id]})
	}
	return result
}

func optionalTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

func formatDuration(value time.Duration) string {
	return config.Duration(value).String()
}
