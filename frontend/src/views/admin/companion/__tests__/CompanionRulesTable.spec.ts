import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

import CompanionRulesTable from '../CompanionRulesTable.vue'
import type { CompanionAccountRule, CompanionAccountRuleGroup } from '@/api/admin/companion'

// 只给需要断言插值的文案配模板，其余 key 原样返回，断言失败时也能看出是哪条文案
const messages: Record<string, string> = {
  'admin.companion.rules.groupLabel': '{name} · #{id}',
  'admin.companion.rules.groupMeta': '分组 #{id} · {count} 个渠道',
  'admin.companion.rules.groupAccountsInline': '本表列出 {count} 个账号',
  'admin.companion.rules.unconfiguredGroupsCount': '有调用待配置 {count} 个分组',
  'admin.companion.rules.recentGroupMeta': '分组 #{id}',
  'admin.companion.rules.recentGroupDeleted': '分组 #{id}（已删除）',
  'admin.companion.rules.noGroup': '未关联当前分组',
  'admin.companion.rules.noGroupHint': '不属于任何分组',
  'admin.companion.rules.noUsage': '暂无调用记录',
  'admin.companion.rules.configured': '已配置',
  'admin.companion.rules.unconfigured': '待配置',
  'admin.companion.rules.unnamed': '未命名账号',
  'admin.companion.rules.unnamedGroup': '未命名分组'
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
 * DataTable 替身：渲染本用例关心的那几列，并把行类型放进 data-kind 便于定位。
 *
 * 分组头行与账号子行共用同一套插槽，所以断言时必须能区分两者：
 * 组头行看「分组名 / 渠道数 / 组级状态」，子行看账号自己的字段与交互。
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
      <div v-for="row in data" :key="row.key" class="rule-row" :data-kind="row.kind">
        <div class="cell-group"><slot name="cell-group" :row="row" /></div>
        <div class="cell-account"><slot name="cell-account" :row="row" /></div>
        <div class="cell-recent-model"><slot name="cell-recentModel" :row="row" /></div>
        <div class="cell-recent-group"><slot name="cell-recentGroup" :row="row" /></div>
        <div class="cell-usage"><slot name="cell-usageCount" :row="row" /></div>
        <div class="cell-value"><slot name="cell-value" :row="row" /></div>
        <div class="cell-state"><slot name="cell-state" :row="row" /></div>
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

function group(overrides: Partial<CompanionAccountRuleGroup> = {}): CompanionAccountRuleGroup {
  return {
    group_id: 10,
    group_name: '分组十',
    group_priority: 1,
    group_channel_count: 1,
    configured: false,
    usage_count: 0,
    token_keys: [],
    accounts: [],
    ...overrides
  }
}

function mountTable(groups: CompanionAccountRuleGroup[], unconfiguredGroupCount = 0) {
  return mount(CompanionRulesTable, {
    props: { groups, unconfiguredGroupCount },
    global: {
      stubs: { DataTable: DataTableStub, Icon: true }
    }
  })
}

/**
 * 表格按「分组」组织：一行一个分组，组内渠道账号各占一条子行。
 *
 * 业务单位是分组，而同一个分组下的账号对应完全不同的上游令牌
 * （实例：分组 codex-官方0.1折 #10 下有 #47 openai-0.1折 与 #44 openai-1折），
 * 所以规则仍按账号逐个保存，子行必须常显且各自可编辑。
 */
describe('CompanionRulesTable 按分组一行', () => {
  it('行序是「分组头行 → 该组账号子行」，不重排后端给的顺序', () => {
    const wrapper = mountTable([
      group({
        group_id: 10,
        group_name: 'codex-官方0.1折',
        group_channel_count: 2,
        accounts: [
          rule({ account_id: 47, account_name: 'openai-0.1折', group_id: 10 }),
          rule({ account_id: 44, account_name: 'openai-1折', group_id: 10 })
        ]
      }),
      group({ group_id: 7, group_name: '分组七', accounts: [rule({ account_id: 46, group_id: 7 })] })
    ])

    const kinds = wrapper.findAll('.rule-row').map((row) => row.attributes('data-kind'))
    expect(kinds).toEqual(['group', 'account', 'account', 'group', 'account'])

    const groups = wrapper.findAll('[data-kind="group"]')
    const accounts = wrapper.findAll('[data-kind="account"]')
    expect(groups).toHaveLength(2)
    expect(accounts).toHaveLength(3)
    expect(groups[0].text()).toContain('codex-官方0.1折 · #10')
    expect(groups[1].text()).toContain('分组七 · #7')

    wrapper.unmount()
  })

  it('组头行的「M 个渠道」用后端真实渠道数，不用组内账号条数', () => {
    const wrapper = mountTable([
      group({
        group_id: 10,
        group_name: '分组十',
        // 真实渠道数比本表列出的账号多：还有账号把它当次优先级分组，本表挂在别的分组下
        group_channel_count: 3,
        accounts: [rule({ account_id: 47 }), rule({ account_id: 44 })]
      })
    ])

    const header = wrapper.get('[data-kind="group"]')
    expect(header.text()).toContain('分组 #10 · 3 个渠道')
    expect(header.text()).not.toContain('2 个渠道')
    // 「本表列出 2 个账号」是另一套口径，明确写出来避免和渠道数混淆
    expect(header.get('.cell-account').text()).toBe('本表列出 2 个账号')

    wrapper.unmount()
  })

  it('后端漏 group_channel_count 时回落到 0，不显示 NaN', () => {
    const incomplete = group({ group_id: 3, group_name: '三分组' })
    delete (incomplete as Partial<CompanionAccountRuleGroup>).group_channel_count

    const wrapper = mountTable([incomplete])

    expect(wrapper.get('[data-kind="group"]').text()).toContain('分组 #3 · 0 个渠道')

    wrapper.unmount()
  })

  it('没有归属任何分组的账号（group_id 0）显示未关联当前分组，且不重复同一句话', () => {
    const wrapper = mountTable([
      group({ group_id: 0, group_name: '', accounts: [rule({ account_id: 99 })] })
    ])

    const header = wrapper.get('[data-kind="group"]')
    expect(header.text()).toContain('未关联当前分组')
    expect(header.text()).toContain('不属于任何分组')
    // group_id 0 不该渲染成「分组 #0 · N 个渠道」
    expect(header.text()).not.toContain('分组 #0')

    wrapper.unmount()
  })

  it('分组名缺失时用占位文案，而不是只留一个 #ID', () => {
    const wrapper = mountTable([group({ group_id: 10, group_name: '' })])

    expect(wrapper.get('[data-kind="group"]').text()).toContain('未命名分组 · #10')

    wrapper.unmount()
  })

  it('分组头行的用量是组内合计，账号子行是各自用量', () => {
    const wrapper = mountTable([
      group({
        group_id: 10,
        usage_count: 12,
        accounts: [
          rule({ account_id: 47, usage_count: 5 }),
          rule({ account_id: 44, usage_count: 7 })
        ]
      })
    ])

    const usage = wrapper.findAll('.cell-usage').map((cell) => cell.text())
    expect(usage).toEqual(['12', '5', '7'])

    wrapper.unmount()
  })

  it('分组头行显示组内已配置的令牌标识（去重），没配时用占位符', () => {
    const wrapper = mountTable([
      group({ group_id: 10, token_keys: ['openai-0.1折', 'openai-1折'] }),
      group({ group_id: 7, accounts: [rule({ account_id: 46, group_id: 7 })] })
    ])

    const values = wrapper.findAll('[data-kind="group"] .cell-value').map((cell) => cell.text())
    expect(values[0]).toBe('openai-0.1折 / openai-1折')
    expect(values[1]).toBe('—')

    wrapper.unmount()
  })
})

/**
 * 状态列分两级：分组级用后端的 configured（组内全部账号都已配置才为 true），
 * 账号级仍是各自的状态。两级不能互相冒充。
 */
describe('CompanionRulesTable 状态列', () => {
  it('分组级显示组状态，账号级显示各自状态', () => {
    const wrapper = mountTable([
      group({
        group_id: 10,
        configured: false,
        accounts: [
          rule({ account_id: 47, configured: true }),
          rule({ account_id: 44, configured: false })
        ]
      }),
      group({ group_id: 7, configured: true, accounts: [rule({ account_id: 46, configured: true })] })
    ])

    const rows = wrapper.findAll('.rule-row')
    expect(rows[0].get('.cell-state').text()).toBe('待配置')
    expect(rows[1].get('.cell-state').text()).toBe('已配置')
    expect(rows[2].get('.cell-state').text()).toBe('待配置')
    expect(rows[3].get('.cell-state').text()).toBe('已配置')

    wrapper.unmount()
  })

  it('角标数字来自分组级的待配置数', () => {
    const wrapper = mountTable([group({ group_id: 10 })], 3)

    expect(wrapper.get('span.rounded-full').text()).toBe('有调用待配置 3 个分组')

    wrapper.unmount()
  })
})

/**
 * 规则是按账号存的，所以保存/删除只能出现在账号子行上。
 * 分组头行如果也放一对按钮，管理员会以为能整组保存——那是做不到的。
 */
describe('CompanionRulesTable 保存与删除只在账号级', () => {
  it('分组头行没有保存/删除按钮', () => {
    const wrapper = mountTable([
      group({ group_id: 10, accounts: [rule({ account_id: 47, provider: 'a6', token_name: 'x' })] })
    ])

    const header = wrapper.get('[data-kind="group"]')
    expect(header.get('.cell-actions').findAll('button')).toHaveLength(0)
    // 账号子行仍保留完整交互
    expect(wrapper.get('[data-kind="account"]').get('.cell-actions').findAll('button')).toHaveLength(2)

    wrapper.unmount()
  })

  it('每个账号单独保存：改哪个子行就只发哪个 account_id', async () => {
    const wrapper = mountTable([
      group({
        group_id: 10,
        accounts: [
          rule({ account_id: 47, token_name: 'openai-0.1折', provider: 'a6' }),
          rule({ account_id: 44, token_name: 'openai-1折', provider: 'a6' })
        ]
      })
    ])

    const accountRows = wrapper.findAll('[data-kind="account"]')
    const secondInput = accountRows[1].get('.cell-value input[type="text"]')
    expect((secondInput.element as HTMLInputElement).value).toBe('openai-1折')

    await secondInput.setValue('openai-1折-v2')
    await accountRows[1].get('.cell-actions button').trigger('click')

    expect(wrapper.emitted('save')?.[0]?.[0]).toEqual({ accountId: 44, value: 'openai-1折-v2' })
    expect(accountRows[0].get('.cell-value input[type="text"]').element).toHaveProperty(
      'value',
      'openai-0.1折'
    )

    wrapper.unmount()
  })

  it('令牌名为空时拦在本地，不发出 save 事件', async () => {
    const wrapper = mountTable([
      group({ group_id: 10, accounts: [rule({ account_id: 43, token_name: '' })] })
    ])

    await wrapper.get('[data-kind="account"] .cell-actions button').trigger('click')

    expect(wrapper.emitted('save')).toBeFalsy()

    wrapper.unmount()
  })

  it('删除事件带的是该账号自己的 account_id', async () => {
    const wrapper = mountTable([
      group({
        group_id: 10,
        accounts: [
          rule({ account_id: 47, provider: 'a6', token_name: 'a' }),
          rule({ account_id: 44, provider: 'a6', token_name: 'b' })
        ]
      })
    ])

    const buttons = wrapper.findAll('[data-kind="account"]')[1].get('.cell-actions').findAll('button')
    await buttons[1].trigger('click')

    expect(wrapper.emitted('remove')?.[0]?.[0]).toBe(44)

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
  function accountRow(overrides: Partial<CompanionAccountRule> = {}) {
    return mountTable([group({ group_id: 10, accounts: [rule(overrides)] })])
  }

  it('渲染后端给的 recent_model / recent_group_*，而不是窗口内模型并集', () => {
    const wrapper = accountRow({
      account_id: 1,
      // 窗口内没有调用……
      usage_count: 0,
      // ……但全历史最后一次调用有记录
      recent_model: 'claude-3-5-sonnet',
      recent_group_id: 2,
      recent_group_name: '默认分组'
    })

    const row = wrapper.get('[data-kind="account"]')
    expect(row.get('.cell-recent-model').text()).toBe('claude-3-5-sonnet')
    expect(row.get('.cell-recent-group').text()).toContain('默认分组')
    expect(row.get('.cell-recent-group').text()).toContain('分组 #2')

    wrapper.unmount()
  })

  it('没有任何调用记录时两列都显示占位文案', () => {
    const wrapper = accountRow({ account_id: 5, recent_model: '', recent_group_id: 0 })

    const row = wrapper.get('[data-kind="account"]')
    expect(row.get('.cell-recent-model').text()).toBe('暂无调用记录')
    expect(row.get('.cell-recent-group').text()).toBe('暂无调用记录')

    wrapper.unmount()
  })

  it('最近分组已被删除时用 group_id 明确标出，而不是显示空白', () => {
    const wrapper = accountRow({
      account_id: 8,
      recent_model: 'gpt-4o',
      recent_group_id: 99,
      recent_group_name: ''
    })

    const cell = wrapper.get('[data-kind="account"]').get('.cell-recent-group')
    expect(cell.text()).toContain('分组 #99（已删除）')

    wrapper.unmount()
  })

  it('分组头行的最近模型/最近分组不冒充某个账号的值', () => {
    const wrapper = mountTable([group({ group_id: 10, accounts: [rule({ recent_model: 'gpt-4o' })] })])

    const header = wrapper.get('[data-kind="group"]')
    expect(header.get('.cell-recent-model').text()).toBe('—')
    expect(header.get('.cell-recent-group').text()).toBe('—')

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
    const wrapper = mountTable([group({ group_id: 10, accounts: [rule({ account_id: 1 })] })])

    expect(wrapper.get('.cell-provider-slot-count').text()).toBe('0')

    wrapper.unmount()
  })

  it('列定义里没有「上游类型」列，令牌名列的文案不再提 Subarx 倍率', () => {
    const wrapper = mountTable([group({ group_id: 10, accounts: [rule({ account_id: 1 })] })])
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
    const wrapper = mountTable([
      group({ group_id: 10, accounts: [rule({ account_id: 42, token_name: '' })] })
    ])

    // 输入框预填自 token_name，编辑后点真实的保存按钮。
    const input = wrapper.get('[data-kind="account"] .cell-value input[type="text"]')
    await input.setValue('2.0-kimik3')
    await wrapper.get('[data-kind="account"] .cell-actions button').trigger('click')

    const emitted = wrapper.emitted('save')
    expect(emitted).toBeTruthy()
    expect(emitted?.[0]?.[0]).toEqual({ accountId: 42, value: '2.0-kimik3' })

    wrapper.unmount()
  })
})
