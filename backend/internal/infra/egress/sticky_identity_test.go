package egress

import (
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/application/egress"
)

func TestRenderAccountProxyURLKeepsPlusSuffix(t *testing.T) {
	got, err := renderAccountProxyURL("socks5h://g2a."+egress.ProxyAccountPlaceholder+":token@127.0.0.1:10800", "sticky_acc+1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "g2a.sticky_acc+1") {
		t.Fatalf("plus suffix dropped: %s", got)
	}
	if strings.Contains(got, "sticky_acc_1") {
		t.Fatal("plus must not be rewritten to underscore")
	}
}
