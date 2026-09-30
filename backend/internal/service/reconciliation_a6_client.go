package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/shopspring/decimal"
)

// ==================== 常量 ====================

const (
	// a6LogComponent 本文件所有日志的组件名。
	a6LogComponent = "service.reconciliation_a6"

	// a6StatusPath 取计费单位（quota_per_unit）的接口路径。
	a6StatusPath = "/api/status"
	// a6SelfLogPath 拉取逐笔账单的接口路径。
	a6SelfLogPath = "/api/log/self"
	// a6LogTypeConsumption 账单类型：2 表示消费类日志，固定传 2。
	a6LogTypeConsumption = "2"
	// a6UserAgent 让上游访问日志能识别出采集来源。
	a6UserAgent = "sub2api-reconciliation/1.0"

	// a6DefaultTimeout 单次请求的整体超时；配置未注入 Timeout 时使用（90 秒）。
	a6DefaultTimeout = 90 * time.Second
	// a6MaxResponseBytes 单次响应体读取上限：16 MB，超限直接报错而不是截断后解析。
	a6MaxResponseBytes = 16 << 20
	// a6BodyDrainBytes 提前返回（例如 401/403）时最多再读掉多少字节，仅为复用连接。
	a6BodyDrainBytes = 4 << 10

	// a6MaxAttempts 单页请求的最大尝试次数：1 次首发 + 2 次重试。
	a6MaxAttempts = 3

	// a6QuotaPerUnitTTL quota_per_unit 的缓存有效期；期内不重复请求 /api/status。
	a6QuotaPerUnitTTL = 10 * time.Minute

	// a6DefaultPageSize 未指定时的每页条数。
	a6DefaultPageSize = 100
	// a6MaxPageSize 允许的每页条数上限，防止一次拉取过重。
	a6MaxPageSize = 500
	// a6DefaultMaxPages 未指定时的翻页上限。
	a6DefaultMaxPages = 50
	// a6MaxMaxPages 翻页上限的硬顶。
	a6MaxMaxPages = 1000

	// a6TotalUnknown 上游未回传 total 时 A6BillPage.Total 的取值。
	a6TotalUnknown = -1

	// a6MaxJSONUnwrapLayers other 字段最多解开几层 JSON 编码（兼容被双重编码的字符串）。
	a6MaxJSONUnwrapLayers = 3
	// a6UpstreamMessageLimit 上游 message 写进错误信息时的最大字符数，避免超长文本进日志。
	a6UpstreamMessageLimit = 200

	// a6MinPlausibleUnix 时间戳合理区间下限（2000-01-01 UTC），更低的值视为脏数据。
	a6MinPlausibleUnix int64 = 946_684_800
	// a6MaxPlausibleUnix 时间戳合理区间上限（2100-01-01 UTC）。
	a6MaxPlausibleUnix int64 = 4_102_444_800
	// a6MillisThreshold 秒与毫秒的分界：大于该值按毫秒处理并除以 1000
	// （1e12 秒约合公元 33658 年，正常账单不可能取到）。
	a6MillisThreshold int64 = 1_000_000_000_000
)

// a6RetryBackoffs 是第 2、3 次尝试前的退避时长：0.5s / 1.0s。
//
// 长度必须等于 a6MaxAttempts-1，由数组长度表达式保证。
var a6RetryBackoffs = [a6MaxAttempts - 1]time.Duration{500 * time.Millisecond, time.Second}

// a6TimeLayouts 时间字符串的兼容解析格式。
//
// 注意 "2006-01-02 15:04:05" 这类不带时区的写法按 UTC 解释：上游如果实际写的是
// 本地时间，账单时间会整体偏移。宁可让账单落到「上游待匹配」再人工核对，
// 也不在这里猜时区（猜错会让匹配窗口内的账单静默错配）。
var a6TimeLayouts = [...]string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02",
}

// ==================== 错误 ====================

var (
	// ErrReconciliationA6NotConfigured 站点基址或凭据缺失，客户端无法发起请求。
	ErrReconciliationA6NotConfigured = infraerrors.ServiceUnavailable(
		"RECONCILIATION_A6_NOT_CONFIGURED",
		"A6 base_url/access_token/user_id is not configured",
	)
	// ErrReconciliationA6AuthFailed 上游以 401/403 拒绝凭据。
	//
	// 单独成型是为了让上层能区分「凭据无效」与「网络不通」。刻意用 502 而不是
	// 401/403：管理端契约要求对账接口绝不返回 401/403，否则前端会清掉登录态跳 /login。
	// 上游原始状态码放在 metadata.upstream_status 里。
	ErrReconciliationA6AuthFailed = infraerrors.New(
		http.StatusBadGateway,
		"RECONCILIATION_A6_AUTH_FAILED",
		"A6 rejected the configured credentials (upstream 401/403)",
	)
	// ErrReconciliationA6Timeout 请求超时（连接、响应头或整体超时）。
	ErrReconciliationA6Timeout = infraerrors.GatewayTimeout(
		"RECONCILIATION_A6_TIMEOUT",
		"A6 request timed out",
	)
	// ErrReconciliationA6RequestFailed 传输层失败，或上游返回非 2xx（认证失败除外）。
	ErrReconciliationA6RequestFailed = infraerrors.ServiceUnavailable(
		"RECONCILIATION_A6_REQUEST_FAILED",
		"A6 request failed",
	)
	// ErrReconciliationA6ResponseTooLarge 响应体超过 16 MB 上限。
	ErrReconciliationA6ResponseTooLarge = infraerrors.New(
		http.StatusBadGateway,
		"RECONCILIATION_A6_RESPONSE_TOO_LARGE",
		"A6 response exceeded the 16 MB limit",
	)
	// ErrReconciliationA6InvalidResponse 响应不是合法 JSON，或缺少约定的数据结构。
	ErrReconciliationA6InvalidResponse = infraerrors.New(
		http.StatusBadGateway,
		"RECONCILIATION_A6_INVALID_RESPONSE",
		"A6 returned a malformed response",
	)
	// ErrReconciliationA6Rejected 上游以 success=false 明确拒绝本次请求。
	ErrReconciliationA6Rejected = infraerrors.New(
		http.StatusBadGateway,
		"RECONCILIATION_A6_REJECTED",
		"A6 rejected the request",
	)
	// ErrReconciliationA6QuotaPerUnitUnavailable 从未成功取到过 quota_per_unit，无法把 quota 换算成金额。
	ErrReconciliationA6QuotaPerUnitUnavailable = infraerrors.ServiceUnavailable(
		"RECONCILIATION_A6_QUOTA_PER_UNIT_UNAVAILABLE",
		"A6 quota_per_unit is unavailable",
	)
	// ErrReconciliationA6PageLimitReached 翻页达到上限时窗口仍未取完。
	//
	// 该错误会与已经取到的账单一起返回：调用方应先入库这部分，再缩小窗口或拆分区间重拉。
	ErrReconciliationA6PageLimitReached = infraerrors.ServiceUnavailable(
		"RECONCILIATION_A6_PAGE_LIMIT_REACHED",
		"A6 bill window exceeded the page limit",
	)
	// ErrReconciliationA6InvalidWindow 时间窗口缺失或倒置（EndTime 早于 StartTime）。
	ErrReconciliationA6InvalidWindow = infraerrors.BadRequest(
		"RECONCILIATION_A6_INVALID_WINDOW",
		"A6 bill window is invalid",
	)
)

