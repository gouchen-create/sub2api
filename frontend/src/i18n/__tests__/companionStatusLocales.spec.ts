import { describe, expect, it } from 'vitest'
import en from '@/i18n/locales/en'
import zh from '@/i18n/locales/zh'

/**
 * 经营对账「六种状态」文案的逐字回归。
 *
 * 背景：六种状态里有两组语义相反的配对，文案一旦撞名，管理员就会把两个方向
 * 完全相反的结论看成一件事，直接改错上游配置：
 *   - a6_waiting（等账单来）vs a6_pending（账单来了但没匹配上）
 *   - rule_unconfigured（没规则）vs pending（有规则但快照还没采集）
 * 上一版就出现过 `a6Pending` 与 `upstreamUnmatched` 文案完全一样、
 * `pending` 的提示语写成了「尚未登记上游类型与计费规则」的情况。
 *
 * 这里是硬约束：六条文案必须两两不同，且与文档 6.3 的逐字文案一致。
 * 文案改动必须同步改这个测试——这正是它存在的意义。
 */
const STATUS_KEYS = [
  'billed',
  'pending',
  'rule_unconfigured',
  'a6_pending',
  'a6_waiting',
  'upstream_unmatched',
] as const

// i18n 里的键名是 camelCase，与后端 cost_source 的 snake_case 一一对应。
const STATUS_KEY_TO_I18N: Record<(typeof STATUS_KEYS)[number], string> = {
  billed: 'billed',
  pending: 'pending',
  rule_unconfigured: 'ruleUnconfigured',
  a6_pending: 'a6Pending',
  a6_waiting: 'a6Waiting',
  upstream_unmatched: 'upstreamUnmatched',
}

/** 文档 6.3 规定的逐字文案（中文）。 */
const EXPECTED_ZH: Record<(typeof STATUS_KEYS)[number], string> = {
  billed: '账单实扣',
  pending: '待对账',
  rule_unconfigured: '规则待配置',
  a6_pending: '上游账单待匹配',
  a6_waiting: '等待上游账单',
  upstream_unmatched: '上游待匹配',
}

/** 英文对应文案，同样要求两两不同。 */
const EXPECTED_EN: Record<(typeof STATUS_KEYS)[number], string> = {
  billed: 'Billed (actual charge)',
  pending: 'Pending',
  rule_unconfigured: 'Rule not configured',
  a6_pending: 'Upstream billing unmatched',
  a6_waiting: 'Waiting for upstream billing',
  upstream_unmatched: 'Upstream unmatched',
}

type Dict = Record<string, any>

function statusText(locale: Dict, key: string): unknown {
  return locale?.admin?.companion?.requests?.statusText?.[key]
}

function statusHint(locale: Dict, key: string): unknown {
  return locale?.admin?.companion?.requests?.statusHint?.[key]
}

describe('companion 六种状态文案', () => {
  it('zh 的六条状态文案逐字符合文档 6.3', () => {
    for (const status of STATUS_KEYS) {
      expect(statusText(zh, STATUS_KEY_TO_I18N[status])).toBe(EXPECTED_ZH[status])
    }
  })

  it('en 的六条状态文案与 zh 一一对应', () => {
    for (const status of STATUS_KEYS) {
      expect(statusText(en, STATUS_KEY_TO_I18N[status])).toBe(EXPECTED_EN[status])
    }
  })

  it('zh 的六条状态文案两两不同', () => {
    const texts = STATUS_KEYS.map((status) => statusText(zh, STATUS_KEY_TO_I18N[status]))
    for (const text of texts) {
      expect(typeof text).toBe('string')
      expect(text).not.toBe('')
    }
    // 用 Set 判重：任何两条相同都会让长度变小。
    expect(new Set(texts).size).toBe(STATUS_KEYS.length)
  })

  it('en 的六条状态文案两两不同', () => {
    const texts = STATUS_KEYS.map((status) => statusText(en, STATUS_KEY_TO_I18N[status]))
    for (const text of texts) {
      expect(typeof text).toBe('string')
      expect(text).not.toBe('')
    }
    expect(new Set(texts).size).toBe(STATUS_KEYS.length)
  })

  it('易混淆的两组配对语义相反，文案不得复用', () => {
    const pairs: Array<[string, string]> = [
      // 等上游账单 vs 上游账单来了但没匹配上
      ['a6Waiting', 'a6Pending'],
      // 没登记规则 vs 有规则但快照还没采集
      ['ruleUnconfigured', 'pending'],
    ]
    for (const [left, right] of pairs) {
      expect(statusText(zh, left)).not.toBe(statusText(zh, right))
      expect(statusHint(zh, left)).not.toBe(statusHint(zh, right))
      expect(statusText(en, left)).not.toBe(statusText(en, right))
      expect(statusHint(en, left)).not.toBe(statusHint(en, right))
    }
  })

  it('pending 的提示语描述的是「快照未采集」，不是「规则未配置」', () => {
    // pending = 调用已产生、快照还没采集（采集器 30 秒一轮会补上）。
    // 写成「规则未配置」会把管理员引向错误的排查方向。
    expect(statusHint(zh, 'pending')).toBe(
      '调用已产生，快照尚未采集完成，采集器下一轮会补上',
    )
    expect(statusHint(en, 'pending')).toBe(
      'The call exists but its snapshot is not collected yet; the collector will fill it in on the next round',
    )
    // 与 rule_unconfigured 必须是两句话。
    expect(statusHint(zh, 'pending')).not.toBe(statusHint(zh, 'ruleUnconfigured'))
  })

  it('六个 statusHint 都非空，且不残留已下线通道的说法', () => {
    for (const status of STATUS_KEYS) {
      const key = STATUS_KEY_TO_I18N[status]
      const zhHint = statusHint(zh, key)
      const enHint = statusHint(en, key)
      expect(typeof zhHint).toBe('string')
      expect(typeof enHint).toBe('string')
      expect((zhHint as string).length).toBeGreaterThan(0)
      expect((enHint as string).length).toBeGreaterThan(0)
      // a6_waiting / a6_pending 不再提「已建立上游请求映射」这类早已废弃的机制。
      if (status === 'a6_waiting' || status === 'a6_pending') {
        expect(zhHint as string).not.toContain('请求映射')
      }
    }
  })
})
