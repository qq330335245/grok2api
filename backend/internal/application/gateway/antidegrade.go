package gateway

import (
	"github.com/chenyme/grok2api/backend/internal/application/antidegrade"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

func (s *Service) SetAntiDegrade(controller *antidegrade.Controller) {
	if s == nil {
		return
	}
	s.antiDegrade.Store(controller)
}

func (s *Service) antiDegradeCtl() *antidegrade.Controller {
	if s == nil {
		return nil
	}
	return s.antiDegrade.Load()
}

func usedEgressNodeID(trace *infraegress.Trace, provider accountdomain.Provider, fallback ...uint64) uint64 {
	if trace != nil {
		if selection, ok := trace.Selection(primaryEgressScope(provider)); ok && selection.NodeID != 0 {
			return selection.NodeID
		}
	}
	for _, value := range fallback {
		if value != 0 {
			return value
		}
	}
	return 0
}

func firstNonZero(values ...uint64) uint64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

// antiDegradeRetryMiss is true when a same-account ExitIP retry still has no
// streamed thinking. Short first-attempt replies may deliver; retries must not
// count as success.
func antiDegradeRetryMiss(pin uint64, streamedThinking bool) bool {
	return pin != 0 && !streamedThinking
}
