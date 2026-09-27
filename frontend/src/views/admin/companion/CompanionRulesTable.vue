<template>
  <div
    class="rounded-3xl bg-white shadow-sm ring-1 ring-gray-900/5 dark:bg-dark-800 dark:ring-dark-700"
  >
    <div class="flex flex-col gap-2 border-b border-gray-100 p-6 pb-4 dark:border-dark-700 sm:flex-row sm:items-center sm:justify-between">
      <div class="min-w-0">
        <h3 class="flex items-center gap-2 text-sm font-bold text-gray-900 dark:text-white">
          <Icon name="key" size="md" class="shrink-0 text-amber-500" />
          {{ t('admin.companion.rules.title') }}
        </h3>
        <p class="mt-1 text-xs text-gray-400 dark:text-gray-500">{{ t('admin.companion.rules.subtitle') }}</p>
      </div>
      <span
        class="shrink-0 self-start rounded-full px-3 py-1 text-xs font-semibold sm:self-auto"
        :class="
          props.unconfiguredCount > 0
            ? 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
            : 'bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-300'
        "
      >
        {{
          props.unconfiguredCount > 0
            ? t('admin.companion.rules.unconfiguredCount', { count: props.unconfiguredCount })
            : t('admin.companion.rules.allConfigured')
        }}
      </span>
    </div>

    <DataTable
      :columns="columns"
      :data="props.items"
      :loading="props.loading"
      row-key="account_id"
      :sticky-first-column="false"
      :sticky-actions-column="false"
    >
      <template #cell-group="{ row }">
        <div class="min-w-0 max-w-[180px]">
          <div class="truncate font-medium text-gray-900 dark:text-white" :title="row.group_name">
            {{ row.group_name || t('admin.companion.rules.noGroup') }}
          </div>
          <div class="mt-0.5 truncate text-xs text-gray-400">
            {{
              row.group_id
                ? t('admin.companion.rules.groupMeta', { id: row.group_id, count: groupChannelCount(row.group_id) })
                : t('admin.companion.rules.noGroup')
            }}
          </div>
        </div>
      </template>

      <template #cell-account="{ row }">
        <div class="min-w-0 max-w-[220px]">
          <div class="truncate font-medium text-gray-900 dark:text-white" :title="row.account_name">
            {{
              t('admin.companion.rules.accountLabel', {
                id: row.account_id,
                name: row.account_name || t('admin.companion.rules.unnamed')
              })
            }}
          </div>
          <div class="mt-0.5 truncate text-xs text-gray-400">
            {{
              t('admin.companion.rules.channelMeta', {
                platform: row.account_platform || '—',
                status: row.account_status || '—',
                schedulable: row.account_schedulable
                  ? t('admin.companion.rules.schedulable')
                  : t('admin.companion.rules.disabled')
              })
            }}
          </div>
        </div>
      </template>

      <template #cell-recentModel="{ row }">
        <span
          class="block max-w-[180px] truncate text-gray-600 dark:text-gray-300"
          :title="row.models"
        >
          {{ row.models || t('admin.companion.rules.noUsage') }}
        </span>
      </template>

      <template #cell-usageCount="{ value }">
        <span class="whitespace-nowrap font-mono text-gray-700 dark:text-gray-300">{{ value || 0 }}</span>
      </template>

      <template #cell-provider="{ row }">
        <div class="w-full min-w-[140px]">
          <Select
            :model-value="draftFor(row).provider"
            :options="providerOptions"
            :disabled="props.savingId === row.account_id || props.removingId === row.account_id"
            @update:model-value="onProviderChange(row, $event)"
          />
        </div>
      </template>

      <template #cell-value="{ row }">
        <input
          :value="draftFor(row).value"
          type="text"
          class="input min-w-[160px]"
          :placeholder="valuePlaceholder(draftFor(row).provider)"
          :disabled="props.savingId === row.account_id || props.removingId === row.account_id"
          @input="onValueInput(row, $event)"
        />
      </template>

      <template #cell-state="{ row }">
        <span
          class="inline-flex items-center gap-1.5 whitespace-nowrap rounded-full px-2.5 py-0.5 text-xs font-semibold"
          :class="
            row.configured
              ? 'bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-300'
              : 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
          "
          :title="
            row.version
              ? t('admin.companion.rules.versionTooltip', { version: row.version })
              : t('admin.companion.rules.noSnapshot')
          "
        >
          <span class="h-1.5 w-1.5 rounded-full" :class="row.configured ? 'bg-green-500' : 'bg-amber-500'"></span>
          {{ row.configured ? t('admin.companion.rules.configured') : t('admin.companion.rules.unconfigured') }}
        </span>
      </template>

      <template #cell-actions="{ row }">
        <div class="flex flex-wrap items-center gap-3">
          <button
            type="button"
            class="inline-flex items-center gap-1 font-medium text-primary-600 transition-colors hover:text-primary-700 disabled:cursor-not-allowed disabled:opacity-50 dark:text-primary-400 dark:hover:text-primary-300"
            :disabled="props.savingId === row.account_id || props.removingId === row.account_id"
            @click="onSave(row)"
          >
            <Icon name="check" size="sm" />
            {{ props.savingId === row.account_id ? t('admin.companion.rules.saving') : t('admin.companion.rules.save') }}
          </button>
          <button
            v-if="row.provider"
            type="button"
            class="inline-flex items-center gap-1 font-medium text-red-600 transition-colors hover:text-red-700 disabled:cursor-not-allowed disabled:opacity-50 dark:text-red-400 dark:hover:text-red-300"
            :disabled="props.savingId === row.account_id || props.removingId === row.account_id"
            @click="emit('remove', row.account_id)"
          >
            <Icon name="trash" size="sm" />
            {{ t('admin.companion.rules.remove') }}
          </button>
        </div>
      </template>

      <template #empty>
        <div class="flex flex-col items-center py-10">
          <Icon name="server" size="xl" class="mb-4 h-12 w-12 text-gray-300 dark:text-dark-600" />
          <p class="text-sm font-medium text-gray-500 dark:text-gray-400">
            {{ t('admin.companion.rules.empty') }}
          </p>
        </div>
      </template>
    </DataTable>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores'
