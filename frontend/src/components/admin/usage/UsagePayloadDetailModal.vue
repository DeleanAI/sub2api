<template>
  <BaseDialog
    :show="show"
    :title="t('usage.requestPayload')"
    width="extra-wide"
    @close="emit('close')"
  >
    <div v-if="loading" class="flex min-h-48 items-center justify-center text-sm text-gray-500 dark:text-gray-400">
      {{ t('common.loading') }}
    </div>

    <div v-else-if="unavailable" class="flex min-h-48 items-center justify-center text-center text-sm text-gray-500 dark:text-gray-400">
      {{ t('usage.requestPayloadUnavailable') }}
    </div>

    <div v-else-if="errorMessage" class="flex min-h-48 items-center justify-center text-center text-sm text-rose-600 dark:text-rose-400">
      {{ errorMessage }}
    </div>

    <div v-else-if="payload" class="space-y-5">
      <dl class="grid grid-cols-1 gap-x-6 gap-y-3 border-b border-gray-200 pb-5 text-sm dark:border-dark-700 sm:grid-cols-2 lg:grid-cols-4">
        <div>
          <dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('usage.method') }}</dt>
          <dd class="mt-1 font-mono text-gray-900 dark:text-white">{{ metadataText('method') }}</dd>
        </div>
        <div>
          <dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('usage.statusCode') }}</dt>
          <dd class="mt-1 font-mono text-gray-900 dark:text-white">{{ metadataText('status_code') }}</dd>
        </div>
        <div class="sm:col-span-2">
          <dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('usage.path') }}</dt>
          <dd class="mt-1 break-all font-mono text-gray-900 dark:text-white">{{ metadataText('path') }}</dd>
        </div>
        <div>
          <dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('usage.storedSize') }}</dt>
          <dd class="mt-1 font-mono text-gray-900 dark:text-white">{{ formatBytes(payload.stored_bytes) }}</dd>
        </div>
        <div>
          <dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('usage.requestBytes') }}</dt>
          <dd class="mt-1 font-mono text-gray-900 dark:text-white">{{ formatBytes(metadataNumber('request_bytes')) }}</dd>
        </div>
        <div>
          <dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('usage.responseBytes') }}</dt>
          <dd class="mt-1 font-mono text-gray-900 dark:text-white">{{ formatBytes(metadataNumber('response_bytes')) }}</dd>
        </div>
        <div>
          <dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('usage.capturedAt') }}</dt>
          <dd class="mt-1 break-all font-mono text-xs text-gray-900 dark:text-white">{{ metadataText('captured_at') }}</dd>
        </div>
      </dl>

      <div class="grid grid-cols-1 gap-5 xl:grid-cols-2">
        <section class="min-w-0">
          <h4 class="mb-2 text-sm font-semibold text-gray-900 dark:text-white">{{ t('usage.requestBody') }}</h4>
          <pre class="max-h-[48vh] min-h-36 overflow-auto rounded-md border border-gray-200 bg-gray-50 p-3 font-mono text-xs leading-5 text-gray-800 dark:border-dark-600 dark:bg-dark-900 dark:text-gray-200">{{ formatBody(payload.request_body) }}</pre>
        </section>
        <section class="min-w-0">
          <h4 class="mb-2 text-sm font-semibold text-gray-900 dark:text-white">{{ t('usage.responseBody') }}</h4>
          <pre class="max-h-[48vh] min-h-36 overflow-auto rounded-md border border-gray-200 bg-gray-50 p-3 font-mono text-xs leading-5 text-gray-800 dark:border-dark-600 dark:bg-dark-900 dark:text-gray-200">{{ formatBody(payload.response_body) }}</pre>
        </section>
      </div>

      <section>
        <h4 class="mb-2 text-sm font-semibold text-gray-900 dark:text-white">{{ t('usage.payloadMetadata') }}</h4>
        <pre class="max-h-56 overflow-auto rounded-md border border-gray-200 bg-gray-50 p-3 font-mono text-xs leading-5 text-gray-800 dark:border-dark-600 dark:bg-dark-900 dark:text-gray-200">{{ JSON.stringify(payload.metadata, null, 2) }}</pre>
      </section>

      <section v-if="conversationPreview.hasContent">
        <h4 class="mb-2 text-sm font-semibold text-gray-900 dark:text-white">{{ t('usage.conversationPreview') }}</h4>
        <div class="grid grid-cols-1 gap-5 xl:grid-cols-2">
          <article v-if="conversationPreview.userText" class="min-w-0">
            <h5 class="mb-2 text-xs font-medium text-gray-500 dark:text-gray-400">{{ t('usage.lastUserText') }}</h5>
            <div
              class="payload-markdown max-h-[40vh] overflow-auto rounded-md border border-gray-200 bg-white p-4 text-sm text-gray-800 dark:border-dark-600 dark:bg-dark-900 dark:text-gray-200"
              data-testid="last-user-markdown"
              v-html="renderMarkdown(conversationPreview.userText)"
            ></div>
          </article>
          <article v-if="conversationPreview.modelOutput" class="min-w-0">
            <h5 class="mb-2 text-xs font-medium text-gray-500 dark:text-gray-400">{{ t('usage.modelOutput') }}</h5>
            <div
              class="payload-markdown max-h-[40vh] overflow-auto rounded-md border border-gray-200 bg-white p-4 text-sm text-gray-800 dark:border-dark-600 dark:bg-dark-900 dark:text-gray-200"
              data-testid="model-output-markdown"
              v-html="renderMarkdown(conversationPreview.modelOutput)"
            ></div>
          </article>
        </div>
      </section>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import type { RequestPayloadDetail } from '@/types'

