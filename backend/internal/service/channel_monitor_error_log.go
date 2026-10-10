package service

import (
	"context"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// channelMonitorErrorLogComponent 日志组件名。
const channelMonitorErrorLogComponent = "service.channel_monitor_error_log"

// ChannelMonitorErrorRecorder 把渠道监控探针的**失败**补记进 ops_error_logs。
//
// 为什么需要它：探针失败原本只落在监控自己的历史表里，**不会出现在「错误请求」页**，
// 于是管理员看到某条渠道一直探测失败，却无从知道失败原因、也无从在页面上直接处置。
// 这里把失败按网关的口径补记一条错误日志，让探针失败与真实业务失败在同一处可见。
//
// 与 ChannelMonitorUsageRecorder 的分工是**互补且不重叠**的：
//   - usage recorder 只记 2xx（那是真花了钱、要进成本对账的）；
//   - 这里只记非 2xx 与网络失败（那些不产生消费，但正是要看原因的那批）。
//
// 只写**确定的失败**（error / failed）：degraded 表示「慢但能用」，把它当错误报进去
// 会让错误率虚高，也会稀释真正需要处置的记录。
type ChannelMonitorErrorRecorder struct {
	ops *OpsService
}

// NewChannelMonitorErrorRecorder 创建错误记录器。
func NewChannelMonitorErrorRecorder(ops *OpsService) *ChannelMonitorErrorRecorder {
	return &ChannelMonitorErrorRecorder{ops: ops}
}

// record 把本轮的失败结果批量写进 ops_error_logs。
//
// best-effort：任何失败只记日志，绝不影响探针自身的判定与历史落库
// （与 usageRecorder.record 同样的取舍——记账是旁路，主流程不能因为它变脆）。
func (r *ChannelMonitorErrorRecorder) record(ctx context.Context, m *ChannelMonitor, results []*CheckResult) {
	if r == nil || r.ops == nil || m == nil || len(results) == 0 {
		return
	}

	entries := make([]*OpsInsertErrorLogInput, 0, len(results))
	for index, res := range results {
		if res == nil || !channelMonitorResultIsFailure(res.Status) {
			continue
		}
		entries = append(entries, buildChannelMonitorErrorEntry(m, res, index))
	}
	if len(entries) == 0 {
		return
	}

	// 用脱离取消的上下文：探针的 ctx 常带超时，一旦它先到期，
	// 这批"正因为超时才要记"的错误日志反而会写不进去。
	writeCtx, cancel := detachedBillingContext(ctx)
	defer cancel()

	if err := r.ops.RecordErrorBatch(writeCtx, entries); err != nil {
		logger.LegacyPrintf(channelMonitorErrorLogComponent,
			"channel_monitor_error_log_failed: monitor_id=%d count=%d err=%v", m.ID, len(entries), err)
	}
}

// channelMonitorResultIsFailure 判断一个探针状态是否属于「确定的失败」。
func channelMonitorResultIsFailure(status string) bool {
	switch status {
	case MonitorStatusError, MonitorStatusFailed:
		return true
	default:
		return false
	}
}

// buildChannelMonitorErrorEntry 把一条失败的探针结果转成 ops 错误日志条目。
//
// 刻意**不填** UserID / APIKeyID / AccountID：探针不是任何用户发起的调用，
// 硬塞一个归属方只会让「这条错误是谁的」变得含糊，也会污染按用户维度的错误统计。
func buildChannelMonitorErrorEntry(m *ChannelMonitor, res *CheckResult, index int) *OpsInsertErrorLogInput {
	model := strings.TrimSpace(res.Model)
	if model == "" {
		model = strings.TrimSpace(m.PrimaryModel)
	}

	inbound := ChannelMonitorUsageInboundEndpoint
	upstreamEndpoint := strings.TrimSpace(res.UpstreamEndpoint)

	createdAt := res.CheckedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	entry := &OpsInsertErrorLogInput{
		// 与探针记账行共用同一套 request_id：失败与成功两边的记录能对上同一次探测。
		RequestID:       channelMonitorUsageRequestID(m.ID, index),
		Platform:        strings.TrimSpace(m.Provider),
		Model:           model,
		RequestedModel:  model,
		RequestPath:     upstreamEndpoint,
		InboundEndpoint: inbound,
		Stream:          res.Stream,
		ErrorPhase:      "upstream",
		ErrorType:       "upstream_error",
		Severity:        "P2",
		ErrorMessage:    strings.TrimSpace(res.Message),
		CreatedAt:       createdAt,
	}
	if upstreamEndpoint != "" {
		entry.UpstreamEndpoint = upstreamEndpoint
	}
	if res.StatusCode > 0 {
		code := res.StatusCode
		entry.StatusCode = code
		// 探针拿到的就是上游返回的状态码；同时写进上游侧字段，
		// 让「上游状态码」一列在错误详情里也能显示（与网关口径一致）。
		entry.UpstreamStatusCode = &code
		if msg := strings.TrimSpace(res.Message); msg != "" {
			entry.UpstreamErrorMessage = &msg
		}
	}
	if m.GroupID != nil {
		groupID := *m.GroupID
		entry.GroupID = &groupID
	}
	if rid := strings.TrimSpace(res.UpstreamRequestID); rid != "" {
		// 存下来供对账循环随后反查「是哪家上游商户打回的」并回填。
		// 探针失败那一刻还查不到商户（要再打一次上游接口），所以这里只能先存 ID。
		entry.UpstreamRequestID = rid
	}
	return entry
}