import DataTable from '@/components/common/DataTable.vue'
import Select from '@/components/common/Select.vue'
import Icon from '@/components/icons/Icon.vue'
import type { Column } from '@/components/common/types'
import type { CompanionAccountRule } from '@/api/admin/companion'

interface Props {
  items: CompanionAccountRule[]
  unconfiguredCount: number
  loading?: boolean
  /** 正在保存的账号 ID，用于禁用该行交互 */
  savingId?: number | null
  /** 正在删除的账号 ID，用于禁用该行交互 */
  removingId?: number | null
}

const props = withDefaults(defineProps<Props>(), {
  loading: false,
  savingId: null,
  removingId: null
})

const emit = defineEmits<{
  (e: 'save', payload: { accountId: number; provider: 'a6' | 'subarx'; value: string }): void
  (e: 'remove', accountId: number): void
}>()

const { t } = useI18n()
const appStore = useAppStore()

interface RuleDraft {
  provider: string
  value: string
  /** 用户是否手工改过；未改动的草稿会随后端刷新重新同步 */
  dirty: boolean
}

const drafts = ref<Record<number, RuleDraft>>({})

function buildDraft(item: CompanionAccountRule): RuleDraft {
  const provider = item.provider || ''
  return {
    provider,
    value: provider === 'a6' ? item.token_name || '' : provider === 'subarx' ? item.multiplier || '' : '',
    dirty: false
  }
}

// 只重建未改动的草稿：自动刷新时不能覆盖用户正在编辑的输入框
watch(
  () => props.items,
  (items) => {
    const next: Record<number, RuleDraft> = {}
    for (const item of items) {
      const existing = drafts.value[item.account_id]
      next[item.account_id] = existing && existing.dirty ? existing : buildDraft(item)
    }
    drafts.value = next
  },
  { immediate: true }
)

function draftFor(item: CompanionAccountRule): RuleDraft {
  if (!drafts.value[item.account_id]) {
    drafts.value[item.account_id] = buildDraft(item)
  }
  return drafts.value[item.account_id]
}

const groupSizes = computed(() => {
  const sizes = new Map<number, number>()
  for (const item of props.items) {
    if (!item.group_id) continue
    sizes.set(item.group_id, (sizes.get(item.group_id) || 0) + 1)
  }
  return sizes
})

function groupChannelCount(groupId: number): number {
  return groupSizes.value.get(groupId) || 0
}

const columns = computed<Column[]>(() => [
  { key: 'group', label: t('admin.companion.rules.columns.group') },
  { key: 'account', label: t('admin.companion.rules.columns.account') },
  { key: 'recentModel', label: t('admin.companion.rules.columns.recentModel') },
  { key: 'usageCount', label: t('admin.companion.rules.columns.usageCount') },
  { key: 'provider', label: t('admin.companion.rules.columns.provider') },
  { key: 'value', label: t('admin.companion.rules.columns.value') },
  { key: 'state', label: t('admin.companion.rules.columns.state') },
  { key: 'actions', label: t('admin.companion.rules.columns.actions') }
])

const providerOptions = computed(() => [
  { value: '', label: t('admin.companion.rules.providerPlaceholder') },
  { value: 'a6', label: t('admin.companion.rules.providerA6') },
  { value: 'subarx', label: t('admin.companion.rules.providerSubarx') }
])

function valuePlaceholder(provider: string): string {
  if (provider === 'a6') return t('admin.companion.rules.valuePlaceholderA6')
  if (provider === 'subarx') return t('admin.companion.rules.valuePlaceholderSubarx')
  return t('admin.companion.rules.valuePlaceholderEmpty')
}

function onValueInput(item: CompanionAccountRule, event: Event) {
  const draft = draftFor(item)
  draft.value = (event.target as HTMLInputElement).value
  draft.dirty = true
}

function onProviderChange(item: CompanionAccountRule, value: string | number | boolean | null) {
  const draft = draftFor(item)
  const provider = String(value ?? '')
  if (draft.provider !== provider) {
    // 切换上游类型后原参数不再适用，清空让用户重新填写
    draft.value = ''
  }
  draft.provider = provider
  draft.dirty = true
}

function onSave(item: CompanionAccountRule) {
  const draft = draftFor(item)
  if (!draft.provider) {
    appStore.showError(t('admin.companion.rules.providerRequired'))
    return
  }
  const value = draft.value.trim()
  if (!value) {
    appStore.showError(t('admin.companion.rules.valueRequired'))
    return
  }
  draft.value = value
  draft.dirty = false
  emit('save', { accountId: item.account_id, provider: draft.provider as 'a6' | 'subarx', value })
}
</script>
