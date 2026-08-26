package web

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/browserheaders"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

var (
	homeBotFlagSourcePattern  = regexp.MustCompile(`botFlagSource"\s*:\s*(null|-?\d+)`)
	homeBotFlagDetailsPattern = regexp.MustCompile(`botFlagDetails"\s*:\s*(?:null|"([^"]*)")`)
)

type homeBotFlag struct {
	Source  int
	Found   bool
	Details string
	Denied  bool
}

// ParseHomeBotFlagSource extracts grok.com page botFlagSource. found is false when the
// field is absent; that is not the same as source 0.
func ParseHomeBotFlagSource(body []byte) (source int, found bool) {
	parsed := parseHomeBotFlag(body)
	return parsed.Source, parsed.Found
}

func parseHomeBotFlag(body []byte) homeBotFlag {
	normalized := bytes.ReplaceAll(body, []byte(`\"`), []byte(`"`))
	result := homeBotFlag{}
	if match := homeBotFlagSourcePattern.FindSubmatch(normalized); len(match) >= 2 {
		result.Found = true
		if string(match[1]) != "null" {
			if value, err := strconv.Atoi(string(match[1])); err == nil && value >= 0 && value <= 2 {
				result.Source = value
			}
		}
	}
	if match := homeBotFlagDetailsPattern.FindSubmatch(normalized); len(match) >= 1 {
		result.Found = true
		if len(match) > 1 {
			result.Details = string(match[1])
		}
	}
	for _, item := range strings.Split(result.Details, ",") {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(key), "policy") && strings.EqualFold(strings.TrimSpace(value), "deny") {
			result.Denied = true
		}
	}
	if result.Denied && result.Source == 0 {
		result.Source = 1
	}
	return result
}

// InspectHomeBotFlag GETs grok.com with the given Web SSO and returns the page botFlagSource.
// httpStatus is the upstream status when a page was fetched.
func (a *Adapter) InspectHomeBotFlag(ctx context.Context, ssoToken string) (source int, httpStatus int, err error) {
	ssoToken = strings.TrimSpace(ssoToken)
	if ssoToken == "" {
		return 0, 0, fmt.Errorf("SSO 为空")
	}
	if a == nil || a.egress == nil {
		return 0, 0, fmt.Errorf("Grok Web 出口未初始化")
	}
	baseURL := strings.TrimRight(a.config().BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://grok.com"
	}
	affinity := "botflag:" + security.HashToken(ssoToken)
	if len(affinity) > 24 {
		affinity = affinity[:24]
	}
	lease, err := a.egress.Acquire(ctx, domainegress.ScopeWeb, affinity)
	if err != nil {
		lease, err = a.egress.Acquire(ctx, domainegress.ScopeBuild, affinity)
	}
	if err != nil {
		lease, err = a.egress.Acquire(ctx, domainegress.ScopeConsole, affinity)
	}
	if err != nil {
		return 0, 0, err
	}
	defer lease.Release()

	page, err := fetchBotFlagDocument(ctx, baseURL, ssoToken, lease, "/", lease.Do)
	if err != nil {
		return 0, 0, err
	}
	if page.statusCode < 200 || page.statusCode >= 300 {
		return 0, page.statusCode, fmt.Errorf("Grok 首页返回 %d", page.statusCode)
	}
	parsed := parseHomeBotFlag(page.body)
	if !parsed.Found {
		rsc, rscErr := fetchBotFlagDocument(ctx, baseURL, ssoToken, lease, "/", withRSC(lease.Do))
		if rscErr == nil && rsc.statusCode >= 200 && rsc.statusCode < 300 {
			parsed = parseHomeBotFlag(rsc.body)
		}
	}
	if !parsed.Found {
		ok, userErr := inspectGetUserPresent(ctx, baseURL, ssoToken, lease)
		if userErr != nil {
			return 0, page.statusCode, userErr
		}
		if ok {
			// proto3 省略默认 0；当前首页也不再内嵌 botFlagSource。
			parsed = homeBotFlag{Found: true, Source: 0}
		}
	}
	if !parsed.Found {
		return 0, page.statusCode, fmt.Errorf("Grok 首页未包含 botFlagSource")
	}
	return parsed.Source, page.statusCode, nil
}

func withRSC(do func(*http.Request) (*http.Response, error)) func(*http.Request) (*http.Response, error) {
	return func(request *http.Request) (*http.Response, error) {
		request.Header.Set("RSC", "1")
		request.Header.Set("Next-Url", "/")
		request.Header.Set("Accept", "*/*")
		return do(request)
	}
}

