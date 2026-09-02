package gateway

import (
	"net/http"
	"testing"
)

func TestHasExplicitUpstreamError(t *testing.T) {
	t.Parallel()
	if hasExplicitUpstreamError(nil) {
		t.Fatal("nil")
	}
	if hasExplicitUpstreamError(&UpstreamFailure{HTTPStatus: 503, Code: ErrorEgressUnavailable}) {
		t.Fatal("synthetic no-exit must not mask itself as upstream")
	}
	if !hasExplicitUpstreamError(&UpstreamFailure{HTTPStatus: http.StatusTooManyRequests, Code: "upstream_error"}) {
		t.Fatal("429 must be kept")
	}
	if !hasExplicitUpstreamError(&UpstreamFailure{HTTPStatus: http.StatusTooManyRequests, Code: "upstream_rate_limited", Fingerprint: "429:team_model_rate_limit"}) {
		t.Fatal("team model 429 must be kept")
	}
	if hasExplicitUpstreamError(&UpstreamFailure{HTTPStatus: http.StatusBadGateway, Code: "upstream_network_error"}) {
		t.Fatal("generic network error is not an explicit upstream code")
	}
}
