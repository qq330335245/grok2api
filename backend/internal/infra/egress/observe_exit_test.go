package egress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

func TestObserveLeaseExitIPUsesSameLease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cdn-cgi/trace" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		io.WriteString(w, "fl=1\nip=198.51.100.24\n")
	}))
	defer server.Close()
	previous := exitObserveEndpoints
	exitObserveEndpoints = []string{server.URL + "/cdn-cgi/trace"}
	defer func() { exitObserveEndpoints = previous }()

	lease := &Lease{client: server.Client(), Scope: domain.ScopeBuild}
	if got := ObserveLeaseExitIP(context.Background(), lease); got != "198.51.100.24" {
		t.Fatalf("exit ip=%q", got)
	}
}

func TestRecordSelectionExitIPKeepsNodeName(t *testing.T) {
	ctx, trace := WithTrace(context.Background())
	recordSelection(ctx, Selection{NodeID: 8, NodeName: "katabump 008", Scope: domain.ScopeBuild, Proxied: true})
	RecordSelectionExitIP(ctx, domain.ScopeBuild, "51.75.118.171")
	got, ok := trace.Selection(domain.ScopeBuild)
	if !ok || got.NodeName != "katabump 008" || got.ExitIP != "51.75.118.171" {
		t.Fatalf("selection=%#v ok=%v", got, ok)
	}
}

func TestSkipPhysicalCallDoesNotCountObserve(t *testing.T) {
	if skipPhysicalCall(context.Background()) {
		t.Fatal("default context must count physical calls")
	}
	if !skipPhysicalCall(withSkipPhysicalCall(context.Background())) {
		t.Fatal("observe requests must skip physical-call accounting")
	}
}

func TestObserveTransportExitIPParsesJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"ip":"2001:db8::8"}`)
	}))
	defer server.Close()
	previous := exitObserveEndpoints
	exitObserveEndpoints = []string{server.URL}
	defer func() { exitObserveEndpoints = previous }()
	got := ObserveTransportExitIP(context.Background(), server.Client().Transport)
	if got != "2001:db8::8" {
		t.Fatalf("exit ip=%q", got)
	}
	if !strings.Contains(got, ":") {
		t.Fatal("expected ipv6")
	}
}
