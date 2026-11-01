package handler

import (
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/stretchr/testify/require"
)

func openAIOverloadTestBody(retryAfterSeconds string) []byte {
	if retryAfterSeconds == "" {
		return []byte(`{"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later.","type":"service_unavailable_error"},"sequence_number":3,"type":"error"}`)
	}
	return []byte(`{"error":{"code":"server_is_overloaded","headers":{"retry-after":"` + retryAfterSeconds + `"},"message":"Our servers are currently overloaded. Please try again later.","type":"service_unavailable_error"},"sequence_number":3,"type":"error"}`)
}

// newOpenAIOverloadTestErr 模拟 service.newOpenAIUpstreamFailoverError 对过载事件构造的错误：
// 请求级瞬时、可同号重试，SameAccountRetryDelay 为封顶 8s 的 Retry-After。
func newOpenAIOverloadTestErr(retryAfterSeconds string) *service.UpstreamFailoverError {
	body := openAIOverloadTestBody(retryAfterSeconds)
	delay := service.OpenAIRequestScopedRetryAfter(http.Header{}, body)
	if delay > defaultOpenAIOverloadRetryMaxDelay {
		delay = defaultOpenAIOverloadRetryMaxDelay
	}
	return &service.UpstreamFailoverError{
		StatusCode:             http.StatusServiceUnavailable,
		ResponseBody:           body,
		ResponseHeaders:        http.Header{},
		RetryableOnSameAccount: true,
		RequestScopedTransient: true,
		SameAccountRetryDelay:  delay,
	}
}

func openAIOverloadTestOAuthAccount(id int64) *service.Account {
	return &service.Account{ID: id, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}
}

func openAIOverloadTestConfig(retries, maxDelayMs, switchRetryAfterSeconds, maxSwitches int) *config.Config {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIOverloadSameAccountRetries = retries
	cfg.Gateway.OpenAIOverloadRetryMaxDelayMs = maxDelayMs
	cfg.Gateway.OpenAIOverloadSwitchAccountRetryAfterSeconds = switchRetryAfterSeconds
	cfg.Gateway.OpenAIOverloadMaxAccountSwitches = maxSwitches
	return cfg
}

func TestOpenAIOverloadPolicyForNilConfigUsesDefaults(t *testing.T) {
	require.Equal(t, openAIOverloadPolicy{sameAccountRetries: 3, retryMaxDelay: 8 * time.Second}, openAIOverloadPolicyFor(nil))
}

// 默认配置（3 / 8000 / 0 / 0）必须与上游原逻辑逐项一致。
func TestOpenAISameAccountRetryDecisionDefaultsMatchLegacy(t *testing.T) {
	defaults := openAIOverloadTestConfig(3, 8000, 0, 0)
	poolAccount := &service.Account{ID: 9, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"pool_mode": true, "pool_mode_retry_count": float64(5)}}
	for _, cfg := range []*config.Config{nil, defaults} {
		for _, account := range []*service.Account{openAIOverloadTestOAuthAccount(1), poolAccount} {
			for _, hint := range []string{"", "1", "6", "310"} {
				for retryCount := 0; retryCount <= 5; retryCount++ {
					failoverErr := newOpenAIOverloadTestErr(hint)
					action, limit, delay := openAISameAccountRetryDecision(cfg, failoverErr, account, retryCount, &openAIOverloadFailoverState{})

					legacyLimit := effectiveSameAccountRetryLimit(failoverErr, account)
					legacyRetry := sameAccountRetryAllowed(failoverErr, retryCount, legacyLimit)
					require.Equal(t, legacyLimit, limit, "hint=%s count=%d", hint, retryCount)
					if legacyRetry {
						require.Equal(t, openAIRetryActionRetry, action, "hint=%s count=%d", hint, retryCount)
						require.Equal(t, sameAccountRetryDelayFor(failoverErr, retryCount+1), delay, "hint=%s count=%d", hint, retryCount)
					} else {
						require.Equal(t, openAIRetryActionSwitch, action, "hint=%s count=%d", hint, retryCount)
					}
				}
			}
		}
	}
}

