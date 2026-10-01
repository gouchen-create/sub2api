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
          props.unconfiguredGroupCount > 0
            ? 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
            : 'bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-300'
        "
      >
        {{
          props.unconfiguredGroupCount > 0
            ? t('admin.companion.rules.unconfiguredGroupsCount', { count: props.unconfiguredGroupCount })
            : t('admin.companion.rules.allConfigured')
        }}
      </span>
    </div>

    <DataTable
      :columns="columns"
      :data="rows"
      :loading="props.loading"
      :row-key="rowKeyOf"
      :sticky-first-column="false"
      :sticky-actions-column="false"
    >
      <template #cell-group="{ row }">
        <!-- 分组头行：分组名 + #分组ID，副标题给出该分组的真实渠道数 -->
        <div v-if="row.kind === 'group'" class="min-w-0 max-w-[200px]">
          <div class="flex items-center gap-1.5">
            <Icon name="users" size="sm" class="shrink-0 text-gray-400 dark:text-dark-500" />
            <span class="truncate font-semibold text-gray-900 dark:text-white" :title="groupTitle(row.group)">
              {{ groupTitle(row.group) }}
            </span>
          </div>
          <div class="mt-0.5 truncate text-xs text-gray-400 dark:text-gray-500" :title="groupMeta(row.group)">
            {{ groupMeta(row.group) }}
          </div>
        </div>
        <!-- 账号子行：只画一个折角连接符，标明它挂在上一行分组下 -->
        <div v-else class="pl-2">
          <span class="block h-4 w-3 rounded-bl border-b border-l border-gray-300 dark:border-dark-600"></span>
        </div>
      </template>

      <template #cell-account="{ row }">
        <!-- 分组头行只说「本表列出几个账号」：它与第一列的真实渠道数口径不同，不能互相冒充 -->
        <span
          v-if="row.kind === 'group'"
          class="whitespace-nowrap text-xs text-gray-400 dark:text-gray-500"
        >
          {{ t('admin.companion.rules.groupAccountsInline', { count: accountCount(row.group) }) }}
        </span>
        <div v-else class="min-w-0 max-w-[220px]">
          <div class="truncate font-medium text-gray-900 dark:text-white" :title="row.account.account_name">
            {{
              t('admin.companion.rules.accountLabel', {
                id: row.account.account_id,
                name: row.account.account_name || t('admin.companion.rules.unnamed')
              })
            }}
          </div>
          <div class="mt-0.5 truncate text-xs text-gray-400">
            {{
              t('admin.companion.rules.channelMeta', {
                platform: row.account.account_platform || '—',
                status: row.account.account_status || '—',
                schedulable: row.account.account_schedulable
                  ? t('admin.companion.rules.schedulable')
                  : t('admin.companion.rules.disabled')
              })
            }}
          </div>
        </div>
      </template>

      <template #cell-recentModel="{ row }">
        <span v-if="row.kind === 'group'" class="text-gray-300 dark:text-dark-600">—</span>
        <span
          v-else
          class="block max-w-[180px] truncate text-gray-600 dark:text-gray-300"
          :title="row.account.recent_model"
        >
          {{ row.account.recent_model || t('admin.companion.rules.noUsage') }}
        </span>
      </template>

      <template #cell-recentGroup="{ row }">
        <span v-if="row.kind === 'group'" class="text-gray-300 dark:text-dark-600">—</span>
        <div v-else class="min-w-0 max-w-[160px]">
          <div class="truncate text-gray-600 dark:text-gray-300" :title="row.account.recent_group_name">
            {{
              row.account.recent_group_name ||
              (row.account.recent_group_id
                ? recentGroupDeleted(row.account)
                : t('admin.companion.rules.noUsage'))
            }}
          </div>
          <div v-if="row.account.recent_group_id" class="mt-0.5 truncate text-xs text-gray-400">
            {{ t('admin.companion.rules.recentGroupMeta', { id: row.account.recent_group_id }) }}
          </div>
        </div>
      </template>

      <template #cell-usageCount="{ row }">
        <span class="whitespace-nowrap font-mono text-gray-700 dark:text-gray-300">{{ usageCount(row) }}</span>
      </template>

      <template #cell-value="{ row }">
        <!-- 分组头行只汇总组内已配置的令牌标识：规则按账号保存，组级没有可编辑的对象 -->
        <span
          v-if="row.kind === 'group'"
          class="block max-w-[180px] truncate text-gray-500 dark:text-gray-400"
          :title="t('admin.companion.rules.groupTokenKeysHint')"
        >
          {{ tokenKeys(row.group) }}
        </span>
        <input
          v-else
          :value="draftFor(row.account).value"
          type="text"
          class="input min-w-[160px]"
          :placeholder="t('admin.companion.rules.valuePlaceholderA6')"
          :disabled="props.savingId === row.account.account_id || props.removingId === row.account.account_id"
          @input="onValueInput(row.account, $event)"
        />
      </template>

      <template #cell-state="{ row }">
        <span
          class="inline-flex items-center gap-1.5 whitespace-nowrap rounded-full px-2.5 py-0.5 text-xs font-semibold"
          :class="
            isConfigured(row)
              ? 'bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-300'
              : 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
          "
          :title="row.kind === 'group' ? groupStateHint(row.group) : accountStateHint(row.account)"
        >
          <span class="h-1.5 w-1.5 rounded-full" :class="isConfigured(row) ? 'bg-green-500' : 'bg-amber-500'"></span>
          {{ isConfigured(row) ? t('admin.companion.rules.configured') : t('admin.companion.rules.unconfigured') }}
        </span>
      </template>

      <template #cell-actions="{ row }">
        <!-- 规则按账号保存，分组行没有保存/删除的对象 -->
        <div v-if="row.kind === 'account'" class="flex flex-wrap items-center gap-3">
          <button
            type="button"
            class="inline-flex items-center gap-1 font-medium text-primary-600 transition-colors hover:text-primary-700 disabled:cursor-not-allowed disabled:opacity-50 dark:text-primary-400 dark:hover:text-primary-300"
            :disabled="props.savingId === row.account.account_id || props.removingId === row.account.account_id"
            @click="onSave(row.account)"
          >
            <Icon name="check" size="sm" />
            {{
              props.savingId === row.account.account_id
                ? t('admin.companion.rules.saving')
                : t('admin.companion.rules.save')
            }}
          </button>
          <button
            v-if="row.account.provider"
            type="button"
            class="inline-flex items-center gap-1 font-medium text-red-600 transition-colors hover:text-red-700 disabled:cursor-not-allowed disabled:opacity-50 dark:text-red-400 dark:hover:text-red-300"
            :disabled="props.savingId === row.account.account_id || props.removingId === row.account.account_id"
            @click="emit('remove', row.account.account_id)"
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
import Icon from '@/components/icons/Icon.vue'
import type { Column } from '@/components/common/types'
import type { CompanionAccountRule, CompanionAccountRuleGroup } from '@/api/admin/companion'

