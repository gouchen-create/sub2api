package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// 请求体大小上限，防止异常大的导入请求打爆面板内存。
const companionMaxRequestBytes = 8 << 20

// CompanionHandler 提供经营对账（Companion）的管理端接口。
//
// 这些接口原先是反向代理到独立的 companion 旁路服务，服务端持有该服务的 HTTP Basic
// 凭据。对账能力内置进本进程后改为直接调用服务层，因此不再需要任何上游地址与凭据，
// 也不存在凭据泄露到浏览器的可能。
//
// 路径、方法与响应结构与代理版本逐字保持一致，管理后台前端无需任何改动。
type CompanionHandler struct {
	ledgerSvc   *service.ReconciliationLedgerService
	ruleSvc     *service.ReconciliationAccountRuleService
	syncSvc     *service.ReconciliationSyncService
	settingsSvc *service.ReconciliationA6SettingsService
}

// NewCompanionHandler 构造对账管理端处理器。
func NewCompanionHandler(
	ledgerSvc *service.ReconciliationLedgerService,
	ruleSvc *service.ReconciliationAccountRuleService,
	syncSvc *service.ReconciliationSyncService,
	settingsSvc *service.ReconciliationA6SettingsService,
) *CompanionHandler {
	return &CompanionHandler{
		ledgerSvc:   ledgerSvc,
		ruleSvc:     ruleSvc,
		syncSvc:     syncSvc,
		settingsSvc: settingsSvc,
	}
}

// Enabled 报告对账能力是否可用。
//
// 内置实现恒为 true：这个返回值曾经表示「是否配置了上游地址」，
// 现在对账就在本进程里，永远可用。
func (h *CompanionHandler) Enabled() bool {
	return h != nil && h.ledgerSvc != nil
}

// ==================== 错误映射 ====================

// writeReconciliationError 把服务层错误翻译成管理端错误信封。
//
// 一律不使用 401/403：前端把 401 视为会话失效并跳登录页，对账功能出问题
// 绝不能把管理员踢出去，因此配置类故障用 503、参数类故障用 400。
func writeReconciliationError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrReconciliationInvalidWindow):
		response.Error(c, http.StatusBadRequest, "COMPANION_BAD_REQUEST: 时间窗口无效，请检查起止时间")
	case errors.Is(err, service.ErrReconciliationProviderUnsupported):
		response.Error(c, http.StatusBadRequest, "COMPANION_BAD_REQUEST: 只支持 A6 上游，Subarx 已下线")
	case errors.Is(err, service.ErrReconciliationTokenNameRequired):
		response.Error(c, http.StatusBadRequest, "COMPANION_BAD_REQUEST: A6 规则必须填写上游令牌名")
	case errors.Is(err, service.ErrReconciliationTokenNameTooLong):
		response.Error(c, http.StatusBadRequest, "COMPANION_BAD_REQUEST: 上游令牌名过长（上限 128 字符）")
	case errors.Is(err, service.ErrReconciliationBillSourceUnavailable):
		response.Error(c, http.StatusServiceUnavailable, "COMPANION_NOT_CONFIGURED: 尚未配置 A6 上游凭据")
	case errors.Is(err, service.ErrReconciliationInvalidBaseURL):
		response.Error(c, http.StatusBadRequest, "COMPANION_BAD_REQUEST: A6 基址必须是合法的 http/https URL")
	case errors.Is(err, service.ErrReconciliationInvalidFxRate):
		response.Error(c, http.StatusBadRequest, "COMPANION_BAD_REQUEST: 汇率必须大于 0 且不超过 100000")
	default:
		response.Error(c, http.StatusInternalServerError, "COMPANION_INTERNAL: "+err.Error())
	}
}

// ==================== 参数与格式化 ====================

// companionWindow 解析时间窗口查询参数。
//
// 前端会在每个 GET 上额外注入 timezone 参数，这里只读取自己认识的键，
// 其余参数自然被忽略，不会因此报错。
func (h *CompanionHandler) companionWindow(c *gin.Context) (time.Time, time.Time, error) {
	var fromPtr, toPtr *time.Time
	if from, ok, err := parseCompanionTimeParam(c, "from"); err != nil {
		return time.Time{}, time.Time{}, err
	} else if ok {
		fromPtr = &from
	}
	if to, ok, err := parseCompanionTimeParam(c, "to"); err != nil {
		return time.Time{}, time.Time{}, err
	} else if ok {
		toPtr = &to
	}
	return h.ledgerSvc.ResolveWindow(fromPtr, toPtr)
}

func parseCompanionTimeParam(c *gin.Context, key string) (time.Time, bool, error) {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return time.Time{}, false, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false, service.ErrReconciliationInvalidWindow
	}
	return parsed.UTC(), true, nil
}

// companionAmount 把金额格式化成接口约定的 8 位小数字符串。
func companionAmount(value float64) string {
	return strconv.FormatFloat(value, 'f', 8, 64)
}

