package handler

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// OpenAI 上游过载（server_is_overloaded / slow_down）的同账号重试与换号策略。
//
// 上游默认行为：按 Retry-After（封顶 8s）在同一账号重试 3 次再换号。前置网关通常有
// 首包超时，这段时间内既没有响应头也没有输出，整段重试会被前置网关当成卡死。
// 这里把次数、单次等待、长 Retry-After 直接换号、过载换号预算做成配置；默认值与
// 上游行为逐项一致。

const (
	// defaultOpenAIOverloadSameAccountRetries 与 Account.GetPoolModeRetryCount() 对非池模式账号的返回值一致。
	defaultOpenAIOverloadSameAccountRetries = 3
	// defaultOpenAIOverloadRetryMaxDelay 与 service.openAIRequestScopedRetryMaxDelay 一致。
	defaultOpenAIOverloadRetryMaxDelay = 8 * time.Second
)

type openAISameAccountRetryAction uint8

const (
	// openAIRetryActionSwitch 走原有的换号逻辑。
	openAIRetryActionSwitch openAISameAccountRetryAction = iota
	// openAIRetryActionRetry 等待 retryDelay 后重新进入选号（不钉住账号，通常由粘性会话选回同一账号）。
	openAIRetryActionRetry
	// openAIRetryActionStop 过载换号预算已用完，直接把过载错误返回客户端。
	openAIRetryActionStop
)

// openAIOverloadFailoverState 记录单个请求内返回过过载的不同账号；零值可用。
type openAIOverloadFailoverState struct {
	accountIDs map[int64]struct{}
}

func (s *openAIOverloadFailoverState) observe(accountID int64) int {
	if s.accountIDs == nil {
		s.accountIDs = make(map[int64]struct{})
	}
	s.accountIDs[accountID] = struct{}{}
	return len(s.accountIDs)
}

type openAIOverloadPolicy struct {
	sameAccountRetries int
	retryMaxDelay      time.Duration
	switchRetryAfter   time.Duration
	maxAccountSwitches int
}

func openAIOverloadPolicyFor(cfg *config.Config) openAIOverloadPolicy {
	if cfg == nil {
		return openAIOverloadPolicy{
			sameAccountRetries: defaultOpenAIOverloadSameAccountRetries,
			retryMaxDelay:      defaultOpenAIOverloadRetryMaxDelay,
		}
	}
	gw := cfg.Gateway
	return openAIOverloadPolicy{
		sameAccountRetries: gw.OpenAIOverloadSameAccountRetries,
		retryMaxDelay:      time.Duration(gw.OpenAIOverloadRetryMaxDelayMs) * time.Millisecond,
		switchRetryAfter:   time.Duration(gw.OpenAIOverloadSwitchAccountRetryAfterSeconds) * time.Second,
		maxAccountSwitches: gw.OpenAIOverloadMaxAccountSwitches,
	}
}

// isOpenAIOverloadFailover 只覆盖 OpenAI 平台的请求级容量削减；带截止时间的 OAuth 429
// 窗口与 Grok 等其它平台的同类错误保持原逻辑。
func isOpenAIOverloadFailover(failoverErr *service.UpstreamFailoverError, account *service.Account) bool {
	return failoverErr != nil && account != nil &&
		account.Platform == service.PlatformOpenAI &&
		failoverErr.SameAccountRetryDeadline.IsZero() &&
		failoverErr.IsOpenAICapacityShed()
}

// openAISameAccountRetryDecision 决定一次 RetryableOnSameAccount 失败后是同号重试、换号还是停止。
// retryCount 是该账号已做过的同号重试次数；返回的 retryDelay 仅在 Retry 时有意义。
func openAISameAccountRetryDecision(
	cfg *config.Config,
	failoverErr *service.UpstreamFailoverError,
	account *service.Account,
	retryCount int,
	state *openAIOverloadFailoverState,
) (action openAISameAccountRetryAction, retryLimit int, retryDelay time.Duration) {
	retryLimit = effectiveSameAccountRetryLimit(failoverErr, account)
	if !isOpenAIOverloadFailover(failoverErr, account) {
		if sameAccountRetryAllowed(failoverErr, retryCount, retryLimit) {
			return openAIRetryActionRetry, retryLimit, sameAccountRetryDelayFor(failoverErr, retryCount+1)
		}
		return openAIRetryActionSwitch, retryLimit, 0
	}

	policy := openAIOverloadPolicyFor(cfg)
	seen := 0
	if policy.maxAccountSwitches > 0 && state != nil {
		seen = state.observe(account.ID)
		// 同号重试会重新选号，可能跳到另一个账号；超出 N+1 个账号即停止。
		if seen > policy.maxAccountSwitches+1 {
			return openAIRetryActionStop, retryLimit, 0
		}
	}
	if !account.IsPoolMode() {
		retryLimit = policy.sameAccountRetries
		if retryLimit > 0 && failoverErr.SameAccountRetryMax > 0 && failoverErr.SameAccountRetryMax < retryLimit {
			retryLimit = failoverErr.SameAccountRetryMax
		}
	}

	hint := service.OpenAIRequestScopedRetryAfter(failoverErr.ResponseHeaders, failoverErr.ResponseBody)
	longHint := policy.switchRetryAfter > 0 && hint >= policy.switchRetryAfter
	if !longHint && sameAccountRetryAllowed(failoverErr, retryCount, retryLimit) {
		if policy.retryMaxDelay > 0 && hint > 0 {
			return openAIRetryActionRetry, retryLimit, min(hint, policy.retryMaxDelay)
		}
		// 无 Retry-After 提示或配置为 0：沿用请求级瞬时错误的 500ms 翻倍退避。
		return openAIRetryActionRetry, retryLimit,
			sameAccountRetryDelayFor(&service.UpstreamFailoverError{RequestScopedTransient: true}, retryCount+1)
	}
	if seen > 0 && seen >= policy.maxAccountSwitches+1 {
		return openAIRetryActionStop, retryLimit, 0
	}
	return openAIRetryActionSwitch, retryLimit, 0
}