func TestOpenAISameAccountRetryDecisionRetryCount(t *testing.T) {
	account := openAIOverloadTestOAuthAccount(1)

	oneRetry := openAIOverloadTestConfig(1, 2000, 0, 0)
	action, limit, delay := openAISameAccountRetryDecision(oneRetry, newOpenAIOverloadTestErr("6"), account, 0, nil)
	require.Equal(t, openAIRetryActionRetry, action)
	require.Equal(t, 1, limit)
	require.Equal(t, 2*time.Second, delay)
	action, _, _ = openAISameAccountRetryDecision(oneRetry, newOpenAIOverloadTestErr("6"), account, 1, nil)
	require.Equal(t, openAIRetryActionSwitch, action)

	noRetry := openAIOverloadTestConfig(0, 2000, 0, 0)
	action, _, _ = openAISameAccountRetryDecision(noRetry, newOpenAIOverloadTestErr("6"), account, 0, nil)
	require.Equal(t, openAIRetryActionSwitch, action)

	// 池模式账号仍使用账号自身的 pool_mode_retry_count。
	poolAccount := &service.Account{ID: 2, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"pool_mode": true, "pool_mode_retry_count": float64(5)}}
	action, limit, _ = openAISameAccountRetryDecision(oneRetry, newOpenAIOverloadTestErr("6"), poolAccount, 3, nil)
	require.Equal(t, openAIRetryActionRetry, action)
	require.Equal(t, 5, limit)
}

func TestOpenAISameAccountRetryDecisionLongRetryAfterSwitches(t *testing.T) {
	account := openAIOverloadTestOAuthAccount(1)
	cfg := openAIOverloadTestConfig(3, 2000, 60, 0)
	for _, test := range []struct {
		hint string
		want openAISameAccountRetryAction
	}{
		{hint: "310", want: openAIRetryActionSwitch},
		{hint: "60", want: openAIRetryActionSwitch},
		{hint: "59", want: openAIRetryActionRetry},
		{hint: "", want: openAIRetryActionRetry},
	} {
		action, _, _ := openAISameAccountRetryDecision(cfg, newOpenAIOverloadTestErr(test.hint), account, 0, nil)
		require.Equal(t, test.want, action, "hint=%s", test.hint)
	}

	// 阈值为 0 时关闭该功能。
	action, _, _ := openAISameAccountRetryDecision(openAIOverloadTestConfig(3, 2000, 0, 0), newOpenAIOverloadTestErr("310"), account, 0, nil)
	require.Equal(t, openAIRetryActionRetry, action)
}

func TestOpenAISameAccountRetryDecisionMaxDelay(t *testing.T) {
	account := openAIOverloadTestOAuthAccount(1)

	_, _, delay := openAISameAccountRetryDecision(openAIOverloadTestConfig(3, 2000, 0, 0), newOpenAIOverloadTestErr("6"), account, 0, nil)
	require.Equal(t, 2*time.Second, delay)
	_, _, delay = openAISameAccountRetryDecision(openAIOverloadTestConfig(3, 2000, 0, 0), newOpenAIOverloadTestErr("1"), account, 0, nil)
	require.Equal(t, time.Second, delay)
	_, _, delay = openAISameAccountRetryDecision(openAIOverloadTestConfig(3, 30000, 0, 0), newOpenAIOverloadTestErr("20"), account, 0, nil)
	require.Equal(t, 20*time.Second, delay)
	_, _, delay = openAISameAccountRetryDecision(openAIOverloadTestConfig(3, 30000, 0, 0), newOpenAIOverloadTestErr("310"), account, 0, nil)
	require.Equal(t, 30*time.Second, delay)

	// 0 表示忽略 Retry-After，回到 500ms 翻倍退避。
	_, _, delay = openAISameAccountRetryDecision(openAIOverloadTestConfig(3, 0, 0, 0), newOpenAIOverloadTestErr("6"), account, 0, nil)
	require.Equal(t, 500*time.Millisecond, delay)
	_, _, delay = openAISameAccountRetryDecision(openAIOverloadTestConfig(3, 0, 0, 0), newOpenAIOverloadTestErr("6"), account, 1, nil)
	require.Equal(t, time.Second, delay)
}

