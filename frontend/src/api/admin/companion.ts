/**
 * Admin Companion API endpoints
 *
 * 经营对账已并入主服务进程（不再有独立的 sub2api-companion 容器）：主服务在管理员
 * 鉴权组下提供 /admin/companion/* 接口，内部直接访问上游 A6 与本库数据。
 *
 * 上游 A6 的接入参数（站点基址、用户标识、访问令牌、汇率）可在管理后台页面上直接
 * 配置，见 /admin/companion/settings。访问令牌只以密文保存在服务端，读取接口仅返回
 * 脱敏串，明文既不会回传也不会出现在 URL 里。
 */

import { apiClient } from '../client'

// ==================== 状态 ====================

/** GET /admin/companion/status 的响应（上游可达性探测结果） */
export interface CompanionStatus {
  /** 主服务是否具备可用的上游 A6 配置（页面配置或环境变量任一来源） */
  enabled: boolean
  /** 上游 A6 是否可用（未配置访问令牌时恒为 false） */
  healthy: boolean
  /** 上游探测的 HTTP 状态码，仅在探测成功时返回 */
  status?: number
  /** 探测失败原因（网络错误或上游错误体摘要） */
  detail?: string
  /**
   * 最近一次上游采集失败的真实发生时间（毫秒精度的 RFC3339），没有失败记录时缺省。
   *
   * 与页面上「更新于」（页面刷新时刻）不是一回事：排查故障要看的是这个时间。
   */
  last_error_at?: string
  /**
   * 下游用量采集游标的当前位置（毫秒精度的 RFC3339）。
   *
   * 每次采集都会推进（无新调用时推进到 now-1min），所以它同时是「采集器还活着吗」
   * 的判据：长时间不动说明采集协程卡住或被 leader 锁挡住。
   */
  usage_cursor_at?: string
  /** 本轮采集实际取到的行数上限（配置的 CollectBatchSize） */
  usage_last_batch_size?: number
  /**
   * 本轮是否被单轮上限截断。
   *
   * true 表示「本轮取满了上限、游标只推进到本批最后一行，剩下的要等下一轮」——
   * 这时看板上的收入会偏低，不是数据错，而是还没采完。
   */
  usage_batch_truncated?: boolean
  /**
   * 待采集行数。被截断时由采集器顺手数出来，探针上限 10 万：
   * 等于 100000 表示「至少还有这么多」，不是精确值。未被截断时为 0。
   */
  usage_backlog?: number
}

// ==================== 上游 A6 配置 ====================

/**
 * GET /admin/companion/settings 的响应，也是 PUT 的响应。
 *
 * 后端把「页面配置」以覆盖项的形式叠加在环境变量之上：override_keys 列出当前由页面
 * 配置生效的字段名，未列出的字段仍取环境变量或内置默认值。
 */
export interface CompanionSettings {
  /** 上游 A6 站点基址，例如 https://a6.example.com */
  a6_base_url: string
  /** 上游 A6 用户标识 */
  a6_user_id: string
  /** 是否已经配置访问令牌（页面配置或环境变量任一来源） */
  a6_token_configured: boolean
  /** 访问令牌的脱敏串，仅供展示；未配置时为空串 */
  a6_token_mask: string
  /** A6 美元成本折算 CNY 使用的汇率 */
  fx_usd_cny_rate: number
  /** 当前由页面配置覆盖的字段名列表 */
  override_keys: string[]
}

/**
 * PUT /admin/companion/settings 的请求体。
 *
 * 字段缺省或空串表示不改动该字段；clear_a6_access_token 为 true 时清掉页面配置的
 * 访问令牌，回落到环境变量。
 */
export interface CompanionSettingsInput {
  a6_base_url?: string
  a6_user_id?: string
  a6_access_token?: string
  fx_usd_cny_rate?: number
  clear_a6_access_token?: boolean
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
  /**
   * 上游类型。当前只有 a6 会被后端接受；历史数据里可能残留 subarx
   * （Subarx 已于 v0.2.9 下线），所以读取侧保留宽松的 string 而不是收窄成 'a6'。
   */
  provider: string
  /** A6 令牌名（provider 为 a6 时有值） */
  token_name: string
  /**
   * 历史遗留字段：Subarx 倍率。
   *
   * Subarx 下线后它恒为空串，但后端契约仍在返回它（文档 5.4 明确要求保留这些
   * 字段以免破坏前端契约），所以类型上继续保留并标注语义。
   */
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
  /**
   * 该分组的真实渠道数（与主站分组页 account_count 同口径，只算未软删账号）。
   *
   * 不要用「本列表里该分组的行数」代替它：一行是一个账号，而账号可以同时属于多个
   * 分组，这里只展示它优先级最高的那一个，所以行数会少算。
   */
  group_channel_count: number
  account_name: string
  account_platform: string
  account_status: string
  account_schedulable: boolean
  /** **范围内**的调用数（受时间筛选影响） */
  usage_count: number
  /** **范围内**首次调用时间（受时间筛选影响） */
  first_seen: string
  /** **范围内**最后调用时间（受时间筛选影响） */
  last_seen: string
  /**
   * 该账号**全历史最后一次调用**用到的模型，不受时间筛选影响。
   *
   * 与 usage_count / first_seen / last_seen 的口径刻意不同（文档 19）：
   * 管理员给账号登记上游令牌名时最需要的就是这条线索，窗口不该把它抹掉。
   */
  recent_model: string
  /** 全历史最后一次调用所在的分组 ID；0 表示该账号没有任何调用记录 */
  recent_group_id: number
  /** 全历史最后一次调用所在的分组名；分组已删除时为空字符串，此时用 recent_group_id 展示 */
  recent_group_name: string
}

