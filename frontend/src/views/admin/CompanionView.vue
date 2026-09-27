<template>
  <AppLayout>
    <div class="space-y-6 pb-12">
      <!-- 未配置上游地址：引导态（不提供任何凭据输入框） -->
      <div
        v-if="notConfigured"
        class="rounded-3xl bg-white p-6 shadow-sm ring-1 ring-gray-900/5 dark:bg-dark-800 dark:ring-dark-700 sm:p-8"
      >
        <div class="flex items-start gap-4">
          <div class="flex h-12 w-12 shrink-0 items-center justify-center rounded-2xl bg-amber-100 dark:bg-amber-900/30">
            <Icon name="exclamationTriangle" size="lg" class="text-amber-600 dark:text-amber-400" />
          </div>
          <div class="min-w-0 flex-1">
            <h3 class="text-base font-bold text-gray-900 dark:text-white">
              {{ t('admin.companion.status.setupTitle') }}
            </h3>
            <p class="mt-2 text-sm text-gray-600 dark:text-gray-400">
              {{ t('admin.companion.status.setupIntro') }}
            </p>

            <dl class="mt-4 space-y-3">
              <div class="rounded-xl bg-gray-50 p-3 dark:bg-dark-900">
                <dt class="font-mono text-xs font-bold text-primary-700 dark:text-primary-300">
                  COMPANION_BASE_URL
                </dt>
                <dd class="mt-1 text-xs text-gray-600 dark:text-gray-400">
                  {{ t('admin.companion.status.envBaseUrl') }}
                </dd>
              </div>
              <div class="rounded-xl bg-gray-50 p-3 dark:bg-dark-900">
                <dt class="font-mono text-xs font-bold text-primary-700 dark:text-primary-300">
                  COMPANION_ADMIN_USER
                </dt>
                <dd class="mt-1 text-xs text-gray-600 dark:text-gray-400">
                  {{ t('admin.companion.status.envAdminUser') }}
                </dd>
              </div>
              <div class="rounded-xl bg-gray-50 p-3 dark:bg-dark-900">
                <dt class="font-mono text-xs font-bold text-primary-700 dark:text-primary-300">
                  COMPANION_ADMIN_PASSWORD
                </dt>
                <dd class="mt-1 text-xs text-gray-600 dark:text-gray-400">
                  {{ t('admin.companion.status.envAdminPassword') }}
                </dd>
              </div>
              <div class="rounded-xl bg-gray-50 p-3 dark:bg-dark-900">
                <dt class="font-mono text-xs font-bold text-gray-500 dark:text-gray-400">
                  COMPANION_HTTP_TIMEOUT
                </dt>
                <dd class="mt-1 text-xs text-gray-600 dark:text-gray-400">
                  {{ t('admin.companion.status.envTimeout') }}
                </dd>
              </div>
            </dl>

            <p class="mt-4 flex items-start gap-2 text-xs text-gray-500 dark:text-gray-400">
              <Icon name="lock" size="sm" class="mt-0.5 shrink-0" />
              <span>{{ t('admin.companion.status.networkHint') }}</span>
            </p>

            <button
              type="button"
              class="btn btn-secondary mt-5"
              :disabled="statusLoading"
              @click="retryFromScratch"
            >
              <Icon name="refresh" size="sm" class="mr-1.5" />
              {{ t('admin.companion.status.retry') }}
            </button>
          </div>
        </div>
      </div>

      <template v-else>
        <!-- 顶部状态条 -->
        <div
          class="rounded-3xl bg-white p-5 shadow-sm ring-1 ring-gray-900/5 dark:bg-dark-800 dark:ring-dark-700"
        >
          <div class="flex flex-col gap-4 lg:flex-row lg:items-center lg:justify-between">
            <div class="flex min-w-0 items-start gap-3">
              <span class="mt-1.5 flex h-2.5 w-2.5 shrink-0 rounded-full" :class="statusDotClass"></span>
              <div class="min-w-0">
                <div class="flex flex-wrap items-center gap-2">
                  <span class="text-sm font-bold text-gray-900 dark:text-white">
                    {{ t('admin.companion.status.title') }}
                  </span>
                  <span
                    class="rounded-full px-2.5 py-0.5 text-xs font-semibold"
                    :class="statusBadgeClass"
                  >
                    {{ statusLabel }}
                  </span>
                  <span v-if="lastUpdated" class="text-xs text-gray-400 dark:text-gray-500">
                    {{ t('admin.companion.updatedAt', { time: lastUpdatedText }) }}
                  </span>
                  <span v-else class="text-xs text-gray-400 dark:text-gray-500">
                    {{ t('admin.companion.neverUpdated') }}
                  </span>
                </div>
                <p v-if="statusDetail" class="mt-1 break-all text-xs text-gray-500 dark:text-gray-400">
                  {{ t('admin.companion.status.detailLabel') }}: {{ statusDetail }}
                </p>
              </div>
            </div>

            <div class="flex flex-wrap items-center gap-3">
              <label class="inline-flex items-center gap-2 text-xs font-medium text-gray-600 dark:text-gray-300" :title="t('admin.companion.actions.autoRefreshHint')">
                <input
                  v-model="autoRefresh"
                  type="checkbox"
                  class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500 dark:border-dark-600 dark:bg-dark-800"
                />
                {{ t('admin.companion.actions.autoRefresh') }}
              </label>
              <button
                type="button"
                class="btn btn-primary"
                :disabled="collecting || loading"
                @click="onCollect"
              >
                <Icon name="sync" size="sm" class="mr-1.5" :class="collecting ? 'animate-spin' : ''" />
                {{ collecting ? t('admin.companion.actions.collecting') : t('admin.companion.actions.collect') }}
              </button>
            </div>
          </div>

          <!-- 时间范围 -->
          <div class="mt-5 flex flex-wrap items-center gap-2 border-t border-gray-100 pt-4 dark:border-dark-700">
            <button
              v-for="preset in presets"
              :key="preset"
              type="button"
              class="rounded-xl px-3 py-1.5 text-xs font-semibold transition-colors"
              :class="
                range === preset
                  ? 'bg-primary-600 text-white'
                  : 'bg-gray-100 text-gray-600 hover:bg-gray-200 dark:bg-dark-900 dark:text-gray-300 dark:hover:bg-dark-700'
              "
              @click="selectRange(preset)"
            >
              {{ t(RANGE_LABEL_KEY[preset]) }}
            </button>

            <div class="ml-auto flex flex-wrap items-center gap-2">
              <label class="flex items-center gap-2 text-xs text-gray-500 dark:text-gray-400">
                {{ t('admin.companion.range.start') }}
                <input v-model="customFrom" type="datetime-local" step="1" class="input w-52 py-1.5 text-xs" />
              </label>
              <label class="flex items-center gap-2 text-xs text-gray-500 dark:text-gray-400">
                {{ t('admin.companion.range.end') }}
                <input v-model="customTo" type="datetime-local" step="1" class="input w-52 py-1.5 text-xs" />
              </label>
              <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" @click="applyCustomRange">
                {{ t('admin.companion.range.apply') }}
              </button>
              <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" @click="refresh">
                <Icon name="refresh" size="sm" class="mr-1.5" />
                {{ t('admin.companion.actions.refresh') }}
              </button>
            </div>
          </div>

          <p v-if="rangeSummary" class="mt-3 text-xs text-gray-400 dark:text-gray-500">
            {{ rangeSummary }}
          </p>
        </div>

        <!-- 错误提示（未配置态已单独处理） -->
        <div
          v-if="errorInfo"
          class="rounded-2xl bg-red-50 p-4 text-sm text-red-600 dark:bg-red-900/20 dark:text-red-400"
        >
          <div class="flex items-start gap-2">
            <Icon name="exclamationCircle" size="md" class="mt-0.5 shrink-0" />
            <div class="min-w-0">
              <p class="font-semibold">{{ errorTitle }}</p>
              <p v-if="errorInfo.message" class="mt-1 break-all font-mono text-xs opacity-80">
                {{ errorInfo.message }}
              </p>
            </div>
          </div>
        </div>

        <!-- 首次加载骨架 -->
        <div v-if="loading && !firstLoadDone" class="space-y-6">
          <div class="grid grid-cols-2 gap-4 md:grid-cols-4">
            <div
              v-for="i in 8"
              :key="i"
              class="h-24 animate-pulse rounded-3xl bg-gray-100 dark:bg-dark-800"
            ></div>
          </div>
          <div class="h-[380px] animate-pulse rounded-3xl bg-gray-100 dark:bg-dark-800"></div>
        </div>

        <template v-else>
          <!-- 汇总卡片：对账/待对账/上游待匹配/记录总数可点击筛选明细 -->
          <div class="grid grid-cols-2 gap-4 md:grid-cols-4">
            <button
              v-for="card in metricCards"
              :key="card.key"
              type="button"
              class="min-w-0 rounded-3xl bg-white p-4 text-left shadow-sm ring-1 ring-gray-900/5 dark:bg-dark-800 dark:ring-dark-700"
              :class="
                card.filter
                  ? [
                      'cursor-pointer transition-colors hover:ring-primary-400',
                      statusFilter === card.filter ? 'ring-2 ring-primary-500' : ''
                    ]
                  : 'cursor-default'
              "
              :title="card.hint"
              @click="card.filter ? applyStatusFilter(card.filter) : undefined"
            >
              <span class="block text-xs font-medium text-gray-500 dark:text-gray-400">{{ card.label }}</span>
              <strong class="mt-1 block truncate text-xl font-bold text-gray-900 dark:text-white" :title="card.value">
                {{ card.value }}
              </strong>
              <span v-if="card.extra" class="mt-1 block truncate text-xs text-gray-400 dark:text-gray-500">
                {{ card.extra }}
              </span>
            </button>
          </div>

          <!-- 趋势图 -->
          <CompanionTrendChart
            v-model:mode="chartMode"
            :points="timeseries?.points ?? []"
            :bucket="timeseries?.bucket ?? ''"
            :loading="loading"
          />

          <!-- 账号规则 -->
          <CompanionRulesTable
            :items="rules?.items ?? []"
            :unconfigured-count="rules?.unconfigured_accounts ?? 0"
            :loading="loading && !firstLoadDone"
            :saving-id="savingId"
            :removing-id="removingId"
            @save="onRuleSave"
            @remove="onRuleRemoveRequest"
          />

          <!-- 调用明细 -->
          <CompanionRequestsTable
            :items="requests?.items ?? []"
            :loading="loading && !firstLoadDone"
            :total="requests?.total ?? 0"
            :page="page"
            :page-size="pageSize"
            :status="statusFilter"
            @update:page="onPageChange"
            @update:pageSize="onPageSizeChange"
            @update:status="applyStatusFilter"
          />
        </template>
      </template>

      <!-- 删除规则确认 -->
      <ConfirmDialog
        :show="pendingRemove !== null"
        :title="t('admin.companion.rules.removeConfirmTitle')"
        :message="t('admin.companion.rules.removeConfirmMessage')"
        :confirm-text="t('admin.companion.rules.removeConfirm')"
        :cancel-text="t('common.cancel')"
        danger
        @confirm="onRuleRemoveConfirmed"
        @cancel="pendingRemove = null"
      />
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores'
import AppLayout from '@/components/layout/AppLayout.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import CompanionTrendChart from './companion/CompanionTrendChart.vue'
import CompanionRulesTable from './companion/CompanionRulesTable.vue'
import CompanionRequestsTable from './companion/CompanionRequestsTable.vue'
import {
  companionAPI,
  classifyCompanionError,
  type CompanionAccountRuleList,
  type CompanionErrorInfo,
  type CompanionRequestPage,
  type CompanionRequestStatus,
  type CompanionStatus,
  type CompanionSummary,
  type CompanionTimeSeries,
  type CompanionTimeWindowParams
} from '@/api/admin/companion'

