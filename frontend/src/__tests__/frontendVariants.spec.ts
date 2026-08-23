import { describe, expect, it } from 'vitest'
import fs from 'node:fs'
import path from 'node:path'
import {
  BASE_VARIANT,
  FRONTEND_DIR,
  MANIFEST_FILE,
  VARIANTS_DIR,
  VARIANT_NAME_PATTERN,
  discoverVariants,
  isValidVariantName,
  readVariantManifest,
} from '../../scripts/build-variant.mjs'

/**
 * 变体机制的前端侧护栏。全部按声明遍历（样本表、frontend-variants/ 目录本身），
 * 不写死任何一个变体名：新加一个变体当天就被这些用例覆盖。
 */

const samples = JSON.parse(
  fs.readFileSync(path.join(FRONTEND_DIR, 'scripts', 'variant-name.samples.json'), 'utf8'),
) as { valid: string[]; invalid: string[] }

const variantDirs = fs.existsSync(VARIANTS_DIR)
  ? fs
      .readdirSync(VARIANTS_DIR, { withFileTypes: true })
      .filter((entry) => entry.isDirectory())
      .map((entry) => entry.name)
  : []

describe('变体名规则（与 Go 侧共用样本表）', () => {
  it('样本表两侧都不为空，否则这条对账线是摆设', () => {
    expect(samples.valid.length).toBeGreaterThan(0)
    expect(samples.invalid.length).toBeGreaterThan(0)
  })

  it.each(samples.valid)('接受 %j', (name) => {
    expect(isValidVariantName(name)).toBe(true)
  })

  it.each(samples.invalid)('拒绝 %j', (name) => {
    expect(isValidVariantName(name)).toBe(false)
  })

  it('基础版名字本身必须合法', () => {
    expect(isValidVariantName(BASE_VARIANT)).toBe(true)
    expect(VARIANT_NAME_PATTERN).toMatch(/^\^/)
  })
})

describe('frontend-variants/ 目录', () => {
  it('至少有一个变体（README 里承诺的 example）', () => {
    expect(variantDirs.length).toBeGreaterThan(0)
  })

  it('扫描全部变体不报错', () => {
    expect(() => discoverVariants()).not.toThrow()
  })

  it('不允许出现 default/：它是基础版 frontend/ 的保留名', () => {
    expect(variantDirs).not.toContain(BASE_VARIANT)
  })

  it.each(variantDirs)('%s 的 variant.json 合法且 name 等于目录名', (dirName) => {
    const manifest = readVariantManifest(path.join(VARIANTS_DIR, dirName))
    expect(manifest.name).toBe(dirName)
    expect(manifest.displayName).not.toBe('')
    expect(isValidVariantName(manifest.name)).toBe(true)
  })

  /**
   * 覆盖层可以替换 index.html，但不能把入口 <script> 删掉——vite 就是靠它找到应用入口的。
   * 删掉之后构建照样成功，只是页面打开后什么都不渲染，属于最难自查的一类错误。
   */
  it.each(variantDirs)('%s 如果替换了 index.html，必须保留基础版的入口脚本', (dirName) => {
    const overlayIndex = path.join(VARIANTS_DIR, dirName, 'index.html')
    if (!fs.existsSync(overlayIndex)) {
      return
    }
    const baseHtml = fs.readFileSync(path.join(FRONTEND_DIR, 'index.html'), 'utf8')
    const entry = baseHtml.match(/<script[^>]+type="module"[^>]+src="([^"]+)"/)
    expect(entry, '基础版 index.html 里找不到入口脚本，这条断言的前提没了').not.toBeNull()
    expect(fs.readFileSync(overlayIndex, 'utf8')).toContain(entry![1])
  })

  it.each(variantDirs)('%s 的覆盖层里不放 node_modules / dist', (dirName) => {
    for (const forbidden of ['node_modules', 'dist']) {
      expect(fs.existsSync(path.join(VARIANTS_DIR, dirName, forbidden))).toBe(false)
    }
  })

  it('每个变体目录都直接带着 variant.json（不靠继承）', () => {
    for (const dirName of variantDirs) {
      expect(fs.existsSync(path.join(VARIANTS_DIR, dirName, MANIFEST_FILE))).toBe(true)
    }
  })
})
