<template>
  <div
    class="rounded-3xl bg-white p-6 shadow-sm ring-1 ring-gray-900/5 dark:bg-dark-800 dark:ring-dark-700"
  >
    <div class="mb-4 flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
      <h3 class="flex min-w-0 items-center gap-2 text-sm font-bold text-gray-900 dark:text-white">
        <Icon name="trendingUp" size="md" class="shrink-0 text-blue-500" />
        {{ t('admin.companion.chart.title') }}
        <span v-if="props.bucket" class="truncate text-xs font-normal text-gray-400 dark:text-gray-500">
          {{ t('admin.companion.chart.bucket', { bucket: props.bucket }) }}
        </span>
      </h3>

      <!-- 指标切换：金额 / 调用量；两种模式都会保留右侧的调用量轴 -->
      <div class="flex shrink-0 items-center gap-1 rounded-xl bg-gray-100 p-1 dark:bg-dark-900">
        <button
          v-for="mode in modes"
          :key="mode"
          type="button"
          class="rounded-lg px-3 py-1 text-xs font-semibold transition-colors"
          :class="
            props.mode === mode
              ? 'bg-white text-primary-600 shadow-sm dark:bg-dark-700 dark:text-primary-300'
              : 'text-gray-500 hover:text-gray-700 dark:text-gray-400 dark:hover:text-gray-200'
          "
          @click="emit('update:mode', mode)"
        >
          {{ t(mode === 'money' ? 'admin.companion.chart.money' : 'admin.companion.chart.count') }}
        </button>
      </div>
    </div>

    <div class="h-[320px]">
      <Line v-if="chartData" :data="chartData" :options="chartOptions" />
      <div v-else-if="props.loading" class="flex h-full items-center justify-center">
        <div class="h-8 w-8 animate-spin rounded-full border-b-2 border-primary-600"></div>
      </div>
      <div v-else class="flex h-full items-center justify-center">
        <EmptyState :title="t('common.noData')" :description="t('admin.companion.chart.empty')" />
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  Chart as ChartJS,
  CategoryScale,
  Filler,
  Legend,
  LineElement,
  LinearScale,
  PointElement,
  Title,
  Tooltip
} from 'chart.js'
import { Line } from 'vue-chartjs'
import Icon from '@/components/icons/Icon.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import type { CompanionTimeSeriesPoint } from '@/api/admin/companion'

ChartJS.register(Title, Tooltip, Legend, LineElement, LinearScale, PointElement, CategoryScale, Filler)

type ChartMode = 'money' | 'count'

interface Props {
  points: CompanionTimeSeriesPoint[]
  /** 上游给出的分桶粒度标签（1小时 / 6小时 / 1天 / 1周） */
  bucket?: string
  loading?: boolean
  mode: ChartMode
}

const props = withDefaults(defineProps<Props>(), { bucket: '', loading: false })
const emit = defineEmits<{ (e: 'update:mode', mode: ChartMode): void }>()

const { t } = useI18n()

const modes: ChartMode[] = ['money', 'count']

const isDarkMode = computed(() => document.documentElement.classList.contains('dark'))
const colors = computed(() => ({
  blue: '#3b82f6',
  blueAlpha: '#3b82f620',
  amber: '#f59e0b',
  amberAlpha: '#f59e0b20',
  green: '#10b981',
  greenAlpha: '#10b98120',
  red: '#ef4444',
  redAlpha: '#ef444420',
  grid: isDarkMode.value ? '#374151' : '#f3f4f6',
  text: isDarkMode.value ? '#9ca3af' : '#6b7280'
}))

function toNumber(value: string | number | null | undefined): number {
  const parsed = Number(value)
  return Number.isFinite(parsed) ? parsed : 0
}

/** 金额轴刻度：与 Companion 原看板一致的量级自适应格式 */
function formatMoneyAxis(value: number): string {
  if (value === 0) return '¥0'
  const sign = value < 0 ? '-' : ''
  const abs = Math.abs(value)
  if (abs >= 100) return `${sign}¥${abs.toFixed(0)}`
  if (abs >= 1) return `${sign}¥${abs.toFixed(2)}`
  if (abs >= 0.01) return `${sign}¥${abs.toFixed(4)}`
  return `${sign}¥${abs.toFixed(8)}`
}

function formatAxisValue(value: number): string {
  return props.mode === 'money' ? formatMoneyAxis(value) : Math.round(value).toLocaleString()
}

/**
 * 用相邻数据点的间隔推断分桶粒度，避免依赖上游的中文粒度标签。
 * 小于 24 小时的桶在横轴标签里带上时分。
 */
const bucketHours = computed(() => {
  const points = props.points
  if (points.length < 2) return 24
  const delta = (new Date(points[1].start).getTime() - new Date(points[0].start).getTime()) / 3600000
  return Number.isFinite(delta) && delta > 0 ? delta : 24
})