// companionOptionalAmount 在金额未知时返回空字符串。
//
// 未知与 0 是两回事：未知时显示空串让页面渲染「—」，显示 0 会被误读成
// 「这笔上游没花钱」。
func companionOptionalAmount(value float64, known bool) string {
	if !known {
		return ""
	}
	return companionAmount(value)
}

// companionRFC3339 统一时间输出格式，保证前端 new Date() 可解析。
func companionRFC3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// companionRFC3339Millis 是 companionRFC3339 的毫秒精度变体。
//
// companionRFC3339 用 RFC3339Nano，会产出 9 位小数（如 2026-10-01T04:14:59.459253264Z）。
// ECMAScript 的 Date Time String Format 只定义了 3 位小数，多出来的位数目前只是靠浏览器
// 宽松截断才能解析——那是实现细节，不是可以依赖的契约。凡是要交给前端 new Date() 的
// 新增时间字段一律用这个函数，把精度钉死在毫秒，不给自己埋坑。
func companionRFC3339Millis(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// ==================== 状态 ====================

// companionA6NotConfiguredDetail 是 A6 凭据缺失时的提示文案。
//
// 必须点名「去哪个页面填」：只写「未配置」，管理员不知道该动哪里。
const companionA6NotConfiguredDetail = "尚未配置 A6 上游凭据，请在页面「上游 A6 配置」中填写"

// Status 返回对账功能的可用状态，供页面展示健康指示与故障原因。
//
// GET /admin/companion/status
func (h *CompanionHandler) Status(c *gin.Context) {
	payload := gin.H{"enabled": true, "healthy": true}
	ctx := c.Request.Context()

	status, err := h.syncSvc.Status(ctx)
	if err != nil {
		payload["healthy"] = false
		payload["detail"] = err.Error()
		response.Success(c, payload)
		return
	}

	switch {
	case !h.a6CredentialsReady(ctx):
		// A6 没配时看板上会全是 0，此时报「健康」会让管理员以为数据本身就是 0，
		// 所以这里必须明确报出不健康，并指出下一步动作。
		payload["healthy"] = false
		payload["detail"] = companionA6NotConfiguredDetail
	case !status.BillSourceReady:
		payload["healthy"] = false
		payload["detail"] = "尚未配置 A6 上游凭据，上游账单无法采集；本地用量与规则功能正常"
	case status.LastError != "":
		payload["healthy"] = false
		payload["detail"] = status.LastError
		// 真实失败时间：页面上的「更新于」是页面刷新时间，会掩盖错误实际发生的时刻。
		// 只在确实存在一条失败记录时给出——成功一轮只清 a6_last_sync_error，并不清
		// a6_last_sync_error_at，所以这个时间戳单独看有可能已经过期。
		if status.LastErrorAt != nil {
			payload["last_error_at"] = companionRFC3339Millis(*status.LastErrorAt)
		}
	}

	// 采集进度单独输出：它回答的是「下游用量有没有追平」，
	// 与「A6 上游账单能不能拉」是两条独立的链路，缺一不可。
	//
	// 这里只增字段、不改既有键与取值：前端与外部脚本都靠 healthy/detail 判断可用性。
	if status.UsageCursorAt != nil {
		payload["usage_cursor_at"] = companionRFC3339Millis(*status.UsageCursorAt)
	}
	payload["usage_last_batch_size"] = status.UsageLastBatchSize
	payload["usage_batch_truncated"] = status.UsageBatchTruncated
	payload["usage_backlog"] = status.UsageBacklog

	response.Success(c, payload)
}

// a6CredentialsReady 报告 A6 生效凭据（面板覆盖 → 配置 → 默认）是否齐备。
//
// 判定必须用生效值：管理员在页面上填完保存后，健康指示要立刻跟着变，
// 不能等到重启进程才认新配置。
func (h *CompanionHandler) a6CredentialsReady(ctx context.Context) bool {
	if h == nil || h.settingsSvc == nil {
		// 设置服务缺席（例如只装配了看板的最小化用法）时不冒充不健康，
		// 让后面的 BillSourceReady 分支去表达。
		return true
	}
	return h.settingsSvc.Effective(ctx).Configured()
}

// ==================== 上游 A6 配置 ====================

// companionSettingsDTO 是 GET/PUT /settings 的 data。
//
// 令牌只以「是否已配置 + 脱敏提示」两种形态出现，任何情况下都不返回明文。
type companionSettingsDTO struct {
	A6BaseURL         string   `json:"a6_base_url"`
	A6UserID          string   `json:"a6_user_id"`
	A6TokenConfigured bool     `json:"a6_token_configured"`
	A6TokenMask       string   `json:"a6_token_mask"`
	FxUSDCNYRate      float64  `json:"fx_usd_cny_rate"`
	OverrideKeys      []string `json:"override_keys"`
}

func toCompanionSettingsDTO(view service.ReconciliationA6SettingsView) companionSettingsDTO {
	// 前端直接遍历 override_keys，null 会让它崩掉。
	keys := view.OverrideKeys
	if keys == nil {
		keys = []string{}
	}
	return companionSettingsDTO{
		A6BaseURL:         view.A6BaseURL,
		A6UserID:          view.A6UserID,
		A6TokenConfigured: view.A6TokenConfigured,
		A6TokenMask:       view.A6TokenMask,
		FxUSDCNYRate:      view.FxUSDCNYRate,
		OverrideKeys:      keys,
	}
}

// Settings 返回 A6 上游配置的生效状态。
//
// GET /admin/companion/settings
func (h *CompanionHandler) Settings(c *gin.Context) {
	if h == nil || h.settingsSvc == nil {
		response.InternalError(c, "COMPANION_INTERNAL: A6 设置服务未装配")
		return
	}
	response.Success(c, toCompanionSettingsDTO(h.settingsSvc.Effective(c.Request.Context()).View()))
}

// companionSettingsInputDTO 是 PUT /settings 的请求体。
//
// 指针字段是刻意的：只有它才能把「没传这个字段」与「传了空串」分开，
// 而这两种含义完全不同（不改动 / 清除该覆盖）。
type companionSettingsInputDTO struct {
	A6BaseURL          *string  `json:"a6_base_url"`
	A6UserID           *string  `json:"a6_user_id"`
	A6AccessToken      *string  `json:"a6_access_token"`
	FxUSDCNYRate       *float64 `json:"fx_usd_cny_rate"`
	ClearA6AccessToken bool     `json:"clear_a6_access_token"`
}

// UpdateSettings 保存 A6 上游配置的面板覆盖值。
//
// 语义（前端按此实现）：
//   - 字段缺省 = 不改动；
//   - a6_base_url / a6_user_id 传空串 = 清除该覆盖，回落到配置/环境变量；
//   - a6_access_token 缺省或空串 = 保持原值（「留空 = 不改」）；
//     clear_a6_access_token = true 才清除覆盖；
//   - fx_usd_cny_rate 必须 > 0 且 <= 100000。
//
// 返回与 GET 完全相同的结构，前端保存后可以直接用它刷新表单。
//
// PUT /admin/companion/settings
func (h *CompanionHandler) UpdateSettings(c *gin.Context) {
	if h == nil || h.settingsSvc == nil {
		response.InternalError(c, "COMPANION_INTERNAL: A6 设置服务未装配")
		return
	}

	var input companionSettingsInputDTO
	if err := bindCompanionJSON(c, &input); err != nil {
		response.BadRequest(c, "COMPANION_BAD_REQUEST: 请求体不是合法的 JSON: "+err.Error())
		return
	}

	settings, err := h.settingsSvc.Update(c.Request.Context(), service.ReconciliationA6SettingsInput{
		BaseURL:          input.A6BaseURL,
		UserID:           input.A6UserID,
		AccessToken:      input.A6AccessToken,
		FxUSDCNYRate:     input.FxUSDCNYRate,
		ClearAccessToken: input.ClearA6AccessToken,
	})
	if err != nil {
		writeReconciliationError(c, err)
		return
	}

	response.Success(c, toCompanionSettingsDTO(settings.View()))
}

// ==================== 汇总 ====================

type companionSummaryDTO struct {
	From                   string `json:"from"`
	To                     string `json:"to"`
	Revenue                string `json:"revenue"`
	MatchedRevenue         string `json:"matched_revenue"`
	UpstreamCost           string `json:"upstream_cost"`
	BilledUpstreamCost     string `json:"billed_upstream_cost"`
	GrossProfit            string `json:"gross_profit"`
	MarginPercent          string `json:"margin_percent"`
	Matched                int64  `json:"matched"`
	Unmatched              int64  `json:"unmatched"`
	DownstreamMatched      int64  `json:"downstream_matched"`
	DownstreamUnmatched    int64  `json:"downstream_unmatched"`
	UpstreamUnmatched      int64  `json:"upstream_unmatched"`
	RecordTotal            int64  `json:"record_total"`
	BilledCount            int64  `json:"billed_count"`
	CostPolicy             string `json:"cost_policy"`
	CalculatedCount        int64  `json:"calculated_count"`
	SubarxUnallocatedCost  string `json:"subarx_unallocated_cost"`
	SubarxUnallocatedCount int64  `json:"subarx_unallocated_count"`
	ProfitScope            string `json:"profit_scope"`
	Currency               string `json:"currency"`
	FxUSDCNY               string `json:"fx_usd_cny"`
	FxSource               string `json:"fx_source"`
	FxEffectiveAt          string `json:"fx_effective_at"`
	FxStale                bool   `json:"fx_stale"`
}

// Summary 返回经营看板的汇总指标。
//
// GET /admin/companion/summary
func (h *CompanionHandler) Summary(c *gin.Context) {
	from, to, err := h.companionWindow(c)
	if err != nil {
		writeReconciliationError(c, err)
		return
	}

	summary, err := h.ledgerSvc.Summary(c.Request.Context(), from, to)
	if err != nil {
		writeReconciliationError(c, err)
		return
	}

	grossProfit := summary.MatchedRevenueCNY - summary.UpstreamCostCNY
	marginPercent := 0.0
	if summary.MatchedRevenueCNY > 0 {
		marginPercent = grossProfit / summary.MatchedRevenueCNY * 100
	}
	fxRate := h.syncSvc.EffectiveFxRate(c.Request.Context())

	response.Success(c, companionSummaryDTO{
		From:                companionRFC3339(from),
		To:                  companionRFC3339(to),
		Revenue:             companionAmount(summary.RevenueCNY),
		MatchedRevenue:      companionAmount(summary.MatchedRevenueCNY),
		UpstreamCost:        companionAmount(summary.UpstreamCostCNY),
		BilledUpstreamCost:  companionAmount(summary.UpstreamCostCNY),
		GrossProfit:         companionAmount(grossProfit),
		MarginPercent:       strconv.FormatFloat(marginPercent, 'f', 2, 64),
		Matched:             summary.Matched,
		Unmatched:           summary.Unmatched,
		DownstreamMatched:   summary.Matched,
		DownstreamUnmatched: summary.Unmatched,
		UpstreamUnmatched:   summary.UpstreamUnmatched,
		RecordTotal:         summary.Matched + summary.Unmatched + summary.UpstreamUnmatched,
		BilledCount:         summary.BilledCount,
		CostPolicy:          "billed_or_subarx_api_or_rule",
		// calculated_count 与 billed_count 是一对：billed_count 是「成本来自真实账单」
		// 的条数，calculated_count 是「成本由规则/接口推算得出」的条数。
		// 收编后成本只来自 A6 真实账单，按规则折算成本的路径已不存在，故恒为 0——
		// 与同为已下线机制的 subarx_unallocated_count 保持一致。不要改成 matched。
		CalculatedCount:       0,
		SubarxUnallocatedCost: companionAmount(0),
		// Subarx 已下线，这两个字段保留是为了不改动前端契约。
		SubarxUnallocatedCount: 0,
		ProfitScope:            "matched_only",
		Currency:               "CNY",
		FxUSDCNY:               strconv.FormatFloat(fxRate, 'f', -1, 64),
		FxSource:               "配置值或运行时覆盖",
		// 汇率是记账口径而非实时牌价，没有「生效时刻」这种概念；
		// 这里给本次读取的时刻，表示这份汇总用的是此刻的汇率。
		FxEffectiveAt: time.Now().UTC().Format(time.RFC3339Nano),
		FxStale:       false,
	})
}

// ==================== 趋势 ====================

type companionTimeseriesPointDTO struct {
	Start             string `json:"start"`
	Revenue           string `json:"revenue"`
	UpstreamCost      string `json:"upstream_cost"`
	GrossProfit       string `json:"gross_profit"`
	Matched           int64  `json:"matched"`
	Unmatched         int64  `json:"unmatched"`
	UpstreamUnmatched int64  `json:"upstream_unmatched"`
	RecordTotal       int64  `json:"record_total"`
}

type companionTimeseriesDTO struct {
	From   string                        `json:"from"`
	To     string                        `json:"to"`
	Bucket string                        `json:"bucket"`
	Points []companionTimeseriesPointDTO `json:"points"`
}

// Timeseries 返回经营看板的趋势分桶。
//
// GET /admin/companion/timeseries
func (h *CompanionHandler) Timeseries(c *gin.Context) {
	from, to, err := h.companionWindow(c)
	if err != nil {
		writeReconciliationError(c, err)
		return
	}

	series, err := h.ledgerSvc.TimeSeries(c.Request.Context(), from, to)
	if err != nil {
		writeReconciliationError(c, err)
		return
	}

	// 即使没有任何数据也要返回空数组而不是 null：前端直接对 points 做遍历。
	points := make([]companionTimeseriesPointDTO, 0, len(series.Points))
	for i := range series.Points {
		point := &series.Points[i]
		grossProfit := point.RevenueCNY - point.UpstreamCostCNY
		points = append(points, companionTimeseriesPointDTO{
			Start:             companionRFC3339(point.Start),
			Revenue:           companionAmount(point.RevenueCNY),
			UpstreamCost:      companionAmount(point.UpstreamCostCNY),
			GrossProfit:       companionAmount(grossProfit),
			Matched:           point.Matched,
			Unmatched:         point.Unmatched,
			UpstreamUnmatched: point.UpstreamUnmatched,
			RecordTotal:       point.Matched + point.Unmatched + point.UpstreamUnmatched,
		})
	}

	response.Success(c, companionTimeseriesDTO{
		From:   companionRFC3339(from),
		To:     companionRFC3339(to),
		Bucket: series.BucketLabel,
		Points: points,
	})
}

// ==================== 明细 ====================

type companionRequestRowDTO struct {
	RecordType           string `json:"record_type"`
	SourceID             int64  `json:"source_id"`
	CreatedAt            string `json:"created_at"`
	RequestID            string `json:"request_id"`
	UpstreamRequestID    string `json:"upstream_request_id"`
	UserID               int64  `json:"user_id"`
	UserEmail            string `json:"user_email"`
	APIKeyID             int64  `json:"api_key_id"`
	AccountID            int64  `json:"account_id"`
	GroupID              int64  `json:"group_id"`
	GroupName            string `json:"group_name"`
	Model                string `json:"model"`
	InputTokens          int    `json:"input_tokens"`
	OutputTokens         int    `json:"output_tokens"`
	CacheTokens          int    `json:"cache_tokens"`
	Revenue              string `json:"revenue"`
	UpstreamCost         string `json:"upstream_cost"`
	BilledUpstreamCost   string `json:"billed_upstream_cost"`
	UpstreamCostOriginal string `json:"upstream_cost_original"`
	UpstreamCurrency     string `json:"upstream_currency"`
	GrossProfit          string `json:"gross_profit"`
	CostSource           string `json:"cost_source"`
	FxRateToCNY          string `json:"fx_rate_to_cny"`
	CostSourceLabel      string `json:"cost_source_label"`
	Matched              bool   `json:"matched"`
}

type companionRequestPageDTO struct {
	Items      []companionRequestRowDTO `json:"items"`
	Page       int                      `json:"page"`
	PageSize   int                      `json:"page_size"`
	Total      int64                    `json:"total"`
	TotalPages int64                    `json:"total_pages"`
	From       string                   `json:"from"`
	To         string                   `json:"to"`
	Status     string                   `json:"status"`
}

// Requests 返回经营看板的明细分页。
//
// GET /admin/companion/requests
func (h *CompanionHandler) Requests(c *gin.Context) {
	from, to, err := h.companionWindow(c)
	if err != nil {
		writeReconciliationError(c, err)
		return
	}

	page := parseCompanionInt(c.Query("page"), 1)
	pageSize := parseCompanionInt(c.Query("page_size"), service.ReconciliationDefaultPageSize)
	status := strings.TrimSpace(c.Query("status"))

	// 先在接口层归一化，再把生效值交给服务层并原样回显。
	//
	// 归一化必须只有一份实现：旧代码里接口层「越界就退回 50」、服务层「越界就截到 100」，
	// 于是 ?page_size=200 实际按 100 取数、响应却回显 50、total_pages 也按 50 算，
	// 前端据此渲染的分页控件与真实数据集对不上。status 同理：传 bogus 时返回的是
	// 全量数据，回显却写着 "bogus"。
	if page < 1 {
		page = 1
	}
	pageSize = service.NormalizeReconciliationPageSize(pageSize)
	status = service.NormalizeReconciliationStatus(status)

	rows, total, err := h.ledgerSvc.Requests(c.Request.Context(), from, to, status, page, pageSize)
	if err != nil {
		writeReconciliationError(c, err)
		return
	}

	fxRate := h.syncSvc.EffectiveFxRate(c.Request.Context())

	items := make([]companionRequestRowDTO, 0, len(rows))
	for i := range rows {
		row := &rows[i]

		// 上游成本未知时输出空串而不是 0，避免页面把「还没对账」显示成「上游免费」。
		upstreamCost := companionOptionalAmount(row.UpstreamCostCNY, row.HasUpstreamCost)
		billedCost := upstreamCost
		original := companionOptionalAmount(row.UpstreamCostOrig, row.HasUpstreamCost)
		currency := row.UpstreamCurrency
		if !row.HasUpstreamCost {
			currency = ""
		}
		grossProfit, hasProfit := row.ReconciliationGrossProfitCNY()

		rowFxRate := row.UpstreamFxRateCNY
		if rowFxRate <= 0 {
			rowFxRate = fxRate
		}

		items = append(items, companionRequestRowDTO{
			RecordType:           row.RecordType,
			SourceID:             row.SourceID,
			CreatedAt:            companionRFC3339(row.CreatedAt),
			RequestID:            row.RequestID,
			UpstreamRequestID:    row.UpstreamRequestID,
			UserID:               row.UserID,
			UserEmail:            row.UserEmail,
			APIKeyID:             row.APIKeyID,
			AccountID:            row.AccountID,
			GroupID:              row.GroupID,
			GroupName:            row.GroupName,
			Model:                row.Model,
			InputTokens:          row.InputTokens,
			OutputTokens:         row.OutputTokens,
			CacheTokens:          row.CacheTokens,
			Revenue:              companionAmount(row.RevenueCNY),
			UpstreamCost:         upstreamCost,
			BilledUpstreamCost:   billedCost,
			UpstreamCostOriginal: original,
			UpstreamCurrency:     currency,
			GrossProfit:          companionOptionalAmount(grossProfit, hasProfit),
			CostSource:           string(row.CostSource),
			FxRateToCNY:          strconv.FormatFloat(rowFxRate, 'f', -1, 64),
			// 标签必须非空：前端在未知 cost_source 时直接回落到这个字符串展示。
			CostSourceLabel: service.ReconciliationCostSourceLabel(row.CostSource),
			Matched:         row.Matched,
		})
	}

	totalPages := int64(0)
	if total > 0 {
		totalPages = (total + int64(pageSize) - 1) / int64(pageSize)
	}

	response.Success(c, companionRequestPageDTO{
		Items:      items,
		Page:       page,
		PageSize:   pageSize,
		Total:      total,
		TotalPages: totalPages,
		From:       companionRFC3339(from),
		To:         companionRFC3339(to),
		Status:     status,
	})
}

// ==================== 账号规则 ====================

// companionAccountRuleDTO 是账号规则列表的一行。
//
// GroupChannelCount 是该分组的真实渠道数（与主站分组页 account_count 同口径，只算未软删
// 账号），不是本列表里该分组的行数——一行是一个账号，账号只展示它优先级最高的那个分组，
// 按行数统计会把「展示位被别的分组抢走」的成员当成不存在（dev 库真实踩过：分组 7 有 2 个
// 渠道却显示 1 个）。
type companionAccountRuleDTO struct {
	AccountID          int64  `json:"account_id"`
	Provider           string `json:"provider"`
	TokenName          string `json:"token_name"`
	Multiplier         string `json:"multiplier"`
	Version            int64  `json:"version"`
	Enabled            bool   `json:"enabled"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
	Configured         bool   `json:"configured"`
	Current            bool   `json:"current"`
	GroupID            int64  `json:"group_id"`
	GroupName          string `json:"group_name"`
	GroupPriority      int    `json:"group_priority"`
	GroupChannelCount  int64  `json:"group_channel_count"`
	AccountName        string `json:"account_name"`
	AccountPlatform    string `json:"account_platform"`
	AccountStatus      string `json:"account_status"`
	AccountSchedulable bool   `json:"account_schedulable"`
	UsageCount         int64  `json:"usage_count"`
	FirstSeen          string `json:"first_seen"`
	LastSeen           string `json:"last_seen"`
	// RecentModel / RecentGroup* 是账号**全历史最后一次调用**的模型与分组，
	// 不受请求里的时间筛选影响（文档 19）。Field 名与语义都刻意与窗口内的
	// usage_count / first_seen / last_seen 区分开。
	RecentModel     string `json:"recent_model"`
	RecentGroupID   int64  `json:"recent_group_id"`
	RecentGroupName string `json:"recent_group_name"`
}

type companionAccountRuleListDTO struct {
	Items                []companionAccountRuleDTO `json:"items"`
	UnconfiguredAccounts int64                     `json:"unconfigured_accounts"`
	From                 string                    `json:"from"`
	To                   string                    `json:"to"`
}

func toCompanionAccountRuleDTO(view *service.ReconciliationAccountRuleView) companionAccountRuleDTO {
	dto := companionAccountRuleDTO{
		AccountID:          view.AccountID,
		Provider:           view.Provider,
		TokenName:          view.ExternalKey,
		Version:            view.Version,
		Enabled:            view.Enabled,
		Configured:         view.Configured,
		Current:            view.Current,
		GroupID:            view.GroupID,
		GroupName:          view.GroupName,
		GroupPriority:      view.GroupPriority,
		GroupChannelCount:  view.GroupChannelCount,
		AccountName:        view.AccountName,
		AccountPlatform:    view.AccountPlatform,
		AccountStatus:      view.AccountStatus,
		AccountSchedulable: view.AccountSchedulable,
		UsageCount:         view.UsageCount,
		RecentModel:        view.RecentModel,
		RecentGroupID:      view.RecentGroupID,
		RecentGroupName:    view.RecentGroupName,
	}
	if view.Multiplier != nil {
		dto.Multiplier = strconv.FormatFloat(*view.Multiplier, 'f', -1, 64)
	}
	if !view.CreatedAt.IsZero() {
		dto.CreatedAt = companionRFC3339(view.CreatedAt)
	}
	if !view.UpdatedAt.IsZero() {
		dto.UpdatedAt = companionRFC3339(view.UpdatedAt)
	}
	if view.FirstSeen != nil {
		dto.FirstSeen = companionRFC3339(*view.FirstSeen)
	}
	if view.LastSeen != nil {
		dto.LastSeen = companionRFC3339(*view.LastSeen)
	}
	return dto
}

// AccountRules 返回账号规则视图。
//
// 列表包含全部账号（含范围内零调用的账号），因此永远不会是空页面，
// 管理员可以为任何一个账号补规则。
//
// GET /admin/companion/account-rules
func (h *CompanionHandler) AccountRules(c *gin.Context) {
	from, to, err := h.companionWindow(c)
	if err != nil {
		writeReconciliationError(c, err)
		return
	}

	list, err := h.ruleSvc.List(c.Request.Context(), from, to)
	if err != nil {
		writeReconciliationError(c, err)
		return
	}

	items := make([]companionAccountRuleDTO, 0, len(list.Items))
	for i := range list.Items {
		items = append(items, toCompanionAccountRuleDTO(&list.Items[i]))
	}

	response.Success(c, companionAccountRuleListDTO{
		Items:                items,
		UnconfiguredAccounts: list.UnconfiguredAccounts,
		From:                 companionRFC3339(list.From),
		To:                   companionRFC3339(list.To),
	})
}

type companionAccountRuleInputDTO struct {
	Provider   string   `json:"provider"`
	TokenName  string   `json:"token_name"`
	Multiplier *float64 `json:"multiplier"`
	Enabled    *bool    `json:"enabled"`
}

type companionAccountRuleSavedDTO struct {
	Success bool                    `json:"success"`
	Rule    companionAccountRuleDTO `json:"rule"`
}

// UpsertAccountRule 保存指定账号的上游规则。
//
// PUT /admin/companion/account-rules/:account_id
func (h *CompanionHandler) UpsertAccountRule(c *gin.Context) {
	accountID, ok := companionAccountID(c)
	if !ok {
		return
	}

	var input companionAccountRuleInputDTO
	if err := bindCompanionJSON(c, &input); err != nil {
		response.BadRequest(c, "COMPANION_BAD_REQUEST: 请求体不是合法的 JSON: "+err.Error())
		return
	}

	// enabled 缺省视为启用：面板上勾掉才是停用，不传不应该等于停用。
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}

	rule, err := h.ruleSvc.Upsert(c.Request.Context(), accountID, input.Provider, input.TokenName, input.Multiplier, enabled)
	if err != nil {
		writeReconciliationError(c, err)
		return
	}

	response.Success(c, companionAccountRuleSavedDTO{
		Success: true,
		Rule:    toCompanionAccountRuleDTO(rule),
	})
}

type companionAccountRuleDeletedDTO struct {
	Success bool  `json:"success"`
	Deleted int64 `json:"deleted"`
}

// DeleteAccountRule 删除指定账号的上游规则。
//
// DELETE /admin/companion/account-rules/:account_id
func (h *CompanionHandler) DeleteAccountRule(c *gin.Context) {
	accountID, ok := companionAccountID(c)
	if !ok {
		return
	}

	deleted, err := h.ruleSvc.Delete(c.Request.Context(), accountID)
	if err != nil {
		writeReconciliationError(c, err)
		return
	}
	response.Success(c, companionAccountRuleDeletedDTO{Success: true, Deleted: deleted})
}

// companionAccountID 解析并校验路径上的账号 ID。
func companionAccountID(c *gin.Context) (int64, bool) {
	raw := strings.TrimSpace(c.Param("account_id"))
	accountID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "COMPANION_BAD_REQUEST: account_id 必须是数字账号 ID")
		return 0, false
	}
	return accountID, true
}

// ==================== 同步动作 ====================

type companionCollectResultDTO struct {
	Success bool `json:"success"`
}

// Collect 触发一次同步采集。
//
// 立即返回而不是等采集跑完：前端 HTTP 超时是 30 秒，而上游单次拉取最长 90 秒，
// 同步等待必然超时。采集在后台继续，进度体现在随后的汇总与明细里。
//
// POST /admin/companion/collect
func (h *CompanionHandler) Collect(c *gin.Context) {
	// 忽略请求体内容：窗口由服务端配置决定，不接受前端指定，避免被诱导去拉超长区间。
	_, _ = io.Copy(io.Discard, io.LimitReader(c.Request.Body, companionMaxRequestBytes))

	if !h.syncSvc.TriggerAsync(c.Request.Context()) {
		response.Error(c, http.StatusConflict, "COMPANION_BAD_REQUEST: 已有一轮采集正在进行，请稍后再试")
		return
	}
	response.Success(c, companionCollectResultDTO{Success: true})
}

type companionBackfillStatusDTO struct {
	Status    string `json:"status"`
	Running   bool   `json:"running"`
	From      string `json:"from"`
	To        string `json:"to"`
	Cursor    string `json:"cursor"`
	Processed int64  `json:"processed"`
	Error     string `json:"error"`
}

// A6BackfillStatus 返回 A6 历史回填的进度。
//
// GET /admin/companion/a6/backfill
func (h *CompanionHandler) A6BackfillStatus(c *gin.Context) {
	status, err := h.syncSvc.BackfillStatus(c.Request.Context())
	if err != nil {
		writeReconciliationError(c, err)
		return
	}

	dto := companionBackfillStatusDTO{
		Status:    status.Status,
		Running:   status.Running,
		Processed: status.Processed,
		Error:     status.Error,
	}
	if status.From != nil {
		dto.From = companionRFC3339(*status.From)
	}
	if status.To != nil {
		dto.To = companionRFC3339(*status.To)
	}
	if status.Cursor != nil {
		dto.Cursor = companionRFC3339(*status.Cursor)
	}
	response.Success(c, dto)
}

type companionBackfillRequestDTO struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type companionBackfillStartedDTO struct {
	Success bool   `json:"success"`
	From    string `json:"from"`
	To      string `json:"to"`
}

// StartA6Backfill 启动 A6 历史账单回填。
//
// 与采集一样立即返回，回填在后台分段推进，进度由 A6BackfillStatus 查询。
//
// POST /admin/companion/a6/backfill
func (h *CompanionHandler) StartA6Backfill(c *gin.Context) {
	var input companionBackfillRequestDTO
	if err := bindCompanionJSON(c, &input); err != nil {
		response.BadRequest(c, "COMPANION_BAD_REQUEST: 请求体不是合法的 JSON: "+err.Error())
		return
	}

	var fromPtr, toPtr *time.Time
	if strings.TrimSpace(input.From) != "" {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(input.From))
		if err != nil {
			response.BadRequest(c, "COMPANION_BAD_REQUEST: from 需要 RFC3339 时间")
			return
		}
		parsed = parsed.UTC()
		fromPtr = &parsed
	}
	if strings.TrimSpace(input.To) != "" {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(input.To))
		if err != nil {
			response.BadRequest(c, "COMPANION_BAD_REQUEST: to 需要 RFC3339 时间")
			return
		}
		parsed = parsed.UTC()
		toPtr = &parsed
	}

	from, to, err := h.syncSvc.StartBackfill(c.Request.Context(), fromPtr, toPtr)
	if err != nil {
		writeReconciliationError(c, err)
		return
	}

	response.Accepted(c, companionBackfillStartedDTO{
		Success: true,
		From:    companionRFC3339(from),
		To:      companionRFC3339(to),
	})
}

type companionUpstreamRecordDTO struct {
	Provider            string  `json:"provider"`
	UpstreamRequestID   string  `json:"upstream_request_id"`
	Cost                float64 `json:"cost"`
	Currency            string  `json:"currency"`
	FxRateToCNY         float64 `json:"fx_rate_to_cny"`
	OccurredAt          string  `json:"occurred_at"`
	Model               string  `json:"model"`
	TokenName           string  `json:"token_name"`
	InputTokens         int     `json:"input_tokens"`
	OutputTokens        int     `json:"output_tokens"`
	CacheReadTokens     int     `json:"cache_read_tokens"`
	CacheCreationTokens int     `json:"cache_creation_tokens"`
	CacheTokensTotal    int     `json:"cache_tokens_total"`
	Source              string  `json:"source"`
}

type companionUpstreamImportRequestDTO struct {
	Records []companionUpstreamRecordDTO `json:"records"`
}

type companionUpstreamImportResultDTO struct {
	Success  bool  `json:"success"`
	Imported int64 `json:"imported"`
}

// ImportUpstream 手动导入上游逐笔账单。
//
// 请求体既接受 {"records":[...]}，也接受顶层直接是一个数组，两种写法在旧实现里都被用过。
//
// POST /admin/companion/upstream/import
func (h *CompanionHandler) ImportUpstream(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, companionMaxRequestBytes))
	if err != nil {
		response.BadRequest(c, "COMPANION_BAD_REQUEST: 读取请求体失败: "+err.Error())
		return
	}

	records, err := decodeCompanionUpstreamRecords(body)
	if err != nil {
		response.BadRequest(c, "COMPANION_BAD_REQUEST: 请求体格式不正确: "+err.Error())
		return
	}

	inputs := make([]service.ReconciliationUpstreamRecordInput, 0, len(records))
	for i := range records {
		record := &records[i]
		input := service.ReconciliationUpstreamRecordInput{
			Provider:          record.Provider,
			UpstreamRequestID: record.UpstreamRequestID,
			Cost:              record.Cost,
			Currency:          record.Currency,
			FxRateToCNY:       record.FxRateToCNY,
			Model:             record.Model,
			TokenName:         record.TokenName,
			InputTokens:       record.InputTokens,
			OutputTokens:      record.OutputTokens,
			CacheReadTokens:   record.CacheReadTokens,
			CacheCreation:     record.CacheCreationTokens,
			CacheTokensTotal:  record.CacheTokensTotal,
			Source:            record.Source,
		}
		if strings.TrimSpace(record.OccurredAt) != "" {
			parsed, parseErr := time.Parse(time.RFC3339, strings.TrimSpace(record.OccurredAt))
			if parseErr != nil {
				response.BadRequest(c, "COMPANION_BAD_REQUEST: occurred_at 需要 RFC3339 时间")
				return
			}
			parsed = parsed.UTC()
			input.OccurredAt = &parsed
		}
		inputs = append(inputs, input)
	}

	imported, err := h.syncSvc.ImportUpstreamRecords(c.Request.Context(), inputs)
	if err != nil {
		writeReconciliationError(c, err)
		return
	}
	response.Success(c, companionUpstreamImportResultDTO{Success: true, Imported: imported})
}

// decodeCompanionUpstreamRecords 兼容两种请求体写法。
func decodeCompanionUpstreamRecords(body []byte) ([]companionUpstreamRecordDTO, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var records []companionUpstreamRecordDTO
		if err := json.Unmarshal([]byte(trimmed), &records); err != nil {
			return nil, err
		}
		return records, nil
	}
	var request companionUpstreamImportRequestDTO
	if err := json.Unmarshal([]byte(trimmed), &request); err != nil {
		return nil, err
	}
	return request.Records, nil
}

// ==================== 小工具 ====================

// bindCompanionJSON 解析可选请求体：空体不报错，按零值处理。
func bindCompanionJSON(c *gin.Context, target any) error {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, companionMaxRequestBytes))
	if err != nil {
		return err
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil
	}
	return json.Unmarshal([]byte(trimmed), target)
}

// parseCompanionInt 解析查询串里的整数，非法或缺失时返回兜底值。
func parseCompanionInt(raw string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return parsed
}
