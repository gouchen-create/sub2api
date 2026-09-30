package service

import (
	"context"
	"time"
)

// reconciliationA6BillSource 把 A6 客户端适配成上游账单来源端口。
//
// 适配层只做「字段改名 + 时间窗口放大」，不加任何业务判断：匹配口径、
// 汇率换算与落库都在主线，避免同一个规则在客户端里再实现一遍。
//
// 它同时负责把面板上的覆盖值刷进客户端：A6 凭据以前只能靠环境变量注入，
// 现在管理员在「上游 A6 配置」页面改完，下一轮拉取就要用上，因此刷新点放在
// 每轮拉取的最前面，而不是进程启动时。
type reconciliationA6BillSource struct {
	client   *A6Client
	settings *ReconciliationA6SettingsService
}

// NewReconciliationA6BillSource 创建 A6 账单来源。
//
// settings 允许为 nil：此时来源退化成「按构造时注入的静态配置工作」，
// 便于只关心适配逻辑的单测不引入设置服务。
func NewReconciliationA6BillSource(client *A6Client, settings *ReconciliationA6SettingsService) ReconciliationUpstreamBillSource {
	return &reconciliationA6BillSource{client: client, settings: settings}
}

// applySettings 把生效凭据刷进客户端，并报告凭据是否齐备。
func (s *reconciliationA6BillSource) applySettings(ctx context.Context) bool {
	if s.client == nil {
		return false
	}
	if s.settings != nil {
		s.client.SetConfig(s.settings.Effective(ctx).ClientConfig())
	}
	return s.client.Configured()
}

// FetchBills 拉取并规范化窗口内的 A6 账单。
func (s *reconciliationA6BillSource) FetchBills(ctx context.Context, query ReconciliationBillQuery) ([]ReconciliationUpstreamBillPayload, error) {
	if !s.applySettings(ctx) {
		return nil, ErrReconciliationBillSourceUnavailable
	}

	bills, err := s.client.FetchBills(ctx, A6BillQuery{
		TokenName: query.TokenName,
		StartTime: query.From,
		// 终点放宽 60 秒吸收本机与上游的时钟偏差。
		// 客户端约定不自行放大时间，所以要由调用方补上这一段。
		EndTime:  query.To.Add(time.Minute),
		PageSize: query.PageSize,
	})
	if err != nil {
		return nil, err
	}

	payloads := make([]ReconciliationUpstreamBillPayload, 0, len(bills))
	for i := range bills {
		bill := &bills[i]
		payloads = append(payloads, ReconciliationUpstreamBillPayload{
			Provider:            ReconciliationProviderA6,
			UpstreamRequestID:   bill.RequestID,
			OccurredAt:          bill.OccurredAt,
			Model:               bill.Model,
			TokenName:           bill.TokenName,
			InputTokens:         bill.InputTokens,
			OutputTokens:        bill.OutputTokens,
			CacheReadTokens:     bill.CacheReadTokens,
			CacheCreationTokens: bill.CacheCreationTokens,
			// 合计单独透传：上游只回合并值时两个分列字段为 0，
			// 而组合匹配以合计为准，漏掉它会让这类账单全部匹配失败。
			CacheTokensTotal: bill.CacheTokensTotal,
			CostOriginal:     bill.CostUSD.InexactFloat64(),
			Currency:         "USD",
			Source:           "a6",
			Raw:              bill.Raw,
		})
	}
	return payloads, nil
}
