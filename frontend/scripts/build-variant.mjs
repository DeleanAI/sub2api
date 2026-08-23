#!/usr/bin/env node
/**
 * 前端变体构建器：把 frontend/（基础版）与 frontend-variants/<name>/（覆盖层）合成后逐套构建，
 * 产物落在 backend/internal/web/dist/<name>/，由 //go:embed all:dist 一次性打进同一个二进制。
 *
 * 为什么要合成到临时目录再构建：覆盖层只放"改动的那些文件"，而 vite/vue-tsc 需要一棵完整的源码树。
 * 临时目录还原了仓库的相对布局（frontend/ 与 docs/ 同级），因为 src 里存在
 * `../../../../docs/legal/*.md?raw` 这类跨出 frontend/ 的构建期导入，少一层目录就会解析失败。
 *
 * 用法：
 *   node scripts/build-variant.mjs                 # 基础版 + 全部变体（并清理过期产物）
 *   node scripts/build-variant.mjs acme,acme-dark  # 基础版 + 指定变体（不清理）
 *   node scripts/build-variant.mjs --skip-base     # 只重建变体，跳过基础版（本地迭代用）
 *   FRONTEND_VARIANTS=acme node scripts/build-variant.mjs   # 同上，供 Dockerfile 的 build-arg 透传
 */
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const scriptDir = path.dirname(fileURLToPath(import.meta.url))

/** frontend/ 基础版目录 */
export const FRONTEND_DIR = path.resolve(scriptDir, '..')
/** 仓库根目录 */
export const REPO_ROOT = path.resolve(FRONTEND_DIR, '..')
/** 变体覆盖层根目录 */
export const VARIANTS_DIR = path.join(REPO_ROOT, 'frontend-variants')
/** 嵌入后端的产物根目录 */
export const DIST_ROOT = path.join(REPO_ROOT, 'backend', 'internal', 'web', 'dist')
/** 每个变体的元数据文件名 */
export const MANIFEST_FILE = 'variant.json'
/** 基础版对应的变体名；server.frontend_variant 的默认值就是它 */
export const BASE_VARIANT = 'default'

/**
 * 变体名规则。这是 JS 侧的唯一一份，Go 侧的唯一一份在
 * backend/internal/pkg/frontendvariant/name.go 的 NamePattern；
 * 两份由 scripts/variant-name.samples.json 这张共享样本表在两侧各测一遍，改单边会被测试挡下。
 */
export const VARIANT_NAME_PATTERN = '^[a-z0-9][a-z0-9-]*$'
const variantNameRegExp = new RegExp(VARIANT_NAME_PATTERN)

/**
 * 合成时不带进临时目录的东西：
 * - node_modules：改用软链复用基础版的依赖，复制一份既慢又可能踩坏 pnpm 的符号链接布局
 * - dist / coverage / .vite / .cache：构建产物与缓存，不是构建输入
 * - *.tsbuildinfo：vue-tsc -b 的增量状态，带过去会让新树按旧状态判断"无需重新检查"
 */
const COMPOSE_SKIP_DIRS = new Set(['node_modules', 'dist', 'coverage', '.vite', '.cache'])
const COMPOSE_SKIP_SUFFIXES = ['.tsbuildinfo']

function log(message) {
  console.log(`[build-variant] ${message}`)
}

function warn(message) {
  console.warn(`[build-variant] ${message}`)
}

/** 变体名是否合法 */
export function isValidVariantName(name) {
  return typeof name === 'string' && variantNameRegExp.test(name)
}

/**
 * 读取并校验一个变体的 variant.json。
 * name 必须等于目录名：这两者一旦不一致，运行时按目录名选中的变体会自称另一个名字，
 * 排查时看到的每一条线索都是错的，所以这里直接让构建失败。
 */
export function readVariantManifest(variantDir) {
  const dirName = path.basename(variantDir)
  const manifestPath = path.join(variantDir, MANIFEST_FILE)
  if (!fs.existsSync(manifestPath)) {
    throw new Error(`变体 ${dirName} 缺少 ${MANIFEST_FILE}（期望路径 ${manifestPath}）`)
  }
  let manifest
  try {
    manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'))
  } catch (error) {
    throw new Error(`变体 ${dirName} 的 ${MANIFEST_FILE} 不是合法 JSON: ${error.message}`)
  }
  if (!isValidVariantName(manifest.name)) {
    throw new Error(
      `变体 ${dirName} 的 ${MANIFEST_FILE} 里 name=${JSON.stringify(manifest.name)} 不符合 ${VARIANT_NAME_PATTERN}`,
    )
  }
  if (manifest.name !== dirName) {
    throw new Error(
      `变体目录名与 ${MANIFEST_FILE} 不一致：目录 ${dirName}，name=${manifest.name}。` +
        `运行时按目录名选择变体，名字对不上会让日志和配置互相矛盾。`,
    )
  }
  const displayName = typeof manifest.display_name === 'string' ? manifest.display_name.trim() : ''
  if (!displayName) {
    throw new Error(`变体 ${dirName} 的 ${MANIFEST_FILE} 缺少非空的 display_name`)
  }
  return { name: manifest.name, displayName, dir: variantDir }
}