// IsReconciliationA6AuthError 判断错误是否属于「凭据被上游拒绝」。
//
// 供上层给出「A6 凭据无效」而不是「A6 网络不通」；对包装过的错误同样有效。
func IsReconciliationA6AuthError(err error) bool {
	return errors.Is(err, ErrReconciliationA6AuthFailed)
}

// ==================== 配置与结果类型 ====================

// ReconciliationA6Config 是 A6 客户端的注入配置。
//
// 凭据一律由 config 层注入：本文件不读环境变量、不读数据库、不落任何日志。
type ReconciliationA6Config struct {
	// BaseURL A6 站点基址，例如 https://a6api.com；末尾斜杠可有可无。
	BaseURL string
	// AccessToken A6 系统访问令牌，作为 Authorization: Bearer 发送。
	AccessToken string
	// UserID A6 用户标识，作为 New-API-User 头发送。
	UserID string
	// Timeout 单次请求整体超时；<=0 时用 a6DefaultTimeout（90 秒）。
	Timeout time.Duration
}

// A6BillQuery 描述一次账单拉取的窗口与过滤条件。
//
// StartTime / EndTime 会按 Unix 秒下发（不是毫秒、不是 RFC3339）。
// 上层若要吸收时钟偏差，请在 EndTime 上自行加 60 秒——客户端不会再放大时间。
type A6BillQuery struct {
	// TokenName A6 令牌名；为空表示不按令牌过滤（此时不下发该参数）。
	TokenName string
	// ModelName 模型名；为空表示不按模型过滤（此时不下发该参数）。
	ModelName string
	// StartTime 窗口起点（含）。
	StartTime time.Time
	// EndTime 窗口终点（含）。
	EndTime time.Time
	// PageSize 每页条数；<=0 用 a6DefaultPageSize（100），上限 a6MaxPageSize（500）。
	PageSize int
	// MaxPages 翻页上限；<=0 用 a6DefaultMaxPages（50），硬顶 a6MaxMaxPages（1000）。
	MaxPages int
}

// A6BillPage 是单页账单的解析结果。
type A6BillPage struct {
	// Items 规范化后的账单。
	Items []ReconciliationA6Bill
	// Total 上游声明的窗口内总条数；上游没回传时为 a6TotalUnknown（-1）。
	Total int
	// Page 上游回显的页码；缺失时为本次请求的页码。
	Page int
	// PageSize 上游回显的每页条数；缺失时为本次请求的每页条数。
	PageSize int
	// HasMore 是否还有下一页（优先按 total 判断，total 缺失时按本页是否取满推断）。
	HasMore bool
}

// ReconciliationA6Bill 是一条规范化后的 A6 账单，供主线转换成数据库行。
//
// 金额保持 decimal：quota 是整数额度、quota_per_unit 是每单位额度对应的美元数，
// 用 decimal 做除法才不会让金额在 float64 上丢精度。
type ReconciliationA6Bill struct {
	// RequestID A6 账单的请求标识，对应 reconciliation_upstream_bills.upstream_request_id。
	RequestID string
	// OccurredAt 账单发生时间，恒为 UTC，对应 occurred_at。
	OccurredAt time.Time
	// Model 模型名，对应 model。
	Model string
	// TokenName A6 令牌名，对应 token_name，用于反查本站账号。
	TokenName string
	// InputTokens 输入 token 数，对应 input_tokens。
	InputTokens int
	// OutputTokens 输出 token 数，对应 output_tokens。
	OutputTokens int
	// CacheReadTokens 缓存读 token 数，对应 cache_read_tokens。
	CacheReadTokens int
	// CacheCreationTokens 缓存写 token 数，对应 cache_creation_tokens。
	CacheCreationTokens int
	// CacheTokensTotal 上游口径的缓存合计。
	//
	// 分列字段存在时等于 CacheReadTokens + CacheCreationTokens；上游只回一个合并值
	// （other.cache_tokens）时，这里保留该合并值而两个分列字段为 0。旧实现就是拿这个
	// 合计值做匹配的，所以主线做组合匹配时应以本字段为准。
	CacheTokensTotal int
	// Quota A6 原始额度值（未换算）。
	Quota decimal.Decimal
	// QuotaPerUnit 本次换算所用的 quota_per_unit。
	QuotaPerUnit decimal.Decimal
	// CostUSD 原始金额（美元）= Quota ÷ QuotaPerUnit，对应 cost_original。
	CostUSD decimal.Decimal
	// Other 原始 other 字段：对象与被双重编码的 JSON 字符串都已解开；无法解析时为 nil。
	Other map[string]any
	// Raw 上游原始记录，供 raw jsonb 留存与重放。
	Raw map[string]any
}

