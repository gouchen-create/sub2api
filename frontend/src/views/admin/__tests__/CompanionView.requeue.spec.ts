import { describe, expect, it, beforeEach, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'

import CompanionView from '../CompanionView.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import type {
  CompanionRequeueUnmatchedResult,
  CompanionSettings,
  CompanionStatus
} from '@/api/admin/companion'

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
  collect: vi.fn(),
  requeueUnmatched: vi.fn()
}))

/** 提示必须能被断言：失败要走既有的 classifyCompanionError + showError 通道 */
const store = vi.hoisted(() => ({
  showInfo: vi.fn(),
  showSuccess: vi.fn(),
  showError: vi.fn(),
  showWarning: vi.fn()
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
      // 保留 key 便于断言；带参数时把参数值按顺序拼在后面，
      // 这样「退回条数」与「匹配条数」有没有真的插值、有没有串位都能验证
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key} ${Object.values(params).join(' ')}` : key,
      locale: { value: 'zh' }
    })
  }
})

vi.mock('@/stores', () => ({
  useAppStore: () => store
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

function requeueResult(
  overrides: Partial<CompanionRequeueUnmatchedResult> = {}
): CompanionRequeueUnmatchedResult {
  return {
    success: true,
    requeued: 1000,
    matched: 812,
    from: '2026-09-30T00:00:00Z',
    to: '2026-10-02T00:00:00Z',
    ...overrides
  }
}

async function settle(times = 3) {
  for (let i = 0; i < times; i += 1) {
    await flushPromises()
  }
}

async function mountView() {
  const wrapper = mount(CompanionView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        CompanionTrendChart: true,
        CompanionRulesTable: true,
        CompanionRequestsTable: true,
        ConfirmDialog: true
      }
    }
  })
  await settle()
  return wrapper
}

function button(wrapper: VueWrapper) {
  return wrapper.get('[data-testid="companion-requeue-button"]')
}

function shownDialog(wrapper: VueWrapper) {
  return wrapper.findAllComponents(ConfirmDialog).find((dialog) => dialog.props('show'))
}

/** 点按钮 → 二次确认框必须弹出，然后把确认框返回给调用方决定确认还是取消 */
async function openConfirm(wrapper: VueWrapper) {
  await button(wrapper).trigger('click')
  await settle()
  const dialog = shownDialog(wrapper)
  if (!dialog) throw new Error('点击「退回重试」后必须先弹出二次确认')
  return dialog
}

/** 页面当前窗口：直接读页面上那两个时间输入框，避免把窗口写死在测试里 */
function currentWindow(wrapper: VueWrapper) {
  const inputs = wrapper.findAll('input[type="datetime-local"]')
  expect(inputs).toHaveLength(2)
  return {
    from: new Date((inputs[0].element as HTMLInputElement).value).toISOString(),
    to: new Date((inputs[1].element as HTMLInputElement).value).toISOString()
  }
}

describe('CompanionView 退回重试未匹配账单', () => {
  beforeEach(() => {
    Object.values(api).forEach((fn) => fn.mockReset())
    Object.values(store).forEach((fn) => fn.mockReset())
    api.getStatus.mockResolvedValue(status())
    api.getSettings.mockResolvedValue(settings())
    api.getSummary.mockResolvedValue({ currency: 'CNY', fx_usd_cny: '6.71', fx_source: 'page' })
    api.getTimeseries.mockResolvedValue({ points: [], bucket: '1小时' })
    api.getRequests.mockResolvedValue({ items: [], total: 0 })
    api.getAccountRules.mockResolvedValue({
      items: [],
      unconfigured_accounts: 0,
      unconfigured_groups: 0,
      groups: []
    })
  })

  it('取消确认时不发请求，也不会改账单状态', async () => {
    const wrapper = await mountView()

    const dialog = await openConfirm(wrapper)
    // 动作必须带二次确认：标题与正文都要说清这是写操作
    expect(dialog.props('title')).toBe('admin.companion.requeue.confirmTitle')
    expect(dialog.props('message')).toBe('admin.companion.requeue.confirmBody')

    dialog.vm.$emit('cancel')
    await settle()

    expect(api.requeueUnmatched).not.toHaveBeenCalled()
    expect(shownDialog(wrapper)).toBeUndefined()

    wrapper.unmount()
  })

  it('确认后按页面当前窗口调用 requeueUnmatched（与同页其它接口同一套窗口）', async () => {
    const wrapper = await mountView()

    const expected = currentWindow(wrapper)
    const dialog = await openConfirm(wrapper)
    dialog.vm.$emit('confirm')
    await settle()

    expect(api.requeueUnmatched).toHaveBeenCalledTimes(1)
    expect(api.requeueUnmatched).toHaveBeenCalledWith(expected)
    // 同页其它接口拿到的就是同一个窗口：后端对 from/to 是同一套解析
    expect(api.getSummary).toHaveBeenCalledWith(expected)

    wrapper.unmount()
  })

  it('成功后同时展示退回条数与实际匹配上的条数，并提示可继续点击', async () => {
    api.requeueUnmatched.mockResolvedValue(requeueResult({ requeued: 1000, matched: 812 }))

    const wrapper = await mountView()

    const dialog = await openConfirm(wrapper)
    dialog.vm.$emit('confirm')
    await settle()

    const result = wrapper.get('[data-testid="companion-requeue-result"]')
    expect(result.text()).toContain('admin.companion.requeue.success 1000 812')
    // 只看退回条数看不出规则改动有没有生效，两个数字缺一不可
    expect(result.text()).toContain('1000')
    expect(result.text()).toContain('812')
    expect(result.text()).toContain('admin.companion.requeue.moreHint')

    wrapper.unmount()
  })

  it('一条也没退回时不提示「再次点击」', async () => {
    api.requeueUnmatched.mockResolvedValue(requeueResult({ requeued: 0, matched: 0 }))

    const wrapper = await mountView()

    const dialog = await openConfirm(wrapper)
    dialog.vm.$emit('confirm')
    await settle()

    const result = wrapper.get('[data-testid="companion-requeue-result"]')
    expect(result.text()).toContain('admin.companion.requeue.success 0 0')
    expect(result.text()).not.toContain('admin.companion.requeue.moreHint')

    wrapper.unmount()
  })

  it('请求进行中按钮进入 loading 且不可再点，不会重复调用', async () => {
    let resolveRequeue: (value: CompanionRequeueUnmatchedResult) => void = () => {}
    api.requeueUnmatched.mockImplementation(
      () =>
        new Promise<CompanionRequeueUnmatchedResult>((resolve) => {
          resolveRequeue = resolve
        })
    )

    const wrapper = await mountView()

    const dialog = await openConfirm(wrapper)
    dialog.vm.$emit('confirm')
    await settle()

    const pending = button(wrapper)
    expect(pending.attributes('disabled')).toBeDefined()
    expect(pending.text()).toContain('admin.companion.requeue.running')

    // 再点一次：既不重开确认框，也不会发出第二个请求（后端是写操作，不能重复触发）
    await pending.trigger('click')
    await settle()
    expect(shownDialog(wrapper)).toBeUndefined()
    expect(api.requeueUnmatched).toHaveBeenCalledTimes(1)

    resolveRequeue(requeueResult())
    await settle()
    expect(button(wrapper).attributes('disabled')).toBeUndefined()

    wrapper.unmount()
  })

  it('失败时走既有错误提示通道，展示服务端原文', async () => {
    api.requeueUnmatched.mockRejectedValue({ status: 502, message: 'COMPANION_UNREACHABLE' })

    const wrapper = await mountView()

    const dialog = await openConfirm(wrapper)
    dialog.vm.$emit('confirm')
    await settle()

    // classifyCompanionError 归类后取服务端 message，与「立即同步」失败时同一个通道
    expect(store.showError).toHaveBeenCalledWith('COMPANION_UNREACHABLE')
    expect(wrapper.find('[data-testid="companion-requeue-result"]').exists()).toBe(false)

    wrapper.unmount()
  })
})
