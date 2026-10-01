import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

import CompanionRulesTable from '../CompanionRulesTable.vue'
import type { CompanionAccountRule } from '@/api/admin/companion'

// 只给需要断言插值的文案配模板，其余 key 原样返回，断言失败时也能看出是哪条文案
const messages: Record<string, string> = {
  'admin.companion.rules.groupMeta': '分组 #{id} · {count} 个渠道',
  'admin.companion.rules.recentGroupMeta': '分组 #{id}',
  'admin.companion.rules.recentGroupDeleted': '分组 #{id}（已删除）',
  'admin.companion.rules.noGroup': '未关联当前分组',
  'admin.companion.rules.noGroupHint': '不属于任何分组',
  'admin.companion.rules.noUsage': '暂无调用记录'
}

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) => {
        const template = messages[key] ?? key
        if (!params) return template
        return Object.entries(params).reduce(
          (text, [name, value]) => text.split(`{${name}}`).join(String(value)),
          template
        )
      },
      locale: { value: 'zh' }
    })
  }
})

vi.mock('@/stores', () => ({ useAppStore: () => ({ showError: vi.fn() }) }))

/**
 * DataTable 替身：渲染本用例关心的那几列。
 *
 * 分组列用于「M 个渠道」断言；最近模型 / 最近分组两列用于文档 19 的口径断言。
 */
const DataTableStub = {
  name: 'DataTable',
  props: {
    columns: { type: Array, default: () => [] },
    data: { type: Array, default: () => [] },
    loading: { type: Boolean, default: false }
  },
  template: `
    <div>
      <div v-for="row in data" :key="row.account_id" class="rule-row">
        <div class="cell-group"><slot name="cell-group" :row="row" /></div>
        <div class="cell-recent-model"><slot name="cell-recentModel" :row="row" /></div>
        <div class="cell-recent-group"><slot name="cell-recentGroup" :row="row" /></div>
        <div class="cell-value"><slot name="cell-value" :row="row" /></div>
        <div class="cell-actions"><slot name="cell-actions" :row="row" /></div>
        <div class="cell-provider-slot-count">{{ $slots['cell-provider'] ? 1 : 0 }}</div>
      </div>
    </div>
  `
}

function rule(overrides: Partial<CompanionAccountRule> = {}): CompanionAccountRule {
  return {
    account_id: 1,
    provider: '',
    token_name: '',
    multiplier: '',
    version: 0,
    enabled: false,
    created_at: '',
    updated_at: '',
    configured: false,
    current: true,
    group_id: 0,
    group_name: '',
    group_priority: 0,
    group_channel_count: 0,
    account_name: '',
    account_platform: 'anthropic',
    account_status: 'active',
    account_schedulable: true,
    usage_count: 0,
    first_seen: '',
    last_seen: '',
    recent_model: '',
    recent_group_id: 0,
    recent_group_name: '',
    ...overrides
  }
}

function mountTable(items: CompanionAccountRule[]) {
  return mount(CompanionRulesTable, {
    props: { items, unconfiguredCount: 0 },
    global: {
      stubs: { DataTable: DataTableStub, Icon: true }
    }
  })
}

/**
 * 「M 个渠道」必须是分组的真实渠道数，不是本表格的行数。
 *
 * dev 库实测：分组 7 真实有 2 个渠道（账号 #46 priority 1、#44 priority 3），但账号 #44
 * 的分组列被分组 #10 抢走（它取优先级最小的分组），于是表格里分组 7 只剩 #46 一行，
 * 旧实现就渲染成「1 个渠道」——数字是假的。
 */
describe('CompanionRulesTable 分组渠道数', () => {
  it('用后端给的真实渠道数，而不是本表格的行数', () => {
    const wrapper = mountTable([
      rule({ account_id: 46, group_id: 7, group_name: '分组七', group_channel_count: 2 }),
      rule({ account_id: 44, group_id: 10, group_name: '分组十', group_channel_count: 1 })
    ])

    const rows = wrapper.findAll('.rule-row')
    expect(rows).toHaveLength(2)
    // 分组 7 在本表里只有 1 行，但真实渠道数是 2
    expect(rows[0].text()).toContain('分组 #7 · 2 个渠道')
    expect(rows[0].text()).not.toContain('1 个渠道')
    expect(rows[1].text()).toContain('分组 #10 · 1 个渠道')

    wrapper.unmount()
  })

  it('无分组的账号主文案与灰色小字不再重复同一句话', () => {
    const wrapper = mountTable([rule({ account_id: 99, group_id: 0, group_name: '' })])

    const text = wrapper.get('.rule-row').text()
    expect(text).toContain('未关联当前分组')
    expect(text).toContain('不属于任何分组')

    wrapper.unmount()
  })

  it('后端漏字段时回落到 0，不显示 NaN', () => {
    const incomplete = rule({ account_id: 7, group_id: 3, group_name: '三分组' })
    delete (incomplete as Partial<CompanionAccountRule>).group_channel_count

    const wrapper = mountTable([incomplete])

    expect(wrapper.get('.rule-row').text()).toContain('分组 #3 · 0 个渠道')

    wrapper.unmount()
  })
})

