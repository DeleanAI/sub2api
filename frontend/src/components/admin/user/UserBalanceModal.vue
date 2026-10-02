<template>
  <BaseDialog :show="show" :title="operation === 'add' ? t('admin.users.deposit') : t('admin.users.withdraw')" width="narrow" @close="$emit('close')">
    <form v-if="user" id="balance-form" @submit.prevent="handleBalanceSubmit" class="space-y-5">
      <div class="flex items-center gap-3 rounded-xl bg-gray-50 p-4 dark:bg-dark-700">
        <div class="flex h-10 w-10 items-center justify-center rounded-full bg-primary-100"><span class="text-lg font-medium text-primary-700">{{ user.email.charAt(0).toUpperCase() }}</span></div>
        <div class="flex-1"><p class="font-medium text-gray-900 dark:text-gray-100">{{ user.email }}</p><p class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.users.currentBalance') }}: {{ $currency }}{{ formatBalance(user.balance) }}</p></div>
      </div>
      <div>
        <label class="input-label">{{ operation === 'add' ? t('admin.users.depositAmount') : t('admin.users.withdrawAmount') }}</label>
        <div class="relative flex gap-2">
          <select v-model="form.currency" class="input w-28 shrink-0 font-mono" data-testid="balance-currency" :aria-label="t('admin.users.balanceCurrency')">
            <option v-for="code in currencyChoices" :key="code" :value="code">{{ code }}</option>
          </select>
          <input v-model.number="form.amount" type="number" step="any" min="0" required class="input flex-1" />
          <button v-if="operation === 'subtract'" type="button" @click="fillAllBalance" class="btn btn-secondary whitespace-nowrap">{{ t('admin.users.withdrawAll') }}</button>
        </div>
        <p v-if="isForeign" class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.users.balanceCurrencyHint', { accounting: accountingCurrency }) }}</p>
      </div>
      <div><label class="input-label">{{ t('admin.users.notes') }}</label><textarea v-model="form.notes" rows="3" class="input"></textarea></div>
      <div v-if="form.amount > 0" class="space-y-2 rounded-xl border border-blue-200 bg-blue-50 p-4 dark:border-blue-800 dark:bg-blue-950">
        <template v-if="isForeign">
          <p v-if="previewLoading" class="text-sm text-gray-500 dark:text-gray-400">{{ t('admin.users.convertingPreview') }}</p>
          <p v-else-if="previewError" class="text-sm text-red-600 dark:text-red-400" data-testid="balance-preview-error">{{ previewError }}</p>
          <template v-else-if="preview">
            <div class="flex items-center justify-between text-sm"><span class="text-gray-700 dark:text-gray-300">{{ t('admin.users.creditedAmount') }}:</span><span class="font-medium text-gray-900 dark:text-gray-100" data-testid="balance-credited">{{ $currency }}{{ formatBalance(preview.converted) }}</span></div>
            <CurrencyConversionNote v-if="preview.conversion" :conversion="preview.conversion" />
          </template>
        </template>
        <div v-if="creditAmount !== null" class="flex items-center justify-between text-sm"><span class="text-gray-700 dark:text-gray-300">{{ t('admin.users.newBalance') }}:</span><span class="font-bold text-gray-900 dark:text-gray-100">{{ $currency }}{{ formatBalance(calculateNewBalance()) }}</span></div>
      </div>
    </form>
    <template #footer>
      <div class="flex justify-end gap-3">
        <button @click="$emit('close')" class="btn btn-secondary">{{ t('common.cancel') }}</button>
        <button type="submit" form="balance-form" :disabled="submitting || !form.amount || creditAmount === null" class="btn" :class="operation === 'add' ? 'bg-emerald-600 text-white' : 'btn-danger'">{{ submitting ? t('common.saving') : t('common.confirm') }}</button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import type { ExchangeRateConversion } from '@/api/admin/exchangeRates'
import { extractApiErrorMessage } from '@/utils/apiError'
import { balanceCurrencyCode } from '@/utils/balanceCurrency'
import { useCurrencyOptions } from '@/composables/useCurrencyOptions'
import type { AdminUser } from '@/types'
import BaseDialog from '@/components/common/BaseDialog.vue'
import CurrencyConversionNote from '@/components/common/CurrencyConversionNote.vue'

