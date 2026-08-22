<template>
  <div class="border-t border-gray-200 pt-4 mt-4 dark:border-dark-400">
    <div class="flex items-start justify-between gap-4">
      <div>
        <h4 class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('admin.groups.modelRateMultipliers.title') }}</h4>
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.groups.modelRateMultipliers.description') }}</p>
      </div>
      <button type="button" class="btn btn-secondary" data-testid="model-rate-multipliers-add" @click="addEntry">
        <Icon name="plus" size="sm" class="mr-1" />{{ t('admin.groups.modelRateMultipliers.add') }}
      </button>
    </div>

    <div v-if="entries.length === 0" class="mt-3 text-xs italic text-gray-400 dark:text-gray-500">
      {{ t('admin.groups.modelRateMultipliers.empty') }}
    </div>

    <div v-else class="mt-3 space-y-2">
      <div class="grid grid-cols-[minmax(0,1fr)_7rem_minmax(0,1fr)_2rem] items-center gap-2 px-1 text-xs font-medium text-gray-500 dark:text-gray-400">
        <span>{{ t('admin.groups.modelRateMultipliers.pattern') }}</span>
        <span>{{ t('admin.groups.modelRateMultipliers.multiplier') }}</span>
        <span>{{ t('admin.groups.modelRateMultipliers.preview') }}</span>
        <span></span>
      </div>
      <div
        v-for="(entry, index) in entries"
        :key="index"
        class="rounded-lg border border-gray-200 bg-gray-50 p-2 dark:border-dark-600 dark:bg-dark-800"
        data-testid="model-rate-multiplier-row"
      >
        <div class="grid grid-cols-[minmax(0,1fr)_7rem_minmax(0,1fr)_2rem] items-center gap-2">
          <input
            :value="entry.model_pattern"
            type="text"
            autocomplete="off"
            class="input w-full font-mono text-sm"
            :class="{ 'border-red-400 dark:border-red-500': rowError(index) }"
            :placeholder="t('admin.groups.modelRateMultipliers.patternPlaceholder')"
            data-testid="model-rate-multiplier-pattern"
            @input="updateEntry(index, { model_pattern: ($event.target as HTMLInputElement).value })"
          />
          <input
            :value="entry.multiplier ?? ''"
            type="number"
            step="0.001"
            min="0.001"
            :max="MAX_GROUP_MODEL_RATE_MULTIPLIER"
            autocomplete="off"
            class="hide-spinner input w-full text-center text-sm font-medium"
            :class="{ 'border-red-400 dark:border-red-500': rowError(index) }"
            placeholder="1.0"
            data-testid="model-rate-multiplier-value"
            @input="updateEntry(index, { multiplier: ($event.target as HTMLInputElement).value })"
          />
          <span class="truncate text-xs text-gray-500 dark:text-gray-400" :title="previewText(entry)">
            {{ previewText(entry) }}
          </span>
          <button
            type="button"
            class="flex-shrink-0 rounded p-1 text-gray-400 hover:text-red-500"
            :aria-label="t('common.delete')"
            data-testid="model-rate-multiplier-remove"
            @click="removeEntry(index)"
          >
            <Icon name="trash" size="sm" />
          </button>
        </div>
        <p v-if="rowError(index)" class="mt-1 text-xs text-red-500" data-testid="model-rate-multiplier-error">
          {{ t(`admin.groups.modelRateMultipliers.${rowError(index)}`) }}
        </p>
      </div>
      <p class="text-xs text-gray-400 dark:text-gray-500">{{ t('admin.groups.modelRateMultipliers.orderHint') }}</p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import {
  MAX_GROUP_MODEL_RATE_MULTIPLIER,
  emptyModelRateMultiplierEntry,
  validateModelRateMultiplierEntries,
  type ModelRateMultiplierFormEntry,
} from '@/views/admin/groupsModelRateMultipliers'

// 分组逐模型倍率编辑器：受控组件，创建/编辑分组表单共用。
// 校验口径与提交前校验同源（validateModelRateMultiplierEntries），只负责就地提示，
// 最终以后端 NormalizeGroupModelRateMultipliers 为准。
const props = defineProps<{
  entries: ModelRateMultiplierFormEntry[]
  /** 分组基础倍率，用于预览"基础 × 逐模型因子"的最终倍率 */
  baseRateMultiplier: number | string | null
}>()

const emit = defineEmits<{
  'update:entries': [entries: ModelRateMultiplierFormEntry[]]
}>()

const { t } = useI18n()

const validationError = computed(() => validateModelRateMultiplierEntries(props.entries))

// 只在出错行展示提示；校验返回第一条出错行，后续行待前一行修正后再提示。
const rowError = (index: number): string | null => {
  const error = validationError.value
  return error && error.index === index ? error.errorKey : null
}

const previewText = (entry: ModelRateMultiplierFormEntry): string => {
  const base = Number(props.baseRateMultiplier)
  const factor = Number(entry.multiplier)
  if (!Number.isFinite(base) || base <= 0 || !Number.isFinite(factor) || factor <= 0) {
    return '—'
  }
  return t('admin.groups.modelRateMultipliers.previewFormula', {
    base: formatRate(base),
    factor: formatRate(factor),
    effective: formatRate(base * factor),
  })
}

const formatRate = (value: number): string => parseFloat(value.toFixed(6)).toString()

const addEntry = () => {
  emit('update:entries', [...props.entries, emptyModelRateMultiplierEntry()])
}

const removeEntry = (index: number) => {
  emit('update:entries', props.entries.filter((_, i) => i !== index))
}

const updateEntry = (index: number, patch: Partial<ModelRateMultiplierFormEntry>) => {
  emit(
    'update:entries',
    props.entries.map((entry, i) => (i === index ? { ...entry, ...patch } : entry)),
  )
}
</script>

<style scoped>
.hide-spinner::-webkit-outer-spin-button,
.hide-spinner::-webkit-inner-spin-button {
  -webkit-appearance: none;
  margin: 0;
}
.hide-spinner {
  -moz-appearance: textfield;
}
</style>