// ==================== 客户端 ====================

// A6Client 访问上游 A6 的账单接口。
//
// 只做「取数 + 规范化」：鉴权头、quota_per_unit 缓存、分页、重试、金额换算。
// 不做采集调度、不落库、不做匹配——那些由主线负责。
type A6Client struct {
	// cfgMu 保护 cfg：面板上改完 A6 配置后客户端不重建，只热替换这一份配置。
	// 与 mu（quota_per_unit 缓存）分开，两把锁互不牵连。
	cfgMu sync.RWMutex
	cfg   ReconciliationA6Config

	httpClient *http.Client
	// now 便于单测控制时间；生产为 time.Now。
	now func() time.Time

	// mu 保护 quotaPerUnit / quotaFetchedAt。
	mu sync.Mutex
	// quotaPerUnit 最近一次成功取到的计费单位；刷新失败时保留旧值继续可用。
	quotaPerUnit decimal.Decimal
	// quotaFetchedAt quotaPerUnit 的取回时刻，用于 TTL 判断。
	quotaFetchedAt time.Time
}

// normalizeA6Config 统一基址与凭据的空白处理，并给超时兜底。
//
// 构造与热替换共用它，避免两条路径对同一份配置做出不同的规整。
func normalizeA6Config(cfg ReconciliationA6Config) ReconciliationA6Config {
	if cfg.Timeout <= 0 {
		cfg.Timeout = a6DefaultTimeout
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.AccessToken = strings.TrimSpace(cfg.AccessToken)
	cfg.UserID = strings.TrimSpace(cfg.UserID)
	return cfg
}

// a6ConfigUsable 报告一份配置是否齐备且可用：三者非空 + 基址是绝对的 http(s) URL。
//
// 客户端发请求前的校验与接口层 /status 的健康判定共用这一个谓词，
// 避免出现「页面显示健康、采集器却因为地址不合法一直跳过」的自相矛盾。
func a6ConfigUsable(cfg ReconciliationA6Config) bool {
	if strings.TrimSpace(cfg.BaseURL) == "" ||
		strings.TrimSpace(cfg.AccessToken) == "" ||
		strings.TrimSpace(cfg.UserID) == "" {
		return false
	}
	return isValidReconciliationA6BaseURL(cfg.BaseURL)
}

// NewA6Client 创建 A6 客户端。
//
// 配置缺失不在这里报错（config 层可能先建对象后填值），而是在每次请求前由校验
// 返回 ErrReconciliationA6NotConfigured。
func NewA6Client(cfg ReconciliationA6Config) *A6Client {
	cfg = normalizeA6Config(cfg)
	return &A6Client{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: cfg.Timeout},
		now:        time.Now,
	}
}

// Config 返回当前配置的快照。
func (c *A6Client) Config() ReconciliationA6Config {
	if c == nil {
		return ReconciliationA6Config{}
	}
	c.cfgMu.RLock()
	defer c.cfgMu.RUnlock()
	return c.cfg
}

// SetConfig 热替换客户端使用的基址与凭据。
//
// 面板保存后下一轮拉取就能用上新凭据，不需要重建客户端、更不需要重启进程。
// 在途请求用各自开始时读到的快照（见 getJSONWithRetry 的参数），不会被中途换掉；
// HTTP 超时仍取自构造时的配置，面板没有修改入口，因此这里不动 httpClient。
func (c *A6Client) SetConfig(cfg ReconciliationA6Config) {
	if c == nil {
		return
	}
	cfg = normalizeA6Config(cfg)
	c.cfgMu.Lock()
	c.cfg = cfg
	c.cfgMu.Unlock()
}

// Configured 报告基址与凭据是否齐全可用。
//
// 采集侧应先问它，缺失时直接跳过本轮，而不是等第一次请求失败再报错。
func (c *A6Client) Configured() bool {
	return c.validate(c.Config()) == nil
}

// validate 检查基址与凭据；同时校验基址是绝对的 HTTP(S) URL。
//
// 配置由调用方以快照传入：校验与随后的请求必须用同一份，否则并发热替换时
// 会出现「校验通过、请求用了一份空配置」的窗口。
func (c *A6Client) validate(cfg ReconciliationA6Config) error {
	if c == nil {
		return ErrReconciliationA6NotConfigured
	}
	if !a6ConfigUsable(cfg) {
		return ErrReconciliationA6NotConfigured
	}
	return nil
}

// ==================== 计费单位 ====================

// QuotaPerUnit 返回 A6 的计费单位 quota_per_unit（quota ÷ quota_per_unit = 美元）。
//
// 带 TTL 缓存：缓存有效期内直接返回缓存值，不重复请求 /api/status。
// 刷新失败时沿用上一次成功取到的值（只记一条日志），只有从未成功取到过才返回
// ErrReconciliationA6QuotaPerUnitUnavailable。
func (c *A6Client) QuotaPerUnit(ctx context.Context) (decimal.Decimal, error) {
	cfg := c.Config()
	if err := c.validate(cfg); err != nil {
		return decimal.Zero, err
	}
	now := c.now()
	c.mu.Lock()
	cached := c.quotaPerUnit
	fetchedAt := c.quotaFetchedAt
	c.mu.Unlock()
	if cached.IsPositive() && now.Sub(fetchedAt) < a6QuotaPerUnitTTL {
		return cached, nil
	}

	// 取数在锁外：慢请求不该把其它 goroutine 堵在缓存锁上。
	value, err := c.fetchQuotaPerUnit(ctx, cfg)
	if err != nil {
		if cached.IsPositive() {
			logger.LegacyPrintf(a6LogComponent, "quota_per_unit_refresh_failed_keep_cached: err=%v", err)
			return cached, nil
		}
		logger.LegacyPrintf(a6LogComponent, "quota_per_unit_unavailable: err=%v", err)
		return decimal.Zero, ErrReconciliationA6QuotaPerUnitUnavailable.WithCause(err)
	}

	c.mu.Lock()
	c.quotaPerUnit = value
	c.quotaFetchedAt = now
	c.mu.Unlock()
	return value, nil
}

