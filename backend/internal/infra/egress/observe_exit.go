package egress

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"time"
)

const exitObserveTimeout = 5 * time.Second

var exitObserveEndpoints = []string{cloudflareIPv4ProbeEndpoint, cloudflareIPv6ProbeEndpoint}

// ObserveLeaseExitIP resolves the live public IP through an already-acquired
// lease so sticky {account}+n identities report the same exit as the probe.
func ObserveLeaseExitIP(ctx context.Context, lease *Lease) string {
	if lease == nil {
		return ""
	}
	for _, endpoint := range exitObserveEndpoints {
		if ip := fetchExitIP(ctx, func(req *http.Request) (*http.Response, error) {
			return lease.Do(req)
		}, endpoint); ip != "" {
			return ip
		}
	}
	return ""
}

// ObserveTransportExitIP resolves the live public IP through a raw transport,
// used when Build falls back to a direct connection with no lease.
func ObserveTransportExitIP(ctx context.Context, transport http.RoundTripper) string {
	if transport == nil {
		return ""
	}
	client := &http.Client{Transport: transport, Timeout: exitObserveTimeout}
	for _, endpoint := range exitObserveEndpoints {
		if ip := fetchExitIP(ctx, client.Do, endpoint); ip != "" {
			return ip
		}
	}
	return ""
}

func fetchExitIP(ctx context.Context, do func(*http.Request) (*http.Response, error), endpoint string) string {
	if ctx == nil {
		ctx = context.Background()
	}
	observeCtx, cancel := context.WithTimeout(withSkipPhysicalCall(ctx), exitObserveTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(observeCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ""
	}
	request.Header.Set("User-Agent", DefaultUserAgent)
	response, err := do(request)
	if err != nil || response == nil {
		return ""
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return ""
	}
	ip, err := decodeProbeIP(body)
	if err != nil {
		return ""
	}
	address, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	return address.String()
}
