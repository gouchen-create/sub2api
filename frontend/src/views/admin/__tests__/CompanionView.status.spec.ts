import { describe, expect, it, beforeEach, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import CompanionView from '../CompanionView.vue'
import type { CompanionSettings, CompanionStatus } from '@/api/admin/companion'

const api = vi.hoisted(() => ({
  getStatus: vi.fn(),
  getSettings: vi.fn(),
  updateSettings: vi.fn(),
  getSummary: vi.fn(),
  getTimeseries: vi.fn(),
  getRequests: vi.fn(),
  getAccountRules: vi.fn(),
  deleteAccountRule: vi.fn(),
  upsertAccountRule: vi.fn(),
  collect: vi.fn()
}))

vi.mock('@/api/admin/companion', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/admin/companion')>('@/api/admin/companion')
  return { ...actual, companionAPI: api }
})

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      // 保留 key 便于断言；带参数时把参数值拼在后面，这样时间插值是否真的发生也能验证
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key} ${Object.values(params).join(' ')}` : key,
      locale: { value: 'zh' }
    })
  }
})

vi.mock('@/stores', () => ({
  useAppStore: () => ({
    showInfo: vi.fn(),
    showSuccess: vi.fn(),
    showError: vi.fn(),
    showWarning: vi.fn()
  })
}))

function settings(overrides: Partial<CompanionSettings> = {}): CompanionSettings {
  return {
    a6_base_url: 'https://a6.example.com',
    a6_user_id: 'admin',
    a6_token_configured: true,
    a6_token_mask: 'sk-****abcd',
    fx_usd_cny_rate: 6.71,
    override_keys: [],
    ...overrides
  }
}

function status(overrides: Partial<CompanionStatus> = {}): CompanionStatus {
  return { enabled: true, healthy: true, status: 200, detail: '', ...overrides }
}

async function mountView() {
  const wrapper = mount(CompanionView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        CompanionTrendChart: true,
        CompanionRulesTable: true,
        CompanionRequestsTable: true
      }
    }
  })
  await flushPromises()
  await flushPromises()
  return wrapper
}

describe('CompanionView 上游状态优先级', () => {
  beforeEach(() => {
    Object.values(api).forEach((fn) => fn.mockReset())
    api.getSummary.mockResolvedValue({ currency: 'CNY', fx_usd_cny: '6.71', fx_source: 'page' })
    api.getTimeseries.mockResolvedValue({ points: [], bucket: '1小时' })
    api.getRequests.mockResolvedValue({ items: [], total: 0 })
    api.getAccountRules.mockResolvedValue({ items: [], unconfigured_accounts: 0 })
  })

  it('缺凭据时显示「未配置」而不是「不可达」——后端此时同样返回 healthy:false', async () => {
    api.getStatus.mockResolvedValue(
      status({ healthy: false, detail: '尚未配置 A6 上游凭据，请在页面「上游 A6 配置」中填写' })
    )
    api.getSettings.mockResolvedValue(settings({ a6_token_configured: false, a6_token_mask: '' }))

    const wrapper = await mountView()

    const badge = wrapper.get('[data-testid="companion-status-badge"]')
    expect(badge.text()).toBe('admin.companion.status.disabled')
    expect(badge.text()).not.toBe('admin.companion.status.unhealthy')
    // 未配置用灰点，避免和「不可达」的琥珀点混淆
    expect(wrapper.get('[data-testid="companion-status-dot"]').classes()).toContain('bg-gray-400')
    // 引导卡片指向下方的 A6 配置，看板照常渲染
    expect(wrapper.text()).toContain('admin.companion.status.setupTitle')
    expect(wrapper.text()).toContain('admin.companion.status.setupIntro')

    wrapper.unmount()
  })

  it('凭据齐备但上游不通时才是「不可达」', async () => {
    api.getStatus.mockResolvedValue(status({ healthy: false, detail: 'dial tcp: i/o timeout' }))
    api.getSettings.mockResolvedValue(settings())

    const wrapper = await mountView()

    const badge = wrapper.get('[data-testid="companion-status-badge"]')
    expect(badge.text()).toBe('admin.companion.status.unhealthy')
    expect(wrapper.get('[data-testid="companion-status-dot"]').classes()).toContain('bg-amber-500')

    wrapper.unmount()
  })

  it('一切正常时显示「已连接」', async () => {
    api.getStatus.mockResolvedValue(status())
    api.getSettings.mockResolvedValue(settings())

    const wrapper = await mountView()

    const badge = wrapper.get('[data-testid="companion-status-badge"]')
    expect(badge.text()).toBe('admin.companion.status.healthy')
    expect(wrapper.get('[data-testid="companion-status-dot"]').classes()).toContain('bg-green-500')
    expect(wrapper.text()).not.toContain('admin.companion.status.setupTitle')

    wrapper.unmount()
  })

  it('配置还没读到时不下「未配置」结论，按探测结果显示', async () => {
    api.getStatus.mockResolvedValue(status({ healthy: true }))
    api.getSettings.mockRejectedValue({ status: 500, message: 'COMPANION_INTERNAL' })

    const wrapper = await mountView()

    expect(wrapper.get('[data-testid="companion-status-badge"]').text()).toBe(
      'admin.companion.status.healthy'
    )
    // 读取失败只在配置卡片内提示，不需要整页引导
    expect(wrapper.text()).toContain('admin.companion.settings.loadFailed')

    wrapper.unmount()
  })

  it('不可达时显示真实失败时间，而不是页面刷新时间', async () => {
    const failedAt = '2026-10-01T04:14:59.459Z'
    api.getStatus.mockResolvedValue(
      status({
        healthy: false,
        detail: 'A6 rejected the configured credentials (upstream 401/403)',
        last_error_at: failedAt
      })
    )
    api.getSettings.mockResolvedValue(settings())

    const wrapper = await mountView()

    const stamp = wrapper.get('[data-testid="companion-last-error-at"]')
    expect(stamp.text()).toContain('admin.companion.status.lastErrorAt')
    // 插值出来的必须是数据库里那个真实失败时刻
    expect(stamp.text()).toContain(new Date(failedAt).toLocaleTimeString())
    // 故障场景下不再拿「更新于」（页面刷新时间）冒充失败时间
    expect(wrapper.text()).not.toContain('admin.companion.updatedAt')

    wrapper.unmount()
  })

  it('已连接时不展示失败时间，回到「更新于」', async () => {
    api.getStatus.mockResolvedValue(status({ last_error_at: '2026-10-01T04:14:59.459Z' }))
    api.getSettings.mockResolvedValue(settings())

    const wrapper = await mountView()

    expect(wrapper.find('[data-testid="companion-last-error-at"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('admin.companion.updatedAt')

    wrapper.unmount()
  })

  it('不可达但没有失败时间戳时回落到「更新于」，不显示空时间', async () => {
    api.getStatus.mockResolvedValue(status({ healthy: false, detail: 'dial tcp: i/o timeout' }))
    api.getSettings.mockResolvedValue(settings())

    const wrapper = await mountView()

    expect(wrapper.find('[data-testid="companion-last-error-at"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('admin.companion.updatedAt')

    wrapper.unmount()
  })
})

/**
 * 采集积压必须可见。
 *
 * 单轮采集有上限（CollectBatchSize），被塞满时游标只推进到本批最后一行，
 * 剩余调用要等下一轮才补上；这期间看板上的收入是偏低的。
 * 积压信号藏在日志里等于没有——它必须出现在状态卡片上。
 */
describe('CompanionView 采集积压提示', () => {
  beforeEach(() => {
    Object.values(api).forEach((fn) => fn.mockReset())
    api.getSummary.mockResolvedValue({ currency: 'CNY', fx_usd_cny: '6.71', fx_source: 'page' })
    api.getTimeseries.mockResolvedValue({ points: [], bucket: '1小时' })
    api.getRequests.mockResolvedValue({ items: [], total: 0 })
    api.getAccountRules.mockResolvedValue({ items: [], unconfigured_accounts: 0 })
    api.getSettings.mockResolvedValue(settings())
  })

  it('本轮被上限截断时显示积压行数', async () => {
    api.getStatus.mockResolvedValue(
      status({ usage_batch_truncated: true, usage_backlog: 39975, usage_last_batch_size: 5000 })
    )

    const wrapper = await mountView()

    const backlog = wrapper.get('[data-testid="companion-usage-backlog"]')
    expect(backlog.text()).toContain('admin.companion.status.usageBacklog')
    // 插值出来的必须是真实的待采集行数
    expect(backlog.text()).toContain('39975')

    wrapper.unmount()
  })

  it('被截断但探针数不出行数时用不含数字的兜底文案', async () => {
    api.getStatus.mockResolvedValue(status({ usage_batch_truncated: true, usage_backlog: 0 }))

    const wrapper = await mountView()

    const backlog = wrapper.get('[data-testid="companion-usage-backlog"]')
    expect(backlog.text()).toBe('admin.companion.status.usageTruncated')

    wrapper.unmount()
  })

  it('没有被截断时不显示积压提示，避免制造无意义的焦虑', async () => {
    api.getStatus.mockResolvedValue(
      status({ usage_batch_truncated: false, usage_backlog: 0, usage_cursor_at: '2026-10-01T04:14:59.459Z' })
    )

    const wrapper = await mountView()

    expect(wrapper.find('[data-testid="companion-usage-backlog"]').exists()).toBe(false)

    wrapper.unmount()
  })

  it('后端还没返回新字段（旧版本）时不显示、也不报错', async () => {
    api.getStatus.mockResolvedValue(status())

    const wrapper = await mountView()

    expect(wrapper.find('[data-testid="companion-usage-backlog"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="companion-status-badge"]').text()).toBe(
      'admin.companion.status.healthy'
    )

    wrapper.unmount()
  })
})