// fetchQuotaPerUnit 取一次 /api/status 并解析 quota_per_unit（数字或字符串都能解析）。
func (c *A6Client) fetchQuotaPerUnit(ctx context.Context, cfg ReconciliationA6Config) (decimal.Decimal, error) {
	envelope, err := c.getJSONWithRetry(ctx, cfg, a6StatusPath, nil)
	if err != nil {
		return decimal.Zero, err
	}
	if err := a6CheckSuccess(envelope); err != nil {
		return decimal.Zero, err
	}
	data, _ := a6MapValue(mustA6Lookup(envelope, "data"))
	raw, ok := a6Lookup(data, "quota_per_unit")
	if !ok {
		return decimal.Zero, ErrReconciliationA6InvalidResponse.WithCause(
			errors.New("A6 status response has no quota_per_unit"),
		)
	}
	value, ok := a6Decimal(raw)
	if !ok || !value.IsPositive() {
		return decimal.Zero, ErrReconciliationA6InvalidResponse.WithCause(
			errors.New("A6 returned an invalid quota_per_unit"),
		)
	}
	return value, nil
}

// ==================== 账单拉取 ====================

// FetchBillsPage 拉取单页账单。
//
// page 从 1 开始。内部每页最多尝试 a6MaxAttempts 次（退避 0.5s / 1.0s），
// ctx 取消会立即中止。金额按缓存的 quota_per_unit 换算。
//
// 返回的 Items 只包含可用的记录：缺少 request_id、时间无法解析或 quota 非法的记录
// 会被跳过（页级记一条日志），不会以 0 成本混进账本。
func (c *A6Client) FetchBillsPage(ctx context.Context, query A6BillQuery, page int) (*A6BillPage, error) {
	cfg := c.Config()
	if err := c.validate(cfg); err != nil {
		return nil, err
	}
	if err := a6ValidateQuery(query); err != nil {
		return nil, err
	}
	if page < 1 {
		page = 1
	}
	pageSize := a6NormalizePageSize(query.PageSize)
	quotaPerUnit, err := c.QuotaPerUnit(ctx)
	if err != nil {
		return nil, err
	}

	envelope, err := c.getJSONWithRetry(ctx, cfg, a6SelfLogPath, a6BillQueryParams(query, page, pageSize))
	if err != nil {
		return nil, err
	}
	if err := a6CheckSuccess(envelope); err != nil {
		return nil, err
	}
	extracted, err := a6ExtractBillItems(envelope)
	if err != nil {
		return nil, err
	}

	bills := make([]ReconciliationA6Bill, 0, len(extracted.items))
	skipped := 0
	firstSkipReason := ""
	for _, item := range extracted.items {
		bill, skipReason := c.normalizeBill(item, quotaPerUnit)
		if skipReason != "" {
			skipped++
			if firstSkipReason == "" {
				firstSkipReason = skipReason
			}
			continue
		}
		bills = append(bills, bill)
	}
	if skipped > 0 {
		logger.LegacyPrintf(
			a6LogComponent,
			"bill_records_skipped: page=%d skipped=%d parsed=%d first_reason=%s",
			page, skipped, len(bills), firstSkipReason,
		)
	}

	echoPage := extracted.page
	if echoPage <= 0 {
		echoPage = page
	}
	echoPageSize := extracted.pageSize
	if echoPageSize <= 0 {
		echoPageSize = pageSize
	}
	return &A6BillPage{
		Items:    bills,
		Total:    extracted.total,
		Page:     echoPage,
		PageSize: echoPageSize,
		HasMore:  a6HasMorePage(extracted.total, echoPage, echoPageSize, len(extracted.items)),
	}, nil
}

// FetchBills 按时间窗口分页拉取全部账单。
//
// 内部循环翻页直到取完或达到 MaxPages；每页重试 3 次、退避 0.5s / 1.0s，并全程尊重 ctx。
//
// 出错时已成功拉到的账单照常返回，调用方可以先入库再决定补拉：
//   - 达到翻页上限仍未取完：返回 ErrReconciliationA6PageLimitReached，应缩小窗口重试；
//   - ctx 取消：返回 context.Canceled / context.DeadlineExceeded；
//   - 其它错误：见文件顶部的错误定义。
//
// 上游在翻页期间若有新账单写入，理论上可能出现重复行；幂等由数据库的
// (provider, upstream_request_id) 唯一索引负责，客户端不做去重。
func (c *A6Client) FetchBills(ctx context.Context, query A6BillQuery) ([]ReconciliationA6Bill, error) {
	if err := c.validate(c.Config()); err != nil {
		return nil, err
	}
	if err := a6ValidateQuery(query); err != nil {
		return nil, err
	}
	maxPages := a6NormalizeMaxPages(query.MaxPages)
	bills := make([]ReconciliationA6Bill, 0, a6NormalizePageSize(query.PageSize))
	for page := 1; page <= maxPages; page++ {
		result, err := c.FetchBillsPage(ctx, query, page)
		if err != nil {
			return bills, err
		}
		bills = append(bills, result.Items...)
		if !result.HasMore {
			return bills, nil
		}
	}
	logger.LegacyPrintf(
		a6LogComponent,
		"bills_page_limit_reached: token_name=%s model_name=%s pages=%d fetched=%d",
		query.TokenName, query.ModelName, maxPages, len(bills),
	)
	return bills, ErrReconciliationA6PageLimitReached.WithMetadata(map[string]string{
		"max_pages": strconv.Itoa(maxPages),
	})
}