// 生产配置下，非过载错误、其它平台、OAuth 429 截止窗口与 nil 账号都保持原逻辑。
func TestOpenAISameAccountRetryDecisionIgnoresOtherErrors(t *testing.T) {
	production := openAIOverloadTestConfig(0, 2000, 60, 1)

	poolAccount := &service.Account{ID: 3, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"pool_mode": true, "pool_mode_retry_count": float64(2)}}
	poolErr := &service.UpstreamFailoverError{StatusCode: http.StatusTooManyRequests, RetryableOnSameAccount: true}
	action, limit, delay := openAISameAccountRetryDecision(production, poolErr, poolAccount, 0, &openAIOverloadFailoverState{})
	require.Equal(t, openAIRetryActionRetry, action)
	require.Equal(t, 2, limit)
	require.Equal(t, sameAccountRetryDelay, delay)

	grokAccount := &service.Account{ID: 4, Platform: service.PlatformGrok, Type: service.AccountTypeOAuth}
	action, _, _ = openAISameAccountRetryDecision(production, newOpenAIOverloadTestErr("6"), grokAccount, 0, &openAIOverloadFailoverState{})
	require.Equal(t, openAIRetryActionRetry, action)

	withDeadline := newOpenAIOverloadTestErr("6")
	withDeadline.SameAccountRetryDeadline = time.Now().Add(time.Minute)
	action, _, _ = openAISameAccountRetryDecision(production, withDeadline, openAIOverloadTestOAuthAccount(5), 0, &openAIOverloadFailoverState{})
	require.Equal(t, openAIRetryActionRetry, action)

	action, limit, _ = openAISameAccountRetryDecision(production, newOpenAIOverloadTestErr("6"), nil, 0, &openAIOverloadFailoverState{})
	require.Equal(t, openAIRetryActionSwitch, action)
	require.Zero(t, limit)
}

func TestOpenAISameAccountRetryDecisionAccountSwitchBudget(t *testing.T) {
	// N=0 不限制。
	state := &openAIOverloadFailoverState{}
	for id := int64(1); id <= 5; id++ {
		action, _, _ := openAISameAccountRetryDecision(openAIOverloadTestConfig(0, 2000, 0, 0), newOpenAIOverloadTestErr("6"), openAIOverloadTestOAuthAccount(id), 0, state)
		require.Equal(t, openAIRetryActionSwitch, action)
	}
	require.Empty(t, state.accountIDs)

	// N=1，不同号重试：账号 1 换号，账号 2 停止。
	state = &openAIOverloadFailoverState{}
	noRetry := openAIOverloadTestConfig(0, 2000, 0, 1)
	action, _, _ := openAISameAccountRetryDecision(noRetry, newOpenAIOverloadTestErr("6"), openAIOverloadTestOAuthAccount(1), 0, state)
	require.Equal(t, openAIRetryActionSwitch, action)
	action, _, _ = openAISameAccountRetryDecision(noRetry, newOpenAIOverloadTestErr("6"), openAIOverloadTestOAuthAccount(2), 0, state)
	require.Equal(t, openAIRetryActionStop, action)

	// N=1，重试 1 次且重试跳到了别的账号：账号 1、2 各自重试，第 3 个账号停止。
	state = &openAIOverloadFailoverState{}
	oneRetry := openAIOverloadTestConfig(1, 2000, 0, 1)
	action, _, _ = openAISameAccountRetryDecision(oneRetry, newOpenAIOverloadTestErr("6"), openAIOverloadTestOAuthAccount(1), 0, state)
	require.Equal(t, openAIRetryActionRetry, action)
	action, _, _ = openAISameAccountRetryDecision(oneRetry, newOpenAIOverloadTestErr("6"), openAIOverloadTestOAuthAccount(2), 0, state)
	require.Equal(t, openAIRetryActionRetry, action)
	action, _, _ = openAISameAccountRetryDecision(oneRetry, newOpenAIOverloadTestErr("6"), openAIOverloadTestOAuthAccount(3), 0, state)
	require.Equal(t, openAIRetryActionStop, action)

	// 非过载错误不计入预算。
	state = &openAIOverloadFailoverState{}
	plainErr := &service.UpstreamFailoverError{StatusCode: http.StatusBadGateway}
	action, _, _ = openAISameAccountRetryDecision(noRetry, plainErr, openAIOverloadTestOAuthAccount(1), 0, state)
	require.Equal(t, openAIRetryActionSwitch, action)
	require.Empty(t, state.accountIDs)
}
