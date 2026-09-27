<template>
  <div
    class="rounded-3xl bg-white shadow-sm ring-1 ring-gray-900/5 dark:bg-dark-800 dark:ring-dark-700"
  >
    <div class="flex flex-col gap-3 border-b border-gray-100 p-6 pb-4 dark:border-dark-700 sm:flex-row sm:items-center sm:justify-between">
      <div class="min-w-0">
        <h3 class="flex items-center gap-2 text-sm font-bold text-gray-900 dark:text-white">
          <Icon name="document" size="md" class="shrink-0 text-blue-500" />
          {{ t('admin.companion.requests.title') }}
        </h3>
        <p class="mt-1 text-xs text-gray-400 dark:text-gray-500">{{ t('admin.companion.requests.subtitle') }}</p>
      </div>
      <div class="w-full shrink-0 sm:w-48">
        <Select
          :model-value="props.status"
          :options="statusOptions"
          @update:model-value="onStatusChange"
        />
      </div>
    </div>

    <DataTable
      :columns="columns"
      :data="props.items"
      :loading="props.loading"
      :row-key="rowKey"
      :sticky-actions-column="false"
    >
      <template #cell-created_at="{ value }">
        <span class="whitespace-nowrap text-gray-600 dark:text-gray-300">{{ formatDateTime(value) }}</span>
      </template>

      <template #cell-user="{ row }">
        <div class="min-w-0 max-w-[220px]">
          <template v-if="row.record_type === 'upstream_unmatched'">
            <span class="text-gray-400">—</span>
          </template>
          <template v-else>
            <div class="truncate font-medium text-gray-900 dark:text-white" :title="row.user_email">
              {{ row.user_email || `#${row.user_id}` }}
            </div>
            <div class="mt-0.5 truncate text-xs text-gray-400">#{{ row.user_id }}</div>
          </template>
        </div>
      </template>

      <template #cell-group="{ row }">
        <div class="min-w-0 max-w-[160px]">
          <div class="truncate text-gray-700 dark:text-gray-300" :title="row.group_name">
            {{ row.group_name || '—' }}
          </div>
          <div v-if="row.group_id" class="mt-0.5 truncate text-xs text-gray-400">#{{ row.group_id }}</div>
        </div>
      </template>

      <template #cell-account="{ value }">
        <span class="whitespace-nowrap font-mono text-gray-600 dark:text-gray-300">
          {{ value ? `#${value}` : '—' }}
        </span>
      </template>

      <template #cell-model="{ value }">
        <span class="block max-w-[200px] truncate text-gray-700 dark:text-gray-300" :title="value">
          {{ value || '—' }}
        </span>
      </template>

      <template #cell-tokens="{ row }">
        <span
          class="whitespace-nowrap font-mono text-xs text-gray-600 dark:text-gray-300"
          :title="t('admin.companion.requests.tokensHint', { input: row.input_tokens, output: row.output_tokens, cache: row.cache_tokens })"
        >
          {{ row.input_tokens }} / {{ row.output_tokens }} / {{ row.cache_tokens }}
        </span>
      </template>

      <template #cell-revenue="{ value }">
        <span class="whitespace-nowrap font-mono text-gray-700 dark:text-gray-300">{{ formatCny(value) }}</span>
      </template>

      <template #cell-cost="{ value }">
        <span class="whitespace-nowrap font-mono text-gray-700 dark:text-gray-300">{{ formatCny(value) }}</span>
      </template>

      <template #cell-original="{ row }">
        <span class="whitespace-nowrap font-mono text-gray-500 dark:text-gray-400">
          {{ formatOriginal(row.upstream_cost_original, row.upstream_currency) }}
        </span>
      </template>

      <template #cell-profit="{ row }">
        <span
          class="whitespace-nowrap font-mono"
          :class="
            row.matched
              ? Number(row.gross_profit) >= 0
                ? 'text-green-600 dark:text-green-400'
                : 'text-red-600 dark:text-red-400'
              : 'text-gray-400 dark:text-gray-500'
          "
        >
          {{ row.matched ? formatCny(row.gross_profit) : '—' }}
        </span>
      </template>

      <template #cell-status="{ row }">
        <span
          class="inline-flex items-center gap-1.5 whitespace-nowrap rounded-full px-2.5 py-0.5 text-xs font-semibold"
          :class="statusBadgeClass(row)"
          :title="statusHint(row)"
        >
          <span class="h-1.5 w-1.5 rounded-full" :class="statusDotClass(row)"></span>
          {{ statusText(row) }}
        </span>
      </template>

      <template #cell-request="{ row }">
        <div class="max-w-[220px] font-mono text-xs text-gray-500 dark:text-gray-400">
          <div
            class="truncate"
            :title="`${t('admin.companion.requests.requestHint')}: ${row.request_id || '—'} / ${row.upstream_request_id || '—'}`"
          >
            {{ shortId(row.request_id) }}
          </div>
          <div class="mt-0.5 truncate text-gray-400 dark:text-gray-500" :title="row.upstream_request_id">
            {{ shortId(row.upstream_request_id) }}
          </div>
        </div>
      </template>

      <template #empty>
        <div class="flex flex-col items-center py-10">
          <Icon name="inbox" size="xl" class="mb-4 h-12 w-12 text-gray-300 dark:text-dark-600" />
          <p class="text-sm font-medium text-gray-500 dark:text-gray-400">
            {{ t('admin.companion.requests.empty') }}
          </p>
        </div>
      </template>
    </DataTable>

    <Pagination
      v-if="props.total > 0"
      :total="props.total"
      :page="props.page"
      :page-size="props.pageSize"
      :page-size-options="[25, 50, 100]"
      @update:page="emit('update:page', $event)"
      @update:pageSize="emit('update:pageSize', $event)"
    />
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import DataTable from '@/components/common/DataTable.vue'
import Pagination from '@/components/common/Pagination.vue'
import Select from '@/components/common/Select.vue'
import Icon from '@/components/icons/Icon.vue'
import type { Column } from '@/components/common/types'
import type { CompanionRequestRow, CompanionRequestStatus } from '@/api/admin/companion'

