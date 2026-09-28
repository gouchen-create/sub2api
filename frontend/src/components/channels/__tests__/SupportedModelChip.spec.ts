import { afterEach, describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import SupportedModelChip from '../SupportedModelChip.vue'
import zh from '@/i18n/locales/zh'
import { BILLING_MODE_TOKEN } from '@/constants/channel'
import type { UserSupportedModel } from '@/api/channels'

function mountChip(model: UserSupportedModel) {
  const i18n = createI18n({
    legacy: false,
    locale: 'zh',
    messages: { zh },
  })

  return mount(SupportedModelChip, {
    props: { model },
    global: {
      plugins: [i18n],
      stubs: {
        PlatformIcon: true,
      },
    },
    attachTo: document.body,
  })
}

afterEach(() => {
  document.body.innerHTML = ''
})

describe('SupportedModelChip', () => {
  it('shows yuan in Chinese per-million token pricing units', async () => {
    const wrapper = mountChip({
      name: 'gpt-test',
      platform: 'openai',
      pricing: {
        billing_mode: BILLING_MODE_TOKEN,
        input_price: 0.000003,
        output_price: 0.000012,
        cache_write_price: null,
        cache_read_price: null,
        image_output_price: null,
        per_request_price: null,
        intervals: [],
      },
    } as UserSupportedModel)

    await wrapper.get('[tabindex="0"]').trigger('mouseenter')

    expect(document.body.textContent).toContain('$3元 / 1M token')
    expect(document.body.textContent).toContain('$12元 / 1M token')
  })
})
