import type { PublicSettings } from '@/types'

declare global {
  interface Window {
    __APP_CONFIG__?: PublicSettings
  }
}

declare module 'vue' {
  interface ComponentCustomProperties {
    /** 站内余额单位的符号（如 "$"、"¥"），见 utils/balanceCurrency.ts。 */
    $currency: string
  }
}

export {}
