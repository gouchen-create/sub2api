<template>
  <div
    class="rounded-3xl bg-white shadow-sm dark:bg-dark-800"
    :class="
      props.highlight
        ? 'ring-2 ring-amber-400 dark:ring-amber-500/70'
        : 'ring-1 ring-gray-900/5 dark:ring-dark-700'
    "
  >
    <div
      class="flex flex-col gap-2 border-b border-gray-100 p-6 pb-4 dark:border-dark-700 sm:flex-row sm:items-center sm:justify-between"
    >
      <div class="min-w-0">
        <h3 class="flex items-center gap-2 text-sm font-bold text-gray-900 dark:text-white">
          <Icon name="key" size="md" class="shrink-0 text-primary-500" />
          {{ t('admin.companion.settings.title') }}
        </h3>
        <p class="mt-1 text-xs text-gray-400 dark:text-gray-500">
          {{ t('admin.companion.settings.subtitle') }}
        </p>
      </div>

      <div class="flex shrink-0 items-center gap-2 self-start sm:self-auto">
        <span class="rounded-full px-3 py-1 text-xs font-semibold" :class="tokenBadgeClass">
          {{
            props.settings?.a6_token_configured
              ? t('admin.companion.settings.configured')
              : t('admin.companion.settings.notConfigured')
          }}
        </span>
        <button
          type="button"
          class="btn btn-secondary btn-sm"
          :disabled="busy"
          @click="emit('reload')"
        >
          <Icon name="refresh" size="sm" class="mr-1" :class="props.loading ? 'animate-spin' : ''" />
          {{ t('admin.companion.settings.reload') }}
        </button>
      </div>
    </div>

    <div class="p-6 pt-4">
      <!-- 读取失败：保留上一次拿到的表单，只有首次加载失败时才只剩错误提示 -->
      <div
        v-if="props.error"
        class="rounded-2xl bg-red-50 p-4 text-sm text-red-600 dark:bg-red-900/20 dark:text-red-400"
        :class="props.settings ? 'mb-4' : ''"
      >
        <div class="flex items-start gap-2">
          <Icon name="exclamationCircle" size="md" class="mt-0.5 shrink-0" />
          <div class="min-w-0">
            <p class="font-semibold">{{ t('admin.companion.settings.loadFailed') }}</p>
            <p v-if="props.error.message" class="mt-1 break-all font-mono text-xs opacity-80">
              {{ props.error.message }}
            </p>
          </div>
        </div>
      </div>

      <div v-if="props.loading && !props.settings" class="grid grid-cols-1 gap-4 md:grid-cols-2">
        <div
          v-for="i in 4"
          :key="i"
          class="h-[62px] animate-pulse rounded-xl bg-gray-100 dark:bg-dark-900"
        ></div>
      </div>

      <template v-else-if="props.settings">
        <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
          <div class="min-w-0">
            <label class="mb-1 block text-xs font-medium text-gray-600 dark:text-gray-400">
              {{ t('admin.companion.settings.baseUrl') }}
            </label>
            <input
              v-model="draft.baseUrl"
              type="text"
              class="input w-full"
              data-testid="a6-settings-base-url"
              :placeholder="t('admin.companion.settings.baseUrlPlaceholder')"
              :disabled="busy"
              @input="onTouch"
            />
          </div>

          <div class="min-w-0">
            <label class="mb-1 block text-xs font-medium text-gray-600 dark:text-gray-400">
              {{ t('admin.companion.settings.userId') }}
            </label>
            <input
              v-model="draft.userId"
              type="text"
              class="input w-full"
              data-testid="a6-settings-user-id"
              :placeholder="t('admin.companion.settings.userIdPlaceholder')"
              :disabled="busy"
              @input="onTouch"
            />
            <!-- 这里必须写清「不是分组名/账号名/令牌名」：线上真实踩过把中文分组名填进来
                 的坑，而 A6（new-api）只会回一句笼统的 401，让人完全看不出问题在哪。
                 非数字只改颜色与措辞提示、不拦截保存，理由见 userIdLooksInvalid。 -->
            <p
              class="mt-1 text-xs"
              :class="
                userIdLooksInvalid
                  ? 'text-amber-600 dark:text-amber-400'
                  : 'text-gray-500 dark:text-gray-400'
              "
              data-testid="a6-settings-user-id-hint"
            >
              {{
                userIdLooksInvalid
                  ? t('admin.companion.settings.userIdWarning')
                  : t('admin.companion.settings.userIdHint')
              }}
            </p>
          </div>

          <div class="min-w-0">
            <label class="mb-1 block text-xs font-medium text-gray-600 dark:text-gray-400">
              {{ t('admin.companion.settings.accessToken') }}
            </label>
            <input
              v-model="draft.accessToken"
              type="password"
              autocomplete="new-password"
              class="input w-full font-mono"
              data-testid="a6-settings-access-token"
              :placeholder="tokenPlaceholder"
              :disabled="busy"
              @input="onTouch"
            />
          </div>

          <div class="min-w-0">
            <label class="mb-1 block text-xs font-medium text-gray-600 dark:text-gray-400">
              {{ t('admin.companion.settings.fxRate') }}
            </label>
            <input
              v-model="draft.fxRate"
              type="number"
              step="0.01"
              min="0"
              class="input w-full"
              data-testid="a6-settings-fx-rate"
              :disabled="busy"
              @input="onTouch"
            />
            <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
              {{ t('admin.companion.settings.fxRateHint') }}
            </p>
          </div>
        </div>

        <p class="mt-4 flex items-start gap-2 text-xs text-gray-500 dark:text-gray-400">
          <Icon name="lock" size="sm" class="mt-0.5 shrink-0" />
          <span>{{ t('admin.companion.settings.blankHint') }}</span>
        </p>

        <div
          v-if="overrideKeys.length > 0"
          class="mt-2 flex flex-wrap items-center gap-2 text-xs text-gray-400 dark:text-gray-500"
        >
          <span>{{ t('admin.companion.settings.overrideLabel') }}</span>
          <span
            v-for="item in overrideKeys"
            :key="item.key"
            class="rounded-full bg-gray-100 px-2 py-0.5 font-medium text-gray-600 dark:bg-dark-900 dark:text-gray-300"
          >
            {{ item.label }}
          </span>
        </div>

        <div
          class="mt-5 flex flex-wrap items-center gap-3 border-t border-gray-100 pt-4 dark:border-dark-700"
        >
          <button
            type="button"
            class="btn btn-primary btn-sm"
            data-testid="a6-settings-save"
            :disabled="busy"
            @click="onSave"
          >
            <Icon name="check" size="sm" class="mr-1.5" />
            {{
              props.saving
                ? t('admin.companion.settings.saving')
                : t('admin.companion.settings.save')
            }}
          </button>
          <button
            v-if="props.settings.a6_token_configured"
            type="button"
            class="btn btn-danger btn-sm"
            data-testid="a6-settings-clear-token"
            :disabled="busy"
            @click="emit('clear-token')"
          >
            <Icon name="trash" size="sm" class="mr-1.5" />
            {{
              props.clearing
                ? t('admin.companion.settings.clearing')
                : t('admin.companion.settings.clearToken')
            }}
          </button>
        </div>
      </template>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores'
