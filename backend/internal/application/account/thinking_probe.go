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
	identities := make([]string, 0, botRiskProbeAttempts)
	for attempt := 1; attempt <= botRiskProbeAttempts; attempt++ {
		if ctx.Err() != nil {
			item.Reason = ctx.Err().Error()
			return item
		}
		identity := stickyProbeIdentity(value, attempt)
		identities = append(identities, identity)
		scan, status, probeErr := s.probeThinkingOnStickyIdentity(ctx, value, billing, identity)
		if status != 0 {
			item.HTTPStatus = status
		}
		if probeErr != nil {
			item.Reason = probeErr.Error()
			return item
		}
		s.logger.Info("bot_risk_probe_attempt", "account_id", value.ID, "identity", identity, "status", status, "verdict", scan.verdict.String(), "event", scan.event, "delta", scan.delta)
		switch scan.verdict {
		case probeThinking:
			if persistErr := s.persistPageBotFlag(ctx, value, 0); persistErr != nil {
				item.Reason = persistErr.Error()
				return item
			}
			item.BotFlagSource = 0
			item.Outcome = BuildDetectOutcomeOK
			item.Reason = fmt.Sprintf("thinking on %s (%s)", identity, scan.event)
			return item
		case probeMissingThinking:
			misses++
		default:
			item.Reason = fmt.Sprintf("%s 无有效思考样本", identity)
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
	item.Reason = fmt.Sprintf("no thinking on %s", strings.Join(identities, ","))
	return item
}

func (s *Service) probeThinkingOnStickyIdentity(ctx context.Context, value accountdomain.Credential, billing *accountdomain.Billing, identity string) (thinkingScan, int, error) {
	adapter, ok := s.providers.Responses(accountdomain.ProviderBuild)
	if !ok {
		return thinkingScan{verdict: probeInconclusive}, 0, fmt.Errorf("Provider %s 未注册 Responses 能力", accountdomain.ProviderBuild)
	}
	attemptCtx, cancel := context.WithTimeout(ctx, botRiskAttemptTimeout)
	defer cancel()
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
	if err != nil {
		return thinkingScan{verdict: probeInconclusive}, 0, nil
	}
	if response.Body != nil {
		defer response.Body.Close()
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return thinkingScan{verdict: probeInconclusive}, response.StatusCode, nil
	}
	return scanThinkingSSE(response.Body), response.StatusCode, nil
}
