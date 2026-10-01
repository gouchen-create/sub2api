//go:build unit

package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本文件是经营对账管理端接口的**响应契约测试**。
//
// 背景：这些接口原先是反向代理到独立的 companion 旁路服务，现在改成直接调用进程内
// 服务，而管理后台前端本次零改动。前端逐字段依赖 /api/v1/admin/companion/* 的 JSON
// 形状（见 frontend/src/api/admin/companion.ts），因此这里把字段名、字段类型、空值与
// 分页语义全部锁死，作为防止后人改坏契约的自动化守卫。
//
// 契约正本：frontend/src/api/admin/companion.ts。断言里出现的字段名必须与该文件的
// TypeScript 类型逐字一致；任何一侧要改，都必须同时改本文件。
//
// 构造方式：三个服务都是具体结构体，但它们依赖的仓库都是 service 包里的**接口**，
// 因此这里手写桩实现（用嵌入接口的方式只覆盖测试用到的方法），再用真实构造函数拼装，
// 全程不连数据库。路由用 gin.New() 注册真实路径（含 :account_id 路径参数），
// 不直接调用 handler 方法，保证连路径匹配一起验证。

// companionBasePath 与 internal/server/routes/admin.go 的 registerCompanionRoutes 一致：
// /api/v1/admin/companion/*。
const companionBasePath = "/api/v1/admin/companion"

// ==================== 契约正本（与 companion.ts 对齐的字段清单） ====================

// companionSummaryContractKeys 是 CompanionSummary 声明的全部字段（25 个）。
var companionSummaryContractKeys = []string{
	"from", "to",
	"revenue", "matched_revenue", "upstream_cost", "billed_upstream_cost",
	"gross_profit", "margin_percent",
	"matched", "unmatched", "downstream_matched", "downstream_unmatched",
	"upstream_unmatched", "record_total", "billed_count", "cost_policy",
	"calculated_count", "subarx_unallocated_cost", "subarx_unallocated_count",
	"profit_scope", "currency", "fx_usd_cny", "fx_source", "fx_effective_at", "fx_stale",
}

// companionSummaryCountFields 是必须序列化成 JSON 数字的计数字段。
// 前端直接把它们当 number 参与运算与格式化，一旦变成字符串就会渲染成 "NaN"。
var companionSummaryCountFields = []string{
	"matched", "unmatched", "downstream_matched", "downstream_unmatched",
	"upstream_unmatched", "record_total", "billed_count", "calculated_count",
	"subarx_unallocated_count",
}

// companionSummaryAmountFields 是必须保留 8 位小数的金额字段。
var companionSummaryAmountFields = []string{
	"revenue", "matched_revenue", "upstream_cost", "billed_upstream_cost",
	"gross_profit", "subarx_unallocated_cost",
}

// companionRequestRowContractKeys 是 CompanionRequestRow 声明的全部字段（25 个）。
var companionRequestRowContractKeys = []string{
	"record_type", "source_id", "created_at", "request_id", "upstream_request_id",
	"user_id", "user_email", "api_key_id", "account_id", "group_id", "group_name",
	"model", "input_tokens", "output_tokens", "cache_tokens",
	"revenue", "upstream_cost", "billed_upstream_cost", "upstream_cost_original",
	"upstream_currency", "gross_profit", "cost_source", "fx_rate_to_cny",
	"cost_source_label", "matched",
}

// companionRequestRowCountFields 是明细行里的数字字段。
var companionRequestRowCountFields = []string{
	"source_id", "user_id", "api_key_id", "account_id", "group_id",
	"input_tokens", "output_tokens", "cache_tokens",
}

// companionTimeseriesPointContractKeys 是 CompanionTimeSeriesPoint 声明的全部字段（8 个）。
var companionTimeseriesPointContractKeys = []string{
	"start", "revenue", "upstream_cost", "gross_profit",
	"matched", "unmatched", "upstream_unmatched", "record_total",
}

// companionAccountRuleContractKeys 是 CompanionAccountRule 声明的全部字段（24 个）。
//
// models 已拆成 recent_model / recent_group_id / recent_group_name：
// 契约只声明一个「最近」概念，由全历史最后一次调用决定（文档 19），
// 而不是窗口内模型名的并集——后者既不是「最近」，也会随筛选条件漂移。
var companionAccountRuleContractKeys = []string{
	"account_id", "provider", "token_name", "multiplier", "version", "enabled",
	"created_at", "updated_at", "configured", "current",
	"group_id", "group_name", "group_priority", "group_channel_count",
	"account_name", "account_platform", "account_status", "account_schedulable",
	"usage_count", "first_seen", "last_seen",
	"recent_model", "recent_group_id", "recent_group_name",
}

// companionAccountRuleListContractKeys 是 CompanionAccountRuleList 声明的全部字段（6 个）。
//
// groups / unconfigured_groups 与 items / unconfigured_accounts 并存：
// 前端新版本按分组渲染（groups），旧版本只认 items。缺任何一侧都不会报错，
// 只会静默渲染成空状态，因此这里逐字锁死。
var companionAccountRuleListContractKeys = []string{
	"items", "unconfigured_accounts", "unconfigured_groups", "groups", "from", "to",
}

// companionAccountRuleGroupContractKeys 是 CompanionAccountRuleGroup 声明的全部字段（8 个）。
//
// 字段名与 CompanionView.vue 里 groups[].xxx 的读取逐字对应：
// 前端用 .length 与 ?? [] 混用，token_keys / accounts 必须是数组而不是 null。
var companionAccountRuleGroupContractKeys = []string{
	"group_id", "group_name", "group_priority", "group_channel_count",
	"configured", "usage_count", "token_keys", "accounts",
}

// companionBackfillStatusContractKeys 是 CompanionA6BackfillStatus 的字段（7 个）。
var companionBackfillStatusContractKeys = []string{
	"status", "running", "from", "to", "cursor", "processed", "error",
}

// companionTimeseriesBucketLabels 是 CompanionTimeSeries.bucket 的全部合法取值。
var companionTimeseriesBucketLabels = []string{"1小时", "6小时", "1天", "1周"}

// companionCostSourceUnion 是 CompanionCostSource 联合类型的全部取值。
//
// 前端对命中映射表的取值走 i18n，对未知取值回落到响应里的 cost_source_label，
// 所以后端既不能产出集合外的取值，也必须保证 label 永远非空。
var companionCostSourceUnion = []string{
	"pending", "billed", "subarx_billed_allocation", "subarx_pending", "subarx_waiting",
	"subarx_rule", "rule_unconfigured", "a6_waiting", "a6_pending",
	"upstream_unmatched", "subarx_unallocated",
}

// companionAmountPattern 匹配 8 位小数字符串金额。
var companionAmountPattern = regexp.MustCompile(`^-?\d+\.\d{8}$`)

// companionMarginPattern 匹配恰好 2 位小数的百分数字符串。
var companionMarginPattern = regexp.MustCompile(`^-?\d+\.\d{2}$`)

// ==================== 响应信封与断言小工具 ====================

// companionContractEnvelope 是本项目统一响应信封，data 保持原始 JSON 以便逐字段核对形状。
type companionContractEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func companionDecodeObject(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	require.NotEmpty(t, raw, "响应缺少 data 字段")
	var out map[string]any
	require.NoErrorf(t, json.Unmarshal(raw, &out), "data 不是 JSON 对象: %s", string(raw))
	return out
}

func companionSortedKeys(data map[string]any) []string {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// companionRequireExactKeys 断言对象键集**恰好**等于契约字段集：多一个、少一个都算失败。
func companionRequireExactKeys(t *testing.T, data map[string]any, expected []string, label string) {
	t.Helper()
	want := append([]string(nil), expected...)
	sort.Strings(want)
	got := companionSortedKeys(data)
	assert.Equalf(t, want, got, "%s 的字段集与前端契约不一致（多字段或少字段）", label)
	assert.Lenf(t, data, len(expected), "%s 字段数量应为 %d", label, len(expected))
}

// companionJSONString 取字符串字段，并显式排除「数字被序列化成字符串」与 null。
func companionJSONString(t *testing.T, data map[string]any, field string) string {
	t.Helper()
	value, ok := data[field]
	require.Truef(t, ok, "字段 %s 缺失", field)
	require.NotNilf(t, value, "字段 %s 是 null，契约要求字符串", field)
	text, isString := value.(string)
	require.Truef(t, isString, "字段 %s 必须是 JSON 字符串，实际 %T = %v", field, value, value)
	return text
}

// companionJSONNumber 取数字字段，并显式排除字符串形态（前端会把它当 number 用）。
func companionJSONNumber(t *testing.T, data map[string]any, field string) float64 {
	t.Helper()
	value, ok := data[field]
	require.Truef(t, ok, "字段 %s 缺失", field)
	if text, isString := value.(string); isString {
		require.Failf(t, "数字字段被序列化成了字符串",
			"字段 %s 必须是 JSON 数字，实际是字符串 %q", field, text)
	}
	number, isNumber := value.(float64)
	require.Truef(t, isNumber, "字段 %s 必须是 JSON 数字，实际 %T = %v", field, value, value)
	return number
}

func companionJSONBool(t *testing.T, data map[string]any, field string) bool {
	t.Helper()
	value, ok := data[field]
	require.Truef(t, ok, "字段 %s 缺失", field)
	boolean, isBool := value.(bool)
	require.Truef(t, isBool, "字段 %s 必须是 JSON 布尔，实际 %T = %v", field, value, value)
	return boolean
}

func companionJSONArray(t *testing.T, data map[string]any, field string) []any {
	t.Helper()
	value, ok := data[field]
	require.Truef(t, ok, "字段 %s 缺失", field)
	require.NotNilf(t, value, "字段 %s 是 null，契约要求数组", field)
	items, isArray := value.([]any)
	require.Truef(t, isArray, "字段 %s 必须是 JSON 数组，实际 %T = %v", field, value, value)
	return items
}

// companionRequireRFC3339 断言时间字段能被前端 new Date() 解析。
func companionRequireRFC3339(t *testing.T, raw string, label string) time.Time {
	t.Helper()
	require.NotEmptyf(t, raw, "%s 不能为空", label)
	parsed, err := time.Parse(time.RFC3339, raw)
	require.NoErrorf(t, err, "%s 必须是 RFC3339 时间，实际 %q", label, raw)
	return parsed
}

// ==================== 桩实现（全部基于 service 包里的端口接口） ====================

// companionLedgerStub 桩掉看板读模型。
type companionLedgerStub struct {
	service.ReconciliationLedgerRepository

	summary *service.ReconciliationSummary
	points  []service.ReconciliationBucketPoint
	rows    []service.ReconciliationLedgerRow
	total   int64
	usage   map[int64]service.ReconciliationAccountUsage
	// recent 是全历史最后一次调用的模型与分组，与窗口无关。
	recent map[int64]service.ReconciliationRecentUsage

	lastStatus   string
	lastPage     int
	lastPageSize int
}

func (s *companionLedgerStub) Summary(_ context.Context, from, to time.Time) (*service.ReconciliationSummary, error) {
	copied := *s.summary
	copied.From, copied.To = from, to
	return &copied, nil
}

func (s *companionLedgerStub) Points(_ context.Context, _, _ time.Time, _ time.Duration) ([]service.ReconciliationBucketPoint, error) {
	return s.points, nil
}

func (s *companionLedgerStub) Rows(_ context.Context, _, _ time.Time, status string, page, pageSize int) ([]service.ReconciliationLedgerRow, int64, error) {
	s.lastStatus, s.lastPage, s.lastPageSize = status, page, pageSize
	return s.rows, s.total, nil
}

func (s *companionLedgerStub) UsageCountsByAccount(_ context.Context, _, _ time.Time) (map[int64]service.ReconciliationAccountUsage, error) {
	return s.usage, nil
}

func (s *companionLedgerStub) RecentUsageByAccount(_ context.Context) (map[int64]service.ReconciliationRecentUsage, error) {
	return s.recent, nil
}

// companionRuleStub 桩掉账号规则仓库。
type companionRuleStub struct {
	service.ReconciliationAccountRuleRepository

	rules   []service.ReconciliationAccountRule
	saved   *service.ReconciliationAccountRule
	deleted int64
	// groupChannelCounts 每个分组的真实渠道数（与行数无关）。
	groupChannelCounts map[int64]int64
}

func (s *companionRuleStub) CountAccountsByGroup(_ context.Context) (map[int64]int64, error) {
	if s.groupChannelCounts == nil {
		return map[int64]int64{}, nil
	}
	return s.groupChannelCounts, nil
}

func (s *companionRuleStub) List(_ context.Context) ([]service.ReconciliationAccountRule, error) {
	return s.rules, nil
}

func (s *companionRuleStub) GetByAccountID(_ context.Context, accountID int64) (*service.ReconciliationAccountRule, error) {
	for i := range s.rules {
		if s.rules[i].AccountID == accountID {
			copied := s.rules[i]
			return &copied, nil
		}
	}
	return nil, nil
}

func (s *companionRuleStub) Upsert(_ context.Context, accountID int64, provider, externalKey string, multiplier *float64, enabled bool) (*service.ReconciliationAccountRule, error) {
	if s.saved != nil {
		copied := *s.saved
		return &copied, nil
	}
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	return &service.ReconciliationAccountRule{
		ID:          1,
		AccountID:   accountID,
		Provider:    provider,
		ExternalKey: externalKey,
		Multiplier:  multiplier,
		Version:     1,
		Enabled:     enabled,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

func (s *companionRuleStub) Delete(_ context.Context, _ int64) (int64, error) {
	return s.deleted, nil
}

// companionStateStub 桩掉同步状态表。
//
// 必须带锁：回填与采集在后台 goroutine 里写状态，而测试同时会打接口读状态，
// 无锁的 map 会触发 Go 运行时的并发读写致命错误。
type companionStateStub struct {
	service.ReconciliationSyncStateRepository

	mu             sync.Mutex
	values         map[string]string
	getErr         error
	getMultipleErr error
}

// raw 读回某个键当前落库的原始字符串。
//
// 断言「令牌不是明文落库」时必须看这一份原始值，而不是看接口返回值。
func (s *companionStateStub) raw(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[key]
}

// companionEncryptorStub 是可逆的假加密器，只用于验证调用契约。
//
// 真实 AES-256-GCM 的落库形态由 internal/repository 的用例覆盖：
// depguard 禁止 handler / service 包 import repository。
type companionEncryptorStub struct{}

const companionCipherPrefix = "test-enc:"

func (companionEncryptorStub) Encrypt(plaintext string) (string, error) {
	return companionCipherPrefix + base64.StdEncoding.EncodeToString([]byte(plaintext)), nil
}

func (companionEncryptorStub) Decrypt(ciphertext string) (string, error) {
	if !strings.HasPrefix(ciphertext, companionCipherPrefix) {
		return "", errors.New("ciphertext has no encryption prefix")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(ciphertext, companionCipherPrefix))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (s *companionStateStub) Get(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return "", s.getErr
	}
	return s.values[key], nil
}

func (s *companionStateStub) Set(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = map[string]string{}
	}
	s.values[key] = value
	return nil
}

func (s *companionStateStub) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getMultipleErr != nil {
		return nil, s.getMultipleErr
	}
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := s.values[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

func (s *companionStateStub) set(key, value string) {
	_ = s.Set(context.Background(), key, value)
}

// companionExtrasStub 桩掉调用侧快照仓库：本测试不触发真实采集路径。
type companionExtrasStub struct {
	service.ReconciliationUsageExtraRepository
}

// companionBillStub 桩掉上游账单仓库。
type companionBillStub struct {
	service.ReconciliationUpstreamBillRepository

	mu       sync.Mutex
	imported []service.ReconciliationUpstreamBillPayload
	// unmatched 是窗口内的孤儿账单（重试接口的输入）。
	unmatched []service.ReconciliationUpstreamBill
	// requeued 记录被要求退回 staging 的账单 ID。
	requeued []int64
}

func (s *companionBillStub) UpsertBatch(_ context.Context, bills []service.ReconciliationUpstreamBillPayload) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.imported = append(s.imported, bills...)
	return int64(len(bills)), nil
}

func (s *companionBillStub) ListStaging(_ context.Context, _, _ time.Time, _ int) ([]service.ReconciliationUpstreamBill, error) {
	return nil, nil
}

func (s *companionBillStub) ListUnmatched(_ context.Context, _, _ time.Time, _ int) ([]service.ReconciliationUpstreamBill, error) {
	return s.unmatched, nil
}

func (s *companionBillStub) RequeueUnmatched(_ context.Context, billIDs []int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requeued = append(s.requeued, billIDs...)
	return int64(len(billIDs)), nil
}

func (s *companionBillStub) requeuedIDs() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.requeued...)
}