import Icon from '@/components/icons/Icon.vue'
import type {
  CompanionErrorInfo,
  CompanionSettings,
  CompanionSettingsInput
} from '@/api/admin/companion'

interface Props {
  /** 服务端配置；为 null 表示还没读到（首次加载中或加载失败） */
  settings: CompanionSettings | null
  /** 读取失败信息 */
  error?: CompanionErrorInfo | null
  loading?: boolean
  saving?: boolean
  clearing?: boolean
  /** 访问令牌未配置时高亮卡片，配合页面级引导 */
  highlight?: boolean
}

const props = withDefaults(defineProps<Props>(), {
  error: null,
  loading: false,
  saving: false,
  clearing: false,
  highlight: false
})

const emit = defineEmits<{
  (e: 'save', payload: CompanionSettingsInput): void
  (e: 'clear-token'): void
  (e: 'reload'): void
}>()

const { t } = useI18n()
const appStore = useAppStore()

/** 后端没有给出有效汇率时的兜底默认值 */
const DEFAULT_FX_RATE = '6.71'

/** override_keys 字段名 → 展示用文案 key */
const OVERRIDE_LABEL_KEY: Record<string, string> = {
  a6_base_url: 'admin.companion.settings.baseUrl',
  a6_user_id: 'admin.companion.settings.userId',
  a6_access_token: 'admin.companion.settings.accessToken',
  fx_usd_cny_rate: 'admin.companion.settings.fxRate'
}

interface SettingsDraft {
  baseUrl: string
  userId: string
  /** 令牌明文只存在于这个本地草稿里，提交后由父组件重建卡片清空 */
  accessToken: string
  /** 注意：<input type="number"> 的 v-model 会把值强制转成 number，清空时是空串 */
  fxRate: string | number
  /** 用户是否手工改过；未改动的草稿会随后端刷新重新同步 */
  dirty: boolean
}