type RangeKind = 'today' | '24h' | '3d' | '7d' | 'month' | 'last-month' | '3m' | '1y' | 'custom'
type ChartMode = 'money' | 'count'

const { t } = useI18n()
const appStore = useAppStore()

const AUTO_REFRESH_MS = 30000

const presets: RangeKind[] = ['today', '24h', '3d', '7d', 'month', 'last-month', '3m', '1y']

/** 快捷区间文案沿用全局静态 key，便于 i18n 完整性测试覆盖 */
const RANGE_LABEL_KEY: Record<RangeKind, string> = {
  today: 'admin.companion.range.today',
  '24h': 'admin.companion.range.h24',
  '3d': 'admin.companion.range.d3',
  '7d': 'admin.companion.range.d7',
  month: 'admin.companion.range.month',
  'last-month': 'admin.companion.range.lastMonth',
  '3m': 'admin.companion.range.m3',
  '1y': 'admin.companion.range.y1',
  custom: 'admin.companion.range.custom'
}

const loading = ref(false)
const collecting = ref(false)
const savingId = ref<number | null>(null)
const removingId = ref<number | null>(null)
const pendingRemove = ref<number | null>(null)
const firstLoadDone = ref(false)
const statusLoading = ref(false)

const statusInfo = ref<CompanionStatus | null>(null)
const statusError = ref<CompanionErrorInfo | null>(null)
const errorInfo = ref<CompanionErrorInfo | null>(null)