func (s *companionBillStub) importedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.imported)
}

// companionUsageSourceStub 桩掉待采集的下游调用来源。
type companionUsageSourceStub struct {
	service.ReconciliationUsageSource
}

func (s *companionUsageSourceStub) ListUsageBetween(_ context.Context, _ service.ReconciliationUsageQuery) ([]service.ReconciliationUsageFact, error) {
	return nil, nil
}

func (s *companionUsageSourceStub) CountUsagePending(_ context.Context, _ service.ReconciliationUsageQuery, _ int64) (int64, error) {
	return 0, nil
}

// companionBillSourceStub 桩掉上游账单来源。
//
// 回填会在后台 goroutine 里调 FetchBills，所以这里必须给出真实实现（返回空账单），
// 不能只嵌入接口——嵌入接口的方法调用会 panic，而 goroutine 里的 panic 会掀翻整个测试进程。
type companionBillSourceStub struct {
	service.ReconciliationUpstreamBillSource
}

func (s *companionBillSourceStub) FetchBills(_ context.Context, _ service.ReconciliationBillQuery) ([]service.ReconciliationUpstreamBillPayload, error) {
	return nil, nil
}

// companionAccountStub 桩掉账号仓库：规则页只用到 ListAllWithFilters。
type companionAccountStub struct {
	service.AccountRepository

	accounts []service.Account
	err      error
}

func (s *companionAccountStub) ListAllWithFilters(_ context.Context, _, _, _, _ string, _ int64, _ string) ([]service.Account, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.accounts, nil
}

// ==================== 被测装配 ====================

type companionHarness struct {
	router     *gin.Engine
	handler    *CompanionHandler
	ledger     *companionLedgerStub
	rules      *companionRuleStub
	state      *companionStateStub
	bills      *companionBillStub
	accounts   *companionAccountStub
	billSource *companionBillSourceStub
	a6Settings *service.ReconciliationA6SettingsService
	a6Defaults service.ReconciliationA6Config
	fxDefault  float64
}

type companionHarnessOptions struct {
	nilBillSource   bool
	brokenStateRepo bool
	// a6Defaults 是配置层（环境变量）注入的 A6 凭据默认值。
	a6Defaults service.ReconciliationA6Config
}

type companionHarnessOption func(*companionHarnessOptions)

// companionWithoutBillSource 模拟「尚未配置 A6 上游凭据」。
func companionWithoutBillSource() companionHarnessOption {
	return func(options *companionHarnessOptions) { options.nilBillSource = true }
}

// companionWithBrokenStateRepo 模拟同步状态表整体不可用。
func companionWithBrokenStateRepo() companionHarnessOption {
	return func(options *companionHarnessOptions) { options.brokenStateRepo = true }
}

// companionWithA6Defaults 覆盖配置层的 A6 凭据默认值，用来复现「环境变量也没配」。
func companionWithA6Defaults(defaults service.ReconciliationA6Config) companionHarnessOption {
	return func(options *companionHarnessOptions) { options.a6Defaults = defaults }
}

// companionA6Defaults 是默认的「配置齐备」凭据：/status 的健康判定以它为前置条件。
func companionA6Defaults() service.ReconciliationA6Config {
	return service.ReconciliationA6Config{
		BaseURL:     "https://a6-config.example.com",
		UserID:      "config-user",
		AccessToken: "config-A6-token-000000",
		Timeout:     30 * time.Second,
	}
}

func newCompanionHarness(options ...companionHarnessOption) *companionHarness {
	gin.SetMode(gin.TestMode)

	config := companionHarnessOptions{a6Defaults: companionA6Defaults()}
	for _, option := range options {
		option(&config)
	}

	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	ledger := &companionLedgerStub{
		summary: &service.ReconciliationSummary{
			RevenueCNY:        100,
			MatchedRevenueCNY: 100,
			UpstreamCostCNY:   12.5,
			Matched:           4,
			Unmatched:         2,
			UpstreamUnmatched: 1,
			BilledCount:       4,
		},
		points: []service.ReconciliationBucketPoint{
			{Start: now, RevenueCNY: 1, UpstreamCostCNY: 0.25, Matched: 1},
			{Start: now.Add(time.Hour), RevenueCNY: 2, UpstreamCostCNY: 0.5, Matched: 2, Unmatched: 1},
			{Start: now.Add(2 * time.Hour), RevenueCNY: 3, UpstreamCostCNY: 0.75, Matched: 1, UpstreamUnmatched: 1},
		},
		rows: []service.ReconciliationLedgerRow{
			{
				RecordType: "downstream", SourceID: 101, CreatedAt: now,
				RequestID: "req-1", UpstreamRequestID: "up-1",
				UserID: 7, UserEmail: "user@example.com", APIKeyID: 9,
				AccountID: 1, GroupID: 2, GroupName: "默认分组",
				Model: "claude-3-5-sonnet", InputTokens: 100, OutputTokens: 200, CacheTokens: 50,
				RevenueCNY:      1.5,
				HasUpstreamCost: true, UpstreamCostCNY: 0.6, UpstreamCostOrig: 0.08,
				UpstreamCurrency: "USD", UpstreamFxRateCNY: 7.5,
				CostSource: service.ReconciliationCostSourceBilled, Matched: true,
			},
			{
				RecordType: "downstream", SourceID: 102, CreatedAt: now.Add(-time.Minute),
				RequestID: "req-2", UserID: 8, UserEmail: "other@example.com", APIKeyID: 10,
				AccountID: 2, GroupID: 2, GroupName: "默认分组",
				Model: "gpt-4o", InputTokens: 10, OutputTokens: 20, CacheTokens: 0,
				RevenueCNY: 0.25, HasUpstreamCost: false,
				CostSource: service.ReconciliationCostSourceA6Waiting, Matched: false,
			},
		},
		total: 2,
		usage: map[int64]service.ReconciliationAccountUsage{
			1: {
				AccountID: 1, Count: 5,
				FirstSeen: now.Add(-time.Hour), LastSeen: now,
			},
			2: {AccountID: 2, Count: 2, FirstSeen: now.Add(-2 * time.Hour), LastSeen: now.Add(-time.Minute)},
		},
		// 最近模型 / 最近分组来自全历史最后一次调用，与窗口无关。
		// 账号 2 的最后一次调用落在已删除的分组上：分组名退化成空串，
		// 但 group_id 与模型名必须保留下来（线索不能整条丢掉）。
		recent: map[int64]service.ReconciliationRecentUsage{
			1: {AccountID: 1, Model: "claude-3-5-sonnet", GroupID: 2, GroupName: "默认分组"},
			2: {AccountID: 2, Model: "gpt-4o", GroupID: 99, GroupName: ""},
		},
	}

	rules := &companionRuleStub{
		rules: []service.ReconciliationAccountRule{
			{
				ID: 1, AccountID: 1, Provider: service.ReconciliationProviderA6,
				ExternalKey: "token-a", Multiplier: companionFloatPtr(1.25),
				Version: 3, Enabled: true,
				CreatedAt: now.Add(-24 * time.Hour), UpdatedAt: now,
			},
		},
		deleted: 1,
		// 分组 2 在本表里只有账号 1 一行，但它真实挂了 3 个渠道：
		// 页面必须显示 3，而不是按行数数出来的 1。
		groupChannelCounts: map[int64]int64{2: 3},
	}

	state := &companionStateStub{}
	if config.brokenStateRepo {
		state.getErr = assert.AnError
		state.getMultipleErr = assert.AnError
	}

	bills := &companionBillStub{}
	var billSource service.ReconciliationUpstreamBillSource = &companionBillSourceStub{}
	if config.nilBillSource {
		billSource = nil
	}

	accounts := &companionAccountStub{
		accounts: []service.Account{
			{
				ID: 1, Name: "主账号", Platform: "anthropic", Status: "active", Schedulable: true,
				AccountGroups: []service.AccountGroup{
					{AccountID: 1, GroupID: 2, Priority: 1, Group: &service.Group{ID: 2, Name: "默认分组"}},
				},
			},
			{ID: 2, Name: "备用账号", Platform: "openai", Status: "inactive", Schedulable: false},
		},
	}

	ledgerSvc := service.NewReconciliationLedgerService(ledger)
	ruleSvc := service.NewReconciliationAccountRuleService(rules, accounts, ledgerSvc)
	syncSvc := service.NewReconciliationSyncService(
		&companionExtrasStub{},
		bills,
		rules,
		state,
		&companionUsageSourceStub{},
		billSource,
		service.ReconciliationSyncConfig{FxUSDCNYRate: 7.2, A6Lookback: time.Hour},
	)
	// 设置服务与同步服务共用同一个状态表桩：面板改汇率时两边必须同时看到。
	a6Settings := service.NewReconciliationA6SettingsService(state, companionEncryptorStub{}, config.a6Defaults, 7.2)

	handler := NewCompanionHandler(ledgerSvc, ruleSvc, syncSvc, a6Settings)

	// 路由与 registerCompanionRoutes 逐条对应，含 :account_id 路径参数。
	router := gin.New()
	group := router.Group("/api/v1/admin/companion")
	group.GET("/status", handler.Status)
	group.GET("/settings", handler.Settings)
	group.PUT("/settings", handler.UpdateSettings)
	group.GET("/summary", handler.Summary)
	group.GET("/timeseries", handler.Timeseries)
	group.GET("/requests", handler.Requests)
	group.GET("/account-rules", handler.AccountRules)
	group.PUT("/account-rules/:account_id", handler.UpsertAccountRule)
	group.DELETE("/account-rules/:account_id", handler.DeleteAccountRule)
	group.POST("/collect", handler.Collect)
	group.POST("/requeue-unmatched", handler.RequeueUnmatched)
	group.GET("/a6/backfill", handler.A6BackfillStatus)
	group.POST("/a6/backfill", handler.StartA6Backfill)
	group.POST("/upstream/import", handler.ImportUpstream)

	return &companionHarness{
		router: router, handler: handler, ledger: ledger, rules: rules,
		state: state, bills: bills, accounts: accounts, billSource: &companionBillSourceStub{},
		a6Settings: a6Settings, a6Defaults: config.a6Defaults, fxDefault: 7.2,
	}
}