func fetchBotFlagDocument(ctx context.Context, baseURL, token string, lease *infraegress.Lease, path string, do func(*http.Request) (*http.Response, error)) (statsigMetaResponse, error) {
	if do == nil {
		return statsigMetaResponse{}, fmt.Errorf("SSO 风控检测缺少出口租约")
	}
	requestCtx, cancel := context.WithTimeout(infraegress.WithPhysicalCallStage(ctx, "botflag_home"), 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, strings.TrimRight(baseURL, "/")+path, nil)
	if err != nil {
		return statsigMetaResponse{}, err
	}
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("Pragma", "no-cache")
	request.Header.Set("Sec-Fetch-Dest", "document")
	request.Header.Set("Sec-Fetch-Mode", "navigate")
	request.Header.Set("Sec-Fetch-Site", "none")
	request.Header.Set("Upgrade-Insecure-Requests", "1")
	if lease != nil {
		request.Header.Set("User-Agent", lease.UserAgent)
		request.Header.Set("Cookie", infraegress.BuildSSOCookie(token, lease.CFCookies))
		browserheaders.ApplyChromiumClientHints(request.Header, lease.UserAgent)
	}
	response, err := do(request)
	if err != nil {
		return statsigMetaResponse{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, statsigMetaBodyLimit+1))
	if err != nil {
		return statsigMetaResponse{}, err
	}
	if len(body) > statsigMetaBodyLimit {
		return statsigMetaResponse{}, fmt.Errorf("Grok 首页超过安全上限")
	}
	return statsigMetaResponse{statusCode: response.StatusCode, body: body}, nil
}

func inspectGetUserPresent(ctx context.Context, baseURL, token string, lease *infraegress.Lease) (bool, error) {
	if lease == nil {
		return false, fmt.Errorf("SSO 风控检测缺少出口租约")
	}
	requestCtx, cancel := context.WithTimeout(infraegress.WithPhysicalCallStage(ctx, "botflag_get_user"), 15*time.Second)
	defer cancel()
	endpoint := strings.TrimRight(baseURL, "/") + "/auth_mgmt.AuthManagement/GetUser"
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader([]byte{0x00, 0x00, 0x00, 0x00, 0x00}))
	if err != nil {
		return false, err
	}
	request.Header.Set("Content-Type", "application/grpc-web+proto")
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Origin", strings.TrimRight(baseURL, "/"))
	request.Header.Set("Referer", strings.TrimRight(baseURL, "/")+"/")
	request.Header.Set("x-grpc-web", "1")
	request.Header.Set("x-user-agent", "connect-es/2.1.1")
	request.Header.Set("User-Agent", lease.UserAgent)
	request.Header.Set("Cookie", infraegress.BuildSSOCookie(token, lease.CFCookies))
	browserheaders.ApplyChromiumClientHints(request.Header, lease.UserAgent)
	response, err := lease.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return false, err
	}
	if response.StatusCode == http.StatusUnauthorized {
		return false, fmt.Errorf("SSO 已失效")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, fmt.Errorf("GetUser 返回 %d", response.StatusCode)
	}
	return grpcWebHasMessage(body), nil
}

func grpcWebHasMessage(body []byte) bool {
	if len(body) < 5 {
		return false
	}
	n := int(body[1])<<24 | int(body[2])<<16 | int(body[3])<<8 | int(body[4])
	return n > 0 && 5+n <= len(body)
}

// InspectHomeBotFlagWithDo is a test helper that uses an injected transport.
func InspectHomeBotFlagWithDo(ctx context.Context, baseURL, ssoToken string, lease *infraegress.Lease, do func(*http.Request) (*http.Response, error)) (int, int, error) {
	page, err := fetchBotFlagDocument(ctx, baseURL, ssoToken, lease, "/", do)
	if err != nil {
		return 0, 0, err
	}
	if page.statusCode < 200 || page.statusCode >= 300 {
		return 0, page.statusCode, fmt.Errorf("Grok 首页返回 %d", page.statusCode)
	}
	parsed := parseHomeBotFlag(page.body)
	if !parsed.Found {
		return 0, page.statusCode, fmt.Errorf("Grok 首页未包含 botFlagSource")
	}
	return parsed.Source, page.statusCode, nil
}