/**
 * 文档 19：最近模型 / 最近分组读该账号全历史最后一次调用，不受时间筛选影响。
 *
 * 这两列渲染的是 recent_* 字段，而不是窗口内的聚合结果；同一行里的
 * 「范围内调用」仍然是窗口口径。两种口径在同一行并存，不能互相污染。
 */
describe('CompanionRulesTable 最近模型与最近分组', () => {
  it('渲染后端给的 recent_model / recent_group_*，而不是窗口内模型并集', () => {
    const wrapper = mountTable([
      rule({
        account_id: 1,
        // 窗口内没有调用……
        usage_count: 0,
        // ……但全历史最后一次调用有记录
        recent_model: 'claude-3-5-sonnet',
        recent_group_id: 2,
        recent_group_name: '默认分组'
      })
    ])

    const row = wrapper.get('.rule-row')
    expect(row.get('.cell-recent-model').text()).toBe('claude-3-5-sonnet')
    expect(row.get('.cell-recent-group').text()).toContain('默认分组')
    expect(row.get('.cell-recent-group').text()).toContain('分组 #2')

    wrapper.unmount()
  })

  it('没有任何调用记录时两列都显示占位文案', () => {
    const wrapper = mountTable([rule({ account_id: 5, recent_model: '', recent_group_id: 0 })])

    const row = wrapper.get('.rule-row')
    expect(row.get('.cell-recent-model').text()).toBe('暂无调用记录')
    expect(row.get('.cell-recent-group').text()).toBe('暂无调用记录')

    wrapper.unmount()
  })

  it('最近分组已被删除时用 group_id 明确标出，而不是显示空白', () => {
    const wrapper = mountTable([
      rule({ account_id: 8, recent_model: 'gpt-4o', recent_group_id: 99, recent_group_name: '' })
    ])

    const cell = wrapper.get('.rule-row').get('.cell-recent-group')
    expect(cell.text()).toContain('分组 #99（已删除）')

    wrapper.unmount()
  })
})

/**
 * Subarx 已于 v0.2.9 下线，后端 ValidateRuleInput 只接受 a6，
 * 提交 subarx 一定 400。规则页不能再提供这个选项：
 * 一个必然失败的按钮比没有按钮更糟——用户会以为问题出在别处。
 */
describe('CompanionRulesTable 不再提供 Subarx 上游类型', () => {
  it('表格里没有 provider 单元格插槽（上游类型选择器已移除）', () => {
    const wrapper = mountTable([rule({ account_id: 1 })])

    expect(wrapper.get('.cell-provider-slot-count').text()).toBe('0')

    wrapper.unmount()
  })

  it('列定义里没有「上游类型」列，令牌名列的文案不再提 Subarx 倍率', () => {
    const wrapper = mountTable([rule({ account_id: 1 })])
    const table = wrapper.findComponent(DataTableStub)
    const labels = (table.props('columns') as Array<{ key: string; label: string }>).map(
      (column) => column.label
    )

    // 未配置到 messages 的 key 会原样返回，正好用来断言「用的是哪个 key」。
    expect(labels).toContain('admin.companion.rules.columns.recentGroup')
    expect(labels).not.toContain('admin.companion.rules.columns.provider')
    expect(labels).toContain('admin.companion.rules.columns.value')
    expect(labels.join(' ')).not.toContain('provider')

    wrapper.unmount()
  })

  it('保存时只提交 accountId 与令牌名（provider 由视图层固定为 a6）', async () => {
    const wrapper = mountTable([rule({ account_id: 42, token_name: '' })])

    // 输入框预填自 token_name，编辑后点真实的保存按钮。
    const input = wrapper.get('.cell-value input[type="text"]')
    await input.setValue('2.0-kimik3')
    await wrapper.get('.cell-actions button').trigger('click')

    const emitted = wrapper.emitted('save')
    expect(emitted).toBeTruthy()
    expect(emitted?.[0]?.[0]).toEqual({ accountId: 42, value: '2.0-kimik3' })

    wrapper.unmount()
  })

  it('令牌名为空时拦在本地，不发出 save 事件', async () => {
    const wrapper = mountTable([rule({ account_id: 43, token_name: '' })])

    await wrapper.get('.cell-actions button').trigger('click')

    expect(wrapper.emitted('save')).toBeFalsy()

    wrapper.unmount()
  })
})
