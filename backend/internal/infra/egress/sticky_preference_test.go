package egress

import (
	"context"
	"strings"
	"testing"

	application "github.com/chenyme/grok2api/backend/internal/application/egress"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestStickySelectAffinityStripsPlusSuffix(t *testing.T) {
	if got := stickySelectAffinity("grok_build_43+1"); got != "grok_build_43" {
		t.Fatalf("got %q", got)
	}
	if got := stickySelectAffinity("sso_abc+2"); got != "sso_abc" {
		t.Fatalf("got %q", got)
	}
	if got := stickySelectAffinity("plain"); got != "plain" {
		t.Fatalf("got %q", got)
	}
	if got := stickySelectAffinity("keep+plus+in+middle+x"); got != "keep+plus+in+middle+x" {
		t.Fatalf("non-numeric suffix must stay: %q", got)
	}
}

func TestStickyLeasePreferenceUsesAccountTemplate(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encrypt := func(raw string) string {
		t.Helper()
		value, encryptErr := cipher.Encrypt(raw)
		if encryptErr != nil {
			t.Fatal(encryptErr)
		}
		return value
	}
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{
		{ID: 27, Name: "阿里云ipv6裂变 027", Scope: domain.ScopeBuild, Enabled: true, Health: 1, EncryptedProxyURL: encrypt("socks5h://fission@127.0.0.1:10827")},
		{ID: 82, Name: "阿里云ipv6动态粘性", Scope: domain.ScopeBuild, Enabled: true, Health: 1, EncryptedProxyURL: encrypt("socks5h://g2a." + application.ProxyAccountPlaceholder + ":token@127.0.0.1:10800")},
	}}, cipher)

	ctx := WithAccountIdentity(WithStickyLeasePreference(context.Background()), "grok_build_43+1")
	lease, configured, err := manager.AcquireIfConfigured(ctx, domain.ScopeBuild, "grok_build_43+1")
	if err != nil || !configured || lease == nil {
		t.Fatalf("sticky+1: configured=%v err=%v", configured, err)
	}
	defer lease.Release()
	if lease.NodeID != 82 || !lease.sticky {
		t.Fatalf("wanted sticky node 82, got id=%d name=%q sticky=%v", lease.NodeID, lease.NodeName, lease.sticky)
	}
	if !strings.Contains(lease.ProxyURL, "g2a.grok_build_43+1") {
		t.Fatalf("plus suffix not rendered: %s", lease.ProxyURL)
	}

	ctx2 := WithAccountIdentity(WithStickyLeasePreference(context.Background()), "grok_build_43+2")
	lease2, configured, err := manager.AcquireIfConfigured(ctx2, domain.ScopeBuild, "grok_build_43+2")
	if err != nil || !configured || lease2 == nil {
		t.Fatalf("sticky+2: configured=%v err=%v", configured, err)
	}
	defer lease2.Release()
	if lease2.NodeID != 82 {
		t.Fatalf("+1 and +2 must stay on the same sticky node, got %d", lease2.NodeID)
	}
	if !strings.Contains(lease2.ProxyURL, "g2a.grok_build_43+2") {
		t.Fatalf("plus suffix 2 not rendered: %s", lease2.ProxyURL)
	}
}

func TestStickyLeasePreferenceIgnoresBoundFissionNode(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encrypt := func(raw string) string {
		t.Helper()
		value, encryptErr := cipher.Encrypt(raw)
		if encryptErr != nil {
			t.Fatal(encryptErr)
		}
		return value
	}
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{
		{ID: 27, Name: "阿里云ipv6裂变 027", Scope: domain.ScopeBuild, Enabled: true, Health: 1, EncryptedProxyURL: encrypt("socks5h://fission@127.0.0.1:10827")},
		{ID: 82, Name: "阿里云ipv6动态粘性", Scope: domain.ScopeBuild, Enabled: true, Health: 1, EncryptedProxyURL: encrypt("socks5h://g2a." + application.ProxyAccountPlaceholder + ":token@127.0.0.1:10800")},
	}}, cipher)
	ctx := WithEgressNode(WithAccountIdentity(WithStickyLeasePreference(context.Background()), "grok_build_43+1"), 27)
	lease, err := manager.AcquireCredential(ctx, domain.ScopeBuild, accountdomain.Credential{
		ID: 43, Provider: accountdomain.ProviderBuild, EgressIdentity: "grok_build_43+1", EgressNodeID: 27,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.NodeID != 82 {
		t.Fatalf("bound fission must not win over sticky template, got %d %q", lease.NodeID, lease.NodeName)
	}
}

func TestStickyLeasePreferenceFallsBackWithoutTemplate(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	fission, err := cipher.Encrypt("socks5h://fission@127.0.0.1:10827")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{
		{ID: 27, Name: "阿里云ipv6裂变 027", Scope: domain.ScopeBuild, Enabled: true, Health: 1, EncryptedProxyURL: fission},
	}}, cipher)
	ctx := WithAccountIdentity(WithStickyLeasePreference(context.Background()), "grok_build_43+1")
	lease, configured, err := manager.AcquireIfConfigured(ctx, domain.ScopeBuild, "grok_build_43+1")
	if err != nil || !configured || lease == nil {
		t.Fatalf("fallback: configured=%v err=%v", configured, err)
	}
	defer lease.Release()
	if lease.NodeID != 27 {
		t.Fatalf("without sticky template wanted fission 27, got %d", lease.NodeID)
	}
}