// ==================== 规范化 ====================

// NormalizeBill 把一条原始 A6 日志记录规范化成 ReconciliationA6Bill。
//
// raw 为 json.Decoder(UseNumber) 解出的对象（手工重放场景也可以传普通 map，
// 数值为 float64 同样能解析）；quotaPerUnit 为换算用的计费单位，通常直接传
// QuotaPerUnit(ctx) 的返回值，<=0 时按 1 处理并把这个 1 记进 QuotaPerUnit 字段，
// 便于发现误用。
//
// 返回 ok=false 表示这条记录不可用：缺少 request_id、时间无法解析，或 quota 缺失/非法/
// 为负——这些记录会被跳过，而不是以 0 成本混进账本。其它字段（含 other）缺失一律
// 降级为零值/nil，绝不因此丢记录。
func (c *A6Client) NormalizeBill(raw map[string]any, quotaPerUnit decimal.Decimal) (ReconciliationA6Bill, bool) {
	bill, skipReason := c.normalizeBill(raw, quotaPerUnit)
	return bill, skipReason == ""
}

// normalizeBill 是 NormalizeBill 的内部实现，额外返回跳过原因（为空表示成功）。
//
// 跳过原因不在这里打日志：页级路径会聚合成一条日志，避免坏页刷屏。
func (c *A6Client) normalizeBill(raw map[string]any, quotaPerUnit decimal.Decimal) (ReconciliationA6Bill, string) {
	if len(raw) == 0 {
		return ReconciliationA6Bill{}, "empty_record"
	}

	requestIDRaw, ok := a6Lookup(raw, "request_id", "requestId", "upstream_request_id")
	if !ok {
		return ReconciliationA6Bill{}, "missing_request_id"
	}
	requestID, _ := a6ScalarString(requestIDRaw)
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return ReconciliationA6Bill{}, "missing_request_id"
	}

	occurredAt, ok := a6OccurredAt(raw)
	if !ok {
		return ReconciliationA6Bill{}, "invalid_created_at"
	}

	quotaRaw, ok := a6Lookup(raw, "quota", "use_quota")
	if !ok {
		return ReconciliationA6Bill{}, "missing_quota"
	}
	quota, ok := a6Decimal(quotaRaw)
	if !ok || quota.IsNegative() {
		return ReconciliationA6Bill{}, "invalid_quota"
	}

	if !quotaPerUnit.IsPositive() {
		quotaPerUnit = decimal.NewFromInt(1)
	}

	other := a6DecodeLooseObject(mustA6Lookup(raw, "other"))
	cacheRead, cacheCreation, cacheTotal := a6ResolveCacheTokens(raw, other)

	return ReconciliationA6Bill{
		RequestID:           requestID,
		OccurredAt:          occurredAt,
		Model:               a6StringField(raw, "model_name", "model", "upstream_model"),
		TokenName:           a6StringField(raw, "token_name", "tokenName"),
		InputTokens:         a6TokenCount(raw, "prompt_tokens", "input_tokens", "promptTokens"),
		OutputTokens:        a6TokenCount(raw, "completion_tokens", "output_tokens", "completionTokens"),
		CacheReadTokens:     cacheRead,
		CacheCreationTokens: cacheCreation,
		CacheTokensTotal:    cacheTotal,
		Quota:               quota,
		QuotaPerUnit:        quotaPerUnit,
		CostUSD:             quota.Div(quotaPerUnit),
		Other:               other,
		Raw:                 raw,
	}, ""
}

// ==================== 内部实现 ====================

// a6ValidateQuery 校验时间窗口；倒置或缺失的窗口不值得发一次注定无用的请求。
func a6ValidateQuery(query A6BillQuery) error {
	if query.StartTime.IsZero() || query.EndTime.IsZero() || query.EndTime.Before(query.StartTime) {
		return ErrReconciliationA6InvalidWindow
	}
	return nil
}

// a6NormalizePageSize 收敛每页条数：<=0 用缺省值，超上限按上限截断。
func a6NormalizePageSize(size int) int {
	if size <= 0 {
		return a6DefaultPageSize
	}
	if size > a6MaxPageSize {
		return a6MaxPageSize
	}
	return size
}

// a6NormalizeMaxPages 收敛翻页上限：<=0 用缺省值，超硬顶按硬顶截断。
func a6NormalizeMaxPages(pages int) int {
	if pages <= 0 {
		return a6DefaultMaxPages
	}
	if pages > a6MaxMaxPages {
		return a6MaxMaxPages
	}
	return pages
}

// a6BillQueryParams 组装 /api/log/self 的查询串。
//
// 时间参数是 Unix 秒；窗口终点由调用方给定（需要吸收时钟偏差时由调用方自己加 60 秒），
// 客户端不做任何时间放大。
func a6BillQueryParams(query A6BillQuery, page, pageSize int) url.Values {
	params := url.Values{
		"p":               {strconv.Itoa(page)},
		"page_size":       {strconv.Itoa(pageSize)},
		"type":            {a6LogTypeConsumption},
		"start_timestamp": {strconv.FormatInt(query.StartTime.Unix(), 10)},
		"end_timestamp":   {strconv.FormatInt(query.EndTime.Unix(), 10)},
	}
	// 空串不下发：部分实现会把 token_name= 当成「精确匹配空令牌名」而返回空集。
	if token := strings.TrimSpace(query.TokenName); token != "" {
		params.Set("token_name", token)
	}
	if model := strings.TrimSpace(query.ModelName); model != "" {
		params.Set("model_name", model)
	}
	return params
}

