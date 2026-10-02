<template>
  <div :class="['space-y-0.5 text-xs', tooltip ? 'text-gray-300' : 'text-gray-500 dark:text-gray-400']" data-testid="currency-conversion-note">
    <div v-if="amounts" :class="['font-medium', tooltip ? 'text-white' : 'text-gray-700 dark:text-gray-300']">{{ amounts }}</div>
    <div v-for="leg in conversion.legs" :key="`${leg.currency}-${leg.rate_date}`">
      {{ formatQuote(leg) }}
      <span :class="tooltip ? 'text-gray-400' : 'text-gray-400 dark:text-gray-500'">
        ·
        <a v-if="leg.source_url && !tooltip" :href="leg.source_url" target="_blank" rel="noopener noreferrer" class="hover:underline">{{ sourceLabel(leg.source) }}</a>
        <template v-else>{{ sourceLabel(leg.source) }}</template>
        {{ leg.rate_date }}
      </span>
    </div>
    <div v-if="conversion.stale" :class="tooltip ? 'text-amber-300' : 'text-amber-600 dark:text-amber-400'">{{ t('currencyConversion.stale') }}</div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { CurrencyConversion } from '@/types'
import { formatConversionAmounts, formatQuote, quoteSourceKey } from '@/utils/currencyConversion'

// variant="tooltip" 用在深色浮层里（用量明细的费用浮层），不放外链。
const props = defineProps<{ conversion: CurrencyConversion, variant?: 'default' | 'tooltip' }>()
const { t, locale } = useI18n()

const tooltip = computed(() => props.variant === 'tooltip')
const amounts = computed(() => formatConversionAmounts(props.conversion, locale.value))

function sourceLabel(source: string): string {
  const key = quoteSourceKey(source)
  return key ? t(key) : source
}
</script>