const props = defineProps<{
  show: boolean
  payload: RequestPayloadDetail | null
  loading: boolean
  unavailable: boolean
  errorMessage?: string
}>()

const emit = defineEmits<{ close: [] }>()
const { t } = useI18n()

type PayloadObject = Record<string, unknown>

marked.setOptions({
  breaks: true,
  gfm: true,
})

const conversationPreview = computed(() => {
  const request = parseBody(props.payload?.request_body)
  const response = parseBody(props.payload?.response_body)
  const userText = extractLastUserText(request)
  const modelOutput = extractModelOutput(response)

  return {
    userText,
    modelOutput,
    hasContent: Boolean(userText || modelOutput),
  }
})

const metadataText = (key: string): string => {
  const value = props.payload?.metadata?.[key]
  if (value == null || value === '') return '-'
  return typeof value === 'string' ? value : JSON.stringify(value)
}

const metadataNumber = (key: string): number => {
  const value = props.payload?.metadata?.[key]
  return typeof value === 'number' && Number.isFinite(value) ? value : 0
}

const formatBytes = (bytes: number): string => {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`
  return `${(bytes / (1024 * 1024)).toFixed(2)} MiB`
}

const formatBody = (body: string): string => {
  if (!body) return t('usage.emptyPayload')
  try {
    return JSON.stringify(JSON.parse(body), null, 2)
  } catch {
    return body
  }
}

const renderMarkdown = (content: string): string => {
  const html = marked.parse(content) as string
  return DOMPurify.sanitize(html)
}

const parseBody = (body: string | undefined): unknown => {
  if (!body) return null
  try {
    return JSON.parse(body)
  } catch {
    return body
  }
}

const isObject = (value: unknown): value is PayloadObject => (
  typeof value === 'object' && value !== null && !Array.isArray(value)
)

const joinText = (parts: string[]): string => parts.filter(Boolean).join('\n\n').trim()

const textFromContent = (value: unknown): string => {
  if (typeof value === 'string') return value.trim()

  if (Array.isArray(value)) {
    return joinText(value.map(textFromContent))
  }

  if (!isObject(value)) return ''

  if (typeof value.text === 'string') return value.text.trim()
  if (typeof value.output_text === 'string') return value.output_text.trim()
  if (typeof value.content === 'string') return value.content.trim()
  if (typeof value.value === 'string' && ['text', 'output_text', 'input_text'].includes(String(value.type))) {
    return value.value.trim()
  }

  if (Array.isArray(value.content)) return textFromContent(value.content)
  if (Array.isArray(value.parts)) return textFromContent(value.parts)
  return ''
}

const findLastText = (items: unknown[], predicate: (item: PayloadObject) => boolean): string => {
  for (let index = items.length - 1; index >= 0; index -= 1) {
    const item = items[index]
    if (!isObject(item) || !predicate(item)) continue
    const text = textFromContent(item.content ?? item.parts ?? item)
    if (text) return text
  }
  return ''
}

const extractLastUserText = (request: unknown): string => {
  if (typeof request === 'string') return request.trim()
  if (!isObject(request)) return ''

  if (Array.isArray(request.messages)) {
    const text = findLastText(request.messages, (message) => String(message.role).toLowerCase() === 'user')
    if (text) return text
  }

  if (Array.isArray(request.input)) {
    const text = findLastText(request.input, (message) => !message.role || String(message.role).toLowerCase() === 'user')
    if (text) return text
  }
  if (typeof request.input === 'string') return request.input.trim()

  if (Array.isArray(request.contents)) {
    const text = findLastText(request.contents, (content) => String(content.role).toLowerCase() === 'user')
    if (text) return text
  }

  if (typeof request.prompt === 'string') return request.prompt.trim()
  return ''
}

const extractModelOutput = (response: unknown): string => {
  if (typeof response === 'string') return response.trim()
  if (!isObject(response)) return ''

  if (Array.isArray(response.output)) {
    const text = findLastText(response.output, (output) => !output.role || String(output.role).toLowerCase() === 'assistant')
    if (text) return text
  }

  if (Array.isArray(response.choices)) {
    for (let index = response.choices.length - 1; index >= 0; index -= 1) {
      const choice = response.choices[index]
      if (!isObject(choice)) continue
      const text = textFromContent(choice.message ?? choice.delta ?? choice.text)
      if (text) return text
    }
  }

  if (Array.isArray(response.content)) {
    const text = textFromContent(response.content)
    if (text) return text
  }

  if (Array.isArray(response.candidates)) {
    for (let index = response.candidates.length - 1; index >= 0; index -= 1) {
      const candidate = response.candidates[index]
      if (!isObject(candidate)) continue
      const text = textFromContent(candidate.content ?? candidate.parts)
      if (text) return text
    }
  }

  return textFromContent(response.output_text ?? response.text)
}
</script>

<style scoped>
.payload-markdown :deep(h1),
.payload-markdown :deep(h2),
.payload-markdown :deep(h3),
.payload-markdown :deep(h4) {
  margin: 1rem 0 0.5rem;
  font-size: 1.05rem;
  font-weight: 600;
  line-height: 1.4;
}

.payload-markdown :deep(h1:first-child),
.payload-markdown :deep(h2:first-child),
.payload-markdown :deep(h3:first-child),
.payload-markdown :deep(h4:first-child) {
  margin-top: 0;
}

.payload-markdown :deep(p) {
  margin: 0 0 0.75rem;
  line-height: 1.6;
}

.payload-markdown :deep(p:last-child) {
  margin-bottom: 0;
}

.payload-markdown :deep(ul),
.payload-markdown :deep(ol) {
  margin: 0 0 0.75rem 1.25rem;
}

.payload-markdown :deep(ul) {
  list-style: disc;
}

.payload-markdown :deep(ol) {
  list-style: decimal;
}

.payload-markdown :deep(pre) {
  margin: 0.75rem 0;
  overflow-x: auto;
  border-radius: 0.25rem;
  background: rgb(249 250 251);
  padding: 0.75rem;
  font-size: 0.75rem;
}

.payload-markdown :deep(code) {
  border-radius: 0.25rem;
  background: rgb(243 244 246);
  padding: 0.1rem 0.25rem;
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
  font-size: 0.85em;
}

.dark .payload-markdown :deep(pre) {
  background: rgb(17 24 39);
}

.dark .payload-markdown :deep(code) {
  background: rgb(55 65 81);
}
</style>
