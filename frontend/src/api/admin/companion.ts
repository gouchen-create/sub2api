/**
 * Admin Companion API endpoints
 *
 * Companion（sub2api-companion）是独立旁路服务，负责上游账单归集与经营对账。
 * 主服务在管理员鉴权组下提供 /admin/companion/* 代理接口，服务端持有 Companion 的
 * HTTP Basic 凭据并附加到上游请求上，浏览器侧永远拿不到该凭据。
 *
 * 因此本模块只与主服务通信，不直接访问 Companion。
 */

import { apiClient } from '../client'

// ==================== 状态 ====================

/** GET /admin/companion/status 的响应（代理自身配置状态 + Companion /health 探测结果） */
export interface CompanionStatus {
  /** 主服务是否配置了 COMPANION_BASE_URL */
  enabled: boolean
  /** Companion /health 是否可用（未配置时恒为 false） */
  healthy: boolean
  /** Companion /health 的 HTTP 状态码，仅在探测成功时返回 */
  status?: number
  /** 探测失败原因（网络错误或上游错误体摘要） */
  detail?: string
}

// ==================== 时间窗口 ====================

/**
 * 所有业务接口共用的时间窗口参数。
 * Companion 侧为 [from, to) 半开区间，缺省值为「当前时间往前 24 小时」。
 */
export interface CompanionTimeWindowParams {
  /** RFC3339 / ISO 字符串 */
  from?: string
  /** RFC3339 / ISO 字符串 */
  to?: string
}

// ==================== 汇总 ====================

/** GET /admin/companion/summary 的响应（透传 Companion /ops/api/summary） */
export interface CompanionSummary {
  from: string
  to: string
  /** 下游收入（全部调用，CNY，8 位小数字符串） */
  revenue: string
  /** 已对账下游收入（CNY） */
  matched_revenue: string
  /** 上游实扣（CNY，等同 billed_upstream_cost） */
  upstream_cost: string
  /** 上游实扣（CNY，兼容字段） */
  billed_upstream_cost: string
  /** 已对账毛利（CNY） */
  gross_profit: string
  /** 已对账毛利率（百分数，2 位小数字符串） */
  margin_percent: string
  /** 已对账调用数 */
  matched: number
  /** 待对账调用数 */
  unmatched: number
  /** 下游已对账调用数（等同 matched） */
  downstream_matched: number
  /** 下游待对账调用数（等同 unmatched） */
  downstream_unmatched: number
  /** 上游账单未匹配到下游调用的记录数 */
  upstream_unmatched: number
  /** 记录总数 = matched + unmatched + upstream_unmatched */
  record_total: number
  /** 已取得上游账单笔数 */
  billed_count: number
  /** 成本口径说明，固定为 billed_or_subarx_api_or_rule */
  cost_policy: string
  /** 按规则回算的调用数 */
  calculated_count: number
  /** Subarx 真实账单中未归属到本站调用的金额（CNY） */
  subarx_unallocated_cost: string
  /** 上述未归属记录数 */
  subarx_unallocated_count: number
  /** 毛利口径说明，固定为 matched_only */
  profit_scope: string
  /** 结算币种，固定为 CNY */
  currency: string
  /** A6 美元成本折算使用的 USD/CNY 汇率 */
  fx_usd_cny: string
  /** 汇率来源 */
  fx_source: string
  /** 汇率生效时间（可能为空字符串） */
  fx_effective_at: string
  /** 汇率是否为陈旧兜底值 */
  fx_stale: boolean
}

// ==================== 趋势 ====================

/** 趋势分桶的单个数据点 */
export interface CompanionTimeSeriesPoint {
  /** 分桶起始时间（RFC3339Nano，UTC） */
  start: string
  /** 该桶下游收入（CNY） */
  revenue: string
  /** 该桶上游实扣（CNY） */
  upstream_cost: string
  /** 该桶已对账毛利（CNY） */
  gross_profit: string
  /** 该桶已对账调用数 */
  matched: number
  /** 该桶待对账调用数 */
  unmatched: number
  /** 该桶上游待匹配记录数 */
  upstream_unmatched: number
  /** 该桶记录总数 */
  record_total: number
}