/** 扫描 frontend-variants/ 下的全部变体（按名字排序）；任何一个不合法都直接抛错。 */
export function discoverVariants() {
  if (!fs.existsSync(VARIANTS_DIR)) {
    warn(`没有 ${path.relative(REPO_ROOT, VARIANTS_DIR)} 目录，本次只构建基础版`)
    return []
  }
  const variants = []
  for (const entry of fs.readdirSync(VARIANTS_DIR, { withFileTypes: true })) {
    if (!entry.isDirectory()) {
      continue
    }
    if (!isValidVariantName(entry.name)) {
      throw new Error(
        `变体目录名 ${JSON.stringify(entry.name)} 不符合 ${VARIANT_NAME_PATTERN}；` +
          `目录名会直接作为 server.frontend_variant 的取值`,
      )
    }
    variants.push(readVariantManifest(path.join(VARIANTS_DIR, entry.name)))
  }
  variants.sort((a, b) => a.name.localeCompare(b.name))
  return variants
}

function shouldSkipComposeEntry(name) {
  return COMPOSE_SKIP_DIRS.has(name) || COMPOSE_SKIP_SUFFIXES.some((suffix) => name.endsWith(suffix))
}

/** 把 sourceDir 复制到 destDir：同路径文件覆盖，新文件追加（就是覆盖层的语义）。 */
function copyTree(sourceDir, destDir) {
  fs.mkdirSync(destDir, { recursive: true })
  for (const entry of fs.readdirSync(sourceDir, { withFileTypes: true })) {
    if (shouldSkipComposeEntry(entry.name)) {
      continue
    }
    const from = path.join(sourceDir, entry.name)
    const to = path.join(destDir, entry.name)
    if (entry.isDirectory()) {
      copyTree(from, to)
    } else {
      fs.rmSync(to, { force: true })
      fs.copyFileSync(from, to)
    }
  }
}

/**
 * 在系统临时目录里还原一份"仓库根"，把合成后的源码树放在其中的 frontend/。
 * 除 frontend/ 与 .git/ 外，仓库根的每一项都做软链，这样任何跨出 frontend/ 的构建期导入
 * （docs/legal/*.md?raw 是现存的一例）都仍然能解析到真实文件。
 */
function createStage() {
  const stageDir = fs.mkdtempSync(path.join(os.tmpdir(), 'sub2api-frontend-variant-'))
  for (const entry of fs.readdirSync(REPO_ROOT, { withFileTypes: true })) {
    if (entry.name === 'frontend' || entry.name === '.git') {
      continue
    }
    fs.symlinkSync(path.join(REPO_ROOT, entry.name), path.join(stageDir, entry.name))
  }
  return stageDir
}

/** 合成 frontend/ + 覆盖层 → <stage>/frontend，返回合成目录。 */
function composeVariantTree(variant, stageDir) {
  const composedDir = path.join(stageDir, 'frontend')
  copyTree(FRONTEND_DIR, composedDir)
  // variant.json 是变体的元数据而不是前端源码，不进构建树。
  const overlayEntries = fs
    .readdirSync(variant.dir, { withFileTypes: true })
    .filter((entry) => entry.name !== MANIFEST_FILE)
  for (const entry of overlayEntries) {
    const from = path.join(variant.dir, entry.name)
    const to = path.join(composedDir, entry.name)
    if (entry.isDirectory()) {
      copyTree(from, to)
    } else {
      fs.rmSync(to, { force: true })
      fs.copyFileSync(from, to)
    }
  }
  log(
    `变体 ${variant.name}：覆盖层 ${overlayEntries.length} 项已叠加到基础版之上` +
      `（合成时跳过 ${[...COMPOSE_SKIP_DIRS].join('/')} 与 *.tsbuildinfo，node_modules 走软链复用）`,
  )
  fs.symlinkSync(path.join(FRONTEND_DIR, 'node_modules'), path.join(composedDir, 'node_modules'))
  return composedDir
}