// a6HasMorePage 判断后面还有没有页。
//
// 空页直接判定取完；有 total 时按 total 判断，缺失时按「本页是否取满」保守推断
// （取满再多翻一页，多出来那次会拿到空页并终止）。
func a6HasMorePage(total, page, pageSize, itemCount int) bool {
	if itemCount == 0 || pageSize <= 0 {
		return false
	}
	if total >= 0 {
		return page*pageSize < total
	}
	return itemCount >= pageSize
}

// getJSONWithRetry 带重试地取一个 JSON 对象：最多 a6MaxAttempts 次，退避 0.5s / 1.0s。
//
// ctx 取消/超时立即中止并把 ctx 的错误交给调用方，不做无意义的重试。
// cfg 由调用方以快照传入：整轮重试都用同一份凭据，中途被面板换掉也不影响本次请求。
// 日志只打路径，不打查询串、不打响应体，也绝不打令牌与用户标识。
func (c *A6Client) getJSONWithRetry(ctx context.Context, cfg ReconciliationA6Config, path string, params url.Values) (map[string]any, error) {
	var lastErr error
	for attempt := 0; attempt < a6MaxAttempts; attempt++ {
		if attempt > 0 {
			if err := a6SleepWithContext(ctx, a6RetryBackoffs[attempt-1]); err != nil {
				return nil, err
			}
		}
		envelope, err := c.getJSON(ctx, cfg, path, params)
		if err == nil {
			return envelope, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		lastErr = err
		if attempt < a6MaxAttempts-1 {
			logger.LegacyPrintf(a6LogComponent, "request_retry: path=%s attempt=%d err=%v", path, attempt+1, err)
		}
	}
	logger.LegacyPrintf(a6LogComponent, "request_failed: path=%s attempts=%d err=%v", path, a6MaxAttempts, lastErr)
	return nil, lastErr
}

// getJSON 发一次 GET 并把响应解成 JSON 对象。
func (c *A6Client) getJSON(ctx context.Context, cfg ReconciliationA6Config, path string, params url.Values) (map[string]any, error) {
	requestURL := cfg.BaseURL + path
	if len(params) > 0 {
		requestURL += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, ErrReconciliationA6RequestFailed.WithCause(err)
	}
	// 三个必带头：Bearer 令牌、用户标识、禁缓存。
	req.Header.Set("Authorization", "Bearer "+cfg.AccessToken)
	req.Header.Set("New-API-User", cfg.UserID)
	req.Header.Set("Cache-Control", "no-store")
	req.Header.Set("User-Agent", a6UserAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, a6ClassifyTransportError(err)
	}
	defer func() {
		// 提前返回时再读掉一点，方便复用连接；读不完的部分由 Close 丢弃。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, a6BodyDrainBytes))
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// 认证类失败单独成型：上层要能给出「凭据无效」而不是「网络不通」。
		return nil, ErrReconciliationA6AuthFailed.WithMetadata(map[string]string{
			"upstream_status": strconv.Itoa(resp.StatusCode),
		})
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, ErrReconciliationA6RequestFailed.WithMetadata(map[string]string{
			"upstream_status": strconv.Itoa(resp.StatusCode),
		})
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, a6MaxResponseBytes+1))
	if err != nil {
		return nil, ErrReconciliationA6RequestFailed.WithCause(err)
	}
	if len(body) > a6MaxResponseBytes {
		return nil, ErrReconciliationA6ResponseTooLarge.WithMetadata(map[string]string{
			"limit_bytes": strconv.Itoa(a6MaxResponseBytes),
		})
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	// UseNumber：大整数与高精度小数保持字面量，不在 float64 上丢精度。
	decoder.UseNumber()
	var envelope map[string]any
	if err := decoder.Decode(&envelope); err != nil {
		return nil, ErrReconciliationA6InvalidResponse.WithCause(err)
	}
	if envelope == nil {
		return nil, ErrReconciliationA6InvalidResponse.WithCause(errors.New("A6 returned an empty JSON body"))
	}
	return envelope, nil
}

// a6ClassifyTransportError 把传输层错误分成「超时」与「其它失败」。
func a6ClassifyTransportError(err error) error {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ErrReconciliationA6Timeout.WithCause(err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrReconciliationA6Timeout.WithCause(err)
	}
	return ErrReconciliationA6RequestFailed.WithCause(err)
}

// a6CheckSuccess 检查响应信封的 success 字段（可能是 bool、数字或字符串）。
//
// 字段缺失不视为失败：有的实现只给 message + data。
func a6CheckSuccess(envelope map[string]any) error {
	raw, ok := a6Lookup(envelope, "success")
	if !ok {
		return nil
	}
	success, recognized := a6Bool(raw)
	if !recognized || success {
		return nil
	}
	message := a6StringField(envelope, "message", "msg", "error")
	return ErrReconciliationA6Rejected.WithCause(
		fmt.Errorf("upstream message: %s", a6Truncate(message, a6UpstreamMessageLimit)),
	)
}

// a6BillItems 是账单记录与分页信息的中间结果。
type a6BillItems struct {
	items    []map[string]any
	total    int
	page     int
	pageSize int
}

// a6ExtractBillItems 从响应信封里取出账单记录与分页信息。
//
// 容错点：data 可能是数组，也可能是带 items 的对象（字段名还可能是 logs / records / list）；
// total / page / page_size 可能是数字也可能是字符串。data 存在但没有记录数组时
// 视为「本页为空」而不是报错——空窗口是正常情况，报错会让整轮采集失败。
func a6ExtractBillItems(envelope map[string]any) (a6BillItems, error) {
	result := a6BillItems{total: a6TotalUnknown}
	data, ok := a6Lookup(envelope, "data")
	if !ok {
		return result, ErrReconciliationA6InvalidResponse.WithCause(
			errors.New("A6 log response has no data"),
		)
	}
	if list, ok := a6SliceValue(data); ok {
		result.items = a6RecordList(list)
		return result, nil
	}
	object, ok := a6MapValue(data)
	if !ok {
		return result, ErrReconciliationA6InvalidResponse.WithCause(
			errors.New("A6 log response data is neither an array nor an object"),
		)
	}
	if rawItems, ok := a6Lookup(object, "items", "logs", "records", "list"); ok {
		list, ok := a6SliceValue(rawItems)
		if !ok {
			return result, ErrReconciliationA6InvalidResponse.WithCause(
				errors.New("A6 log response items is not an array"),
			)
		}
		result.items = a6RecordList(list)
	}
	if raw, ok := a6Lookup(object, "total", "count"); ok {
		if value, ok := a6Int64(raw); ok && value >= 0 {
			result.total = a6ClampInt(value)
		}
	}
	if raw, ok := a6Lookup(object, "page", "p"); ok {
		if value, ok := a6Int64(raw); ok && value > 0 {
			result.page = a6ClampInt(value)
		}
	}
	if raw, ok := a6Lookup(object, "page_size", "pageSize", "size"); ok {
		if value, ok := a6Int64(raw); ok && value > 0 {
			result.pageSize = a6ClampInt(value)
		}
	}
	return result, nil
}

// a6RecordList 把数组元素收敛成对象列表，非对象元素直接跳过。
func a6RecordList(list []any) []map[string]any {
	records := make([]map[string]any, 0, len(list))
	for _, element := range list {
		if record, ok := a6MapValue(element); ok {
			records = append(records, record)
		}
	}
	return records
}

// a6ResolveCacheTokens 解析缓存 token，返回（缓存读、缓存写、上游口径合计）。
//
// 查找顺序：顶层分列字段 → other 里的分列字段 → 顶层合并字段 → other.cache_tokens。
// 最后那一步是为真实上游准备的：A6 常见形态是缓存只给一个合并值 other.cache_tokens，
// 旧实现就是拿它当缓存合计参与匹配的。
func a6ResolveCacheTokens(record, other map[string]any) (int, int, int) {
	cacheRead := a6TokenCount(record, "cache_read_tokens", "cache_read_input_tokens", "cacheReadTokens")
	cacheCreation := a6TokenCount(record, "cache_creation_tokens", "cache_creation_input_tokens", "cache_write_tokens", "cacheCreationTokens")
	if cacheRead <= 0 && cacheCreation <= 0 && other != nil {
		cacheRead = a6TokenCount(other, "cache_read_tokens", "cache_read_input_tokens")
		cacheCreation = a6TokenCount(other, "cache_creation_tokens", "cache_creation_input_tokens", "cache_write_tokens")
	}
	if total := cacheRead + cacheCreation; total > 0 {
		return cacheRead, cacheCreation, total
	}
	total := a6TokenCount(record, "cache_tokens", "cache_tokens_total")
	if total <= 0 && other != nil {
		total = a6TokenCount(other, "cache_tokens", "cache_tokens_total")
	}
	return cacheRead, cacheCreation, total
}

// a6TokenCount 读取 token 数：缺失、类型不符或负数一律按 0 处理。
func a6TokenCount(source map[string]any, names ...string) int {
	raw, ok := a6Lookup(source, names...)
	if !ok {
		return 0
	}
	value, ok := a6Int64(raw)
	if !ok {
		return 0
	}
	return a6ClampInt(value)
}

// a6StringField 读取字符串字段，按名称列表依次尝试。
func a6StringField(source map[string]any, names ...string) string {
	raw, ok := a6Lookup(source, names...)
	if !ok {
		return ""
	}
	value, _ := a6ScalarString(raw)
	return strings.TrimSpace(value)
}

// a6OccurredAt 解析 created_at：兼容 Unix 秒、Unix 毫秒、数字字符串与常见时间字符串。
//
// 无法解析或落在合理区间之外时返回 ok=false——宁可跳过一条时间不明的账单，
// 也不要让它以公元 1 年之类的时间进库污染窗口查询。
func a6OccurredAt(record map[string]any) (time.Time, bool) {
	raw, ok := a6Lookup(record, "created_at", "createdAt", "timestamp", "time")
	if !ok {
		return time.Time{}, false
	}
	// 字符串形态：先按数字（Unix 秒/毫秒）试，再按常见时间格式试。
	if text, ok := raw.(string); ok {
		trimmed := strings.TrimSpace(text)
		if trimmed == "" {
			return time.Time{}, false
		}
		if seconds, ok := a6Int64(trimmed); ok {
			return a6TimeFromUnix(seconds)
		}
		for _, layout := range a6TimeLayouts {
			parsed, err := time.Parse(layout, trimmed)
			if err != nil {
				continue
			}
			if !a6PlausibleUnix(parsed.Unix()) {
				return time.Time{}, false
			}
			return parsed.UTC(), true
		}
		return time.Time{}, false
	}
	// 其余数值形态（json.Number / float64 / int / decimal）统一走 a6Int64：
	// 小数秒直接截断，匹配窗口是分钟级，亚秒精度没有意义。
	if seconds, ok := a6Int64(raw); ok {
		return a6TimeFromUnix(seconds)
	}
	return time.Time{}, false
}

// a6TimeFromUnix 把 Unix 时间戳转成 UTC 时间，兼容上游误传毫秒。
func a6TimeFromUnix(value int64) (time.Time, bool) {
	if value > a6MillisThreshold {
		value /= 1000
	}
	if !a6PlausibleUnix(value) {
		return time.Time{}, false
	}
	return time.Unix(value, 0).UTC(), true
}

// a6PlausibleUnix 判断时间戳是否落在 [2000-01-01, 2100-01-01) 的合理区间。
func a6PlausibleUnix(value int64) bool {
	return value >= a6MinPlausibleUnix && value < a6MaxPlausibleUnix
}

// a6FloatToInt64 把浮点数收敛成 int64；NaN / Inf / 溢出返回 ok=false。
//
// 上界用 >= float64(math.MaxInt64)：2^63 在 float64 里是精确值且已超出 int64 范围，
// 写成 > 会把它放过去，转换结果就变成实现相关值。
func a6FloatToInt64(value float64) (int64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value >= float64(math.MaxInt64) || value < float64(math.MinInt64) {
		return 0, false
	}
	return int64(value), true
}

// a6ClampInt 把上游的 64 位计数收敛到 int 范围，负数归零，避免平台差异下静默溢出。
func a6ClampInt(value int64) int {
	if value <= 0 {
		return 0
	}
	if value > math.MaxInt {
		return math.MaxInt
	}
	return int(value)
}

// a6DecodeLooseObject 把对象 / 被双重编码的 JSON 字符串 / 字节串解成 map。
//
// 最多解开 a6MaxJSONUnwrapLayers 层：真实上游既可能给 JSON 对象，也可能给一个
// 「内容是 JSON 对象」的字符串。任何形态解析失败都返回 nil，绝不因此中断整条记录。
func a6DecodeLooseObject(raw any) map[string]any {
	for layer := 0; layer < a6MaxJSONUnwrapLayers; layer++ {
		switch value := raw.(type) {
		case map[string]any:
			return value
		case []byte:
			raw = string(value)
		case json.RawMessage:
			raw = string(value)
		case string:
			text := strings.TrimSpace(value)
			if text == "" || text == "null" {
				return nil
			}
			decoder := json.NewDecoder(strings.NewReader(text))
			decoder.UseNumber()
			var decoded any
			if err := decoder.Decode(&decoded); err != nil {
				return nil
			}
			raw = decoded
		default:
			return nil
		}
	}
	return nil
}

// a6Lookup 按名称列表依次查找字段，跳过缺失与显式 null 的值。
//
// 列表内同时容纳下划线与驼峰写法，用于容忍上游改名。
func a6Lookup(source map[string]any, names ...string) (any, bool) {
	for _, name := range names {
		value, ok := source[name]
		if !ok || value == nil {
			continue
		}
		return value, true
	}
	return nil, false
}

// mustA6Lookup 只取单个字段名，缺失或 null 返回 nil。
func mustA6Lookup(source map[string]any, name string) any {
	value, _ := a6Lookup(source, name)
	return value
}

// a6MapValue 取值并断言成对象。
func a6MapValue(raw any) (map[string]any, bool) {
	value, ok := raw.(map[string]any)
	return value, ok
}

// a6SliceValue 取值并断言成数组。
func a6SliceValue(raw any) ([]any, bool) {
	value, ok := raw.([]any)
	return value, ok
}

// a6ScalarString 把标量转成字符串；对象/数组/布尔返回 ok=false（不可能是请求标识）。
func a6ScalarString(raw any) (string, bool) {
	switch value := raw.(type) {
	case string:
		return value, true
	case json.Number:
		return value.String(), true
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64), true
	case float32:
		return strconv.FormatFloat(float64(value), 'f', -1, 32), true
	case int64:
		return strconv.FormatInt(value, 10), true
	case int:
		return strconv.Itoa(value), true
	case decimal.Decimal:
		return value.String(), true
	default:
		return "", false
	}
}

