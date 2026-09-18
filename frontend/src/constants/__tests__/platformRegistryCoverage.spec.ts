import { describe, expect, it } from 'vitest'

import { CONCRETE_PLATFORM_OPTIONS, GROUP_PLATFORM_OPTIONS } from '@/constants/platforms'
import {
  isCnFormPlatform,
  isCnProviderPlatform,
  normalizeCnProviderPlatform
} from '@/constants/cnProviders'
import { cnSupportsNativeResponses } from '@/components/account/credentialsBuilder'
import * as platformColors from '@/utils/platformColors'
import { getKeyGroupProvider, KEY_GROUP_PROVIDERS } from '@/utils/keyGroupProviders'
import type { GroupPlatform } from '@/types'

// 护栏走声明：遍历平台注册表，而不是点名今天这几个平台。
// 新接一个平台只要进了 CONCRETE_PLATFORM_OPTIONS，漏配的地方当天就会失败——
// 这正是 2026-09-18 同步上游时抓到 keyGroupProviders 漏了 qwen 的方式。
describe('platform registry coverage', () => {
  const concrete = CONCRETE_PLATFORM_OPTIONS.map(o => o.value)

  // 遍历 platformColors 导出的**全部** platform* 取色/取名函数，而不是点名其中几个：
  // 新增一张样式表若漏配某平台，这条会直接失败。
  const styleFns = Object.entries(platformColors).filter(
    ([name, fn]) => typeof fn === 'function' && name.startsWith('platform')
  ) as Array<[string, (p: string) => string]>

  it('exposes platform style helpers to iterate over', () => {
    expect(styleFns.length).toBeGreaterThan(5)
  })

  it('every platform resolves in every platform style helper', () => {
    for (const platform of GROUP_PLATFORM_OPTIONS.map(o => o.value)) {
      for (const [name, fn] of styleFns) {
        const value = fn(platform)
        expect(value, `${name}(${platform})`).toBeTruthy()
        // 回落到 default 分支时 platformLabel 会原样返回平台名——那意味着漏配。
        if (name === 'platformLabel') {
          expect(value, `${name}(${platform}) fell through to default`).not.toBe(platform)
        }
      }
    }
  })

  // getKeyGroupProvider 有 `?? 'other'` 兜底，所以只断言「返回值合法」永远成立、
  // 抓不到漏配（实测：拿掉 qwen 映射后那种断言依然全绿）。这里按声明断言具体归属：
  // CN 供应商必须归 domestic，漏一个就会掉进兜底而被抓出来。
  it('every CN provider maps to the domestic key-group provider', () => {
    for (const platform of concrete) {
      if (!isCnProviderPlatform(platform)) continue
      expect(getKeyGroupProvider(platform as GroupPlatform), `platform=${platform} 落进了兜底`).toBe(
        'domestic'
      )
    }
  })

  it('every group platform maps to a known key-group provider', () => {
    for (const platform of GROUP_PLATFORM_OPTIONS.map(o => o.value)) {
      const provider = getKeyGroupProvider(platform as GroupPlatform)
      expect(KEY_GROUP_PROVIDERS, `platform=${platform}`).toContain(provider)
    }
  })

  // CN 供应商名单只在 cnProviders 声明一处；这里钉住「归一化认得的」与
  // 「isCnProviderPlatform 认得的」永远是同一组，避免两者再次分叉。
  it('isCnProviderPlatform agrees with normalizeCnProviderPlatform', () => {
    for (const platform of concrete) {
      expect(isCnProviderPlatform(platform), `platform=${platform}`).toBe(
        normalizeCnProviderPlatform(platform) !== null
      )
    }
  })

  // 走 CN 表单的平台 = CN 供应商 ∪ OpenCode。少一边就会漏掉某个平台的
  // account_mode / api_protocol 表单（合并上游时两边各漏过对方的平台）。
  it('isCnFormPlatform covers every CN provider plus OpenCode', () => {
    for (const platform of concrete) {
      const expected = isCnProviderPlatform(platform) || platform === 'opencode_go'
      expect(isCnFormPlatform(platform), `platform=${platform}`).toBe(expected)
    }
  })

  // cnSupportsNativeResponses 必须接受**原始** platform：它内部做归一化。
  // 曾有 5 个调用点各写一遍 `normalize(p) ?? ''`，而 opencode_go 归一化后是 null，
  // 空串让它的 responses 端点被静默清空。
  it('cnSupportsNativeResponses takes raw platform values', () => {
    expect(cnSupportsNativeResponses('opencode_go')).toBe(true)
    expect(cnSupportsNativeResponses('deepseek')).toBe(true)
    expect(cnSupportsNativeResponses('kimi')).toBe(true)
    expect(cnSupportsNativeResponses('minimax')).toBe(true)
    // qwen 没有原生 responses 档位（它的 preset 里也没有）
    expect(cnSupportsNativeResponses('qwen')).toBe(false)
    expect(cnSupportsNativeResponses('zhipu')).toBe(false)
    // 空值与未知平台不得为真
    expect(cnSupportsNativeResponses('')).toBe(false)
    expect(cnSupportsNativeResponses(null)).toBe(false)
    expect(cnSupportsNativeResponses(undefined)).toBe(false)
    // 归一化在函数内部：别名也要认
    expect(cnSupportsNativeResponses(' Kimi ')).toBe(true)
  })

  it('normalization is idempotent and case/alias tolerant', () => {
    for (const platform of concrete) {
      const first = normalizeCnProviderPlatform(platform)
      if (first === null) continue
      expect(normalizeCnProviderPlatform(first), `platform=${platform}`).toBe(first)
      expect(normalizeCnProviderPlatform(platform.toUpperCase()), `platform=${platform}`).toBe(first)
      expect(normalizeCnProviderPlatform(` ${platform} `), `platform=${platform}`).toBe(first)
    }
  })
})
