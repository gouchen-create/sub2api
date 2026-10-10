import { describe, expect, it } from 'vitest'

import {
  durationSeverity,
  firstTokenSeverity,
  overheadRatioSeverity,
  upstreamOverheadPercent,
} from '../latencyHealth'

describe('latencyHealth', () => {
  it('classifies first-token latency at 10s/30s/60s boundaries', () => {
    expect(firstTokenSeverity(0)).toBe('good')
    expect(firstTokenSeverity(9_999)).toBe('good')
    expect(firstTokenSeverity(10_000)).toBe('warn')
    expect(firstTokenSeverity(29_999)).toBe('warn')
    expect(firstTokenSeverity(30_000)).toBe('slow')
    expect(firstTokenSeverity(59_999)).toBe('slow')
    expect(firstTokenSeverity(60_000)).toBe('critical')
  })

  it('classifies total duration at 1min/3min/5min boundaries', () => {
    expect(durationSeverity(0)).toBe('good')
    expect(durationSeverity(59_999)).toBe('good')
    expect(durationSeverity(60_000)).toBe('warn')
    expect(durationSeverity(179_999)).toBe('warn')
    expect(durationSeverity(180_000)).toBe('slow')
    expect(durationSeverity(299_999)).toBe('slow')
    expect(durationSeverity(300_000)).toBe('critical')
  })
})

describe('upstreamOverheadPercent', () => {
  it('以「上游值」为分母计算相对增幅', () => {
    // 本站 1500、上游 1000 → 中转多了 50%。
    expect(upstreamOverheadPercent(1500, 1000)).toBe(50)
    // 本站与上游一致 → 0%。
    expect(upstreamOverheadPercent(1000, 1000)).toBe(0)
    // 本站反而更快 → 负数，不做截断，否则会掩盖时钟/采样异常。
    expect(upstreamOverheadPercent(800, 1000)).toBe(-20)
  })

  it('固定保留两位小数', () => {
    expect(upstreamOverheadPercent(1234, 1000)).toBe(23.4)
    expect(upstreamOverheadPercent(1235, 1000)).toBe(23.5)
    // 1/3 这类无限小数必须被截到两位，否则页面上会出现一长串尾巴。
    expect(upstreamOverheadPercent(1333, 1000)).toBe(33.3)
    expect(upstreamOverheadPercent(1234.567, 1000)).toBe(23.46)
  })

  it('任一侧缺失或上游为 0 时返回 null，而不是 0', () => {
    // 0% 的含义是「完全没有中转开销」，与「上游数据还没对账到」正好相反，
    // 用 0 顶替会让管理员以为中转毫无成本。
    expect(upstreamOverheadPercent(null, 1000)).toBeNull()
    expect(upstreamOverheadPercent(undefined, 1000)).toBeNull()
    expect(upstreamOverheadPercent(1000, null)).toBeNull()
    expect(upstreamOverheadPercent(1000, undefined)).toBeNull()
    // 上游为 0 时除法无意义，必须返回 null 而不是 Infinity。
    expect(upstreamOverheadPercent(1000, 0)).toBeNull()
    expect(upstreamOverheadPercent(0, 0)).toBeNull()
  })
})

describe('overheadRatioSeverity', () => {
  it('按 10%/30%/60% 分档，且负值归入 good', () => {
    expect(overheadRatioSeverity(-50)).toBe('good') // 本站比上游快
    expect(overheadRatioSeverity(0)).toBe('good')
    expect(overheadRatioSeverity(9.99)).toBe('good')
    expect(overheadRatioSeverity(10)).toBe('warn')
    expect(overheadRatioSeverity(29.99)).toBe('warn')
    expect(overheadRatioSeverity(30)).toBe('slow')
    expect(overheadRatioSeverity(59.99)).toBe('slow')
    expect(overheadRatioSeverity(60)).toBe('critical')
  })

  it('分档依据是百分比而不是绝对毫秒', () => {
    // 上游 30 秒、中转再加 2 秒 = +6.7% → good（相对上游微不足道）。
    const smallRelative = upstreamOverheadPercent(32_000, 30_000)!
    expect(overheadRatioSeverity(smallRelative)).toBe('good')
    // 上游 1 秒、中转再加 2 秒 = +200% → critical（中转是上游的三倍）。
    const largeRelative = upstreamOverheadPercent(3_000, 1_000)!
    expect(overheadRatioSeverity(largeRelative)).toBe('critical')
    // 两者绝对增量完全相同（都是 2 秒），但分档截然不同——这正是按百分比分档的意义。
    expect(largeRelative - smallRelative).toBeGreaterThan(190)
  })
})
