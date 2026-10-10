/**
 * 请求延迟健康度分档（用于用量明细"延迟"列的纵向健康扫视）。
 *
 * 首 Token（TTFT）：10s 内正常，10-30s 偏慢，30-60s 缓慢，60s 及以上严重。
 * 总耗时：流式请求整体时长天然更长，阈值放宽为 1min / 3min / 5min。
 */
export type LatencySeverity = 'good' | 'warn' | 'slow' | 'critical'

export const FIRST_TOKEN_THRESHOLDS_MS = {
  warn: 10_000,
  slow: 30_000,
  critical: 60_000,
} as const

export const DURATION_THRESHOLDS_MS = {
  warn: 60_000,
  slow: 180_000,
  critical: 300_000,
} as const

interface Thresholds {
  warn: number
  slow: number
  critical: number
}

const classify = (ms: number, thresholds: Thresholds): LatencySeverity => {
  if (ms >= thresholds.critical) return 'critical'
  if (ms >= thresholds.slow) return 'slow'
  if (ms >= thresholds.warn) return 'warn'
  return 'good'
}

export const firstTokenSeverity = (ms: number): LatencySeverity =>
  classify(ms, FIRST_TOKEN_THRESHOLDS_MS)

export const durationSeverity = (ms: number): LatencySeverity =>
  classify(ms, DURATION_THRESHOLDS_MS)

/**
 * 中转开销占比的分档阈值（百分比）。
 *
 * 与上面两组**绝对时间**阈值不同：那两组衡量「这次请求慢不慢」，本组衡量
 * 「中转本身重不重」。所以阈值按百分比而不是毫秒——上游本身要 30 秒的请求，
 * 中转再加 2 秒只占 7%，不该和「上游 1 秒、中转再加 2 秒」显示成同一档。
 *
 * 分档依据（相对上游的增幅）：
 *   ≤10%   几乎无中转开销
 *   10~30% 有可感开销，仍属正常区间
 *   30~60% 中转明显偏重，值得关注
 *   >60%   中转开销接近甚至超过上游本身，应排查
 *
 * 负值（本站比上游还快，通常是采样/时钟差异）一律归入 good。
 */
export const OVERHEAD_RATIO_PCT_THRESHOLDS = {
  warn: 10,
  slow: 30,
  critical: 60,
} as const

export const overheadRatioSeverity = (pct: number): LatencySeverity =>
  classify(pct, OVERHEAD_RATIO_PCT_THRESHOLDS)

/**
 * 计算「本站延迟相对上游延迟多出来的百分比」，保留两位小数。
 *
 * 分母取**上游值**：这个数的含义是「中转在供应商基础上加了几成」，
 * 是排障时用来判断中转是否成为瓶颈的指标。
 *
 * 任一侧缺失（尚未对账到上游数据）或上游为 0 时返回 null——
 * 返回 0 会被读成「完全没有中转开销」，与「还不知道」正好相反。
 */
export const upstreamOverheadPercent = (
  siteMs: number | null | undefined,
  upstreamMs: number | null | undefined,
): number | null => {
  if (siteMs == null || upstreamMs == null) return null
  if (upstreamMs <= 0) return null
  return Number((((siteMs - upstreamMs) / upstreamMs) * 100).toFixed(2))
}

export const LATENCY_TEXT_CLASSES: Record<LatencySeverity, string> = {
  good: 'text-emerald-600 dark:text-emerald-400',
  warn: 'text-amber-600 dark:text-amber-400',
  slow: 'text-orange-600 dark:text-orange-400',
  critical: 'text-red-600 dark:text-red-400',
}

/** 无首字数据时的纯色色条（仅按总耗时档着色）。 */
export const LATENCY_BAR_CLASSES: Record<LatencySeverity, string> = {
  good: 'bg-emerald-500',
  warn: 'bg-amber-400',
  slow: 'bg-orange-500',
  critical: 'bg-red-500',
}

/** 渐变色条上端（首字档）；与 LATENCY_BAR_TO_CLASSES 组合成上下渐变，避免两段硬切割裂感。 */
export const LATENCY_BAR_FROM_CLASSES: Record<LatencySeverity, string> = {
  good: 'from-emerald-500',
  warn: 'from-amber-400',
  slow: 'from-orange-500',
  critical: 'from-red-500',
}

/** 渐变色条下端（总耗时档）。 */
export const LATENCY_BAR_TO_CLASSES: Record<LatencySeverity, string> = {
  good: 'to-emerald-500',
  warn: 'to-amber-400',
  slow: 'to-orange-500',
  critical: 'to-red-500',
}