/** GET /admin/companion/timeseries 的响应 */
export interface CompanionTimeSeries {
  from: string
  to: string
  /** 分桶粒度标签，由窗口长度决定：1小时 / 6小时 / 1天 / 1周 */
  bucket: string
  points: CompanionTimeSeriesPoint[]
}

// ==================== 明细 ====================

/** 明细状态筛选值（Companion 仅接受这四个值） */
export type CompanionRequestStatus = 'all' | 'matched' | 'unmatched' | 'upstream_unmatched'

/**
 * 成本来源。前端据此展示状态徽标；未知值按原样展示上游给的 cost_source_label。
 */
export type CompanionCostSource =
  | 'pending'
  | 'billed'
  | 'subarx_billed_allocation'
  | 'subarx_pending'
  | 'subarx_waiting'
  | 'subarx_rule'
  | 'rule_unconfigured'
  | 'a6_waiting'
  | 'a6_pending'
  | 'upstream_unmatched'
  | 'subarx_unallocated'

/** 明细行（Companion /ops/api/requests 的 items 元素） */
export interface CompanionRequestRow {
  /** downstream 或 upstream_unmatched */
  record_type: string
  /** 下游调用为 usage_snapshots.source_id；上游待匹配记录为 0 */
  source_id: number
  created_at: string
  /** 下游 Client Request ID */
  request_id: string
  /** 上游请求 ID（A6 映射或上游账单） */
  upstream_request_id: string
  user_id: number
  user_email: string
  api_key_id: number
  account_id: number
  group_id: number
  group_name: string
  model: string
  input_tokens: number
  output_tokens: number
  /** 缓存读取 + 缓存写入 Token 合计 */
  cache_tokens: number
  /** 下游收入（CNY，8 位小数字符串） */
  revenue: string
  /** 当前采用的上游成本（CNY） */
  upstream_cost: string
  /** 上游实扣（CNY） */
  billed_upstream_cost: string
  /** 上游原币金额 */
  upstream_cost_original: string
  /** 上游原币币种 */
  upstream_currency: string
  /** 毛利（CNY）；未对账时为空字符串 */
  gross_profit: string
  cost_source: CompanionCostSource | string
  /** 上游原币折算 CNY 使用的汇率 */
  fx_rate_to_cny: string
  /** 成本来源的中文标签（上游直接给出） */
  cost_source_label: string
  /** 是否已确定上游成本 */
  matched: boolean
}

/** GET /admin/companion/requests 的响应 */
export interface CompanionRequestPage {
  items: CompanionRequestRow[]
  page: number
  page_size: number
  total: number
  total_pages: number
  from: string
  to: string
  status: string
}

/** GET /admin/companion/requests 的查询参数（查询串原样透传给 Companion） */
export interface CompanionRequestQueryParams extends CompanionTimeWindowParams {
  status?: CompanionRequestStatus
  page?: number
  /** Companion 侧上限 100，默认 50 */
  page_size?: number
}

// ==================== 上游账号规则 ====================

/** 账号规则视图（Companion /ops/api/account-rules 的 items 元素） */
export interface CompanionAccountRule {
  account_id: number
  /** a6 / subarx；未配置时为空字符串 */
  provider: string
  /** A6 令牌名（provider 为 a6 时有值） */
  token_name: string
  /** Subarx 倍率（provider 为 subarx 时有值） */
  multiplier: string
  /** 规则版本号；0 表示还没有规则 */
  version: number
  enabled: boolean
  created_at: string
  updated_at: string
  /** 是否已配置且启用 */
  configured: boolean
  /** 是否属于主站当前分组渠道 */
  current: boolean
  group_id: number
  group_name: string
  group_priority: number
  account_name: string
  account_platform: string
  account_status: string
  account_schedulable: boolean
  /** 范围内的调用数 */
  usage_count: number
  first_seen: string
  last_seen: string
  /** 该账号最近使用的模型 */
  models: string
}

/** GET /admin/companion/account-rules 的响应 */
export interface CompanionAccountRuleList extends CompanionTimeWindowParams {
  items: CompanionAccountRule[]
  /** 范围内有调用但尚未配置规则的账号数 */
  unconfigured_accounts: number
  from: string
  to: string
}

