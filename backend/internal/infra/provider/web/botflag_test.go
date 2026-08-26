package web

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

func TestParseHomeBotFlagSource(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		body   string
		source int
		found  bool
	}{
		{name: "zero", body: `{"user":{"botFlagSource":0}}`, source: 0, found: true},
		{name: "one", body: `self.__next_f.push([1,"{\"botFlagSource\":1}"])`, source: 1, found: true},
		{name: "two spaced", body: `window.__data = { "botFlagSource" : 2 }`, source: 2, found: true},
		{name: "null is found zero", body: `{"botFlagSource":null}`, source: 0, found: true},
		{name: "missing", body: `<html><body>login</body></html>`, found: false},
		{name: "out of range found zero", body: `{"botFlagSource":3}`, source: 0, found: true},
		{name: "deny details", body: `{"botFlagSource":null,"botFlagDetails":"policy=deny,event=$login"}`, source: 1, found: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed := parseHomeBotFlag([]byte(tc.body))
			if parsed.Found != tc.found || parsed.Source != tc.source {
				t.Fatalf("source=%d found=%v, want %d %v", parsed.Source, parsed.Found, tc.source, tc.found)
			}
		})
	}
}

func TestInspectHomeBotFlagWithDo(t *testing.T) {
	t.Parallel()
	do := func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/" {
			t.Fatalf("path=%s", request.URL.Path)
		}
		if cookie := request.Header.Get("Cookie"); !strings.Contains(cookie, "sso=test-sso") {
			t.Fatalf("cookie=%q", cookie)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader([]byte(`<html>{"botFlagSource":2}</html>`))),
			Header:     make(http.Header),
		}, nil
	}
	source, status, err := InspectHomeBotFlagWithDo(context.Background(), "https://grok.com", "test-sso", &infraegress.Lease{UserAgent: "test-agent"}, do)
	if err != nil || source != 2 || status != http.StatusOK {
		t.Fatalf("source=%d status=%d err=%v", source, status, err)
	}
}