/** 汇率按 number 约定，但顺手容忍数字字符串，避免把默认值当成本地改动提交 */
function fxRateText(rate: number | string | undefined): string {
  const num = typeof rate === 'string' ? Number(rate) : rate
  return typeof num === 'number' && Number.isFinite(num) && num > 0
    ? String(num)
    : DEFAULT_FX_RATE
}

function buildDraft(settings: CompanionSettings | null): SettingsDraft {
  return {
    baseUrl: settings?.a6_base_url ?? '',
    userId: settings?.a6_user_id ?? '',
    accessToken: '',
    fxRate: fxRateText(settings?.fx_usd_cny_rate),
    dirty: false
  }
}

const draft = ref<SettingsDraft>(buildDraft(null))

// 只重建未改动的草稿：后台刷新不能覆盖用户正在编辑的内容
watch(
  () => props.settings,
  (settings) => {
    if (draft.value.dirty) return
    draft.value = buildDraft(settings)
  },
  { immediate: true }
)

function onTouch() {
  draft.value.dirty = true
}

const busy = computed(() => props.loading || props.saving || props.clearing)

const tokenBadgeClass = computed(() => {
  const base = 'rounded-full px-3 py-1 text-xs font-semibold '
  return props.settings?.a6_token_configured
    ? base + 'bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-300'
    : base + 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
})

/** 已配置时用脱敏串做占位提示，未配置时提示去填写 */
const tokenPlaceholder = computed(() => {
  const settings = props.settings
  if (!settings?.a6_token_configured) return t('admin.companion.settings.accessTokenPlaceholder')
  const mask = (settings.a6_token_mask || '').trim()
  if (!mask) return t('admin.companion.settings.accessTokenConfiguredNoMask')
  return t('admin.companion.settings.accessTokenConfiguredPlaceholder', { mask })
})

const overrideKeys = computed(() =>
  (props.settings?.override_keys ?? []).map((key) => ({
    key,
    label: OVERRIDE_LABEL_KEY[key] ? t(OVERRIDE_LABEL_KEY[key]) : key
  }))
)

/**
 * 「A6 用户标识」看起来不像纯数字用户 ID 时给出提示。
 *
 * 只提示、不拦截保存。依据有二：
 *  1. 拦截会挡住修复路径——填错的值改不动，管理员连「先改汇率、回头再修用户标识」都做不到；
 *  2. A6（new-api）自己对非法值会明确报 New-Api-User header format error，
 *     真错了由连接状态与错误详情来说话，比前端猜更准。
 * 这一条与「已填写 ≠ 已验证」是同一个原则：前端只负责把话说清楚，不下结论。
 */
const userIdLooksInvalid = computed(() => {
  const value = draft.value.userId.trim()
  return value !== '' && !/^\d+$/.test(value)
})

/**
 * 按「只提交改动过的字段」组装请求体：后端把缺省/空串一律当作不改动，
 * 因此空白的输入框直接跳过，避免把环境变量里的值误固化成页面配置。
 */
function buildPayload(): CompanionSettingsInput | null {
  const settings = props.settings
  if (!settings) return null

  const payload: CompanionSettingsInput = {}

  const baseUrl = draft.value.baseUrl.trim()
  if (baseUrl !== '' && baseUrl !== (settings.a6_base_url || '').trim()) {
    if (!/^https?:\/\//i.test(baseUrl)) {
      appStore.showError(t('admin.companion.settings.invalidBaseUrl'))
      return null
    }
    payload.a6_base_url = baseUrl
  }

  const userId = draft.value.userId.trim()
  if (userId !== '' && userId !== (settings.a6_user_id || '').trim()) {
    payload.a6_user_id = userId
  }

  // 令牌没有可比对的明文基准：留空 = 不修改，填了才提交
  const accessToken = draft.value.accessToken.trim()
  if (accessToken !== '') {
    payload.a6_access_token = accessToken
  }

  const rateText = String(draft.value.fxRate ?? '').trim()
  if (rateText !== '') {
    const rate = Number(rateText)
    if (!Number.isFinite(rate) || rate <= 0) {
      appStore.showError(t('admin.companion.settings.invalidFxRate'))
      return null
    }
    if (rate !== Number(settings.fx_usd_cny_rate)) {
      payload.fx_usd_cny_rate = rate
    }
  }

  return payload
}

function onSave() {
  const payload = buildPayload()
  if (!payload) return
  if (Object.keys(payload).length === 0) {
    appStore.showInfo(t('admin.companion.settings.nothingChanged'))
    return
  }
  emit('save', payload)
}
</script>