// a6Bool 解析布尔字面量：兼容 bool、数字与字符串（"true" / "1" / "yes" / "on"）。
//
// 返回 recognized=false 表示这个值无法当成布尔解释，调用方应忽略而不是当成 false。
func a6Bool(raw any) (bool, bool) {
	switch value := raw.(type) {
	case bool:
		return value, true
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true", "1", "yes", "on":
			return true, true
		case "false", "0", "no", "off":
			return false, true
		default:
			return false, false
		}
	default:
		number, ok := a6Int64(raw)
		if !ok {
			return false, false
		}
		return number != 0, true
	}
}

// a6Int64 把上游数值解析成 int64：json.Number/字符串走精确解析，浮点截断取整。
func a6Int64(raw any) (int64, bool) {
	switch value := raw.(type) {
	case json.Number:
		if parsed, err := value.Int64(); err == nil {
			return parsed, true
		}
		floatValue, err := value.Float64()
		if err != nil {
			return 0, false
		}
		return a6FloatToInt64(floatValue)
	case string:
		text := strings.TrimSpace(value)
		if text == "" {
			return 0, false
		}
		if parsed, err := strconv.ParseInt(text, 10, 64); err == nil {
			return parsed, true
		}
		floatValue, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return 0, false
		}
		return a6FloatToInt64(floatValue)
	case float64:
		return a6FloatToInt64(value)
	case float32:
		return a6FloatToInt64(float64(value))
	case int64:
		return value, true
	case int:
		return int64(value), true
	case decimal.Decimal:
		return value.IntPart(), true
	default:
		return 0, false
	}
}