interface Props {
  /** 按分组组织的规则视图（后端 groups 字段） */
  groups: CompanionAccountRuleGroup[]
  /** 范围内有调用但尚未配置规则的分组数 */
  unconfiguredGroupCount: number
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
  (e: 'save', payload: { accountId: number; value: string }): void
  (e: 'remove', accountId: number): void
}>()

const { t } = useI18n()
const appStore = useAppStore()

/** 表格行：一个分组头行，外加该分组下逐条常显的账号子行 */
type RuleRow =
  | { key: string; kind: 'group'; group: CompanionAccountRuleGroup }
  | { key: string; kind: 'account'; account: CompanionAccountRule }

interface RuleDraft {
  value: string
  /** 用户是否手工改过；未改动的草稿会随后端刷新重新同步 */
  dirty: boolean
}

const drafts = ref<Record<number, RuleDraft>>({})

// 渲染结构选择「子行式」而不是「折叠展开式」：
//
// 一个分组下的账号各自对应不同的上游令牌，管理员要逐个比对填写；折叠会把「哪些账号还没
// 配令牌」藏进一次点击之后，而对账页最需要一眼看见的恰恰是待配置项。子行常显还有个副作用
// 是每行高度一致，DataTable 的行高估算与虚拟滚动不会因为展开/收起而抖动。
//
// 行序（组头 → 组内账号）由后端给的 groups 顺序决定，前端不重排。
const rows = computed<RuleRow[]>(() => {
  const list: RuleRow[] = []
  for (const group of props.groups) {
    list.push({ key: `group-${group.group_id}`, kind: 'group', group })
    for (const account of group.accounts ?? []) {
      list.push({ key: `account-${account.account_id}`, kind: 'account', account })
    }
  }
  return list
})

function rowKeyOf(row: RuleRow): string {
  return row.key
}