function runStep(command, args, cwd, outDir) {
  const result = spawnSync(command, args, {
    cwd,
    stdio: 'inherit',
    env: { ...process.env, SUB2API_FRONTEND_OUT_DIR: outDir },
  })
  if (result.error) {
    throw result.error
  }
  if (result.status !== 0) {
    throw new Error(`${path.basename(command)} ${args.join(' ')} 失败（退出码 ${result.status}）`)
  }
}

/** 在 cwd 里执行与 `pnpm run build` 完全相同的两步，只是把输出目录换成 outDir。 */
function runBuild(cwd, outDir) {
  const bin = (name) => path.join(cwd, 'node_modules', '.bin', name)
  runStep(bin('vue-tsc'), ['-b'], cwd, outDir)
  runStep(bin('vite'), ['build'], cwd, outDir)
}

function buildBase() {
  const outDir = path.join(DIST_ROOT, BASE_VARIANT)
  log(`构建基础版 ${BASE_VARIANT} → ${path.relative(REPO_ROOT, outDir)}`)
  runBuild(FRONTEND_DIR, outDir)
}

function buildVariant(variant) {
  const outDir = path.join(DIST_ROOT, variant.name)
  log(`构建变体 ${variant.name}（${variant.displayName}）→ ${path.relative(REPO_ROOT, outDir)}`)
  const stageDir = createStage()
  try {
    const composedDir = composeVariantTree(variant, stageDir)
    runBuild(composedDir, outDir)
  } finally {
    fs.rmSync(stageDir, { recursive: true, force: true })
  }
}

/**
 * 删除本次没有构建的产物目录。只在"构建全集"时执行：
 * dist 下的每个目录都会被 //go:embed 打进镜像并出现在可用变体列表里，
 * 留着一套删掉的变体，运行时就能选中一份没人维护的旧界面。
 */
function pruneStaleDist(builtNames) {
  if (!fs.existsSync(DIST_ROOT)) {
    return
  }
  for (const entry of fs.readdirSync(DIST_ROOT, { withFileTypes: true })) {
    if (entry.name === '.keep' || builtNames.includes(entry.name)) {
      continue
    }
    warn(`删除过期产物 dist/${entry.name}：本次构建的变体集合里没有它，留着会被嵌入镜像`)
    fs.rmSync(path.join(DIST_ROOT, entry.name), { recursive: true, force: true })
  }
}

function splitList(raw) {
  return String(raw || '')
    .split(',')
    .map((item) => item.trim())
    .filter(Boolean)
}

export function parseArgs(argv) {
  const names = []
  let skipBase = false
  for (const arg of argv) {
    if (arg === '--skip-base') {
      skipBase = true
      continue
    }
    if (arg.startsWith('--')) {
      throw new Error(`未知参数 ${arg}；用法见 scripts/build-variant.mjs 顶部注释`)
    }
    names.push(...splitList(arg))
  }
  return { names, skipBase }
}

function main(argv) {
  const { names, skipBase } = parseArgs(argv)
  const requested = names.length > 0 ? names : splitList(process.env.FRONTEND_VARIANTS)
  const buildEverything = requested.length === 0 || requested.includes('all')

  const discovered = discoverVariants()
  const discoveredNames = discovered.map((variant) => variant.name)

  let selected
  if (buildEverything) {
    selected = discoveredNames
  } else {
    selected = requested.filter((name) => name !== BASE_VARIANT)
    const unknown = selected.filter((name) => !discoveredNames.includes(name))
    if (unknown.length > 0) {
      throw new Error(
        `未知变体 ${unknown.join(', ')}；${path.relative(REPO_ROOT, VARIANTS_DIR)} 下现有：` +
          `${discoveredNames.join(', ') || '(空)'}`,
      )
    }
  }

  const built = []
  if (skipBase) {
    warn(`--skip-base：跳过基础版 ${BASE_VARIANT}，dist/${BASE_VARIANT} 沿用上一次构建的结果`)
  } else {
    buildBase()
    built.push(BASE_VARIANT)
  }
  for (const name of selected) {
    buildVariant(discovered.find((variant) => variant.name === name))
    built.push(name)
  }

  if (buildEverything && !skipBase) {
    pruneStaleDist(built)
  } else {
    log(`只构建了子集 [${built.join(', ')}]，不清理 dist 下的其它产物`)
  }
  log(`完成，dist 下现有：${fs.readdirSync(DIST_ROOT).join(', ')}`)
}

// 只有被直接执行时才跑 CLI；测试只 import 上面那些纯函数。
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    main(process.argv.slice(2))
  } catch (error) {
    console.error(`[build-variant] ${error.message}`)
    process.exit(1)
  }
}
