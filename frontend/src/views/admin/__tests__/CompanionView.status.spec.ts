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
      t: (key: string) => key,
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
})
