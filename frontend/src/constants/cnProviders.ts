/**
 * Canonical names used by the frontend for Chinese API providers and other
 * OpenAI-compatible providers configured the same way.
 *
 * 这里是前端平台名的**唯一**归一化入口。归一化而不是到处写死平台名单，是因为
 * 同一个平台可能有多个别名（qwen 也写作 dashscope），写死名单必然漏判。
 *
 * `qwen` / `minimax` / `opencode_go` 都是与后端共享的 canonical 值。
 */
export type CanonicalCnProviderPlatform = 'kimi' | 'zhipu' | 'deepseek' | 'qwen' | 'minimax'

/** OpenCode 与 CN 供应商共享同一套「账号模式 × 协议 × 端点预设」表单，但它不是 CN 供应商。 */
export type CnFormPlatform = CanonicalCnProviderPlatform | 'opencode_go'

export function normalizeCnProviderPlatform(
  platform: string | null | undefined
): CanonicalCnProviderPlatform | null {
  switch (platform?.trim().toLowerCase()) {
    case 'kimi':
      return 'kimi'
    case 'zhipu':
      return 'zhipu'
    case 'deepseek':
      return 'deepseek'
    case 'qwen':
      return 'qwen'
    case 'minimax':
      return 'minimax'
    default:
      return null
  }
}

export function isCnProviderPlatform(platform: string | null | undefined): boolean {
  return normalizeCnProviderPlatform(platform) !== null
}

/**
 * 走 CN 表单（account_mode × api_protocol × base_url 预设）的平台。
 * OpenCode 不是 CN 供应商，但复用同一套表单，因此单独并进来。
 */
export function isCnFormPlatform(platform: string | null | undefined): boolean {
  return isCnProviderPlatform(platform) || platform === 'opencode_go'
}

export function isQwenPlatform(platform: string | null | undefined): boolean {
  return normalizeCnProviderPlatform(platform) === 'qwen'
}

export function isCnCodingPlanPlatform(platform: string | null | undefined): boolean {
  const normalized = normalizeCnProviderPlatform(platform)
  return normalized === 'kimi' || normalized === 'zhipu' || normalized === 'minimax'
}