const summary = ref<CompanionSummary | null>(null)
const timeseries = ref<CompanionTimeSeries | null>(null)
const requests = ref<CompanionRequestPage | null>(null)
const rules = ref<CompanionAccountRuleList | null>(null)

const range = ref<RangeKind>('24h')
const customFrom = ref('')
const customTo = ref('')
const page = ref(1)
const pageSize = ref(50)
const statusFilter = ref<CompanionRequestStatus>('all')
const chartMode = ref<ChartMode>('money')
const autoRefresh = ref(false)
const lastUpdated = ref<Date | null>(null)

let timer: ReturnType<typeof setInterval> | null = null

// ==================== 时间范围 ====================

function pad(value: number): string {
  return String(value).padStart(2, '0')
}

function toDatetimeLocal(date: Date): string {
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`
}

/** 与 Companion 原看板一致的自然日/滚动窗口算法（按浏览器本地时区，再转 UTC 传给后端） */
function rangeDates(kind: RangeKind, now = new Date()): { from: Date; to: Date } {
  let from: Date
  let to = new Date(now)
  switch (kind) {
    case 'today':
      from = new Date(now.getFullYear(), now.getMonth(), now.getDate())
      break
    case '3d':
      from = new Date(now.getTime() - 3 * 86400000)
      break
    case '7d':
      from = new Date(now.getTime() - 7 * 86400000)
      break
    case 'month':
      from = new Date(now.getFullYear(), now.getMonth(), 1)
      break
    case 'last-month':
      from = new Date(now.getFullYear(), now.getMonth() - 1, 1)
      to = new Date(now.getFullYear(), now.getMonth(), 1)
      break
    case '3m':
      from = new Date(now)
      from.setMonth(from.getMonth() - 3)
      break
    case '1y':
      from = new Date(now)
      from.setFullYear(from.getFullYear() - 1)
      break
    case '24h':
    default:
      from = new Date(now.getTime() - 86400000)
      break
  }
  return { from, to }
}

function setInputs(from: Date, to: Date) {
  customFrom.value = toDatetimeLocal(from)
  customTo.value = toDatetimeLocal(to)
}

/** 快捷区间：滚动窗口在每次加载前重新计算，避免自动刷新时范围停滞 */
function selectRange(kind: RangeKind, options: { reload?: boolean } = {}) {
  range.value = kind
  const { from, to } = rangeDates(kind)
  setInputs(from, to)
  page.value = 1
  if (options.reload !== false) void load()
}

function buildWindow(): CompanionTimeWindowParams {
  const from = new Date(customFrom.value)
  const to = new Date(customTo.value)
  if (Number.isNaN(from.getTime()) || Number.isNaN(to.getTime()) || from >= to) {
    throw new Error('INVALID_RANGE')
  }
  return { from: from.toISOString(), to: to.toISOString() }
}

function applyCustomRange() {
  const from = new Date(customFrom.value)
  const to = new Date(customTo.value)
  if (Number.isNaN(from.getTime()) || Number.isNaN(to.getTime()) || from >= to) {
    appStore.showError(t('admin.companion.range.invalid'))
    return
  }
  range.value = 'custom'
  page.value = 1
  void load()
}

const rangeSummary = computed(() => {
  if (!customFrom.value || !customTo.value) return ''
  const from = new Date(customFrom.value)
  const to = new Date(customTo.value)
  if (Number.isNaN(from.getTime()) || Number.isNaN(to.getTime())) return ''
  return `${from.toLocaleString()} → ${to.toLocaleString()}`
})

// ==================== 加载 ====================

async function refreshStatus() {
  statusLoading.value = true
  try {
    statusInfo.value = await companionAPI.getStatus()
    statusError.value = null
  } catch (err) {
    statusError.value = classifyCompanionError(err)
  } finally {
    statusLoading.value = false
  }
}

async function load(options: { silent?: boolean } = {}) {
  if (loading.value) return
  if (statusInfo.value?.enabled === false) return

  let timeWindow: CompanionTimeWindowParams
  try {
    timeWindow = buildWindow()
  } catch {
    if (!options.silent) appStore.showError(t('admin.companion.range.invalid'))
    return
  }

  loading.value = true
  try {
    const [summaryData, seriesData, requestData, ruleData] = await Promise.all([
      companionAPI.getSummary(timeWindow),
      companionAPI.getTimeseries(timeWindow),
      companionAPI.getRequests({
        ...timeWindow,
        status: statusFilter.value,
        page: page.value,
        page_size: pageSize.value
      }),
      companionAPI.getAccountRules(timeWindow)
    ])
    summary.value = summaryData
    timeseries.value = seriesData
    requests.value = requestData
    rules.value = ruleData
    errorInfo.value = null
    lastUpdated.value = new Date()
  } catch (err) {
    // 未配置态会整体切到引导页；其余错误在原位提示，并保留上一次已加载的数据
    const info = classifyCompanionError(err)
    errorInfo.value = info
    if (!options.silent) {
      appStore.showError(info.message || t('admin.companion.actions.refreshFailed'))
    }
    statusInfo.value = { ...(statusInfo.value ?? { enabled: true, healthy: false }), healthy: false }
  } finally {
    loading.value = false
    firstLoadDone.value = true
  }
}

function refresh() {
  void load()
}

function retryFromScratch() {
  void (async () => {
    await refreshStatus()
    if (statusInfo.value?.enabled === false) return
    selectRange(range.value === 'custom' ? '24h' : range.value)
  })()
}

async function onCollect() {
  if (collecting.value) return
  collecting.value = true
  try {
    await companionAPI.collect()
    // 滚动窗口跟随当前时间推进，避免「立即同步」后看到旧区间
    if (range.value !== 'custom') {
      const { from, to } = rangeDates(range.value)
      setInputs(from, to)
    }
    page.value = 1
    await load()
    appStore.showSuccess(t('admin.companion.actions.collectSuccess'))
  } catch (err) {
    const info = classifyCompanionError(err)
    if (info.kind === 'not_configured') {
      statusInfo.value = { enabled: false, healthy: false }
    } else {
      appStore.showError(info.message || t('admin.companion.actions.collectFailed'))
    }
  } finally {
    collecting.value = false
  }
}

function applyStatusFilter(status: CompanionRequestStatus) {
  statusFilter.value = status
  page.value = 1
  void load()
}

function onPageChange(next: number) {
  page.value = next
  void load()
}

function onPageSizeChange(next: number) {
  pageSize.value = next
  page.value = 1
  void load()
}

// 自动刷新：默认关闭，30 秒一次，页面处于后台时暂停
function startTimer() {
  stopTimer()
  timer = setInterval(() => {
    if (!autoRefresh.value || document.hidden) return
    if (range.value !== 'custom') {
      const { from, to } = rangeDates(range.value)
      setInputs(from, to)
    }
    void load({ silent: true })
  }, AUTO_REFRESH_MS)
}

function stopTimer() {
  if (timer !== null) {
    clearInterval(timer)
    timer = null
  }
}

// ==================== 账号规则 ====================

async function onRuleSave(payload: { accountId: number; provider: 'a6' | 'subarx'; value: string }) {
  savingId.value = payload.accountId
  try {
    await companionAPI.upsertAccountRule(
      payload.accountId,
      payload.provider === 'a6'
        ? { provider: 'a6', token_name: payload.value }
        : { provider: 'subarx', multiplier: payload.value }
    )
    appStore.showSuccess(t('admin.companion.rules.saved', { id: payload.accountId }))
    await load()
  } catch (err) {
    appStore.showError(classifyCompanionError(err).message || t('admin.companion.rules.saveFailed'))
  } finally {
    savingId.value = null
  }
}

function onRuleRemoveRequest(accountId: number) {
  pendingRemove.value = accountId
}

async function onRuleRemoveConfirmed() {
  const accountId = pendingRemove.value
  pendingRemove.value = null
  if (accountId === null) return
  removingId.value = accountId
  try {
    await companionAPI.deleteAccountRule(accountId)
    appStore.showSuccess(t('admin.companion.rules.removed', { id: accountId }))
    await load()
  } catch (err) {
    appStore.showError(classifyCompanionError(err).message || t('admin.companion.rules.removeFailed'))
  } finally {
    removingId.value = null
  }
}

// ==================== 展示 ====================

const notConfigured = computed(
  () => statusInfo.value?.enabled === false || errorInfo.value?.kind === 'not_configured'
)

const statusDetail = computed(() => statusInfo.value?.detail || statusError.value?.message || '')

const statusLabel = computed(() => {
  if (!statusInfo.value || statusInfo.value.enabled === false) return t('admin.companion.status.disabled')
  return statusInfo.value.healthy ? t('admin.companion.status.healthy') : t('admin.companion.status.unhealthy')
})

const statusDotClass = computed(() => {
  if (!statusInfo.value?.enabled) return 'bg-gray-400'
  return statusInfo.value.healthy ? 'bg-green-500' : 'bg-amber-500'
})

const statusBadgeClass = computed(() => {
  const base = 'rounded-full px-2.5 py-0.5 text-xs font-semibold '
  if (!statusInfo.value?.enabled) {
    return base + 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'
  }
  return statusInfo.value.healthy
    ? base + 'bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-300'
    : base + 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
})

const errorTitle = computed(() => {
  const kind = errorInfo.value?.kind
  if (!kind) return ''
  switch (kind) {
    case 'unreachable':
      return `${t('admin.companion.status.errorTitle')} · ${t('admin.companion.status.unreachable')}`
    case 'auth_failed':
      return `${t('admin.companion.status.errorTitle')} · ${t('admin.companion.status.authFailed')}`
    case 'bad_request':
      return `${t('admin.companion.status.errorTitle')} · ${t('admin.companion.status.badRequest')}`
    case 'upstream':
      return `${t('admin.companion.status.errorTitle')} · ${t('admin.companion.status.upstream')}`
    case 'invalid_response':
      return `${t('admin.companion.status.errorTitle')} · ${t('admin.companion.status.invalidResponse')}`
    default:
      return `${t('admin.companion.status.errorTitle')} · ${t('admin.companion.status.unknown')}`
  }
})

const lastUpdatedText = computed(() =>
  lastUpdated.value ? lastUpdated.value.toLocaleTimeString() : ''
)

/** 与 Companion 原看板一致的金额格式：按量级自适应小数位 */
function formatCny(value: string | number | null | undefined): string {
  if (value === '' || value === null || value === undefined) return '—'
  const num = Number(value)
  if (!Number.isFinite(num)) return String(value)
  if (num === 0) return '¥0'
  const sign = num < 0 ? '-' : ''
  const abs = Math.abs(num)
  if (abs >= 100) return `${sign}¥${abs.toFixed(0)}`
  if (abs >= 1) return `${sign}¥${abs.toFixed(2)}`
  if (abs >= 0.01) return `${sign}¥${abs.toFixed(4)}`
  return `${sign}¥${abs.toFixed(8)}`
}

function formatCount(value: number | null | undefined): string {
  return typeof value === 'number' ? value.toLocaleString() : '—'
}

const fxText = computed(() => {
  const data = summary.value
  if (!data || !data.fx_usd_cny) return t('admin.companion.summary.fxMissing')
  const base = t('admin.companion.summary.fxRate', {
    rate: data.fx_usd_cny,
    source: data.fx_source || '—'
  })
  const stale = data.fx_stale ? ` · ${t('admin.companion.summary.fxStale')}` : ''
  const gap =
    Number(data.subarx_unallocated_cost) > 0
      ? ` · ${t('admin.companion.summary.subarxGap', { amount: formatCny(data.subarx_unallocated_cost) })}`
      : ''
  return `${base}${stale}${gap}`
})

interface MetricCard {
  key: string
  label: string
  value: string
  hint: string
  extra?: string
  filter?: CompanionRequestStatus
}

const metricCards = computed<MetricCard[]>(() => {
  const data = summary.value
  return [
    {
      key: 'revenue',
      label: t('admin.companion.summary.revenue'),
      value: formatCny(data?.revenue),
      hint: t('admin.companion.summary.recordTotalHint'),
      extra: fxText.value
    },
    {
      key: 'cost',
      label: t('admin.companion.summary.upstreamCost'),
      value: formatCny(data?.upstream_cost),
      hint: t('admin.companion.summary.billedCount'),
      extra: `${t('admin.companion.summary.billedCount')}: ${formatCount(data?.billed_count)}`
    },
    {
      key: 'profit',
      label: t('admin.companion.summary.grossProfit'),
      value: formatCny(data?.gross_profit),
      hint: t('admin.companion.summary.matchedHint'),
      extra: `${t('admin.companion.summary.calculated')}: ${formatCount(data?.calculated_count)}`
    },
    {
      key: 'margin',
      label: t('admin.companion.summary.marginPercent'),
      value: data ? `${data.margin_percent}%` : '—',
      hint: t('admin.companion.summary.subarxUnallocated'),
      extra: `${t('admin.companion.summary.subarxUnallocated')}: ${formatCny(data?.subarx_unallocated_cost)}`
    },
    {
      key: 'matched',
      label: t('admin.companion.summary.matched'),
      value: formatCount(data?.downstream_matched),
      hint: t('admin.companion.summary.matchedHint'),
      filter: 'matched'
    },
    {
      key: 'unmatched',
      label: t('admin.companion.summary.unmatched'),
      value: formatCount(data?.downstream_unmatched),
      hint: t('admin.companion.summary.unmatchedHint'),
      filter: 'unmatched'
    },
    {
      key: 'upstreamUnmatched',
      label: t('admin.companion.summary.upstreamUnmatched'),
      value: formatCount(data?.upstream_unmatched),
      hint: t('admin.companion.summary.upstreamUnmatchedHint'),
      filter: 'upstream_unmatched'
    },
    {
      key: 'recordTotal',
      label: t('admin.companion.summary.recordTotal'),
      value: formatCount(data?.record_total),
      hint: t('admin.companion.summary.recordTotalHint'),
      filter: 'all'
    }
  ]
})

// ==================== 生命周期 ====================

onMounted(async () => {
  startTimer()
  await refreshStatus()
  if (statusInfo.value?.enabled === false) {
    firstLoadDone.value = true
    return
  }
  selectRange('24h')
})

onUnmounted(stopTimer)
</script>