function formatBucketLabel(start: string): string {
  const date = new Date(start)
  if (Number.isNaN(date.getTime())) return start
  if (bucketHours.value < 24) {
    return date.toLocaleString([], { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' })
  }
  return date.toLocaleDateString()
}

const chartData = computed(() => {
  if (!props.points.length) return null
  const c = colors.value
  const labels = props.points.map((point) => formatBucketLabel(point.start))
  const total = props.points.reduce(
    (sum, point) => sum + (props.mode === 'money' ? toNumber(point.revenue) + toNumber(point.upstream_cost) : point.record_total),
    0
  )
  if (total === 0) return null

  if (props.mode === 'count') {
    return {
      labels,
      datasets: [
        {
          label: t('admin.companion.chart.matched'),
          data: props.points.map((point) => point.matched),
          borderColor: c.green,
          backgroundColor: c.greenAlpha,
          fill: true,
          tension: 0.4,
          pointRadius: 0,
          pointHitRadius: 10
        },
        {
          label: t('admin.companion.chart.unmatched'),
          data: props.points.map((point) => point.unmatched),
          borderColor: c.red,
          backgroundColor: c.redAlpha,
          fill: true,
          tension: 0.4,
          pointRadius: 0,
          pointHitRadius: 10
        },
        {
          label: t('admin.companion.chart.upstreamUnmatched'),
          data: props.points.map((point) => point.upstream_unmatched),
          borderColor: c.amber,
          backgroundColor: c.amberAlpha,
          fill: true,
          tension: 0.4,
          pointRadius: 0,
          pointHitRadius: 10
        }
      ]
    }
  }

  return {
    labels,
    datasets: [
      {
        label: t('admin.companion.chart.revenue'),
        data: props.points.map((point) => toNumber(point.revenue)),
        borderColor: c.blue,
        backgroundColor: c.blueAlpha,
        fill: true,
        tension: 0.4,
        pointRadius: 0,
        pointHitRadius: 10
      },
      {
        label: t('admin.companion.chart.upstreamCost'),
        data: props.points.map((point) => toNumber(point.upstream_cost)),
        borderColor: c.amber,
        backgroundColor: c.amberAlpha,
        fill: true,
        tension: 0.4,
        pointRadius: 0,
        pointHitRadius: 10
      },
      {
        label: t('admin.companion.chart.grossProfit'),
        data: props.points.map((point) => toNumber(point.gross_profit)),
        borderColor: c.green,
        backgroundColor: c.greenAlpha,
        fill: false,
        tension: 0.4,
        pointRadius: 0,
        pointHitRadius: 10
      },
      {
        // 金额模式下同时给出调用量曲线，便于把金额波动对应到调用量
        label: t('admin.companion.chart.recordTotal'),
        data: props.points.map((point) => point.record_total),
        borderColor: c.text,
        backgroundColor: 'transparent',
        borderDash: [4, 4],
        fill: false,
        tension: 0.4,
        pointRadius: 0,
        pointHitRadius: 10,
        yAxisID: 'y1'
      }
    ]
  }
})

const chartOptions = computed(() => {
  const c = colors.value
  const moneyMode = props.mode === 'money'
  return {
    responsive: true,
    maintainAspectRatio: false,
    interaction: { intersect: false, mode: 'index' as const },
    plugins: {
      legend: {
        position: 'top' as const,
        align: 'end' as const,
        labels: { color: c.text, usePointStyle: true, boxWidth: 6, font: { size: 10 } }
      },
      tooltip: {
        backgroundColor: isDarkMode.value ? '#1f2937' : '#ffffff',
        titleColor: isDarkMode.value ? '#f3f4f6' : '#111827',
        bodyColor: isDarkMode.value ? '#d1d5db' : '#4b5563',
        borderColor: c.grid,
        borderWidth: 1,
        padding: 10,
        displayColors: true,
        callbacks: {
          label: (context: any) => {
            const label = context.dataset?.label ? `${context.dataset.label}: ` : ''
            const raw = Number(context.parsed?.y ?? 0)
            return `${label}${context.dataset?.yAxisID === 'y1' ? Math.round(raw).toLocaleString() : formatMoneyAxis(raw)}`
          }
        }
      }
    },
    scales: {
      x: {
        type: 'category' as const,
        grid: { display: false },
        ticks: {
          color: c.text,
          font: { size: 10 },
          maxTicksLimit: 8,
          autoSkip: true,
          autoSkipPadding: 10
        }
      },
      y: {
        type: 'linear' as const,
        display: true,
        position: 'left' as const,
        grid: { color: c.grid, borderDash: [4, 4] },
        ticks: {
          color: c.text,
          font: { size: 10 },
          callback: (value: any) => formatAxisValue(Number(value))
        }
      },
      y1: {
        type: 'linear' as const,
        display: moneyMode,
        position: 'right' as const,
        grid: { display: false },
        ticks: { color: c.text, font: { size: 10 } }
      }
    }
  }
})
</script>
