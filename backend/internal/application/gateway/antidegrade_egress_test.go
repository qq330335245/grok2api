package gateway

import (
	"errors"
	"net/http"
	"testing"
)

func TestIsProxyDialFailure(t *testing.T) {
	t.Parallel()
	if isProxyDialFailure(nil) {
		t.Fatal("nil")
	}
	if !isProxyDialFailure(errors.New(`Get "https://x": socks connect tcp 47.242.212.164:10800->x:443: EOF`)) {
		t.Fatal("socks EOF must count")
	}
	if !isProxyDialFailure(errors.New(`proxyconnect tcp: dial tcp 127.0.0.1:17890: connect: connection refused`)) {
		t.Fatal("proxyconnect must count")
	}
	if isProxyDialFailure(errors.New("You are sending requests too quickly. Please try again later.")) {
		t.Fatal("upstream rate-limit text must not cool ExitIP")
	}
	if isProxyDialFailure(errors.New("context deadline exceeded")) {
		t.Fatal("timeout must not count as proxy dial")
	}
}

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