interface Props {
  items: CompanionRequestRow[]
  loading?: boolean
  total: number
  page: number
  pageSize: number
  status: CompanionRequestStatus
}

const props = withDefaults(defineProps<Props>(), { loading: false })

const emit = defineEmits<{
  (e: 'update:page', page: number): void
  (e: 'update:pageSize', pageSize: number): void
  (e: 'update:status', status: CompanionRequestStatus): void
}>()

const { t } = useI18n()

/** 成本来源到 i18n 文案后缀的映射；未命中的来源回落到上游给出的 cost_source_label。 */
const STATUS_SUFFIX: Record<string, string> = {
  pending: 'pending',
  billed: 'billed',
  subarx_billed_allocation: 'subarxBilled',
  subarx_pending: 'subarxPending',
  subarx_waiting: 'subarxWaiting',
  subarx_rule: 'subarxRule',
  rule_unconfigured: 'ruleUnconfigured',
  a6_waiting: 'a6Waiting',
  a6_pending: 'a6Pending',
  upstream_unmatched: 'upstreamUnmatched',
  subarx_unallocated: 'subarxUnallocated'
}

const columns = computed<Column[]>(() => [
  { key: 'created_at', label: t('admin.companion.requests.columns.time') },
  { key: 'user', label: t('admin.companion.requests.columns.user') },
  { key: 'group', label: t('admin.companion.requests.columns.group') },
  { key: 'account', label: t('admin.companion.requests.columns.account') },
  { key: 'model', label: t('admin.companion.requests.columns.model') },
  { key: 'tokens', label: t('admin.companion.requests.columns.tokens') },
  { key: 'revenue', label: t('admin.companion.requests.columns.revenue') },
  { key: 'cost', label: t('admin.companion.requests.columns.cost') },
  { key: 'original', label: t('admin.companion.requests.columns.original') },
  { key: 'profit', label: t('admin.companion.requests.columns.profit') },
  { key: 'status', label: t('admin.companion.requests.columns.status') },
  { key: 'request', label: t('admin.companion.requests.columns.request') }
])

const statusOptions = computed(() => [
  { value: 'all', label: t('admin.companion.requests.filters.all') },
  { value: 'matched', label: t('admin.companion.requests.filters.matched') },
  { value: 'unmatched', label: t('admin.companion.requests.filters.unmatched') },
  { value: 'upstream_unmatched', label: t('admin.companion.requests.filters.upstreamUnmatched') }
])

function onStatusChange(value: string | number | boolean | null) {
  emit('update:status', String(value ?? 'all') as CompanionRequestStatus)
}

/** 下游行与上游待匹配行可能共用 source_id=0，因此用记录类型 + 请求 ID 组合出稳定键 */
function rowKey(row: CompanionRequestRow): string {
  return `${row.record_type}:${row.source_id}:${row.request_id}:${row.upstream_request_id}`
}

function statusSuffix(row: CompanionRequestRow): string | null {
  return STATUS_SUFFIX[row.cost_source] ?? null
}

function statusText(row: CompanionRequestRow): string {
  const suffix = statusSuffix(row)
  if (suffix) return t(`admin.companion.requests.statusText.${suffix}`)
  return row.cost_source_label || (row.matched ? t('admin.companion.requests.statusText.matched') : t('admin.companion.requests.statusText.unmatched'))
}

function statusHint(row: CompanionRequestRow): string {
  const suffix = statusSuffix(row)
  if (suffix) return t(`admin.companion.requests.statusHint.${suffix}`)
  return ''
}

function statusBadgeClass(row: CompanionRequestRow): string {
  const base = 'inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-xs font-semibold '
  if (row.record_type === 'upstream_unmatched') {
    return base + 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
  }
  if (row.cost_source === 'subarx_rule') {
    return base + 'bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-300'
  }
  if (row.matched) {
    return base + 'bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-300'
  }
  return base + 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
}

function statusDotClass(row: CompanionRequestRow): string {
  if (row.cost_source === 'subarx_rule') return 'bg-blue-500'
  return row.matched && row.record_type !== 'upstream_unmatched' ? 'bg-green-500' : 'bg-amber-500'
}

function formatDateTime(raw: string): string {
  const date = new Date(raw)
  if (Number.isNaN(date.getTime())) return raw || '—'
  return date.toLocaleString()
}

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

function formatOriginal(amount: string, currency: string): string {
  if (!amount) return '—'
  if (currency === 'USD') return `$${amount}`
  if (currency === 'CNY') return `¥${amount}`
  return `${currency || ''} ${amount}`.trim()
}

function shortId(value: string): string {
  if (!value) return '—'
  return value.length > 20 ? `${value.slice(0, 10)}…${value.slice(-7)}` : value
}
</script>
