import { describe, expect, it, beforeEach, vi } from 'vitest'
import { mount } from '@vue/test-utils'

import CompanionA6SettingsCard from '../CompanionA6SettingsCard.vue'
import type { CompanionSettings } from '@/api/admin/companion'

const { showInfo, showError } = vi.hoisted(() => ({
  showInfo: vi.fn(),
  showError: vi.fn()
}))

// 只给需要断言插值的文案配模板，其余 key 原样返回，断言失败时也能看出是哪条文案
const messages: Record<string, string> = {
  'admin.companion.settings.accessTokenConfiguredPlaceholder': '已配置：{mask}'
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

vi.mock('@/stores', () => ({
  useAppStore: () => ({ showInfo, showError })
}))

function baseSettings(overrides: Partial<CompanionSettings> = {}): CompanionSettings {
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

function mountCard(settings: CompanionSettings | null) {
  return mount(CompanionA6SettingsCard, {
    props: { settings }
  })
}

/** 取出组件向外抛出的保存请求体 */
function savedPayload(wrapper: ReturnType<typeof mountCard>) {
  const events = wrapper.emitted('save')
  return events ? (events[0][0] as Record<string, unknown>) : null
}

describe('CompanionA6SettingsCard', () => {
  beforeEach(() => {
    showInfo.mockReset()
    showError.mockReset()
  })

  it('没有任何改动时不发请求，只提示「无改动」', async () => {
    const wrapper = mountCard(baseSettings())

    await wrapper.get('[data-testid="a6-settings-save"]').trigger('click')

    expect(wrapper.emitted('save')).toBeUndefined()
    expect(showInfo).toHaveBeenCalledTimes(1)
    expect(showError).not.toHaveBeenCalled()
  })

  it('只提交改动过的字段：访问令牌留空 = 不修改', async () => {
    const wrapper = mountCard(baseSettings())

    await wrapper.get('[data-testid="a6-settings-access-token"]').setValue('sk-brand-new')
    await wrapper.get('[data-testid="a6-settings-save"]').trigger('click')

    expect(savedPayload(wrapper)).toEqual({ a6_access_token: 'sk-brand-new' })
  })

  it('清空输入框不会被当成改动（后端把空串视为不修改）', async () => {
    const wrapper = mountCard(baseSettings())

    await wrapper.get('[data-testid="a6-settings-base-url"]').setValue('   ')
    await wrapper.get('[data-testid="a6-settings-user-id"]').setValue('')
    await wrapper.get('[data-testid="a6-settings-save"]').trigger('click')

    expect(wrapper.emitted('save')).toBeUndefined()
    expect(showInfo).toHaveBeenCalledTimes(1)
  })

  it('汇率按数值比较，改动后随请求提交', async () => {
    const wrapper = mountCard(baseSettings())

    await wrapper.get('[data-testid="a6-settings-fx-rate"]').setValue('7.2')
    await wrapper.get('[data-testid="a6-settings-save"]').trigger('click')

    expect(savedPayload(wrapper)).toEqual({ fx_usd_cny_rate: 7.2 })
  })

  it('站点基址必须以 http(s) 开头，否则拦下并报错', async () => {
    const wrapper = mountCard(baseSettings())

    await wrapper.get('[data-testid="a6-settings-base-url"]').setValue('a6.example.com')
    await wrapper.get('[data-testid="a6-settings-save"]').trigger('click')

    expect(wrapper.emitted('save')).toBeUndefined()
    expect(showError).toHaveBeenCalledTimes(1)
  })

  it('汇率必须是大于 0 的数字', async () => {
    const wrapper = mountCard(baseSettings())

    await wrapper.get('[data-testid="a6-settings-fx-rate"]').setValue('0')
    await wrapper.get('[data-testid="a6-settings-save"]').trigger('click')

    expect(wrapper.emitted('save')).toBeUndefined()
    expect(showError).toHaveBeenCalledTimes(1)
  })

  it('已配置时用脱敏串做占位，并开放「清除令牌」', async () => {
    const wrapper = mountCard(baseSettings())
    const token = wrapper.get('[data-testid="a6-settings-access-token"]')

    expect(token.attributes('type')).toBe('password')
    expect(token.attributes('placeholder')).toBe('已配置：sk-****abcd')

    await wrapper.get('[data-testid="a6-settings-clear-token"]').trigger('click')
    expect(wrapper.emitted('clear-token')).toHaveLength(1)
  })

  it('未配置时不渲染清除按钮，占位提示去填写', () => {
    const wrapper = mountCard(
      baseSettings({ a6_token_configured: false, a6_token_mask: '', fx_usd_cny_rate: 0 })
    )

    expect(wrapper.find('[data-testid="a6-settings-clear-token"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="a6-settings-access-token"]').attributes('placeholder')).toBe(
      'admin.companion.settings.accessTokenPlaceholder'
    )
    // 后端没给有效汇率时回落到默认 6.71
    expect((wrapper.get('[data-testid="a6-settings-fx-rate"]').element as HTMLInputElement).value).toBe(
      '6.71'
    )
  })

  it('保存中禁用全部操作', () => {
    const wrapper = mount(CompanionA6SettingsCard, {
      props: { settings: baseSettings(), saving: true }
    })

    expect(wrapper.get('[data-testid="a6-settings-save"]').attributes('disabled')).toBeDefined()
    expect(wrapper.get('[data-testid="a6-settings-access-token"]').attributes('disabled')).toBeDefined()
  })
})