// 规则只有一个字段：该账号对应的 A6 令牌名。
//
// 上游类型选择器已经整个移除：后端 ValidateRuleInput 只接受 a6，选 Subarx 一定
// 返回 400 COMPANION_BAD_REQUEST——那是个必然失败的按钮，留在页面上只会让人
// 以为「配置没生效」是别的原因。Subarx 已于 v0.2.9 下线（文档 5.4）。
function buildDraft(item: CompanionAccountRule): RuleDraft {
  return {
    value: item.token_name || '',
    dirty: false
  }
}

// 只重建未改动的草稿：自动刷新时不能覆盖用户正在编辑的输入框
watch(
  () => props.groups,
  (groups) => {
    const next: Record<number, RuleDraft> = {}
    for (const group of groups) {
      for (const item of group.accounts ?? []) {
        const existing = drafts.value[item.account_id]
        next[item.account_id] = existing && existing.dirty ? existing : buildDraft(item)
      }
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

// 分组的真实渠道数直接由后端给出（group_channel_count），不从组内账号条数推。
//
// 两者口径不同：一个渠道账号可以同时属于多个分组，本表只把它挂在优先级最高的那一个
// 分组下，所以「本表列出的账号数」永远 ≤ 「真实渠道数」。dev 库实测：分组 7 真实有
// 2 个渠道（账号 #46 priority 1、#44 priority 3），因为 #44 被分组 #10 抢走，
// 旧实现按行数统计就渲染成「1 个渠道」——数字是假的。
function channelCount(group: CompanionAccountRuleGroup): number {
  return group.group_channel_count || 0
}

function accountCount(group: CompanionAccountRuleGroup): number {
  return (group.accounts ?? []).length
}

// 没有归属任何分组的账号由后端归到 group_id 0，这里给出明确提示而不是空白
function groupTitle(group: CompanionAccountRuleGroup): string {
  if (!group.group_id) return t('admin.companion.rules.noGroup')
  return t('admin.companion.rules.groupLabel', {
    id: group.group_id,
    name: group.group_name || t('admin.companion.rules.unnamedGroup')
  })
}

function groupMeta(group: CompanionAccountRuleGroup): string {
  if (!group.group_id) return t('admin.companion.rules.noGroupHint')
  return t('admin.companion.rules.groupMeta', { id: group.group_id, count: channelCount(group) })
}

function isConfigured(row: RuleRow): boolean {
  return row.kind === 'group' ? row.group.configured : row.account.configured
}

function usageCount(row: RuleRow): number {
  return (row.kind === 'group' ? row.group.usage_count : row.account.usage_count) || 0
}

/** 组内已配置的令牌标识（后端去重后给出），一个都没配时用占位符 */
function tokenKeys(group: CompanionAccountRuleGroup): string {
  return (group.token_keys ?? []).join(' / ') || '—'
}

function groupStateHint(group: CompanionAccountRuleGroup): string {
  return group.configured
    ? t('admin.companion.rules.groupConfiguredHint')
    : t('admin.companion.rules.groupUnconfiguredHint')
}

function accountStateHint(account: CompanionAccountRule): string {
  return account.version
    ? t('admin.companion.rules.versionTooltip', { version: account.version })
    : t('admin.companion.rules.noSnapshot')
}

// 「最近分组」取该账号全历史最后一次调用所在的分组。分组可能已被删除，
// 那时后端只回 group_id（分组名为空串），这里明确写出来而不是显示成空白。
function recentGroupDeleted(row: CompanionAccountRule): string {
  return t('admin.companion.rules.recentGroupDeleted', { id: row.recent_group_id })
}

const columns = computed<Column[]>(() => [
  { key: 'group', label: t('admin.companion.rules.columns.group') },
  { key: 'account', label: t('admin.companion.rules.columns.account') },
  { key: 'recentModel', label: t('admin.companion.rules.columns.recentModel') },
  { key: 'recentGroup', label: t('admin.companion.rules.columns.recentGroup') },
  { key: 'usageCount', label: t('admin.companion.rules.columns.usageCount') },
  { key: 'value', label: t('admin.companion.rules.columns.value') },
  { key: 'state', label: t('admin.companion.rules.columns.state') },
  { key: 'actions', label: t('admin.companion.rules.columns.actions') }
])

function onValueInput(item: CompanionAccountRule, event: Event) {
  const draft = draftFor(item)
  draft.value = (event.target as HTMLInputElement).value
  draft.dirty = true
}

function onSave(item: CompanionAccountRule) {
  const draft = draftFor(item)
  const value = draft.value.trim()
  if (!value) {
    appStore.showError(t('admin.companion.rules.valueRequired'))
    return
  }
  draft.value = value
  draft.dirty = false
  emit('save', { accountId: item.account_id, value })
}
</script>
