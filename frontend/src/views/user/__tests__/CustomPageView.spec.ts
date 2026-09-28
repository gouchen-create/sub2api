import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { nextTick } from 'vue'

import CustomPageView from '../CustomPageView.vue'
import { useAppStore, useAuthStore } from '@/stores'

const routeState = vi.hoisted(() => ({
  params: { id: 'support-guide' },
}))

vi.mock('vue-router', () => ({
  useRoute: () => routeState,
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    locale: { value: 'zh-CN' },
    t: (key: string) => key,
  }),
}))

vi.mock('@/components/layout/AppLayout.vue', () => ({
  default: { template: '<div><slot /></div>' },
}))

vi.mock('@/components/icons/Icon.vue', () => ({
  default: { template: '<span />' },
}))

describe('CustomPageView markdown pages', () => {
  let pinia: ReturnType<typeof createPinia>
  const feishuDocUrl = 'https://ucn7260o64vr.feishu.cn/wiki/O2G3wXetEislsIkrYWScVwLZnCd'

  beforeEach(() => {
    vi.restoreAllMocks()
    routeState.params.id = 'support-guide'
    document.documentElement.className = ''
    pinia = createPinia()
    setActivePinia(pinia)
  })

  it('loads configured md: pages as first-party markdown content', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      text: async () =>
        `# Support Guide\n\n<a href="${feishuDocUrl}" target="_blank" rel="noopener noreferrer">Open Feishu guide</a>`,
    })
    vi.stubGlobal('fetch', fetchMock)

    const appStore = useAppStore()
    const authStore = useAuthStore()
    appStore.cachedPublicSettings = {
      custom_menu_items: [
        {
          id: 'support-guide',
          label: 'Support Guide',
          icon_svg: '',
          url: 'md:support-guide',
          visibility: 'user',
          sort_order: 0,
        },
      ],
    } as typeof appStore.cachedPublicSettings
    appStore.publicSettingsLoaded = true
    authStore.token = 'test-token'

    const wrapper = mount(CustomPageView, {
      global: {
        plugins: [pinia],
      },
    })

    await nextTick()
    await flushPromises()

    expect(fetchMock).toHaveBeenCalledWith(
      expect.stringContaining('/pages/support-guide'),
      expect.objectContaining({
        headers: { Authorization: 'Bearer test-token' },
      }),
    )
    expect(wrapper.html()).toContain('Support Guide')
    const docLink = wrapper.find(`a[href="${feishuDocUrl}"]`)
    expect(docLink.exists()).toBe(true)
    expect(docLink.attributes('target')).toBe('_blank')
    expect(docLink.attributes('rel')).toContain('noopener')
    expect(docLink.attributes('rel')).toContain('noreferrer')
    expect(wrapper.find('iframe').exists()).toBe(false)
  })
})