/**
 * 分组视图（Companion /ops/api/account-rules 的 groups 元素）：一个分组一行。
 *
 * 业务单位是分组，而一个分组下可能有多个渠道账号，每个账号对应的上游令牌名又不一样，
 * 所以规则仍然按账号保存，逐个放在 accounts 里（元素结构与 items 元素完全相同）。
 */
export interface CompanionAccountRuleGroup {
  /** 分组 ID；0 表示组内账号不属于任何分组 */
  group_id: number
  group_name: string
  group_priority: number
  /** 该分组的真实渠道数（与主站分组页 account_count 同口径，只算未软删账号） */
  group_channel_count: number
  /** 组内全部账号都已配置才为 true */
  configured: boolean
  /** 组内账号在**范围内**的用量合计 */
  usage_count: number
  /** 组内已配置的令牌标识（去重） */
  token_keys: string[]
  /** 组内渠道账号 */
  accounts: CompanionAccountRule[]
}

/** GET /admin/companion/account-rules 的响应 */
export interface CompanionAccountRuleList extends CompanionTimeWindowParams {
  /**
   * 按账号的扁平列表。
   *
   * 表格渲染已改用 groups；这个字段保留不变，仍有调用方依赖它的扁平结构。
   */
  items: CompanionAccountRule[]
  /** 范围内有调用但尚未配置规则的账号数 */
  unconfigured_accounts: number
  /** 范围内有调用但尚未配置规则的分组数（不属于任何分组的账号算一个分组） */
  unconfigured_groups: number
  /** 按分组组织的规则视图 */
  groups: CompanionAccountRuleGroup[]
  from: string
  to: string
}

/**
 * 保存账号规则的请求体。
 *
 * 上游类型只有 a6：Subarx 已于 v0.2.9 下线，提交 provider: 'subarx' 会被后端
 * 直接拒绝（400 COMPANION_BAD_REQUEST），所以类型上不再开放这个取值。
 * 省略 provider 时后端按 a6 处理。
 */
export interface CompanionAccountRuleInput {
  provider?: 'a6'
  /** A6 令牌名 */
  token_name: string
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

/**
 * POST /admin/companion/requeue-unmatched 的响应。
 *
 * requeued 是本次退回「待匹配」的孤儿账单条数；matched 是退回之后紧接着那一轮
 * 实际匹配上的条数——管理员点这个动作就是为了看「改对的规则到底有没有生效」，
 * 只看 requeued 是看不出结果的，两个数字都必须展示。
 */
export interface CompanionRequeueUnmatchedResult {
  success: boolean
  requeued: number
  matched: number
  from: string
  to: string
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
 * - 503 + COMPANION_NOT_CONFIGURED：后端没有任何可用的上游配置；
 * - 502 + COMPANION_UNREACHABLE：上游 A6 不可达；
 * - 502 + COMPANION_AUTH_FAILED：访问令牌或用户标识与上游不一致；
 * - 400 + COMPANION_BAD_REQUEST：请求参数被上游或代理拒绝；
 * - 500 + COMPANION_INTERNAL：服务端内部错误，无专用分类，按未知错误展示原始 message；
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

/**
 * 读取上游 A6 配置。
 * 访问令牌只返回脱敏串（a6_token_mask），明文不会回传。
 */
export async function getSettings(): Promise<CompanionSettings> {
  const { data } = await apiClient.get<CompanionSettings>('/admin/companion/settings')
  return data
}

/**
 * 保存上游 A6 配置，返回保存后的完整配置。
 *
 * 只把需要改动的字段放进 payload：缺省或空串 = 不改动该字段。
 * 令牌明文只出现在请求体里，绝不放进 URL 或查询串。
 */
export async function updateSettings(input: CompanionSettingsInput): Promise<CompanionSettings> {
  const { data } = await apiClient.put<CompanionSettings>('/admin/companion/settings', input)
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

/**
 * 「退回重试未匹配账单」：把当前窗口内、当前启用规则能解析出账号的孤儿账单退回
 * 待匹配队列，并立刻重试一轮匹配（动作幂等）。
 *
 * 这是写操作，会改数据库里的账单状态，必须由管理员显式触发（孤儿不会自动重试）。
 * 单次最多退回 1000 条（等于后端单轮匹配上限），孤儿多时要连续点几次；被退回的
 * 账单会重新获得一段孤儿宽限期，所以要在宽限期内连续点，间隔过久先前那批会重新变回孤儿。
 *
 * 窗口由查询参数传入，与同页 summary / requests / account-rules 共用同一套解析，
 * 因此必须传页面当前窗口，不要另造窗口。请求体为空（后端显式忽略 body）。
 */
export async function requeueUnmatched(
  params?: CompanionTimeWindowParams
): Promise<CompanionRequeueUnmatchedResult> {
  const { data } = await apiClient.post<CompanionRequeueUnmatchedResult>(
    '/admin/companion/requeue-unmatched',
    null,
    { params }
  )
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
  getSettings,
  updateSettings,
  getSummary,
  getTimeseries,
  getRequests,
  getAccountRules,
  upsertAccountRule,
  deleteAccountRule,
  collect,
  requeueUnmatched,
  getA6BackfillStatus,
  startA6Backfill,
  importUpstream
}

export default companionAPI
