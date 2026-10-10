import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

import UsageTable from '../UsageTable.vue'

// 与 UsageTableUpstreamCost.spec.ts 同样的取舍：**刻意不桩 DataTable**。
//
// DataTable 的列 key 与 cell-<key> 插槽名是一对孪生标识——列定义写成
// `upstream_latency` 而插槽写成 `cell-upstreamLatency` 时，页面会安静地
// 什么都不显示，编译、类型检查、i18n 校验全都不会报错。只有真实 DataTable
// 才能验证「列 key → 插槽名」这条链路真的接通。
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn() }),
}))

const baseRow = {
  request_id: 'req-upstream-latency',
  model: 'gpt-6-astra',
  created_at: '2026-10-10T10:00:00Z',
}

const mountOptions = (rows: Record<string, unknown>[], columns: { key: string; label: string }[]) => ({
  props: {
    data: rows,
    loading: false,
    // 被验证的对象：列 key 必须与 UsageTable 里的 cell-<key> 插槽名一致。
    columns,
  },
  global: {
    stubs: { EmptyState: true, Icon: true, Teleport: true },
  },
})

const LATENCY_COLUMNS = [
  { key: 'latency', label: '延迟' },
  { key: 'upstream_latency', label: '上游延迟' },
]

describe('使用记录「上游延迟」列', () => {
  it('首字与总耗时都取到时，按本站同样的时长格式渲染（不是裸毫秒数）', () => {
    const wrapper = mount(UsageTable as never, mountOptions(
      [{ ...baseRow, first_token_ms: 2100, duration_ms: 4200, upstream_first_token_ms: 1500, upstream_duration_ms: 3000 }],
      LATENCY_COLUMNS,
    ) as never)

    const text = wrapper.text()
    // 断言「渲染值 ≠ 原始值」：1500 必须变成 1.50s。
    // 若渲染成裸的 1500，说明走到的是 DataTable 的默认 row[key] 分支，
    // 也就是 cell-upstream_latency 插槽根本没接上。
    expect(text).toContain('1.50s')
    expect(text).toContain('3.00s')
    expect(text).not.toContain('1500')
    expect(text).not.toContain('3000')
    // 本站观测的两列也必须照常渲染，说明插槽是「新增」而不是「覆盖」。
    expect(text).toContain('2.10s')
    expect(text).toContain('4.20s')
  })

  it('尚未对账到时整列显示破折号，而不是 0ms', () => {
    const wrapper = mount(UsageTable as never, mountOptions(
      [{ ...baseRow, first_token_ms: 2100, duration_ms: 4200, upstream_first_token_ms: null, upstream_duration_ms: null }],
      LATENCY_COLUMNS,
    ) as never)

    const text = wrapper.text()
    // 「上游账单还没反查回来」与「上游 0 毫秒出首字」是完全相反的排障结论，
    // 用 formatDuration 无条件格式化会把 null 变成 '-'、把 0 变成 '0ms'，
    // 这里必须确保显示的是破折号这条分支。
    expect(text).toContain('—')
    expect(text).not.toContain('0ms')
    // 本站观测照常显示，证明破折号来自上游列而不是整表空掉。
    expect(text).toContain('2.10s')
  })

  it('只有首字取到、总耗时未取到时，逐项降级而不是整列消失', () => {
    const wrapper = mount(UsageTable as never, mountOptions(
      [{ ...baseRow, upstream_first_token_ms: 1500, upstream_duration_ms: null }],
      LATENCY_COLUMNS,
    ) as never)

    const text = wrapper.text()
    // 首字照常显示。
    expect(text).toContain('1.50s')
    // 总耗时那一格降级为 '-'（formatDuration 对 null 的既有行为）。
    expect(text).toContain('-')
    // 关键：只要有一项有值就不该整列显示「—」，否则会把「部分取到」误报成「完全没取到」。
    expect(text).not.toContain('—')
  })
})
