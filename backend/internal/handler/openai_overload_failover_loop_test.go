//go:build unit

package handler

import (
	"bytes"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const openAIOverloadLoopBody = `{"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later.","type":"service_unavailable_error"}}`

// openAIOverloadLoopUpstream 对每次调用返回 503 过载（可按账号改为 520），并记录账号顺序。
type openAIOverloadLoopUpstream struct {
	service.HTTPUpstream
	header       http.Header
	plainFailIDs map[int64]bool
	mu           sync.Mutex
	accountIDs   []int64
}

func (u *openAIOverloadLoopUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	u.mu.Unlock()
	if u.plainFailIDs[accountID] {
		return &http.Response{
			StatusCode: 520,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       io.NopCloser(bytes.NewBufferString("<html>520: unknown error</html>")),
		}, nil
	}
	header := http.Header{"Content-Type": []string{"application/json"}}
	for key, values := range u.header {
		header[key] = values
	}
	return &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     header,
		Body:       io.NopCloser(bytes.NewBufferString(openAIOverloadLoopBody)),
	}, nil
}

func (u *openAIOverloadLoopUpstream) calls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.accountIDs...)
}

func newOpenAIOverloadLoopHandler(t *testing.T, upstream service.HTTPUpstream, accountCount, retries, maxDelayMs, switchRetryAfterSeconds, maxSwitches int) *OpenAIGatewayHandler {
	t.Helper()
	accounts := make([]service.Account, 0, accountCount)
	for i := 1; i <= accountCount; i++ {
		accounts = append(accounts, service.Account{
			ID:          int64(i),
			Name:        "overload-account",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Priority:    i - 1,
			Credentials: map[string]any{"access_token": "token"},
		})
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.OpenAIOverloadSameAccountRetries = retries
	cfg.Gateway.OpenAIOverloadRetryMaxDelayMs = maxDelayMs
	cfg.Gateway.OpenAIOverloadSwitchAccountRetryAfterSeconds = switchRetryAfterSeconds
	cfg.Gateway.OpenAIOverloadMaxAccountSwitches = maxSwitches
	gatewayService := service.NewOpenAIGatewayService(
		openAIImagesFailoverAccountRepo{accounts: accounts},
		nil, nil, nil, nil, nil, nil,
		cfg,
		nil, nil, nil, nil, nil,
		upstream,
		nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billingService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingService.Stop)
	handler := NewOpenAIGatewayHandler(
		gatewayService,
		service.NewConcurrencyService(nil),
		billingService,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil, nil, nil, nil,
		cfg,
	)
	handler.maxAccountSwitches = 10
	return handler
}

func TestOpenAIOverloadFailoverLoopResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fastRetryAfter := http.Header{"Retry-After-Ms": []string{"1"}}
	for _, test := range []struct {
		name                    string
		accounts                int
		retries                 int
		maxDelayMs              int
		switchRetryAfterSeconds int
		maxSwitches             int
		header                  http.Header
		plainFailIDs            map[int64]bool
		wantCalls               []int64
		wantStatus              int
	}{
		{name: "defaults unchanged", accounts: 2, retries: 3, maxDelayMs: 8000, header: fastRetryAfter, wantCalls: []int64{1, 1, 1, 1, 2, 2, 2, 2}},
		{name: "one same-account retry", accounts: 2, retries: 1, maxDelayMs: 8000, header: fastRetryAfter, wantCalls: []int64{1, 1, 2, 2}},
		{name: "no same-account retry", accounts: 2, retries: 0, maxDelayMs: 8000, header: fastRetryAfter, wantCalls: []int64{1, 2}},
		{name: "long retry-after switches immediately", accounts: 2, retries: 3, maxDelayMs: 8000, switchRetryAfterSeconds: 60, header: http.Header{"Retry-After": []string{"310"}}, wantCalls: []int64{1, 2}},
		{name: "max delay caps retry-after", accounts: 2, retries: 1, maxDelayMs: 20, header: http.Header{"Retry-After": []string{"5"}}, wantCalls: []int64{1, 1, 2, 2}},
		{name: "overload switch budget stops early", accounts: 3, retries: 1, maxDelayMs: 8000, maxSwitches: 1, header: fastRetryAfter, wantCalls: []int64{1, 1, 2, 2}, wantStatus: http.StatusServiceUnavailable},
		{name: "production values", accounts: 4, retries: 1, maxDelayMs: 2000, switchRetryAfterSeconds: 60, maxSwitches: 2, header: fastRetryAfter, wantCalls: []int64{1, 1, 2, 2, 3, 3}, wantStatus: http.StatusServiceUnavailable},
		{name: "non-overload errors unaffected", accounts: 2, retries: 1, maxDelayMs: 2000, switchRetryAfterSeconds: 60, maxSwitches: 2, plainFailIDs: map[int64]bool{1: true, 2: true}, wantCalls: []int64{1, 2}, wantStatus: http.StatusBadGateway},
		{name: "plain failure does not use the overload budget", accounts: 4, retries: 0, maxDelayMs: 2000, maxSwitches: 1, header: fastRetryAfter, plainFailIDs: map[int64]bool{1: true}, wantCalls: []int64{1, 2, 3}, wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &openAIOverloadLoopUpstream{header: test.header, plainFailIDs: test.plainFailIDs}
			handler := newOpenAIOverloadLoopHandler(t, upstream, test.accounts, test.retries, test.maxDelayMs, test.switchRetryAfterSeconds, test.maxSwitches)
			c, rec := newOpenAIResponsesFailoverTestContext(t, nil)

			start := time.Now()
			handler.Responses(c)

			require.Equal(t, test.wantCalls, upstream.calls())
			require.Less(t, time.Since(start), 3*time.Second, "测试用的 Retry-After 应被快速消化，不应真实等待")
			if test.wantStatus != 0 {
				require.Equal(t, test.wantStatus, rec.Code)
			}
		})
	}
}
