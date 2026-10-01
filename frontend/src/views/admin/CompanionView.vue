<template>
  <AppLayout>
    <div class="space-y-6 pb-12">
      <!-- 顶部状态条：任何状态下都展示，先看探测结果再去改配置 -->
      <div
        class="rounded-3xl bg-white p-5 shadow-sm ring-1 ring-gray-900/5 dark:bg-dark-800 dark:ring-dark-700"
      >
        <div class="flex flex-col gap-4 lg:flex-row lg:items-center lg:justify-between">
          <div class="flex min-w-0 items-start gap-3">
            <span
              class="mt-1.5 flex h-2.5 w-2.5 shrink-0 rounded-full"
              data-testid="companion-status-dot"
              :class="statusDotClass"
            ></span>
            <div class="min-w-0">
              <div class="flex flex-wrap items-center gap-2">
                <span class="text-sm font-bold text-gray-900 dark:text-white">
                  {{ t('admin.companion.status.title') }}
                </span>
                <span
                  class="rounded-full px-2.5 py-0.5 text-xs font-semibold"
                  data-testid="companion-status-badge"
                  :class="statusBadgeClass"
                >
                  {{ statusLabel }}
                </span>
                <!-- 不可达时优先展示「最近失败」：那才是排查要看的时刻。
                     「更新于」是页面刷新时刻，在故障场景下只会误导人。
                     三分支互斥，任何时刻仍只渲染一个行内 span：卡片不新增行、不改变高度。 -->
                <span
                  v-if="statusTone === 'unhealthy' && lastErrorAtText"
                  class="text-xs text-gray-400 dark:text-gray-500"
                  data-testid="companion-last-error-at"
                >
                  {{ t('admin.companion.status.lastErrorAt', { time: lastErrorAtText }) }}
                </span>
                <span v-else-if="lastUpdated" class="text-xs text-gray-400 dark:text-gray-500">
                  {{ t('admin.companion.updatedAt', { time: lastUpdatedText }) }}
                </span>
                <span v-else class="text-xs text-gray-400 dark:text-gray-500">
                  {{ t('admin.companion.neverUpdated') }}
                </span>
              </div>
              <p v-if="statusDetail" class="mt-1 break-all text-xs text-gray-500 dark:text-gray-400">
                {{ t('admin.companion.status.detailLabel') }}: {{ statusDetail }}
              </p>
              <!-- 采集积压告警：单轮上限被塞满时游标只推进到本批最后一行，
                   后面的调用要等下一轮。积压数字必须让人看见——它是「收入正在少算」
                   的唯一信号，藏在日志里等于没有。 -->
              <p
                v-if="usageBacklogText"
                class="mt-1 text-xs font-medium text-amber-600 dark:text-amber-400"
                data-testid="companion-usage-backlog"
              >
                {{ usageBacklogText }}
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
            <!-- 退回重试：孤儿账单不会自动重试，改完规则必须由管理员显式触发一次；
                 与「立即同步」同区，因为它俩都是改数据、看完板的写操作 -->
            <button
              type="button"
              class="btn btn-secondary"
              data-testid="companion-requeue-button"
              :disabled="requeueing || loading"
              @click="onRequeueRequest"
            >
              <Icon name="refresh" size="sm" class="mr-1.5" :class="requeueing ? 'animate-spin' : ''" />
              {{
                requeueing
                  ? t('admin.companion.requeue.running')
                  : t('admin.companion.requeue.button')
              }}
            </button>
          </div>
        </div>

        <!-- 退回重试结果：退回条数与随后一轮实际匹配上的条数必须同时给出，
             只看退回条数看不出「改对的规则到底有没有生效」 -->
        <div
          v-if="requeueResult"
          class="mt-4 rounded-2xl bg-gray-50 px-3 py-2 dark:bg-dark-900"
          data-testid="companion-requeue-result"
        >
          <p class="text-xs font-medium text-gray-700 dark:text-gray-200">
            {{
              t('admin.companion.requeue.success', {
                requeued: requeueResult.requeued,
                matched: requeueResult.matched
              })
            }}
          </p>
          <p
            v-if="requeueResult.requeued > 0"
            class="mt-1 text-xs text-amber-600 dark:text-amber-400"
          >
            {{ t('admin.companion.requeue.moreHint') }}
          </p>
        </div>

        <!-- 时间范围：未配置上游时看板不可读，这里一并收起 -->
        <div
          v-if="!notConfigured"
          class="mt-5 flex flex-wrap items-center gap-2 border-t border-gray-100 pt-4 dark:border-dark-700"
        >
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

        <p v-if="rangeSummary && !notConfigured" class="mt-3 text-xs text-gray-400 dark:text-gray-500">
          {{ rangeSummary }}
        </p>
      </div>

      <!-- 未配置引导：文案指向下方「上游 A6 配置」，凭据只提交给主服务 -->
      <div
        v-if="showSetupGuide"
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
            <p v-if="guideCause" class="mt-2 break-all text-sm text-gray-600 dark:text-gray-400">
              {{ guideCause }}
            </p>
            <p class="mt-2 text-sm text-gray-600 dark:text-gray-400">
              {{ t('admin.companion.status.setupIntro') }}
            </p>

            <p class="mt-4 flex items-start gap-2 text-xs text-gray-500 dark:text-gray-400">
              <Icon name="lock" size="sm" class="mt-0.5 shrink-0" />
              <span>{{ t('admin.companion.status.networkHint') }}</span>
            </p>

            <button
              type="button"
              class="btn btn-secondary mt-5"
              :disabled="statusLoading || settingsLoading"
              @click="retryFromScratch"
            >
              <Icon name="refresh" size="sm" class="mr-1.5" />
              {{ t('admin.companion.status.retry') }}
            </button>
          </div>
        </div>
      </div>

      <!-- 上游 A6 配置：看板上方、状态条下方，保存后立即生效 -->
      <CompanionA6SettingsCard
        :key="settingsFormKey"
        :settings="settings"
        :error="settingsError"
        :loading="settingsLoading"
        :saving="settingsSaving"
        :clearing="tokenClearing"
        :highlight="a6TokenMissing"
        @save="onSettingsSave"
        @clear-token="onClearTokenRequest"
        @reload="refreshSettings"
      />

      <template v-if="!notConfigured">
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

          <!-- 账号规则：按分组组织，一个分组一行，组内账号各占一条子行 -->
          <CompanionRulesTable
            :groups="rules?.groups ?? []"
            :unconfigured-group-count="rules?.unconfigured_groups ?? 0"
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

      <!-- 清除访问令牌确认（回落环境变量，不可逆） -->
      <ConfirmDialog
        :show="pendingClearToken"
        :title="t('admin.companion.settings.clearConfirmTitle')"
        :message="t('admin.companion.settings.clearConfirmMessage')"
        :confirm-text="t('admin.companion.settings.clearConfirm')"
        :cancel-text="t('common.cancel')"
        danger
        @confirm="onClearTokenConfirmed"
        @cancel="pendingClearToken = false"
      />

      <!-- 退回重试确认：写操作，会改数据库里的账单状态；窗口沿用页面当前窗口 -->
      <ConfirmDialog
        :show="pendingRequeue"
        :title="t('admin.companion.requeue.confirmTitle')"
        :message="t('admin.companion.requeue.confirmBody')"
        :confirm-text="t('admin.companion.requeue.button')"
        :cancel-text="t('common.cancel')"
        @confirm="onRequeueConfirmed"
        @cancel="pendingRequeue = false"
      >
        <p v-if="rangeSummary" class="text-xs text-gray-400 dark:text-gray-500">
          {{ rangeSummary }}
        </p>
      </ConfirmDialog>
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
import CompanionA6SettingsCard from './companion/CompanionA6SettingsCard.vue'
import {
  companionAPI,
  classifyCompanionError,
  type CompanionAccountRuleList,
  type CompanionErrorInfo,
  type CompanionRequestPage,
  type CompanionRequestStatus,
  type CompanionRequeueUnmatchedResult,
  type CompanionSettings,
  type CompanionSettingsInput,
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
const requeueing = ref(false)
const pendingRequeue = ref(false)
/** 最近一次「退回重试」的结果：退回条数与匹配上的条数都要展示 */
const requeueResult = ref<CompanionRequeueUnmatchedResult | null>(null)
const savingId = ref<number | null>(null)
const removingId = ref<number | null>(null)
const pendingRemove = ref<number | null>(null)
const firstLoadDone = ref(false)
const statusLoading = ref(false)

const statusInfo = ref<CompanionStatus | null>(null)
const statusError = ref<CompanionErrorInfo | null>(null)
const errorInfo = ref<CompanionErrorInfo | null>(null)

// 上游 A6 配置：卡片自己维护草稿，父组件只持有服务端真相与加载/保存状态
const settings = ref<CompanionSettings | null>(null)
const settingsError = ref<CompanionErrorInfo | null>(null)
const settingsLoading = ref(false)
const settingsSaving = ref(false)
const tokenClearing = ref(false)
const pendingClearToken = ref(false)
/** 保存成功后自增：重建配置卡片，清掉本地令牌明文草稿 */
const settingsFormKey = ref(0)

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

// ==================== 上游 A6 配置 ====================

/** 读取上游 A6 配置；失败只影响配置卡片，页面其余部分照常工作 */
async function refreshSettings() {
  settingsLoading.value = true
  try {
    settings.value = await companionAPI.getSettings()
    settingsError.value = null
  } catch (err) {
    settingsError.value = classifyCompanionError(err)
  } finally {
    settingsLoading.value = false
  }
}

/**
 * 采纳写接口的返回：契约上它与读接口同构；万一后端只回了空 data，
 * 就退回再读一次，避免卡片停在空状态。
 */
async function adoptSettings(result: CompanionSettings | null | undefined): Promise<void> {
  settings.value =
    result && typeof result === 'object' ? result : await companionAPI.getSettings()
}

/**
 * 配置变化后的收尾：重建卡片、作废旧的「未配置」判定，再按新配置重新探测与加载。
 * 令牌保密要求：这里只处理服务端返回的脱敏结果，请求体在调用处即用即弃。
 */
async function afterSettingsChanged() {
  settingsFormKey.value += 1
  errorInfo.value = null
  await refreshStatus()
  if (statusInfo.value?.enabled === false) return
  if (!firstLoadDone.value) {
    selectRange('24h')
    return
  }
  void load({ silent: true })
}

async function onSettingsSave(payload: CompanionSettingsInput) {
  if (settingsSaving.value) return
  settingsSaving.value = true
  try {
    await adoptSettings(await companionAPI.updateSettings(payload))
    settingsError.value = null
    appStore.showSuccess(t('admin.companion.settings.saveSuccess'))
    await afterSettingsChanged()
  } catch (err) {
    // 敏感字段绝不进日志与控制台：只展示服务端返回的 message
    appStore.showError(
      classifyCompanionError(err).message || t('admin.companion.settings.saveFailed')
    )
  } finally {
    settingsSaving.value = false
  }
}

function onClearTokenRequest() {
  pendingClearToken.value = true
}

async function onClearTokenConfirmed() {
  pendingClearToken.value = false
  if (tokenClearing.value) return
  tokenClearing.value = true
  try {
    await adoptSettings(await companionAPI.updateSettings({ clear_a6_access_token: true }))
    settingsError.value = null
    appStore.showSuccess(t('admin.companion.settings.clearSuccess'))
    await afterSettingsChanged()
  } catch (err) {
    appStore.showError(
      classifyCompanionError(err).message || t('admin.companion.settings.clearFailed')
    )
  } finally {
    tokenClearing.value = false
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
    await Promise.all([refreshStatus(), refreshSettings()])
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

// ==================== 退回重试未匹配账单 ====================

function onRequeueRequest() {
  if (requeueing.value) return
  pendingRequeue.value = true
}

/**
 * 「退回重试未匹配账单」：管理员改完规则后，把当前窗口里的孤儿账单退回队列重试。
 *
 * 为什么必须显式点：孤儿账单不会自动重试，而工作区内几千条孤儿一次也退不完。
 *
 * 窗口必须与同页的 summary / requests / account-rules 完全一致（后端同一套解析），
 * 所以这里复用 buildWindow()，不另造窗口；也不能先推进滚动窗口再取参数，否则
 * 退的就不是管理员此刻看到的这一批。
 */
async function onRequeueConfirmed() {
  pendingRequeue.value = false
  if (requeueing.value) return

  let timeWindow: CompanionTimeWindowParams
  try {
    timeWindow = buildWindow()
  } catch {
    appStore.showError(t('admin.companion.range.invalid'))
    return
  }

  requeueing.value = true
  requeueResult.value = null
  try {
    const result = await companionAPI.requeueUnmatched(timeWindow)
    requeueResult.value = result
    // 退回会同时改动「上游待匹配」「待对账」计数，刷新看板才看得出规则改动是否生效
    await load({ silent: true })
  } catch (err) {
    const info = classifyCompanionError(err)
    if (info.kind === 'not_configured') {
      statusInfo.value = { enabled: false, healthy: false }
    } else {
      appStore.showError(info.message || t('admin.companion.requeue.failed'))
    }
  } finally {
    requeueing.value = false
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

// 保存规则：上游类型固定为 a6。
//
// Subarx 分支已删除：后端 ValidateRuleInput 只接受 a6，提交 subarx 必定返回
// 400 COMPANION_BAD_REQUEST（文档 5.4：Subarx 已下线）。留着那条分支等于给
// 用户一个必然失败的按钮。
async function onRuleSave(payload: { accountId: number; value: string }) {
  savingId.value = payload.accountId
  try {
    await companionAPI.upsertAccountRule(payload.accountId, {
      provider: 'a6',
      token_name: payload.value
    })
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

/** 访问令牌未配置：引导与卡片高亮都据此判断；配置还没读到时不做任何断言 */
const a6TokenMissing = computed(
  () => settings.value !== null && settings.value.a6_token_configured === false
)

/** 引导卡片：上游不可用，或访问令牌缺失（后者看板仍可展示已采集数据） */
const showSetupGuide = computed(() => notConfigured.value || a6TokenMissing.value)

const statusDetail = computed(() => statusInfo.value?.detail || statusError.value?.message || '')

/**
 * 真实的失败发生时间，来自 reconciliation_sync_state.a6_last_sync_error_at。
 *
 * 与 lastUpdated（页面刷新时刻）不是一回事：页面上原来只显示「更新于」，于是
 * 「上游 11:58 挂了，我 12:03 刷新页面」会被渲染成「更新于 12:03:00」，
 * 管理员完全看不到故障是什么时候开始的。后端用毫秒精度的 RFC3339 下发，这里只取本地时分秒。
 */
const lastErrorAtText = computed(() => {
  const raw = statusInfo.value?.last_error_at
  if (!raw) return ''
  const at = new Date(raw)
  return Number.isNaN(at.getTime()) ? '' : at.toLocaleTimeString()
})

/** 引导卡片的原因行：令牌缺失时用固定文案，其余情况直接展示探测详情 */
const guideCause = computed(() =>
  a6TokenMissing.value ? t('admin.companion.status.notConfigured') : statusDetail.value
)

/**
 * 采集积压提示。
 *
 * 只有在「本轮被单轮上限截断」时才提示：此时分页游标停在已采集位置，
 * 后面的调用要等下一轮（30 秒）才补上，看板上会短暂显示偏低的收入。
 * 没被截断时不显示，避免制造无意义的焦虑。
 *
 * 数字来自 reconciliation_sync_state.usage_pending_backlog，
 * 由采集器在截断的那一轮顺手数出来（探针上限 10 万，等于上限就表示「至少这么多」）。
 */
const usageBacklogText = computed(() => {
  const status = statusInfo.value
  if (!status || !status.usage_batch_truncated) return ''
  const backlog = status.usage_backlog ?? 0
  if (backlog > 0) {
    return t('admin.companion.status.usageBacklog', { count: backlog })
  }
  return t('admin.companion.status.usageTruncated')
})

/**
 * 状态语气，优先级：未配置 > 不可达 > 已连接。
 *
 * 访问令牌缺失时后端也会返回 healthy:false，但真实原因只是「还没配凭据」，
 * 直接渲染成「不可达」会误导人，所以先看页面配置里的令牌状态。
 */
const statusTone = computed<'disabled' | 'unhealthy' | 'healthy'>(() => {
  if (!statusInfo.value || statusInfo.value.enabled === false || a6TokenMissing.value) {
    return 'disabled'
  }
  return statusInfo.value.healthy ? 'healthy' : 'unhealthy'
})

const statusLabel = computed(() => {
  switch (statusTone.value) {
    case 'healthy':
      return t('admin.companion.status.healthy')
    case 'unhealthy':
      return t('admin.companion.status.unhealthy')
    default:
      return t('admin.companion.status.disabled')
  }
})

const statusDotClass = computed(
  () =>
    ({
      disabled: 'bg-gray-400',
      unhealthy: 'bg-amber-500',
      healthy: 'bg-green-500'
    })[statusTone.value]
)

const statusBadgeClass = computed(() => {
  const base = 'rounded-full px-2.5 py-0.5 text-xs font-semibold '
  return (
    base +
    {
      disabled: 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300',
      unhealthy: 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300',
      healthy: 'bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-300'
    }[statusTone.value]
  )
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
  await Promise.all([refreshStatus(), refreshSettings()])
  if (statusInfo.value?.enabled === false) {
    firstLoadDone.value = true
    return
  }
  selectRange('24h')
})

onUnmounted(stopTimer)
</script>
