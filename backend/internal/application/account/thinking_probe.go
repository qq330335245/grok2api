package account

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

const (
	botRiskProbeAttempts  = 2
	botRiskProbeModel     = "grok-4.5"
	botRiskProbePrompt    = "Think step by step before answering. What is 17 multiplied by 19? Give the integer result."
	botRiskAttemptTimeout = 75 * time.Second
)

type thinkingProbeVerdict int

const (
	probeInconclusive thinkingProbeVerdict = iota
	probeThinking
	probeMissingThinking
)

func (v thinkingProbeVerdict) String() string {
	switch v {
	case probeThinking:
		return "thinking"
	case probeMissingThinking:
		return "missing"
	default:
		return "inconclusive"
	}
}

func stickyProbeIdentity(credential accountdomain.Credential, attempt int) string {
	base := strings.TrimSpace(credential.EgressIdentity)
	if base == "" {
		providerName := string(credential.Provider)
		if providerName == "" {
			providerName = string(accountdomain.ProviderBuild)
		}
		base = providerName + "_" + strconv.FormatUint(credential.ID, 10)
	}
	if attempt < 1 {
		attempt = 1
	}
	return base + "+" + strconv.Itoa(attempt)
}

type thinkingScan struct {
	verdict thinkingProbeVerdict
	event   string
	delta   string
}

func scanThinkingSSE(r io.Reader) thinkingScan {
	if r == nil {
		return thinkingScan{verdict: probeInconclusive}
	}
	reader := bufio.NewReader(r)
	var result thinkingScan
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				payload := bytes.TrimSpace(trimmed[5:])
				if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
					observeThinkingPayload(payload, &result)
					if result.verdict == probeThinking || result.verdict == probeMissingThinking {
						return result
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return result
		}
	}
	return result
}

