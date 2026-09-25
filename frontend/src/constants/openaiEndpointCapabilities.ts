import type { OpenAIEndpointCapability } from '@/types'

/**
 * 账号可在 credentials.openai_capabilities 里声明的端点能力，顺序即存储顺序。
 * 与后端 service.ConfigurableOpenAIEndpointCapabilities 一致——后端测试
 * TestFrontendOpenAIEndpointCapabilitiesMatchBackend 读这个文件对齐两边。
 */
export const CONFIGURABLE_OPENAI_ENDPOINT_CAPABILITIES: readonly OpenAIEndpointCapability[] = [
  'chat_completions',
  'embeddings',
  'seedance'
]

/**
 * 未配置 openai_capabilities 时等价的能力集（后端 service.DefaultOpenAIEndpointCapabilities）。
 * 不含 Seedance：它必须显式开启。
 */
export const DEFAULT_OPENAI_ENDPOINT_CAPABILITIES: readonly OpenAIEndpointCapability[] = [
  'chat_completions',
  'embeddings'
]

export const isOpenAIEndpointCapability = (value: unknown): value is OpenAIEndpointCapability =>
  typeof value === 'string' &&
  (CONFIGURABLE_OPENAI_ENDPOINT_CAPABILITIES as readonly string[]).includes(value)

/** 按声明顺序保留可配置的能力；一个都没有时回落到默认集。 */
export const normalizeOpenAIEndpointCapabilities = (
  values: readonly OpenAIEndpointCapability[]
): OpenAIEndpointCapability[] => {
  const selected = CONFIGURABLE_OPENAI_ENDPOINT_CAPABILITIES.filter((value) => values.includes(value))
  return selected.length > 0 ? selected : [...DEFAULT_OPENAI_ENDPOINT_CAPABILITIES]
}

/** 与「未配置」等价时为 true：此时不写 openai_capabilities（后端同样把它存成未配置）。 */
export const isDefaultOpenAIEndpointCapabilitySet = (
  values: readonly OpenAIEndpointCapability[]
): boolean =>
  values.length === DEFAULT_OPENAI_ENDPOINT_CAPABILITIES.length &&
  DEFAULT_OPENAI_ENDPOINT_CAPABILITIES.every((value) => values.includes(value))