const props = defineProps<{ show: boolean, user: AdminUser | null, operation: 'add' | 'subtract' }>()
const emit = defineEmits(['close', 'success']); const { t } = useI18n(); const appStore = useAppStore()
const { options: currencyOptions } = useCurrencyOptions()

// 金额按选中的币种填：记账币种原样入账；人民币 / 稳定币由后端按当前汇率折算后入账，这里只做同口径的预览。
const accountingCurrency = computed(() => currencyOptions.value?.accounting_currency || balanceCurrencyCode())
const currencyChoices = computed(() => currencyOptions.value?.currencies?.length ? currencyOptions.value.currencies : [accountingCurrency.value])

const submitting = ref(false); const form = reactive({ amount: 0, notes: '', currency: '' })
const isForeign = computed(() => !!form.currency && form.currency !== accountingCurrency.value)
watch(() => props.show, (v) => { if (v) { form.amount = 0; form.notes = ''; form.currency = accountingCurrency.value } })
watch(accountingCurrency, (code, previous) => { if (!form.currency || form.currency === previous) form.currency = code })

const preview = ref<ExchangeRateConversion | null>(null)
const previewError = ref('')
const previewLoading = ref(false)
let previewSeq = 0
let previewTimer: ReturnType<typeof setTimeout> | null = null

watch(() => [form.amount, form.currency, isForeign.value] as const, () => {
  if (previewTimer) clearTimeout(previewTimer)
  preview.value = null
  previewError.value = ''
  if (!isForeign.value || !(form.amount > 0)) { previewLoading.value = false; return }
  previewLoading.value = true
  const seq = ++previewSeq
  previewTimer = setTimeout(async () => {
    try {
      const result = await adminAPI.exchangeRates.convert(form.amount, form.currency)
      if (seq === previewSeq) preview.value = result
    } catch (e: unknown) {
      if (seq === previewSeq) previewError.value = extractApiErrorMessage(e, t('admin.users.convertFailed'))
    } finally {
      if (seq === previewSeq) previewLoading.value = false
    }
  }, 300)
})

/** 入账的记账币种金额；外币还没拿到折算结果时为 null（不能提交）。 */
const creditAmount = computed<number | null>(() => {
  if (!isForeign.value) return form.amount
  return preview.value ? preview.value.converted : null
})

// 格式化余额：显示完整精度，去除尾部多余的0
const formatBalance = (value: number) => {
  if (value === 0) return '0.00'
  // 最多保留8位小数，去除尾部的0
  const formatted = value.toFixed(8).replace(/\.?0+$/, '')
  // 确保至少有2位小数
  const parts = formatted.split('.')
  if (parts.length === 1) return formatted + '.00'
  if (parts[1].length === 1) return formatted + '0'
  return formatted
}

// 填入全部余额（余额是记账币种）
const fillAllBalance = () => {
  if (props.user) {
    form.currency = accountingCurrency.value
    form.amount = props.user.balance
  }
}

const calculateNewBalance = () => {
  if (!props.user || creditAmount.value === null) return 0
  const result = props.operation === 'add' ? props.user.balance + creditAmount.value : props.user.balance - creditAmount.value
  // 避免浮点数精度问题导致的 -0.00 显示
  return Math.abs(result) < 1e-10 ? 0 : result
}
const handleBalanceSubmit = async () => {
  if (!props.user) return
  if (!form.amount || form.amount <= 0) {
    appStore.showError(t('admin.users.amountRequired'))
    return
  }
  if (creditAmount.value === null) return
  // 退款时验证金额不超过实际余额
  if (props.operation === 'subtract' && creditAmount.value > props.user.balance) {
    appStore.showError(t('admin.users.insufficientBalance'))
    return
  }
  submitting.value = true
  try {
    await adminAPI.users.updateBalance(props.user.id, form.amount, props.operation, form.notes, isForeign.value ? form.currency : undefined)
    appStore.showSuccess(t('common.success')); emit('success'); emit('close')
  } catch (e: any) {
    console.error('Failed to update balance:', e)
    appStore.showError(extractApiErrorMessage(e, t('common.error')))
  } finally { submitting.value = false }
}
</script>