func companionFloatPtr(value float64) *float64 { return &value }

// do 打一次真实 HTTP 请求并解出统一信封。
func (h *companionHarness) do(t *testing.T, method, path, body string) (*httptest.ResponseRecorder, companionContractEnvelope) {
	t.Helper()
	req := httptest.NewRequest(method, companionBasePath+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	h.router.ServeHTTP(recorder, req)

	envelope := companionContractEnvelope{}
	if recorder.Body.Len() > 0 {
		require.NoErrorf(t, json.Unmarshal(recorder.Body.Bytes(), &envelope),
			"响应不是合法 JSON: %s", recorder.Body.String())
	}
	return recorder, envelope
}

// ==================== 1. Summary ====================

// TestCompanionSummaryContractExactFieldSet 汇总对象的键集必须与 CompanionSummary 逐字一致。
func TestCompanionSummaryContractExactFieldSet(t *testing.T) {
	harness := newCompanionHarness()

	recorder, envelope := harness.do(t, http.MethodGet, "/summary", "")
	require.Equal(t, http.StatusOK, recorder.Code, "summary 必须返回 200：%s", recorder.Body.String())

	data := companionDecodeObject(t, envelope.Data)
	companionRequireExactKeys(t, data, companionSummaryContractKeys, "CompanionSummary")
}

// TestCompanionSummaryCountFieldsAreJSONNumbers 六个计数器（及 record_total 等）必须是 JSON 数字，不能是字符串。
func TestCompanionSummaryCountFieldsAreJSONNumbers(t *testing.T) {
	harness := newCompanionHarness()

	_, envelope := harness.do(t, http.MethodGet, "/summary", "")
	data := companionDecodeObject(t, envelope.Data)

	for _, field := range companionSummaryCountFields {
		number := companionJSONNumber(t, data, field)
		assert.Equalf(t, number, float64(int64(number)), "字段 %s 应当是整数计数", field)
		_, isString := data[field].(string)
		assert.Falsef(t, isString, "字段 %s 不能是字符串（前端按 number 使用）", field)
	}

	assert.Equal(t, float64(4), companionJSONNumber(t, data, "matched"))
	assert.Equal(t, float64(2), companionJSONNumber(t, data, "unmatched"))
	assert.Equal(t, float64(4), companionJSONNumber(t, data, "downstream_matched"))
	assert.Equal(t, float64(2), companionJSONNumber(t, data, "downstream_unmatched"))
	assert.Equal(t, float64(1), companionJSONNumber(t, data, "upstream_unmatched"))
	assert.Equal(t, float64(4), companionJSONNumber(t, data, "billed_count"))
	// calculated_count 的**取值**不被前端契约约束（companion.ts 只声明它是 number，
	// 语义为「按规则回算的调用数」），因此这里只用上面的循环锁类型，不锁具体数字。
	assert.GreaterOrEqual(t, companionJSONNumber(t, data, "calculated_count"), float64(0))
	assert.Equal(t, float64(0), companionJSONNumber(t, data, "subarx_unallocated_count"))

	// 契约注释明写 downstream_* 与 matched/unmatched 等同。
	assert.Equal(t, companionJSONNumber(t, data, "matched"), companionJSONNumber(t, data, "downstream_matched"))
	assert.Equal(t, companionJSONNumber(t, data, "unmatched"), companionJSONNumber(t, data, "downstream_unmatched"))
}

// TestCompanionSummaryAmountFieldsKeepEightDecimals 金额字段必须是 8 位小数的字符串。
func TestCompanionSummaryAmountFieldsKeepEightDecimals(t *testing.T) {
	harness := newCompanionHarness()

	_, envelope := harness.do(t, http.MethodGet, "/summary", "")
	data := companionDecodeObject(t, envelope.Data)

	for _, field := range companionSummaryAmountFields {
		value := companionJSONString(t, data, field)
		assert.Regexpf(t, companionAmountPattern, value, "字段 %s 必须是 8 位小数字符串", field)
	}

	assert.Equal(t, "100.00000000", companionJSONString(t, data, "revenue"))
	assert.Equal(t, "100.00000000", companionJSONString(t, data, "matched_revenue"))
	assert.Equal(t, "12.50000000", companionJSONString(t, data, "upstream_cost"))
	// 契约注释：upstream_cost 等同 billed_upstream_cost。
	assert.Equal(t, companionJSONString(t, data, "upstream_cost"), companionJSONString(t, data, "billed_upstream_cost"))
	assert.Equal(t, "87.50000000", companionJSONString(t, data, "gross_profit"))
	assert.Equal(t, "0.00000000", companionJSONString(t, data, "subarx_unallocated_cost"))

	// fx_usd_cny 是汇率，不是 8 位小数金额，但必须是可解析的非空字符串。
	fxRate := companionJSONString(t, data, "fx_usd_cny")
	assert.NotEmpty(t, fxRate)
	assert.Equal(t, "7.2", fxRate, "默认汇率应来自 ReconciliationSyncConfig")
	assert.NotEmpty(t, companionJSONString(t, data, "fx_source"))
	assert.False(t, companionJSONBool(t, data, "fx_stale"))
	companionRequireRFC3339(t, companionJSONString(t, data, "fx_effective_at"), "fx_effective_at")
}

// TestCompanionSummaryMarginPercentIsAlwaysTwoDecimals margin_percent 必须恒为 2 位小数字符串。
func TestCompanionSummaryMarginPercentIsAlwaysTwoDecimals(t *testing.T) {
	harness := newCompanionHarness()

	_, envelope := harness.do(t, http.MethodGet, "/summary", "")
	data := companionDecodeObject(t, envelope.Data)

	value := companionJSONString(t, data, "margin_percent")
	require.Regexpf(t, companionMarginPattern, value, "margin_percent 必须是 2 位小数字符串")
	// (100 - 12.5) / 100 * 100 = 87.5
	assert.Equal(t, "87.50", value)
}

// TestCompanionSummaryMarginPercentZeroRevenueIsZeroString revenue = 0 时必须是 "0.00"，
// 不能是 null、NaN 或空串：前端直接把它渲染成百分比文案。
func TestCompanionSummaryMarginPercentZeroRevenueIsZeroString(t *testing.T) {
	harness := newCompanionHarness()
	harness.ledger.summary = &service.ReconciliationSummary{
		RevenueCNY:        0,
		MatchedRevenueCNY: 0,
		UpstreamCostCNY:   0,
		Matched:           0,
		Unmatched:         0,
		UpstreamUnmatched: 0,
	}

	recorder, envelope := harness.do(t, http.MethodGet, "/summary", "")
	require.Equal(t, http.StatusOK, recorder.Code)

	data := companionDecodeObject(t, envelope.Data)
	value := companionJSONString(t, data, "margin_percent")
	assert.Equal(t, "0.00", value)
	assert.NotEqual(t, "NaN", value)
	assert.NotEqual(t, "Inf", value)
	assert.NotEmpty(t, value)

	assert.Equal(t, "0.00000000", companionJSONString(t, data, "revenue"))
	assert.Equal(t, "0.00000000", companionJSONString(t, data, "gross_profit"))
	assert.Equal(t, float64(0), companionJSONNumber(t, data, "record_total"))
}

// TestCompanionSummaryWindowAndFixedValues 固定口径字段、record_total 恒等式与时间窗格式。
func TestCompanionSummaryWindowAndFixedValues(t *testing.T) {
	harness := newCompanionHarness()

	_, envelope := harness.do(t, http.MethodGet, "/summary?from=2026-09-01T00:00:00Z&to=2026-09-28T00:00:00Z", "")
	data := companionDecodeObject(t, envelope.Data)

	assert.Equal(t, "billed_or_subarx_api_or_rule", companionJSONString(t, data, "cost_policy"))
	assert.Equal(t, "matched_only", companionJSONString(t, data, "profit_scope"))
	assert.Equal(t, "CNY", companionJSONString(t, data, "currency"))
	assert.Equal(t, "0.00000000", companionJSONString(t, data, "subarx_unallocated_cost"))
	assert.Equal(t, float64(0), companionJSONNumber(t, data, "subarx_unallocated_count"))

	matched := companionJSONNumber(t, data, "matched")
	unmatched := companionJSONNumber(t, data, "unmatched")
	upstreamUnmatched := companionJSONNumber(t, data, "upstream_unmatched")
	assert.Equal(t, matched+unmatched+upstreamUnmatched, companionJSONNumber(t, data, "record_total"),
		"record_total 必须等于 matched + unmatched + upstream_unmatched")

	from := companionRequireRFC3339(t, companionJSONString(t, data, "from"), "from")
	to := companionRequireRFC3339(t, companionJSONString(t, data, "to"), "to")
	assert.Equal(t, "2026-09-01T00:00:00Z", from.UTC().Format(time.RFC3339))
	assert.Equal(t, "2026-09-28T00:00:00Z", to.UTC().Format(time.RFC3339))
	assert.True(t, from.Before(to), "from 必须早于 to")
}

// TestCompanionSummaryUsesRuntimeFxOverrideOverride 汇率覆盖值必须体现在 fx_usd_cny 上。
func TestCompanionSummaryUsesRuntimeFxOverride(t *testing.T) {
	harness := newCompanionHarness()
	harness.state.set("fx_usd_cny_rate_override", "7.35")

	_, envelope := harness.do(t, http.MethodGet, "/summary", "")
	data := companionDecodeObject(t, envelope.Data)
	assert.Equal(t, "7.35", companionJSONString(t, data, "fx_usd_cny"))
}

// ==================== 2. Status ====================

// TestCompanionStatusEnabledIsAlwaysTrue enabled 必须恒为 true：
// 前端在 enabled === false 时会把整个页面切成「未配置」引导态。
//
// 前置条件：A6 凭据齐备（harness 默认注入完整的配置层凭据）。
// 凭据缺失时 healthy 必须为 false，那两条分支由下面的用例单独覆盖。
func TestCompanionStatusEnabledIsAlwaysTrue(t *testing.T) {
	harness := newCompanionHarness()

	recorder, envelope := harness.do(t, http.MethodGet, "/status", "")
	require.Equal(t, http.StatusOK, recorder.Code)

	data := companionDecodeObject(t, envelope.Data)
	assert.True(t, companionJSONBool(t, data, "enabled"), "enabled 必须为 true")
	assert.True(t, companionJSONBool(t, data, "healthy"), "凭据齐备时 healthy 应为 true")
	assert.True(t, harness.handler.Enabled(), "Enabled() 必须为 true")
}

// TestCompanionStatusUnhealthyWhenA6CredentialsMissing A6 有效凭据缺失时必须报不健康，
// 并给出「去哪里配置」的可执行提示。
//
// 背景：凭据没配时看板上全是 0，报健康会让管理员以为「数据本身就是 0」。
func TestCompanionStatusUnhealthyWhenA6CredentialsMissing(t *testing.T) {
	cases := []struct {
		name     string
		defaults service.ReconciliationA6Config
	}{
		{"完全没有配置", service.ReconciliationA6Config{}},
		{"只有基址", service.ReconciliationA6Config{BaseURL: "https://a6-config.example.com"}},
		{"缺令牌", service.ReconciliationA6Config{BaseURL: "https://a6-config.example.com", UserID: "u"}},
		{"缺用户标识", service.ReconciliationA6Config{BaseURL: "https://a6-config.example.com", AccessToken: "t"}},
		{"基址不是绝对 URL", service.ReconciliationA6Config{BaseURL: "a6-config.example.com", UserID: "u", AccessToken: "t"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness := newCompanionHarness(companionWithA6Defaults(tc.defaults))

			recorder, envelope := harness.do(t, http.MethodGet, "/status", "")
			require.Equal(t, http.StatusOK, recorder.Code)

			data := companionDecodeObject(t, envelope.Data)
			assert.True(t, companionJSONBool(t, data, "enabled"), "enabled 不随配置状态变化")
			assert.False(t, companionJSONBool(t, data, "healthy"), "凭据缺失必须报不健康")
			detail := companionJSONString(t, data, "detail")
			assert.NotEmpty(t, strings.TrimSpace(detail), "不健康时 detail 必须非空")
			assert.Contains(t, detail, "A6", "detail 必须点名缺的是 A6 凭据")
			assert.Contains(t, detail, "上游 A6 配置", "detail 必须告诉管理员去哪个页面填")
		})
	}
}

// TestCompanionStatusHealthyOncePanelCredentialsSaved 面板填完凭据后，健康指示必须立刻转好，
// 不需要重启进程。
func TestCompanionStatusHealthyOncePanelCredentialsSaved(t *testing.T) {
	harness := newCompanionHarness(companionWithA6Defaults(service.ReconciliationA6Config{}))

	_, envelope := harness.do(t, http.MethodGet, "/status", "")
	require.False(t, companionJSONBool(t, companionDecodeObject(t, envelope.Data), "healthy"))

	recorder, _ := harness.do(t, http.MethodPut, "/settings",
		`{"a6_base_url":"https://a6-panel.example.com","a6_user_id":"panel-user","a6_access_token":"panel-token-abcdefgh"}`)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	_, envelope = harness.do(t, http.MethodGet, "/status", "")
	data := companionDecodeObject(t, envelope.Data)
	assert.True(t, companionJSONBool(t, data, "healthy"), "凭据齐备后必须立刻恢复健康：%v", data)
}

// TestCompanionStatusUnhealthyCarriesDetail 不健康时 detail 必须非空，供页面展示故障原因。
func TestCompanionStatusUnhealthyCarriesDetail(t *testing.T) {
	harness := newCompanionHarness()
	harness.state.set("a6_last_sync_error", "A6 上游返回 401")

	_, envelope := harness.do(t, http.MethodGet, "/status", "")
	data := companionDecodeObject(t, envelope.Data)

	assert.True(t, companionJSONBool(t, data, "enabled"), "enabled 不随健康状态变化")
	assert.False(t, companionJSONBool(t, data, "healthy"))
	detail := companionJSONString(t, data, "detail")
	assert.NotEmpty(t, strings.TrimSpace(detail), "不健康时 detail 必须非空")
	assert.Contains(t, detail, "A6 上游返回 401")
}

// TestCompanionStatusCarriesRealFailureTime 不健康时给出真实失败时刻。
//
// 页面上的「更新于」是刷新时刻，会把「上游 11:58 挂了、我 12:03 打开页面」渲染成
// 「更新于 12:03:00」，故障到底什么时候开始的完全看不出来。失败时刻必须由后端给。
func TestCompanionStatusCarriesRealFailureTime(t *testing.T) {
	harness := newCompanionHarness()
	harness.state.set("a6_last_sync_error", "A6 上游返回 401")
	harness.state.set("a6_last_sync_error_at", "2026-10-01T04:14:59.459253264Z")

	_, envelope := harness.do(t, http.MethodGet, "/status", "")
	data := companionDecodeObject(t, envelope.Data)

	assert.False(t, companionJSONBool(t, data, "healthy"))
	// 必须是毫秒精度：RFC3339Nano 的 9 位小数只是靠浏览器宽松截断才能被 new Date() 解析，
	// 那是实现细节不是契约，交给前端的字段一律钉死在 3 位。
	assert.Equal(t, "2026-10-01T04:14:59.459Z", companionJSONString(t, data, "last_error_at"))
}

// TestCompanionStatusHealthyOmitsFailureTime 已连接时不带失败时间。
//
// 成功一轮只清 a6_last_sync_error，并不清 a6_last_sync_error_at，所以这个时间戳单独看
// 有可能已经过期；健康时绝不能把它当成当前状态展示给管理员。
func TestCompanionStatusHealthyOmitsFailureTime(t *testing.T) {
	harness := newCompanionHarness()
	harness.state.set("a6_last_sync_error_at", "2026-10-01T04:14:59.459253264Z")

	_, envelope := harness.do(t, http.MethodGet, "/status", "")
	data := companionDecodeObject(t, envelope.Data)

	require.True(t, companionJSONBool(t, data, "healthy"), "没有失败记录时必须健康：%v", data)
	_, present := data["last_error_at"]
	assert.False(t, present, "健康时不该给出可能已经过期的失败时间")
}

// TestCompanionStatusWithoutBillSourceIsDegradedButEnabled 未配置上游凭据时仍须 enabled = true。
func TestCompanionStatusWithoutBillSourceIsDegradedButEnabled(t *testing.T) {
	harness := newCompanionHarness(companionWithoutBillSource())

	_, envelope := harness.do(t, http.MethodGet, "/status", "")
	data := companionDecodeObject(t, envelope.Data)

	assert.True(t, companionJSONBool(t, data, "enabled"))
	assert.False(t, companionJSONBool(t, data, "healthy"))
	assert.NotEmpty(t, strings.TrimSpace(companionJSONString(t, data, "detail")))
}

// ==================== 2.5 上游 A6 配置（/settings） ====================

// companionSettingsContractKeys 是 CompanionSettings 声明的全部字段（6 个）。
//
// 令牌只允许以 a6_token_configured / a6_token_mask 两种形态出现：
// 任何新增的令牌字段都必须先过「会不会把明文发给浏览器」这一关。
var companionSettingsContractKeys = []string{
	"a6_base_url", "a6_user_id", "a6_token_configured", "a6_token_mask",
	"fx_usd_cny_rate", "override_keys",
}

// TestCompanionSettingsGetContractExactFieldSet GET 的键集与类型必须逐字符合契约。
func TestCompanionSettingsGetContractExactFieldSet(t *testing.T) {
	harness := newCompanionHarness()

	recorder, envelope := harness.do(t, http.MethodGet, "/settings", "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, 0, envelope.Code, "成功必须 code:0")

	data := companionDecodeObject(t, envelope.Data)
	companionRequireExactKeys(t, data, companionSettingsContractKeys, "CompanionSettings")

	assert.Equal(t, "https://a6-config.example.com", companionJSONString(t, data, "a6_base_url"))
	assert.Equal(t, "config-user", companionJSONString(t, data, "a6_user_id"))
	assert.True(t, companionJSONBool(t, data, "a6_token_configured"))
	assert.Equal(t, "conf…0000", companionJSONString(t, data, "a6_token_mask"))
	assert.Equal(t, 7.2, companionJSONNumber(t, data, "fx_usd_cny_rate"))
	assert.Empty(t, companionJSONArray(t, data, "override_keys"), "未覆盖时必须是空数组而不是 null")
}

// TestCompanionSettingsNeverExposesTokenPlaintext 面板保存令牌后：
//   - 库里存的是密文；
//   - GET / PUT 的响应体里都不得出现明文。
func TestCompanionSettingsNeverExposesTokenPlaintext(t *testing.T) {
	harness := newCompanionHarness()
	const token = "abcd123456789012wxyz"

	recorder, envelope := harness.do(t, http.MethodPut, "/settings",
		`{"a6_access_token":"`+token+`"}`)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.NotContains(t, recorder.Body.String(), token, "PUT 响应里不得出现明文令牌")

	stored := harness.state.raw(service.ReconciliationStateKeyA6AccessTokenOverride)
	require.NotEmpty(t, stored)
	require.NotEqual(t, token, stored, "必须密文落库")
	require.NotContains(t, stored, token)
	require.True(t, strings.HasPrefix(stored, companionCipherPrefix), "看起来根本没走加密器: %q", stored)

	recorder, envelope = harness.do(t, http.MethodGet, "/settings", "")
	require.Equal(t, http.StatusOK, recorder.Code)
	require.NotContains(t, recorder.Body.String(), token, "GET 响应里不得出现明文令牌")

	data := companionDecodeObject(t, envelope.Data)
	assert.True(t, companionJSONBool(t, data, "a6_token_configured"))
	assert.Equal(t, "abcd…wxyz", companionJSONString(t, data, "a6_token_mask"), "按契约示例形状脱敏")
	assert.Equal(t, []any{service.ReconciliationOverrideKeyA6AccessToken},
		companionJSONArray(t, data, "override_keys"))
}

// TestCompanionSettingsPutKeepsTokenWhenBlank 令牌留空或全字段缺省都表示「不改动」。
func TestCompanionSettingsPutKeepsTokenWhenBlank(t *testing.T) {
	harness := newCompanionHarness()
	const token = "abcd123456789012wxyz"

	_, _ = harness.do(t, http.MethodPut, "/settings", `{"a6_access_token":"`+token+`"}`)
	storedBefore := harness.state.raw(service.ReconciliationStateKeyA6AccessTokenOverride)

	cases := []struct {
		name string
		body string
	}{
		{"只改基址", `{"a6_base_url":"https://a6-panel.example.com"}`},
		{"令牌传空串", `{"a6_access_token":""}`},
		{"令牌传全空白", `{"a6_access_token":"   "}`},
		{"空对象", `{}`},
		{"空请求体", ``},
		{"显式 clear=false", `{"clear_a6_access_token":false}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder, envelope := harness.do(t, http.MethodPut, "/settings", tc.body)
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

			require.Equal(t, storedBefore, harness.state.raw(service.ReconciliationStateKeyA6AccessTokenOverride),
				"留空必须保持原值不动")
			data := companionDecodeObject(t, envelope.Data)
			assert.True(t, companionJSONBool(t, data, "a6_token_configured"))
			assert.Equal(t, "abcd…wxyz", companionJSONString(t, data, "a6_token_mask"))
		})
	}
}

// TestCompanionSettingsPutClearTokenFallsBackToConfig clear_a6_access_token=true 必须清掉覆盖。
func TestCompanionSettingsPutClearTokenFallsBackToConfig(t *testing.T) {
	harness := newCompanionHarness()
	const token = "abcd12345678wxyz"
	_, _ = harness.do(t, http.MethodPut, "/settings", `{"a6_access_token":"`+token+`"}`)

	recorder, envelope := harness.do(t, http.MethodPut, "/settings",
		`{"clear_a6_access_token":true}`)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	assert.Empty(t, harness.state.raw(service.ReconciliationStateKeyA6AccessTokenOverride), "覆盖必须被清空")
	data := companionDecodeObject(t, envelope.Data)
	assert.True(t, companionJSONBool(t, data, "a6_token_configured"), "配置里还有令牌，仍算已配置")
	assert.Equal(t, "conf…0000", companionJSONString(t, data, "a6_token_mask"), "回落到配置令牌的脱敏")
	assert.Empty(t, companionJSONArray(t, data, "override_keys"))
}

// TestCompanionSettingsOverrideKeysFollowThePanel override_keys 必须精确反映当前来自面板的键。
func TestCompanionSettingsOverrideKeysFollowThePanel(t *testing.T) {
	harness := newCompanionHarness()

	recorder, envelope := harness.do(t, http.MethodPut, "/settings",
		`{"a6_base_url":"https://a6-panel.example.com","fx_usd_cny_rate":6.71,"a6_user_id":"panel-user"}`)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	data := companionDecodeObject(t, envelope.Data)
	assert.Equal(t, []any{
		service.ReconciliationOverrideKeyA6BaseURL,
		service.ReconciliationOverrideKeyA6UserID,
		service.ReconciliationOverrideKeyFxRate,
	}, companionJSONArray(t, data, "override_keys"))
	assert.Equal(t, "https://a6-panel.example.com", companionJSONString(t, data, "a6_base_url"))
	assert.Equal(t, "panel-user", companionJSONString(t, data, "a6_user_id"))
	assert.Equal(t, 6.71, companionJSONNumber(t, data, "fx_usd_cny_rate"))

	// 面板保存的汇率必须立刻成为记账汇率：看板上读到的就是 6.71。
	_, summary := harness.do(t, http.MethodGet, "/summary", "")
	summaryData := companionDecodeObject(t, summary.Data)
	assert.Equal(t, "6.71", companionJSONString(t, summaryData, "fx_usd_cny"))

	// 空串清除覆盖后，override_keys 里对应的键必须消失。
	_, envelope = harness.do(t, http.MethodPut, "/settings", `{"a6_base_url":"","a6_user_id":""}`)
	data = companionDecodeObject(t, envelope.Data)
	assert.Equal(t, []any{service.ReconciliationOverrideKeyFxRate},
		companionJSONArray(t, data, "override_keys"))
	assert.Equal(t, "https://a6-config.example.com", companionJSONString(t, data, "a6_base_url"),
		"清除覆盖后回落到配置层")
}

// TestCompanionSettingsPutReturnsSameShapeAsGet 保存后返回的数据必须与 GET 完全同构，
// 前端保存完可以直接拿它刷新表单。
func TestCompanionSettingsPutReturnsSameShapeAsGet(t *testing.T) {
	harness := newCompanionHarness()

	_, putEnvelope := harness.do(t, http.MethodPut, "/settings",
		`{"a6_base_url":"https://a6-panel.example.com","a6_access_token":"abcd12345678wxyz","fx_usd_cny_rate":6.71}`)
	_, getEnvelope := harness.do(t, http.MethodGet, "/settings", "")

	putData := companionDecodeObject(t, putEnvelope.Data)
	getData := companionDecodeObject(t, getEnvelope.Data)
	companionRequireExactKeys(t, putData, companionSettingsContractKeys, "PUT /settings 的 data")
	assert.Equal(t, getData, putData, "PUT 的 data 必须与随后的 GET 完全一致")
}

// TestCompanionSettingsPutValidationReturnsBadRequest 非法入参必须是 400 + COMPANION_BAD_REQUEST。
//
// 绝不能返回 401/403：前端把 401 视为会话失效并跳登录页，配置填错不该把管理员踢出去。
func TestCompanionSettingsPutValidationReturnsBadRequest(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"基址不是 URL", `{"a6_base_url":"not a url"}`},
		{"基址缺协议", `{"a6_base_url":"a6.example.com"}`},
		{"基址协议不支持", `{"a6_base_url":"ftp://a6.example.com"}`},
		{"汇率不是正数", `{"fx_usd_cny_rate":0}`},
		{"汇率为负", `{"fx_usd_cny_rate":-1}`},
		{"汇率超上限", `{"fx_usd_cny_rate":100001}`},
		{"请求体不是 JSON", `{not json`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness := newCompanionHarness()

			recorder, envelope := harness.do(t, http.MethodPut, "/settings", tc.body)
			require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
			assert.Equal(t, http.StatusBadRequest, envelope.Code, "失败时 code 必须等于 HTTP 状态码")
			assert.NotEqual(t, http.StatusUnauthorized, recorder.Code)
			assert.NotEqual(t, http.StatusForbidden, recorder.Code)
			assert.Contains(t, envelope.Message, "COMPANION_BAD_REQUEST")

			// 校验失败不允许落半个字段。
			assert.Empty(t, harness.state.raw(service.ReconciliationStateKeyA6BaseURLOverride))
			assert.Empty(t, harness.state.raw(service.ReconciliationStateKeyA6UserIDOverride))
			assert.Empty(t, harness.state.raw(service.ReconciliationStateKeyFxRateOverride))
		})
	}

	t.Run("汇率边界值可用", func(t *testing.T) {
		harness := newCompanionHarness()
		recorder, envelope := harness.do(t, http.MethodPut, "/settings", `{"fx_usd_cny_rate":100000}`)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		data := companionDecodeObject(t, envelope.Data)
		assert.Equal(t, float64(100000), companionJSONNumber(t, data, "fx_usd_cny_rate"))
	})
}

// TestCompanionSettingsValidationFailureWritesNothing 校验顺序：
// 一个非法字段不该让同一请求里的其它字段半保存（包括令牌）。
func TestCompanionSettingsValidationFailureWritesNothing(t *testing.T) {
	harness := newCompanionHarness()

	recorder, _ := harness.do(t, http.MethodPut, "/settings",
		`{"a6_base_url":"not a url","a6_access_token":"abcd12345678wxyz"}`)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.Empty(t, harness.state.raw(service.ReconciliationStateKeyA6AccessTokenOverride))
	assert.NotContains(t, recorder.Body.String(), "abcd12345678wxyz")
}

// ==================== 3. Requests ====================

// TestCompanionRequestsRowContractExactFieldSet 明细行的键集必须与 CompanionRequestRow 逐字一致。
func TestCompanionRequestsRowContractExactFieldSet(t *testing.T) {
	harness := newCompanionHarness()

	recorder, envelope := harness.do(t, http.MethodGet, "/requests", "")
	require.Equal(t, http.StatusOK, recorder.Code)

	data := companionDecodeObject(t, envelope.Data)
	items := companionJSONArray(t, data, "items")
	require.Len(t, items, 2)

	for index, item := range items {
		row, isObject := item.(map[string]any)
		require.Truef(t, isObject, "items[%d] 必须是对象", index)
		companionRequireExactKeys(t, row, companionRequestRowContractKeys, "CompanionRequestRow")
	}

	// 分页外层字段同样锁死。
	companionRequireExactKeys(t, data,
		[]string{"items", "page", "page_size", "total", "total_pages", "from", "to", "status"},
		"CompanionRequestPage")
}

// TestCompanionRequestsUnmatchedAmountsAreEmptyStrings 未对账时成本类字段必须是空串。
//
// 这是刻意设计：显示 "0.00000000" 会被误读成「上游免费」，null 会让前端渲染崩掉。
func TestCompanionRequestsUnmatchedAmountsAreEmptyStrings(t *testing.T) {
	harness := newCompanionHarness()

	_, envelope := harness.do(t, http.MethodGet, "/requests?status=unmatched", "")
	data := companionDecodeObject(t, envelope.Data)
	items := companionJSONArray(t, data, "items")
	require.Len(t, items, 2)

	unmatched, isObject := items[1].(map[string]any)
	require.True(t, isObject)

	for _, field := range []string{"upstream_cost", "billed_upstream_cost", "upstream_cost_original", "gross_profit"} {
		value := companionJSONString(t, unmatched, field)
		assert.Equalf(t, "", value, "未对账行的 %s 必须是空字符串", field)
		assert.NotEqualf(t, "0.00000000", value, "未对账行的 %s 不能显示成 0", field)
	}
	assert.Equal(t, "", companionJSONString(t, unmatched, "upstream_currency"))
	assert.False(t, companionJSONBool(t, unmatched, "matched"))
	assert.Equal(t, "a6_waiting", companionJSONString(t, unmatched, "cost_source"))
	assert.NotEmpty(t, strings.TrimSpace(companionJSONString(t, unmatched, "cost_source_label")))

	// 对账行才有值，且同样是 8 位小数。
	matchedRow, isObject := items[0].(map[string]any)
	require.True(t, isObject)
	assert.Equal(t, "0.60000000", companionJSONString(t, matchedRow, "upstream_cost"))
	assert.Equal(t, "0.60000000", companionJSONString(t, matchedRow, "billed_upstream_cost"))
	assert.Equal(t, "0.08000000", companionJSONString(t, matchedRow, "upstream_cost_original"))
	assert.Equal(t, "0.90000000", companionJSONString(t, matchedRow, "gross_profit"))
	assert.Equal(t, "USD", companionJSONString(t, matchedRow, "upstream_currency"))
	assert.Equal(t, "7.5", companionJSONString(t, matchedRow, "fx_rate_to_cny"))
	assert.True(t, companionJSONBool(t, matchedRow, "matched"))
	assert.Regexpf(t, companionAmountPattern, companionJSONString(t, matchedRow, "revenue"), "revenue 必须是 8 位小数字符串")
}

// TestCompanionRequestsRowFieldTypes 明细行的数字/布尔/时间字段类型。
func TestCompanionRequestsRowFieldTypes(t *testing.T) {
	harness := newCompanionHarness()

	_, envelope := harness.do(t, http.MethodGet, "/requests", "")
	data := companionDecodeObject(t, envelope.Data)
	items := companionJSONArray(t, data, "items")
	require.NotEmpty(t, items)
	row := items[0].(map[string]any)

	for _, field := range companionRequestRowCountFields {
		number := companionJSONNumber(t, row, field)
		_, isString := row[field].(string)
		assert.Falsef(t, isString, "字段 %s 不能是字符串", field)
		assert.Equalf(t, number, float64(int64(number)), "字段 %s 应当是整数计数", field)
	}

	assert.Equal(t, float64(101), companionJSONNumber(t, row, "source_id"))
	assert.Equal(t, float64(7), companionJSONNumber(t, row, "user_id"))
	assert.Equal(t, float64(1), companionJSONNumber(t, row, "account_id"))
	assert.Equal(t, float64(50), companionJSONNumber(t, row, "cache_tokens"))

	assert.Equal(t, "downstream", companionJSONString(t, row, "record_type"))
	assert.Equal(t, "req-1", companionJSONString(t, row, "request_id"))
	assert.Equal(t, "up-1", companionJSONString(t, row, "upstream_request_id"))
	assert.Equal(t, "user@example.com", companionJSONString(t, row, "user_email"))
	assert.Equal(t, "claude-3-5-sonnet", companionJSONString(t, row, "model"))

	companionRequireRFC3339(t, companionJSONString(t, row, "created_at"), "created_at")
	assert.NotEmpty(t, strings.TrimSpace(companionJSONString(t, row, "cost_source_label")),
		"cost_source_label 必须非空：前端对未知 cost_source 回落到它")
}

// TestCompanionRequestsCostSourceLabels 逐个覆盖 6 种对账状态（含前端映射表里没有的取值），
// 每种都必须拿到非空的 cost_source_label。
func TestCompanionRequestsCostSourceLabels(t *testing.T) {
	known := []service.ReconciliationCostSource{
		service.ReconciliationCostSourceBilled,
		service.ReconciliationCostSourcePending,
		service.ReconciliationCostSourceRuleUnconfigured,
		service.ReconciliationCostSourceA6Pending,
		service.ReconciliationCostSourceA6Waiting,
		service.ReconciliationCostSourceUpstreamUnmatched,
	}

	for _, source := range known {
		source := source
		t.Run(string(source), func(t *testing.T) {
			harness := newCompanionHarness()
			harness.ledger.rows = []service.ReconciliationLedgerRow{{
				RecordType: "downstream", SourceID: 1, CreatedAt: time.Now().UTC(),
				RevenueCNY: 1, CostSource: source,
			}}

			recorder, envelope := harness.do(t, http.MethodGet, "/requests", "")
			require.Equal(t, http.StatusOK, recorder.Code)

			data := companionDecodeObject(t, envelope.Data)
			items := companionJSONArray(t, data, "items")
			require.Len(t, items, 1)
			row := items[0].(map[string]any)

			assert.Equal(t, string(source), companionJSONString(t, row, "cost_source"))
			label := companionJSONString(t, row, "cost_source_label")
			assert.NotEmptyf(t, strings.TrimSpace(label), "cost_source=%s 的 label 不能为空", source)

			assert.Containsf(t, companionCostSourceUnion, string(source),
				"cost_source 必须是 CompanionCostSource 联合类型里的取值")
		})
	}

	// 前端映射表里没有的取值：后端仍必须给出非空 label 作为回落文案。
	t.Run("unknown_value_still_has_label", func(t *testing.T) {
		harness := newCompanionHarness()
		unknown := service.ReconciliationCostSource("legacy_state_not_in_frontend_union")
		harness.ledger.rows = []service.ReconciliationLedgerRow{{
			RecordType: "downstream", SourceID: 1, CreatedAt: time.Now().UTC(),
			RevenueCNY: 1, CostSource: unknown,
		}}

		_, envelope := harness.do(t, http.MethodGet, "/requests", "")
		data := companionDecodeObject(t, envelope.Data)
		items := companionJSONArray(t, data, "items")
		require.Len(t, items, 1)
		row := items[0].(map[string]any)

		assert.Equal(t, string(unknown), companionJSONString(t, row, "cost_source"))
		assert.NotEmpty(t, strings.TrimSpace(companionJSONString(t, row, "cost_source_label")),
			"未知 cost_source 必须回落到非空 label")
		assert.NotContains(t, companionCostSourceUnion, string(unknown))
	})
}

// TestCompanionRequestsPaginationSemantics 分页字段类型与 total_pages 计算规则。
func TestCompanionRequestsPaginationSemantics(t *testing.T) {
	t.Run("empty_page_has_zero_total_pages_and_empty_items_array", func(t *testing.T) {
		harness := newCompanionHarness()
		harness.ledger.rows = nil
		harness.ledger.total = 0

		recorder, envelope := harness.do(t, http.MethodGet, "/requests", "")
		require.Equal(t, http.StatusOK, recorder.Code)

		// 原始 JSON 层确认是 [] 而不是 null：前端直接对 items 做遍历。
		assert.Contains(t, string(envelope.Data), `"items":[]`, "items 必须是空数组而不是 null")

		data := companionDecodeObject(t, envelope.Data)
		assert.Empty(t, companionJSONArray(t, data, "items"))
		assert.Equal(t, float64(0), companionJSONNumber(t, data, "total"))
		assert.Equal(t, float64(0), companionJSONNumber(t, data, "total_pages"),
			"total = 0 时 total_pages 必须是 0")
		assert.Equal(t, float64(1), companionJSONNumber(t, data, "page"))
		assert.Equal(t, float64(50), companionJSONNumber(t, data, "page_size"))
	})

	t.Run("total_51_page_size_50_is_two_pages", func(t *testing.T) {
		harness := newCompanionHarness()
		harness.ledger.total = 51

		_, envelope := harness.do(t, http.MethodGet, "/requests?page=1&page_size=50", "")
		data := companionDecodeObject(t, envelope.Data)

		assert.Equal(t, float64(51), companionJSONNumber(t, data, "total"))
		assert.Equal(t, float64(50), companionJSONNumber(t, data, "page_size"))
		assert.Equal(t, float64(2), companionJSONNumber(t, data, "total_pages"),
			"ceil(51 / 50) 必须是 2")
	})

	t.Run("page_and_status_are_echoed_through", func(t *testing.T) {
		harness := newCompanionHarness()

		_, envelope := harness.do(t, http.MethodGet, "/requests?page=2&page_size=10&status=matched", "")
		data := companionDecodeObject(t, envelope.Data)

		assert.Equal(t, float64(2), companionJSONNumber(t, data, "page"))
		assert.Equal(t, float64(10), companionJSONNumber(t, data, "page_size"))
		assert.Equal(t, "matched", companionJSONString(t, data, "status"))
		assert.Equal(t, "matched", harness.ledger.lastStatus)
		assert.Equal(t, 2, harness.ledger.lastPage)
		assert.Equal(t, 10, harness.ledger.lastPageSize)
	})

	t.Run("unknown_status_falls_back_to_all_and_echoes_effective_value", func(t *testing.T) {
		harness := newCompanionHarness()

		_, envelope := harness.do(t, http.MethodGet, "/requests?status=whatever", "")
		data := companionDecodeObject(t, envelope.Data)

		assert.Equal(t, "all", harness.ledger.lastStatus, "未知 status 必须按 all 处理而不是报错")
		assert.Equal(t, "all", companionJSONString(t, data, "status"),
			"回显必须是实际生效的 all，不能把请求里的 whatever 原样写回")
	})

	t.Run("oversized_page_size_is_clamped_and_echoed_as_effective_value", func(t *testing.T) {
		harness := newCompanionHarness()

		_, envelope := harness.do(t, http.MethodGet, "/requests?page_size=200", "")
		data := companionDecodeObject(t, envelope.Data)

		assert.Equal(t, 100, harness.ledger.lastPageSize, "超过上限的 page_size 必须收敛到 100")
		assert.Equal(t, float64(100), companionJSONNumber(t, data, "page_size"),
			"回显的 page_size 必须与实际取数一致：旧实现回显 50、实际按 100 取，前端分页控件因此对不上")
		// total = 51：按生效的 100 算只有 1 页；旧实现按 50 算会给出 2 页。
		assert.Equal(t, float64(1), companionJSONNumber(t, data, "total_pages"),
			"total_pages 必须按生效的 page_size 计算")
	})

	t.Run("zero_page_size_falls_back_to_default_and_echoes_it", func(t *testing.T) {
		harness := newCompanionHarness()

		_, envelope := harness.do(t, http.MethodGet, "/requests?page_size=0", "")
		data := companionDecodeObject(t, envelope.Data)

		assert.Equal(t, 50, harness.ledger.lastPageSize)
		assert.Equal(t, float64(50), companionJSONNumber(t, data, "page_size"))
	})

	t.Run("pagination_fields_are_numbers", func(t *testing.T) {
		harness := newCompanionHarness()

		_, envelope := harness.do(t, http.MethodGet, "/requests", "")
		data := companionDecodeObject(t, envelope.Data)
		for _, field := range []string{"page", "page_size", "total", "total_pages"} {
			companionJSONNumber(t, data, field)
		}
		companionRequireRFC3339(t, companionJSONString(t, data, "from"), "from")
		companionRequireRFC3339(t, companionJSONString(t, data, "to"), "to")
	})
}

// ==================== 4. Timeseries ====================

// TestCompanionTimeseriesContract points 无数据时是空数组，数据点的键集恰好等于契约字段。
func TestCompanionTimeseriesContract(t *testing.T) {
	harness := newCompanionHarness()

	recorder, envelope := harness.do(t, http.MethodGet, "/timeseries", "")
	require.Equal(t, http.StatusOK, recorder.Code)

	data := companionDecodeObject(t, envelope.Data)
	companionRequireExactKeys(t, data, []string{"from", "to", "bucket", "points"}, "CompanionTimeSeries")

	bucket := companionJSONString(t, data, "bucket")
	assert.Containsf(t, companionTimeseriesBucketLabels, bucket, "bucket 必须是契约允许的粒度标签")
	assert.Equal(t, "1小时", bucket, "默认 24 小时窗口应使用 1 小时分桶")

	points := companionJSONArray(t, data, "points")
	require.Len(t, points, 3)

	var previous time.Time
	for index, item := range points {
		point, isObject := item.(map[string]any)
		require.Truef(t, isObject, "points[%d] 必须是对象", index)
		companionRequireExactKeys(t, point, companionTimeseriesPointContractKeys, "CompanionTimeSeriesPoint")

		start := companionRequireRFC3339(t, companionJSONString(t, point, "start"), "point.start")
		if index > 0 {
			assert.Truef(t, start.After(previous), "points[%d].start 必须严格升序", index)
		}
		previous = start

		assert.Regexpf(t, companionAmountPattern, companionJSONString(t, point, "revenue"), "point.revenue 必须是 8 位小数")
		assert.Regexpf(t, companionAmountPattern, companionJSONString(t, point, "upstream_cost"), "point.upstream_cost 必须是 8 位小数")
		assert.Regexpf(t, companionAmountPattern, companionJSONString(t, point, "gross_profit"), "point.gross_profit 必须是 8 位小数")

		for _, field := range []string{"matched", "unmatched", "upstream_unmatched", "record_total"} {
			companionJSONNumber(t, point, field)
		}
		assert.Equal(t,
			companionJSONNumber(t, point, "matched")+
				companionJSONNumber(t, point, "unmatched")+
				companionJSONNumber(t, point, "upstream_unmatched"),
			companionJSONNumber(t, point, "record_total"),
			"point.record_total 必须等于三项之和")
	}

	companionRequireRFC3339(t, companionJSONString(t, data, "from"), "from")
	companionRequireRFC3339(t, companionJSONString(t, data, "to"), "to")
}

// TestCompanionTimeseriesEmptyPointsIsArray 无数据时 points 必须是 [] 而不是 null。
func TestCompanionTimeseriesEmptyPointsIsArray(t *testing.T) {
	harness := newCompanionHarness()
	harness.ledger.points = nil

	recorder, envelope := harness.do(t, http.MethodGet, "/timeseries", "")
	require.Equal(t, http.StatusOK, recorder.Code)

	assert.Contains(t, string(envelope.Data), `"points":[]`, "points 必须是空数组而不是 null")
	data := companionDecodeObject(t, envelope.Data)
	assert.Empty(t, companionJSONArray(t, data, "points"))
}

// TestCompanionTimeseriesBucketByWindow 分桶粒度标签随窗口长度变化，取值必须落在契约集合内。
func TestCompanionTimeseriesBucketByWindow(t *testing.T) {
	base := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		span   time.Duration
		bucket string
	}{
		{"24h_is_hourly", 24 * time.Hour, "1小时"},
		{"5d_is_6hour", 5 * 24 * time.Hour, "6小时"},
		{"30d_is_daily", 30 * 24 * time.Hour, "1天"},
		{"200d_is_weekly", 200 * 24 * time.Hour, "1周"},
	}

	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			harness := newCompanionHarness()
			query := "?from=" + base.Add(-testCase.span).Format(time.RFC3339) +
				"&to=" + base.Format(time.RFC3339)

			recorder, envelope := harness.do(t, http.MethodGet, "/timeseries"+query, "")
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

			data := companionDecodeObject(t, envelope.Data)
			bucket := companionJSONString(t, data, "bucket")
			assert.Equal(t, testCase.bucket, bucket)
			assert.Contains(t, companionTimeseriesBucketLabels, bucket)
		})
	}
}

// ==================== 5. AccountRules ====================

// TestCompanionAccountRulesExactFieldSet 规则行的键集必须与 CompanionAccountRule 逐字一致。
func TestCompanionAccountRulesExactFieldSet(t *testing.T) {
	harness := newCompanionHarness()

	recorder, envelope := harness.do(t, http.MethodGet, "/account-rules", "")
	require.Equal(t, http.StatusOK, recorder.Code)

	data := companionDecodeObject(t, envelope.Data)
	companionRequireExactKeys(t, data, companionAccountRuleListContractKeys, "CompanionAccountRuleList")

	items := companionJSONArray(t, data, "items")
	require.Len(t, items, 2)
	for index, item := range items {
		row, isObject := item.(map[string]any)
		require.Truef(t, isObject, "items[%d] 必须是对象", index)
		companionRequireExactKeys(t, row, companionAccountRuleContractKeys, "CompanionAccountRule")
	}
}

// TestCompanionAccountRulesGroupsContract 分组视角的键集与语义必须与前端逐字对齐。
//
// 前端 CompanionView.vue 用 groups[].token_keys / groups[].accounts 渲染分组卡片，
// 并把 group_id == 0 当作「不属于任何分组」的桶。少一个键前端不会报错，
// 只会静默渲染成空状态，因此这里连键集带语义一起锁死。
func TestCompanionAccountRulesGroupsContract(t *testing.T) {
	harness := newCompanionHarness()

	recorder, envelope := harness.do(t, http.MethodGet, "/account-rules", "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	data := companionDecodeObject(t, envelope.Data)
	assert.Equal(t, float64(1), companionJSONNumber(t, data, "unconfigured_groups"),
		"未配置分组数按分组计：账号 2 不属于任何分组且没有规则")

	groups := companionJSONArray(t, data, "groups")
	require.Len(t, groups, 2)

	configured, isObject := groups[0].(map[string]any)
	require.True(t, isObject, "groups[0] 必须是对象")
	companionRequireExactKeys(t, configured, companionAccountRuleGroupContractKeys, "CompanionAccountRuleGroup")
	assert.Equal(t, float64(2), companionJSONNumber(t, configured, "group_id"))
	assert.Equal(t, "默认分组", companionJSONString(t, configured, "group_name"))
	assert.Equal(t, float64(1), companionJSONNumber(t, configured, "group_priority"))
	assert.Equal(t, float64(5), companionJSONNumber(t, configured, "usage_count"))
	assert.Equal(t, float64(3), companionJSONNumber(t, configured, "group_channel_count"),
		"分组渠道数是主站口径（挂了多少渠道），不是本表格里的行数")
	assert.True(t, companionJSONBool(t, configured, "configured"))
	assert.Equal(t, []any{"token-a"}, companionJSONArray(t, configured, "token_keys"))

	groupAccounts := companionJSONArray(t, configured, "accounts")
	require.Len(t, groupAccounts, 1)
	firstAccount, isObject := groupAccounts[0].(map[string]any)
	require.True(t, isObject, "groups[0].accounts[0] 必须是对象")
	assert.Equal(t, float64(1), companionJSONNumber(t, firstAccount, "account_id"))
	assert.Equal(t, "token-a", companionJSONString(t, firstAccount, "token_name"),
		"分组内的账号行与 items 形状一致，前端不必维护两套渲染逻辑")

	// 没有归任何分组的账号落在 group_id == 0、group_name 为空串的桶里。
	ungrouped, isObject := groups[1].(map[string]any)
	require.True(t, isObject, "groups[1] 必须是对象")
	assert.Equal(t, float64(0), companionJSONNumber(t, ungrouped, "group_id"))
	assert.Equal(t, "", companionJSONString(t, ungrouped, "group_name"))
	assert.Equal(t, float64(2), companionJSONNumber(t, ungrouped, "usage_count"))
	assert.False(t, companionJSONBool(t, ungrouped, "configured"))
	assert.Empty(t, companionJSONArray(t, ungrouped, "token_keys"), "未配置的账号不贡献令牌标识")
	assert.Len(t, companionJSONArray(t, ungrouped, "accounts"), 1)
}

// TestCompanionAccountRulesZeroAccountsIsEmptyArray 零账号时 items 也必须是空数组。
func TestCompanionAccountRulesZeroAccountsIsEmptyArray(t *testing.T) {
	harness := newCompanionHarness()
	harness.accounts.accounts = nil

	recorder, envelope := harness.do(t, http.MethodGet, "/account-rules", "")
	require.Equal(t, http.StatusOK, recorder.Code)

	assert.Contains(t, string(envelope.Data), `"items":[]`, "零账号时 items 必须是空数组而不是 null")

	data := companionDecodeObject(t, envelope.Data)
	assert.Empty(t, companionJSONArray(t, data, "items"))
	assert.Equal(t, float64(0), companionJSONNumber(t, data, "unconfigured_accounts"))
	companionRequireRFC3339(t, companionJSONString(t, data, "from"), "from")
	companionRequireRFC3339(t, companionJSONString(t, data, "to"), "to")
}

// TestCompanionAccountRulesFieldTypes multiplier/version/usage_count/recent_* 的类型必须正确。
func TestCompanionAccountRulesFieldTypes(t *testing.T) {
	harness := newCompanionHarness()

	_, envelope := harness.do(t, http.MethodGet, "/account-rules", "")
	data := companionDecodeObject(t, envelope.Data)
	items := companionJSONArray(t, data, "items")
	require.Len(t, items, 2)

	// 有调用的账号排在前面（服务端排序契约）：账号 1 有规则且有 5 次调用。
	configured := items[0].(map[string]any)
	assert.Equal(t, float64(1), companionJSONNumber(t, configured, "account_id"))
	assert.Equal(t, "a6", companionJSONString(t, configured, "provider"))
	assert.Equal(t, "token-a", companionJSONString(t, configured, "token_name"))

	// multiplier 是字符串（不是数字）。
	assert.Equal(t, "1.25", companionJSONString(t, configured, "multiplier"))
	_, multiplierIsNumber := configured["multiplier"].(float64)
	assert.False(t, multiplierIsNumber, "multiplier 必须是字符串而不是数字")

	// version 是数字。
	assert.Equal(t, float64(3), companionJSONNumber(t, configured, "version"))
	// usage_count 是数字。
	assert.Equal(t, float64(5), companionJSONNumber(t, configured, "usage_count"))

	// 最近模型是**单个**模型名（全历史最后一次调用），不是窗口内模型名的并集。
	assert.Equal(t, "claude-3-5-sonnet", companionJSONString(t, configured, "recent_model"))
	_, recentModelIsArray := configured["recent_model"].([]any)
	assert.False(t, recentModelIsArray, "recent_model 必须是单个字符串而不是数组")
	// 最近分组：id 是数字、名字是字符串。
	assert.Equal(t, float64(2), companionJSONNumber(t, configured, "recent_group_id"))
	assert.Equal(t, "默认分组", companionJSONString(t, configured, "recent_group_name"))

	// 布尔字段。
	assert.True(t, companionJSONBool(t, configured, "configured"))
	assert.True(t, companionJSONBool(t, configured, "enabled"))
	assert.True(t, companionJSONBool(t, configured, "current"))
	assert.True(t, companionJSONBool(t, configured, "account_schedulable"))

	// 分组与账号展示字段。
	assert.Equal(t, float64(2), companionJSONNumber(t, configured, "group_id"))
	assert.Equal(t, "默认分组", companionJSONString(t, configured, "group_name"))
	companionJSONNumber(t, configured, "group_priority")
	// group_channel_count 是分组的真实渠道数（3），不是本列表里该分组的行数（1）——
	// 行数会被「账号只展示优先级最高的那个分组」压小，数字必须来自真实统计。
	assert.Equal(t, float64(3), companionJSONNumber(t, configured, "group_channel_count"))
	assert.Equal(t, "主账号", companionJSONString(t, configured, "account_name"))
	assert.Equal(t, "anthropic", companionJSONString(t, configured, "account_platform"))
	assert.Equal(t, "active", companionJSONString(t, configured, "account_status"))

	// 时间字段是字符串；有数据时必须能解析。
	companionRequireRFC3339(t, companionJSONString(t, configured, "created_at"), "created_at")
	companionRequireRFC3339(t, companionJSONString(t, configured, "updated_at"), "updated_at")
	companionRequireRFC3339(t, companionJSONString(t, configured, "first_seen"), "first_seen")
	companionRequireRFC3339(t, companionJSONString(t, configured, "last_seen"), "last_seen")
}

// TestCompanionAccountRulesUnconfiguredAccountZeroValues 没有规则的账号：multiplier 为空串、
// version 为 0、时间字段为空串，且仍是合法 JSON 类型。
func TestCompanionAccountRulesUnconfiguredAccountZeroValues(t *testing.T) {
	harness := newCompanionHarness()
	harness.ledger.usage = nil
	harness.ledger.recent = nil

	_, envelope := harness.do(t, http.MethodGet, "/account-rules", "")
	data := companionDecodeObject(t, envelope.Data)
	items := companionJSONArray(t, data, "items")
	require.Len(t, items, 2)

	row := items[1].(map[string]any)
	assert.Equal(t, float64(2), companionJSONNumber(t, row, "account_id"))
	assert.Equal(t, "", companionJSONString(t, row, "provider"))
	assert.Equal(t, "", companionJSONString(t, row, "token_name"))
	assert.Equal(t, "", companionJSONString(t, row, "multiplier"), "未配置规则时 multiplier 必须是空字符串")
	assert.Equal(t, float64(0), companionJSONNumber(t, row, "version"), "无规则时 version 必须是 0")
	assert.Equal(t, false, companionJSONBool(t, row, "configured"))
	assert.Equal(t, false, companionJSONBool(t, row, "enabled"))
	assert.Equal(t, false, companionJSONBool(t, row, "current"), "status 非 active 时 current 必须为 false")
	assert.Equal(t, "", companionJSONString(t, row, "recent_model"))
	assert.Equal(t, float64(0), companionJSONNumber(t, row, "recent_group_id"))
	assert.Equal(t, "", companionJSONString(t, row, "recent_group_name"))
	assert.Equal(t, "", companionJSONString(t, row, "first_seen"), "无数据时 first_seen 必须是空字符串")
	assert.Equal(t, "", companionJSONString(t, row, "last_seen"), "无数据时 last_seen 必须是空字符串")
	assert.Equal(t, "", companionJSONString(t, row, "created_at"))
	assert.Equal(t, "", companionJSONString(t, row, "updated_at"))
	assert.Equal(t, float64(0), companionJSONNumber(t, row, "usage_count"))
	// 不属于任何分组：group_id 为 0，真实渠道数也必须是 0，绝不能回落到全局某个分组的数
	assert.Equal(t, float64(0), companionJSONNumber(t, row, "group_id"))
	assert.Equal(t, float64(0), companionJSONNumber(t, row, "group_channel_count"))
}

// TestCompanionAccountRulesRecentUsageIgnoresWindow 是文档 19 的回归测试。
//
// 「最近模型 / 最近分组」读该账号**全历史最后一次调用**，不受筛选窗口影响；
// 同时「范围内调用」必须继续严格按窗口统计——同一次响应里两种口径并存，
// 先前实现把窗口内的模型名并集塞进 models，等于把「最近模型」降级成「窗口内模型」。
func TestCompanionAccountRulesRecentUsageIgnoresWindow(t *testing.T) {
	harness := newCompanionHarness()
	// 窗口内一条调用都没有，但两个账号全历史都跑过。
	harness.ledger.usage = nil

	_, envelope := harness.do(t, http.MethodGet, "/account-rules?from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z", "")
	data := companionDecodeObject(t, envelope.Data)
	items := companionJSONArray(t, data, "items")
	require.Len(t, items, 2)

	first := items[0].(map[string]any)
	// 最近模型 / 最近分组跨窗口保留。
	assert.Equal(t, "claude-3-5-sonnet", companionJSONString(t, first, "recent_model"))
	assert.Equal(t, float64(2), companionJSONNumber(t, first, "recent_group_id"))
	assert.Equal(t, "默认分组", companionJSONString(t, first, "recent_group_name"))
	// 范围内调用与首末活动时间仍然是窗口口径：窗口内没有调用就必须是 0 / 空。
	assert.Equal(t, float64(0), companionJSONNumber(t, first, "usage_count"),
		"范围内调用必须严格按窗口统计，不能被最近调用信息带偏")
	assert.Equal(t, "", companionJSONString(t, first, "first_seen"))
	assert.Equal(t, "", companionJSONString(t, first, "last_seen"))

	// 最近一次调用所在分组已被删除（分组名为空串）时，模型名与 group_id 仍要保留。
	second := items[1].(map[string]any)
	assert.Equal(t, "gpt-4o", companionJSONString(t, second, "recent_model"))
	assert.Equal(t, float64(99), companionJSONNumber(t, second, "recent_group_id"))
	assert.Equal(t, "", companionJSONString(t, second, "recent_group_name"))
}

// ==================== 6. 未知查询参数（前端自动注入 timezone） ====================

// TestCompanionToleratesFrontendTimezoneParam 前端会在每个 GET 上注入 ?timezone=Asia/Shanghai，
// 后端必须忽略未知参数而不是 400。
func TestCompanionToleratesFrontendTimezoneParam(t *testing.T) {
	harness := newCompanionHarness()

	paths := []string{"/summary", "/timeseries", "/requests", "/account-rules", "/status", "/a6/backfill"}
	for _, path := range paths {
		path := path
		t.Run(strings.TrimPrefix(path, "/"), func(t *testing.T) {
			recorder, _ := harness.do(t, http.MethodGet, path+"?timezone=Asia%2FShanghai", "")
			assert.Equalf(t, http.StatusOK, recorder.Code,
				"%s 带 timezone 参数必须 200，实际 %d：%s", path, recorder.Code, recorder.Body.String())
		})
	}

	// 再叠一个完全未知的参数，仍然必须被容忍。
	recorder, _ := harness.do(t, http.MethodGet, "/summary?timezone=Asia%2FShanghai&unknown_param=1", "")
	assert.Equal(t, http.StatusOK, recorder.Code)
}

// ==================== 7. 参数校验与「绝不返回 401/403」 ====================

// TestCompanionBadRequestValidation 参数类错误必须是 400 + COMPANION_BAD_REQUEST（绝不 401/403）。
func TestCompanionBadRequestValidation(t *testing.T) {
	harness := newCompanionHarness()

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"non_numeric_account_id_on_put", http.MethodPut, "/account-rules/abc", `{"provider":"a6","token_name":"t"}`},
		{"non_numeric_account_id_on_delete", http.MethodDelete, "/account-rules/abc", ""},
		{"zero_account_id", http.MethodPut, "/account-rules/0", `{"provider":"a6","token_name":"t"}`},
		{"invalid_from", http.MethodGet, "/summary?from=not-a-date", ""},
		{"invalid_to", http.MethodGet, "/summary?to=not-a-date", ""},
		{"inverted_window", http.MethodGet, "/summary?from=2026-09-28T00:00:00Z&to=2026-09-01T00:00:00Z", ""},
		{"window_too_long", http.MethodGet, "/summary?from=2020-01-01T00:00:00Z&to=2026-09-01T00:00:00Z", ""},
		{"invalid_from_on_timeseries", http.MethodGet, "/timeseries?from=nope", ""},
		{"invalid_from_on_requests", http.MethodGet, "/requests?from=nope", ""},
		{"invalid_from_on_account_rules", http.MethodGet, "/account-rules?from=nope", ""},
		{"unsupported_provider", http.MethodPut, "/account-rules/1", `{"provider":"subarx","multiplier":1.5}`},
		{"missing_token_name", http.MethodPut, "/account-rules/1", `{"provider":"a6"}`},
		{"invalid_backfill_from", http.MethodPost, "/a6/backfill", `{"from":"nope"}`},
		{"malformed_json_on_upsert", http.MethodPut, "/account-rules/1", `{`},
	}

	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			recorder, envelope := harness.do(t, testCase.method, testCase.path, testCase.body)
			assert.Equalf(t, http.StatusBadRequest, recorder.Code,
				"%s %s 必须返回 400，实际 %d：%s", testCase.method, testCase.path, recorder.Code, recorder.Body.String())
			assert.Containsf(t, envelope.Message, "COMPANION_BAD_REQUEST",
				"错误 message 必须含 COMPANION_BAD_REQUEST 供前端分类，实际 %q", envelope.Message)
			assert.NotEqual(t, http.StatusUnauthorized, recorder.Code)
			assert.NotEqual(t, http.StatusForbidden, recorder.Code)
		})
	}
}

// TestCompanionNeverReturns401Or403 遍历全部 11 个接口：
// 任何响应都不能是 401/403，否则前端会清登录态并跳 /login。
func TestCompanionNeverReturns401Or403(t *testing.T) {
	harness := newCompanionHarness()

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"status", http.MethodGet, "/status", ""},
		{"summary", http.MethodGet, "/summary", ""},
		{"timeseries", http.MethodGet, "/timeseries", ""},
		{"requests", http.MethodGet, "/requests?page=1&page_size=50&status=all", ""},
		{"account_rules", http.MethodGet, "/account-rules", ""},
		{"upsert_account_rule", http.MethodPut, "/account-rules/1", `{"provider":"a6","token_name":"token-a","multiplier":1.25,"enabled":true}`},
		{"delete_account_rule", http.MethodDelete, "/account-rules/1", ""},
		{"collect", http.MethodPost, "/collect", ""},
		{"a6_backfill_status", http.MethodGet, "/a6/backfill", ""},
		{"start_a6_backfill", http.MethodPost, "/a6/backfill", `{"from":"2026-09-01T00:00:00Z","to":"2026-09-02T00:00:00Z"}`},
		{"import_upstream", http.MethodPost, "/upstream/import", `{"records":[{"upstream_request_id":"up-x","cost":0.02,"currency":"USD","fx_rate_to_cny":7.2}]}`},
	}
	require.Len(t, cases, 11, "必须覆盖全部 11 个接口")

	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			recorder, _ := harness.do(t, testCase.method, testCase.path, testCase.body)

			assert.NotEqualf(t, http.StatusUnauthorized, recorder.Code,
				"%s 不能返回 401（前端会清登录态并跳登录页）", testCase.name)
			assert.NotEqualf(t, http.StatusForbidden, recorder.Code,
				"%s 不能返回 403", testCase.name)
			assert.Containsf(t, []int{http.StatusOK, http.StatusAccepted, http.StatusConflict}, recorder.Code,
				"%s 在桩环境下的状态码超出预期：%d %s", testCase.name, recorder.Code, recorder.Body.String())
		})
	}
}

// TestCompanionNeverReturns401Or403WhenDegraded 上游凭据缺失时也不能返回 401/403。
func TestCompanionNeverReturns401Or403WhenDegraded(t *testing.T) {
	harness := newCompanionHarness(companionWithoutBillSource())

	paths := []string{"/status", "/summary", "/timeseries", "/requests", "/account-rules", "/a6/backfill"}
	for _, path := range paths {
		recorder, _ := harness.do(t, http.MethodGet, path, "")
		assert.NotEqualf(t, http.StatusUnauthorized, recorder.Code, "%s 不能返回 401", path)
		assert.NotEqualf(t, http.StatusForbidden, recorder.Code, "%s 不能返回 403", path)
	}
}

// ==================== 8. 同步动作 ====================

// companionRequeueUnmatchedContractKeys 是「退回重试」结果的字段集（5 个）。
var companionRequeueUnmatchedContractKeys = []string{"success", "requeued", "matched", "from", "to"}

// TestCompanionRequeueUnmatchedOnlyRequeuesEligibleBills 是任务 C 的接口级回归测试。
//
// 孤儿账单不会自动重试，管理员改对令牌名之后必须有一条显式的复活通道：
// 这里断言只把「当前规则能解析出账号」的账单退回去，规则对不上的照旧留在孤儿状态，
// 避免注定匹配不上的账单反复占用孤儿宽限期。
func TestCompanionRequeueUnmatchedOnlyRequeuesEligibleBills(t *testing.T) {
	harness := newCompanionHarness()
	// 预置两条孤儿账单：第一条的令牌名与账号 1 的规则（token-a）一致，第二条无人认领。
	harness.bills.unmatched = []service.ReconciliationUpstreamBill{
		{
			ID: 101,
			Payload: service.ReconciliationUpstreamBillPayload{
				TokenName: "token-a",
				Raw:       map[string]any{"token_id": float64(41210), "token_name": "token-a"},
			},
		},
		{
			ID: 102,
			Payload: service.ReconciliationUpstreamBillPayload{
				TokenName: "someone-else-token",
				Raw:       map[string]any{"token_id": float64(999), "token_name": "someone-else-token"},
			},
		},
	}

	recorder, envelope := harness.do(t, http.MethodPost, "/requeue-unmatched", "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	data := companionDecodeObject(t, envelope.Data)
	companionRequireExactKeys(t, data, companionRequeueUnmatchedContractKeys, "CompanionRequeueUnmatchedResult")
	assert.True(t, companionJSONBool(t, data, "success"))
	assert.Equal(t, float64(1), companionJSONNumber(t, data, "requeued"), "只退回规则能解析出账号的那一条")
	assert.Equal(t, float64(0), companionJSONNumber(t, data, "matched"), "本桩的 staging 为空，随后一轮匹配匹配不到东西")
	companionRequireRFC3339(t, companionJSONString(t, data, "from"), "from")
	companionRequireRFC3339(t, companionJSONString(t, data, "to"), "to")

	assert.Equal(t, []int64{101}, harness.bills.requeuedIDs())
}

// 时间窗口非法时按既有约定返回 400（且带 COMPANION_BAD_REQUEST），不能是 401/403。
func TestCompanionRequeueUnmatchedBadWindow(t *testing.T) {
	harness := newCompanionHarness()

	recorder, envelope := harness.do(t, http.MethodPost, "/requeue-unmatched?from=not-a-time", "")
	assert.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.Contains(t, envelope.Message, "COMPANION_BAD_REQUEST")
	assert.Empty(t, harness.bills.requeuedIDs(), "窗口非法时不得退回任何账单")
}

// TestCompanionCollectSuccessContract 采集成功返回 200 + success = true。
func TestCompanionCollectSuccessContract(t *testing.T) {
	harness := newCompanionHarness()

	recorder, envelope := harness.do(t, http.MethodPost, "/collect", "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	data := companionDecodeObject(t, envelope.Data)
	companionRequireExactKeys(t, data, []string{"success"}, "CompanionCollectResult")
	assert.True(t, companionJSONBool(t, data, "success"))
}

// TestCompanionCollectDegradedSyncDoesNotReturn401 同步服务不可用时只能是 409/503 一类，
// 绝不能是 401/403。
func TestCompanionCollectDegradedSyncDoesNotReturn401(t *testing.T) {
	harness := newCompanionHarness(companionWithBrokenStateRepo())

	recorder, envelope := harness.do(t, http.MethodPost, "/collect", "")
	assert.NotEqual(t, http.StatusUnauthorized, recorder.Code)
	assert.NotEqual(t, http.StatusForbidden, recorder.Code)
	assert.Containsf(t, []int{http.StatusOK, http.StatusConflict, http.StatusServiceUnavailable}, recorder.Code,
		"降级时状态码超出预期：%d", recorder.Code)

	if recorder.Code == http.StatusConflict {
		assert.Contains(t, envelope.Message, "COMPANION_BAD_REQUEST")
	}
}

// TestCompanionA6BackfillStatusContract 回填进度对象的键集与类型。
func TestCompanionA6BackfillStatusContract(t *testing.T) {
	harness := newCompanionHarness()

	recorder, envelope := harness.do(t, http.MethodGet, "/a6/backfill", "")
	require.Equal(t, http.StatusOK, recorder.Code)

	data := companionDecodeObject(t, envelope.Data)
	companionRequireExactKeys(t, data, companionBackfillStatusContractKeys, "CompanionA6BackfillStatus")

	assert.Equal(t, "", companionJSONString(t, data, "status"), "从未回填时 status 是空字符串")
	assert.False(t, companionJSONBool(t, data, "running"))
	assert.Equal(t, float64(0), companionJSONNumber(t, data, "processed"))
	assert.Equal(t, "", companionJSONString(t, data, "error"))
	assert.Equal(t, "", companionJSONString(t, data, "from"))
	assert.Equal(t, "", companionJSONString(t, data, "to"))
	assert.Equal(t, "", companionJSONString(t, data, "cursor"))
}

// TestCompanionStartA6BackfillAccepted 启动回填必须返回 202 Accepted。
func TestCompanionStartA6BackfillAccepted(t *testing.T) {
	harness := newCompanionHarness()

	recorder, envelope := harness.do(t, http.MethodPost, "/a6/backfill",
		`{"from":"2026-09-01T00:00:00Z","to":"2026-09-02T00:00:00Z"}`)
	require.Equal(t, http.StatusAccepted, recorder.Code, "必须用 response.Accepted（202）：%s", recorder.Body.String())

	data := companionDecodeObject(t, envelope.Data)
	companionRequireExactKeys(t, data, []string{"success", "from", "to"}, "CompanionA6BackfillStarted")
	assert.True(t, companionJSONBool(t, data, "success"))
	assert.Equal(t, "2026-09-01T00:00:00Z", companionRequireRFC3339(t, companionJSONString(t, data, "from"), "from").UTC().Format(time.RFC3339))
	assert.Equal(t, "2026-09-02T00:00:00Z", companionRequireRFC3339(t, companionJSONString(t, data, "to"), "to").UTC().Format(time.RFC3339))

	// 空请求体同样合法：窗口缺省。
	emptyRecorder, emptyEnvelope := harness.do(t, http.MethodPost, "/a6/backfill", "")
	require.Equal(t, http.StatusAccepted, emptyRecorder.Code)
	emptyData := companionDecodeObject(t, emptyEnvelope.Data)
	companionRequireRFC3339(t, companionJSONString(t, emptyData, "from"), "from")
	companionRequireRFC3339(t, companionJSONString(t, emptyData, "to"), "to")
}

// TestCompanionStartA6BackfillWithoutBillSource 未配置上游凭据时是 503 COMPANION_NOT_CONFIGURED，
// 依旧不是 401/403。
func TestCompanionStartA6BackfillWithoutBillSource(t *testing.T) {
	harness := newCompanionHarness(companionWithoutBillSource())

	recorder, envelope := harness.do(t, http.MethodPost, "/a6/backfill", "")
	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	assert.Contains(t, envelope.Message, "COMPANION_NOT_CONFIGURED")
	assert.NotEqual(t, http.StatusUnauthorized, recorder.Code)
	assert.NotEqual(t, http.StatusForbidden, recorder.Code)
}

// ==================== 9. 账号规则写接口 ====================

// TestCompanionUpsertAccountRuleResponseContract 保存规则的响应结构与 rule 字段集。
func TestCompanionUpsertAccountRuleResponseContract(t *testing.T) {
	harness := newCompanionHarness()

	// 故意省略 enabled：契约规定缺省按启用处理。
	recorder, envelope := harness.do(t, http.MethodPut, "/account-rules/42",
		`{"provider":"a6","token_name":"token-42","multiplier":1.25}`)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	data := companionDecodeObject(t, envelope.Data)
	companionRequireExactKeys(t, data, []string{"success", "rule"}, "CompanionAccountRuleSaved")
	assert.True(t, companionJSONBool(t, data, "success"))

	rule, isObject := data["rule"].(map[string]any)
	require.True(t, isObject, "rule 必须是对象")
	companionRequireExactKeys(t, rule, companionAccountRuleContractKeys, "CompanionAccountRule")

	assert.Equal(t, float64(42), companionJSONNumber(t, rule, "account_id"))
	assert.Equal(t, "a6", companionJSONString(t, rule, "provider"))
	assert.Equal(t, "token-42", companionJSONString(t, rule, "token_name"))
	assert.Equal(t, "1.25", companionJSONString(t, rule, "multiplier"))
	assert.Equal(t, float64(1), companionJSONNumber(t, rule, "version"))
	assert.True(t, companionJSONBool(t, rule, "enabled"), "省略 enabled 时必须按启用处理")
	assert.True(t, companionJSONBool(t, rule, "configured"))
	assert.False(t, companionJSONBool(t, rule, "current"))
	companionJSONBool(t, rule, "account_schedulable")
	assert.Equal(t, float64(0), companionJSONNumber(t, rule, "usage_count"))
	assert.Equal(t, "", companionJSONString(t, rule, "recent_model"))
	assert.Equal(t, float64(0), companionJSONNumber(t, rule, "recent_group_id"))
	assert.Equal(t, "", companionJSONString(t, rule, "recent_group_name"))
}

// TestCompanionDeleteAccountRuleResponseContract 删除规则的响应结构。
func TestCompanionDeleteAccountRuleResponseContract(t *testing.T) {
	harness := newCompanionHarness()

	recorder, envelope := harness.do(t, http.MethodDelete, "/account-rules/42", "")
	require.Equal(t, http.StatusOK, recorder.Code)

	data := companionDecodeObject(t, envelope.Data)
	companionRequireExactKeys(t, data, []string{"success", "deleted"}, "CompanionAccountRuleDeleted")
	assert.True(t, companionJSONBool(t, data, "success"))
	assert.Equal(t, float64(1), companionJSONNumber(t, data, "deleted"))
}

// ==================== 10. 上游账单导入的两种请求体写法 ====================

// TestCompanionImportUpstreamBothBodyForms 必须同时接受 {"records":[...]} 与顶层数组 [...]。
func TestCompanionImportUpstreamBothBodyForms(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "wrapped_in_records_object",
			body: `{"records":[{"upstream_request_id":"up-1","cost":0.02,"currency":"USD","fx_rate_to_cny":7.2},` +
				`{"upstream_request_id":"up-2","cost":0.03,"currency":"USD","fx_rate_to_cny":7.2}]}`,
		},
		{
			name: "top_level_array",
			body: `[{"upstream_request_id":"up-1","cost":0.02,"currency":"USD","fx_rate_to_cny":7.2},` +
				`{"upstream_request_id":"up-2","cost":0.03,"currency":"USD","fx_rate_to_cny":7.2}]`,
		},
	}

	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			harness := newCompanionHarness()

			recorder, envelope := harness.do(t, http.MethodPost, "/upstream/import", testCase.body)
			require.Equalf(t, http.StatusOK, recorder.Code, "导入失败：%s", recorder.Body.String())

			data := companionDecodeObject(t, envelope.Data)
			companionRequireExactKeys(t, data, []string{"success", "imported"}, "CompanionUpstreamImportResult")
			assert.True(t, companionJSONBool(t, data, "success"))

			value, ok := data["imported"]
			require.True(t, ok, "响应必须含 imported 字段")
			_, isString := value.(string)
			assert.False(t, isString, "imported 不能是字符串")
			assert.Equal(t, float64(2), companionJSONNumber(t, data, "imported"))

			assert.Equal(t, 2, harness.bills.importedCount(), "两条记录都必须落到账单仓库")
		})
	}
}

// TestCompanionImportUpstreamRejectsBadBodies 非法请求体是 400 + COMPANION_BAD_REQUEST。
func TestCompanionImportUpstreamRejectsBadBodies(t *testing.T) {
	harness := newCompanionHarness()

	cases := []struct {
		name string
		body string
	}{
		{"malformed_json", `{`},
		{"bad_occurred_at", `[{"upstream_request_id":"up-1","cost":0.02,"occurred_at":"nope"}]`},
		{"records_is_not_array", `{"records":{"upstream_request_id":"up-1"}}`},
	}

	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			recorder, envelope := harness.do(t, http.MethodPost, "/upstream/import", testCase.body)
			assert.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
			assert.Contains(t, envelope.Message, "COMPANION_BAD_REQUEST")
		})
	}

	// 空请求体是合法的：按零条记录处理，返回 0。
	recorder, envelope := harness.do(t, http.MethodPost, "/upstream/import", "")
	require.Equal(t, http.StatusOK, recorder.Code)
	data := companionDecodeObject(t, envelope.Data)
	assert.Equal(t, float64(0), companionJSONNumber(t, data, "imported"))
}