func observeThinkingPayload(payload []byte, result *thinkingScan) {
	var event struct {
		Type    string          `json:"type"`
		Delta   json.RawMessage `json:"delta"`
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				Reasoning        string `json:"reasoning"`
				ReasoningContent string `json:"reasoning_content"`
				ThinkingContent  string `json:"thinking_content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	switch event.Type {
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		if text := strings.TrimSpace(rawJSONString(event.Delta)); text != "" {
			noteThinking(result, event.Type, text)
		}
	case "response.output_text.delta":
		if text := strings.TrimSpace(rawJSONString(event.Delta)); text != "" {
			noteContent(result, event.Type, text)
		}
	}
	for _, choice := range event.Choices {
		delta := choice.Delta
		switch {
		case strings.TrimSpace(delta.ReasoningContent) != "":
			noteThinking(result, "reasoning_content", delta.ReasoningContent)
		case strings.TrimSpace(delta.ThinkingContent) != "":
			noteThinking(result, "thinking_content", delta.ThinkingContent)
		case strings.TrimSpace(delta.Reasoning) != "":
			noteThinking(result, "reasoning", delta.Reasoning)
		}
		if delta.Content != "" {
			noteContent(result, "content", delta.Content)
		}
	}
}

func noteThinking(result *thinkingScan, event, delta string) {
	if result.verdict == probeThinking {
		return
	}
	result.verdict = probeThinking
	result.event = event
	result.delta = truncateRunes(delta, 80)
}

func noteContent(result *thinkingScan, event, delta string) {
	if result.verdict == probeThinking || result.verdict == probeMissingThinking {
		return
	}
	result.verdict = probeMissingThinking
	result.event = event
	result.delta = truncateRunes(delta, 80)
}

func rawJSONString(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '"' {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return ""
	}
	return text
}

func truncateRunes(value string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max]) + "…"
}

func formatProbeTarget(attempt BotRiskProbeAttempt) string {
	parts := []string{strings.TrimSpace(attempt.Identity)}
	if name := strings.TrimSpace(attempt.NodeName); name != "" {
		parts = append(parts, name)
	}
	if ip := strings.TrimSpace(attempt.ExitIP); ip != "" {
		parts = append(parts, ip)
	}
	return strings.Join(parts, " ")
}

func formatFailedProbeReason(attempt BotRiskProbeAttempt) string {
	parts := make([]string, 0, 4)
	if target := formatProbeTarget(attempt); target != "" {
		parts = append(parts, target)
	}
	if attempt.Status > 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", attempt.Status))
	}
	detail := strings.TrimSpace(attempt.Detail)
	if detail == "" {
		detail = "无有效思考样本"
	}
	parts = append(parts, detail)
	return strings.Join(parts, " · ")
}

func summarizeProbeFailure(status int, body []byte, err error) string {
	if err != nil {
		return truncateRunes(strings.TrimSpace(err.Error()), 160)
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && bytes.Contains(bytes.ToLower(trimmed), []byte("<html")) {
		return "HTML 响应（可能是 Cloudflare 挑战）"
	}
	code, _, message := provider.ExtractUpstreamErrorMetadata(body)
	switch {
	case strings.TrimSpace(message) != "" && strings.TrimSpace(code) != "":
		return truncateRunes(strings.TrimSpace(code)+": "+strings.TrimSpace(message), 160)
	case strings.TrimSpace(message) != "":
		return truncateRunes(strings.TrimSpace(message), 160)
	case strings.TrimSpace(code) != "":
		return truncateRunes(strings.TrimSpace(code), 160)
	case len(trimmed) > 0:
		return truncateRunes(string(trimmed), 160)
	case status > 0:
		return fmt.Sprintf("上游返回 HTTP %d", status)
	default:
		return "请求失败"
	}
}

func (s *Service) inspectBuildBotRisk(ctx context.Context, value accountdomain.Credential) BuildDetectItemResult {
	item := BuildDetectItemResult{AccountID: value.ID, Name: value.Name, Email: value.Email, Outcome: BuildDetectOutcomeFailed}
	if value.Provider != accountdomain.ProviderBuild {
		item.Reason = "仅 Grok Build 账号支持风控检测"
		return item
	}
	billing, err := s.loadDetectBilling(ctx, value.ID)
	if err != nil {
		item.Reason = err.Error()
		return item
	}
	misses := 0
	attempts := make([]BotRiskProbeAttempt, 0, botRiskProbeAttempts)
	for n := 1; n <= botRiskProbeAttempts; n++ {
		if ctx.Err() != nil {
			item.Attempts = attempts
			item.Reason = "检测取消"
			if len(attempts) > 0 {
				item.Reason = "检测取消 · " + formatFailedProbeReason(attempts[len(attempts)-1])
			} else if err := ctx.Err(); err != nil {
				item.Reason = "检测取消 · " + err.Error()
			}
			return item
		}
		scan, status, body, attempt, probeErr := s.probeThinkingOnStickyIdentity(ctx, value, billing, n)
		attempts = append(attempts, attempt)
		item.Attempts = attempts
		if status != 0 {
			item.HTTPStatus = status
		}
		if probeErr != nil {
			item.Reason = formatFailedProbeReason(attempt)
			return item
		}
		if quotaItem, ok := s.finishBotRiskQuotaRejection(ctx, value, billing, attempt.Identity, status, body); ok {
			quotaItem.Attempts = attempts
			if target := formatProbeTarget(attempt); target != "" {
				quotaItem.Reason = quotaItem.Reason + " · " + target
			}
			return quotaItem
		}
		s.logger.Info("bot_risk_probe_attempt", "account_id", value.ID, "identity", attempt.Identity, "node", attempt.NodeName, "exit_ip", attempt.ExitIP, "status", status, "verdict", scan.verdict.String(), "event", scan.event, "delta", scan.delta, "detail", attempt.Detail)
		switch scan.verdict {
		case probeThinking:
			if persistErr := s.persistPageBotFlag(ctx, value, 0); persistErr != nil {
				item.Reason = persistErr.Error()
				return item
			}
			item.BotFlagSource = 0
			item.Outcome = BuildDetectOutcomeOK
			item.Reason = fmt.Sprintf("thinking on %s (%s)", formatProbeTarget(attempt), scan.event)
			return item
		case probeMissingThinking:
			misses++
		default:
			item.Reason = formatFailedProbeReason(attempt)
			return item
		}
	}
	if misses < botRiskProbeAttempts {
		item.Reason = "有效无思考样本不足"
		return item
	}
	if persistErr := s.persistPageBotFlag(ctx, value, 2); persistErr != nil {
		item.Reason = persistErr.Error()
		return item
	}
	item.BotFlagSource = 2
	item.Outcome = BuildDetectOutcomeFlagged
	targets := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		targets = append(targets, formatProbeTarget(attempt))
	}
	item.Reason = fmt.Sprintf("no thinking on %s", strings.Join(targets, ", "))
	return item
}

func (s *Service) probeThinkingOnStickyIdentity(ctx context.Context, value accountdomain.Credential, billing *accountdomain.Billing, attempt int) (thinkingScan, int, []byte, BotRiskProbeAttempt, error) {
	identity := stickyProbeIdentity(value, attempt)
	result := BotRiskProbeAttempt{Identity: identity, Verdict: probeInconclusive.String()}
	adapter, ok := s.providers.Responses(accountdomain.ProviderBuild)
	if !ok {
		result.Detail = fmt.Sprintf("Provider %s 未注册 Responses 能力", accountdomain.ProviderBuild)
		return thinkingScan{verdict: probeInconclusive}, 0, nil, result, fmt.Errorf("%s", result.Detail)
	}
	attemptCtx, cancel := context.WithTimeout(ctx, botRiskAttemptTimeout)
	defer cancel()
	attemptCtx, trace := infraegress.WithTrace(infraegress.WithExitObservation(infraegress.WithStickyLeasePreference(attemptCtx)))
	probe := value
	probe.EgressIdentity = identity
	body := []byte(fmt.Sprintf(`{"model":%q,"input":%q,"stream":true,"reasoning":{"effort":"high"}}`, botRiskProbeModel, botRiskProbePrompt))
	response, err := adapter.ForwardResponse(attemptCtx, provider.ResponseResourceRequest{
		Credential:    probe,
		Billing:       billing,
		Method:        http.MethodPost,
		Path:          "/responses",
		Model:         botRiskProbeModel,
		Body:          body,
		NormalizeBody: true,
		Streaming:     true,
	})
	if sel, ok := trace.Selection(domainegress.ScopeBuild); ok {
		result.NodeName = strings.TrimSpace(sel.NodeName)
		result.ExitIP = strings.TrimSpace(sel.ExitIP)
		if result.NodeName == "" && sel.NodeID == 0 {
			result.NodeName = "direct"
		}
	}
	if err != nil {
		status := providerStatus(err)
		result.Status = status
		result.Detail = summarizeProbeFailure(status, nil, err)
		return thinkingScan{verdict: probeInconclusive}, status, nil, result, nil
	}
	if response.Body != nil {
		defer response.Body.Close()
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body := readDetectBodyForClassification(response.Body)
		if len(body) == 0 && response.Diagnostic != nil {
			body = response.Diagnostic.Body
		}
		result.Status = response.StatusCode
		result.Detail = summarizeProbeFailure(response.StatusCode, body, nil)
		return thinkingScan{verdict: probeInconclusive}, response.StatusCode, body, result, nil
	}
	scan := scanThinkingSSE(response.Body)
	result.Status = response.StatusCode
	result.Verdict = scan.verdict.String()
	if scan.verdict == probeInconclusive {
		result.Detail = "空流，无思考/正文 delta"
	}
	return scan, response.StatusCode, nil, result, nil
}

func providerStatus(err error) int {
	status, _ := provider.ErrorHTTPStatus(err)
	return status
}

func (s *Service) finishBotRiskQuotaRejection(ctx context.Context, value accountdomain.Credential, billing *accountdomain.Billing, identity string, status int, body []byte) (BuildDetectItemResult, bool) {
	rejection := provider.ClassifyCredentialRejection(status, body, nil)
	if !rejection.QuotaExhausted && !rejection.SpendingLimitBlocked && !rejection.ModelQuotaExhausted {
		return BuildDetectItemResult{}, false
	}
	item := BuildDetectItemResult{AccountID: value.ID, Name: value.Name, Email: value.Email, Outcome: BuildDetectOutcomeFailed, HTTPStatus: status}
	switch {
	case rejection.SpendingLimitBlocked:
		item.Reason = "消费限额已满"
		if err := s.markBuildDetectQuotaExhausted(ctx, value, billing); err != nil {
			item.Reason = err.Error()
		}
	case rejection.ModelQuotaExhausted:
		item.Reason = fmt.Sprintf("模型 %s 额度已满", botRiskProbeModel)
		if err := s.markBuildDetectModelQuotaExhausted(ctx, value, item.Reason); err != nil {
			item.Reason = err.Error()
		}
	default:
		item.Reason = "额度已满"
		if err := s.markBuildDetectQuotaExhausted(ctx, value, billing); err != nil {
			item.Reason = err.Error()
		}
	}
	s.logger.Info("bot_risk_probe_quota", "account_id", value.ID, "identity", identity, "status", status, "reason", item.Reason)
	return item, true
}