/**
 * 保存账号规则的请求体。
 * provider 为 a6 时 token_name 必填；provider 为 subarx 时 multiplier 必须是正数小数。
 */
export interface CompanionAccountRuleInput {
  provider: 'a6' | 'subarx'
  /** A6 令牌名 */
  token_name?: string
  /** Subarx 倍率 */
  multiplier?: string
  /** 省略时上游按启用处理 */
  enabled?: boolean
}

/** PUT /admin/companion/account-rules/{account_id} 的响应 */
export interface CompanionAccountRuleSaved {
  success: boolean
  rule: CompanionAccountRule
}

/** DELETE /admin/companion/account-rules/{account_id} 的响应 */
export interface CompanionAccountRuleDeleted {
  success: boolean
  /** 实际删除的行数，账号本来没有规则时为 0 */
  deleted: number
}

// ==================== 同步与回填 ====================

/** POST /admin/companion/collect 的响应 */
export interface CompanionCollectResult {
  success: boolean
}

/** GET /admin/companion/a6/backfill 的响应 */
export interface CompanionA6BackfillStatus {
  /** 空、running、done、failed */
  status: string
  running: boolean
  from: string
  to: string
  cursor: string
  processed: number
  error: string
}

/** POST /admin/companion/a6/backfill 的请求体；省略 from 时上游从最早 A6 调用开始 */
export interface CompanionA6BackfillRequest {
  from?: string
  to?: string
}

/** POST /admin/companion/a6/backfill 的响应（202 Accepted） */
export interface CompanionA6BackfillStarted {
  success: boolean
  from: string
  to: string
}

/**
 * 手动导入的上游账单记录（Companion POST /ops/api/upstream/import 的元素）。
 * 非 CNY 成本必须提供 fx_rate_to_cny；upstream_request_id 必填。
 */
export interface CompanionUpstreamRecord {
  /** 省略时上游按 a6 处理 */
  provider?: string
  upstream_request_id: string
  cost: string
  /** 省略时上游按 USD 处理 */
  currency?: string
  fx_rate_to_cny?: string
  /** RFC3339；省略时上游取当前时间 */
  occurred_at?: string
  model?: string
  input_tokens?: number
  output_tokens?: number
  /** 省略时上游记为 manual_import */
  source?: string
}

/** POST /admin/companion/upstream/import 的响应 */
export interface CompanionUpstreamImportResult {
  success: boolean
  imported: number
}

// ==================== 错误语义 ====================

/**
 * 代理层的错误分类。markers 由主服务在后端硬编码，前端只做字符串识别，
 * 不参与任何鉴权判断。
 */
export type CompanionErrorKind =
  | 'not_configured'
  | 'unreachable'
  | 'auth_failed'
  | 'bad_request'
  | 'upstream'
  | 'invalid_response'
  | 'unknown'

export interface CompanionErrorInfo {
  kind: CompanionErrorKind
  /** HTTP 状态码，0 表示网络层失败 */
  status: number
  /** 服务端返回的原文，便于排查 */
  message: string
}

/**
 * 把 apiClient 抛出的错误归类。
 *
 * - 503 + COMPANION_NOT_CONFIGURED：后端没有配置 COMPANION_BASE_URL，需要设置服务端环境变量；
 * - 502 + COMPANION_UNREACHABLE：Companion 不可达；
 * - 502 + COMPANION_AUTH_FAILED：服务端 Basic 凭据与 Companion 不一致；
 * - 400 + COMPANION_BAD_REQUEST：请求参数被 Companion 拒绝；
 * - 其他：按上游错误或未知错误处理。
 */
export function classifyCompanionError(error: unknown): CompanionErrorInfo {
  const source = (error ?? {}) as { status?: unknown; message?: unknown }
  const status = typeof source.status === 'number' ? source.status : 0
  const message = typeof source.message === 'string' && source.message.trim() !== '' ? source.message : ''

  if (message.includes('COMPANION_NOT_CONFIGURED')) {
    return { kind: 'not_configured', status, message }
  }
  if (message.includes('COMPANION_UNREACHABLE')) {
    return { kind: 'unreachable', status, message }
  }
  if (message.includes('COMPANION_AUTH_FAILED')) {
    return { kind: 'auth_failed', status, message }
  }
  if (message.includes('COMPANION_INVALID_RESPONSE')) {
    return { kind: 'invalid_response', status, message }
  }
  if (message.includes('COMPANION_BAD_REQUEST')) {
    return { kind: 'bad_request', status, message }
  }
  if (message.includes('COMPANION_UPSTREAM_')) {
    return { kind: 'upstream', status, message }
  }
  return { kind: 'unknown', status, message }
}