// a6Decimal 把上游数值解析成 decimal：json.Number/字符串走精确解析（避免浮点误差），
// 浮点走 decimal.NewFromFloat；NaN / Inf / 非法字面量返回 ok=false。
func a6Decimal(raw any) (decimal.Decimal, bool) {
	switch value := raw.(type) {
	case json.Number:
		parsed, err := decimal.NewFromString(value.String())
		return parsed, err == nil
	case string:
		parsed, err := decimal.NewFromString(strings.TrimSpace(value))
		return parsed, err == nil
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return decimal.Zero, false
		}
		return decimal.NewFromFloat(value), true
	case float32:
		converted := float64(value)
		if math.IsNaN(converted) || math.IsInf(converted, 0) {
			return decimal.Zero, false
		}
		return decimal.NewFromFloat(converted), true
	case int64:
		return decimal.NewFromInt(value), true
	case int:
		return decimal.NewFromInt(int64(value)), true
	case decimal.Decimal:
		return value, true
	default:
		return decimal.Zero, false
	}
}

// a6Truncate 按字符（而不是字节）截断，避免把多字节字符切碎。
func a6Truncate(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "..."
}

// a6SleepWithContext 可被 ctx 打断的等待；ctx 取消时返回其错误而不是继续重试。
func a6SleepWithContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