// ==================== 接口 ====================

/**
 * 读取代理配置状态与 Companion 健康检查结果。
 * 未配置上游地址时不会抛错，而是返回 { enabled: false, healthy: false }。
 */
export async function getStatus(): Promise<CompanionStatus> {
  const { data } = await apiClient.get<CompanionStatus>('/admin/companion/status')
  return data
}

/** 经营汇总（时间窗口查询串原样透传给 Companion） */
export async function getSummary(params?: CompanionTimeWindowParams): Promise<CompanionSummary> {
  const { data } = await apiClient.get<CompanionSummary>('/admin/companion/summary', { params })
  return data
}

/** 经营趋势分桶 */
export async function getTimeseries(params?: CompanionTimeWindowParams): Promise<CompanionTimeSeries> {
  const { data } = await apiClient.get<CompanionTimeSeries>('/admin/companion/timeseries', { params })
  return data
}

/** 调用明细分页 */
export async function getRequests(params?: CompanionRequestQueryParams): Promise<CompanionRequestPage> {
  const { data } = await apiClient.get<CompanionRequestPage>('/admin/companion/requests', { params })
  return data
}

/** 上游账号规则视图 */
export async function getAccountRules(
  params?: CompanionTimeWindowParams
): Promise<CompanionAccountRuleList> {
  const { data } = await apiClient.get<CompanionAccountRuleList>('/admin/companion/account-rules', {
    params
  })
  return data
}

/** 新增或更新指定账号的上游计费规则 */
export async function upsertAccountRule(
  accountId: number,
  input: CompanionAccountRuleInput
): Promise<CompanionAccountRuleSaved> {
  const { data } = await apiClient.put<CompanionAccountRuleSaved>(
    `/admin/companion/account-rules/${accountId}`,
    input
  )
  return data
}

/** 删除指定账号的上游计费规则（不影响历史已对账快照） */
export async function deleteAccountRule(accountId: number): Promise<CompanionAccountRuleDeleted> {
  const { data } = await apiClient.delete<CompanionAccountRuleDeleted>(
    `/admin/companion/account-rules/${accountId}`
  )
  return data
}

/** 「立即同步」：强制 Companion 执行一次完整采集与对账 */
export async function collect(): Promise<CompanionCollectResult> {
  const { data } = await apiClient.post<CompanionCollectResult>('/admin/companion/collect')
  return data
}

/** 读取 A6 历史账单回填进度 */
export async function getA6BackfillStatus(): Promise<CompanionA6BackfillStatus> {
  const { data } = await apiClient.get<CompanionA6BackfillStatus>('/admin/companion/a6/backfill')
  return data
}

/** 启动 A6 历史账单回填 */
export async function startA6Backfill(
  payload: CompanionA6BackfillRequest = {}
): Promise<CompanionA6BackfillStarted> {
  const { data } = await apiClient.post<CompanionA6BackfillStarted>(
    '/admin/companion/a6/backfill',
    payload
  )
  return data
}

/**
 * 手动导入上游逐笔账单。
 * 说明：当前管理后台页面只做只读对账与规则维护，未提供该导入入口，
 * 此函数保留完整契约供后续页面或脚本复用。
 */
export async function importUpstream(
  records: CompanionUpstreamRecord[]
): Promise<CompanionUpstreamImportResult> {
  const { data } = await apiClient.post<CompanionUpstreamImportResult>(
    '/admin/companion/upstream/import',
    records
  )
  return data
}

export const companionAPI = {
  getStatus,
  getSummary,
  getTimeseries,
  getRequests,
  getAccountRules,
  upsertAccountRule,
  deleteAccountRule,
  collect,
  getA6BackfillStatus,
  startA6Backfill,
  importUpstream
}

export default companionAPI
