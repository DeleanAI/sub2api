# 前端变体（frontend variants）

同一个镜像里带多套前端，部署时用环境变量 `SERVER_FRONTEND_VARIANT` 决定服务哪一套。

- `frontend/` 是**基础版**，构建产物落在 `backend/internal/web/dist/default/`。
- `frontend-variants/<name>/` 是**覆盖层**，构建产物落在 `backend/internal/web/dist/<name>/`。
- 后端用 `//go:embed all:dist` 把 `dist/` 整个嵌进二进制，运行时按 `server.frontend_variant`
  （环境变量 `SERVER_FRONTEND_VARIANT`，默认 `default`）选其中一套。

## 覆盖层规则

变体不是"另一棵完整的前端源码树"，而是叠在基础版之上的一层：

- **同路径的文件替换基础版的那一份**（`index.html`、`src/**`、`public/**` 都可以）；
- **基础版没有的文件被追加进来**；
- 没写在覆盖层里的文件，一律用基础版的。

所以既可以只改几个组件，也可以把整棵树都放进覆盖层——完全独立的前端是覆盖层的一个特例，反过来不成立。

`variant.json` 不进构建树，它只是变体的元数据。

## 目录约定

```
frontend-variants/
└── <name>/
    ├── variant.json        # 必需：{"name": "<name>", "display_name": "..."}
    ├── index.html          # 可选：替换基础版的 index.html
    ├── src/...             # 可选：替换或新增源码
    └── public/...          # 可选：替换或新增静态资源
```

- `<name>` 必须匹配 `^[a-z0-9][a-z0-9-]*$`，并且**必须等于 `variant.json` 里的 `name`**。
  两者不一致会让构建直接失败：运行时是按目录名选变体的，名字对不上会使日志、配置和实际服务的界面互相矛盾。
  这条规则在三处各有一份实现，由 `frontend/scripts/variant-name.samples.json` 这张共享样本表对账：
  - Go：`backend/internal/pkg/frontendvariant/name.go` 的 `NamePattern`
  - JS：`frontend/scripts/build-variant.mjs` 的 `VARIANT_NAME_PATTERN`
  - 文档：本节
- `display_name` 是给人看的名字（必填、非空），用于工单和部署清单里指代这套界面。
- `default` 是基础版的保留名，不要建 `frontend-variants/default/`。

## 构建

```bash
# 基础版 + 全部变体（并清理 dist 下已删除变体的过期产物）
pnpm --dir frontend run build:all

# 只重建全部变体，跳过基础版（本地迭代用，dist/default 沿用上次结果）
pnpm --dir frontend run build:variants

# 只构建基础版 → dist/default
pnpm --dir frontend run build

# 只构建指定变体（仍会带上基础版，因为 server.frontend_variant 的默认值就是 default）
node frontend/scripts/build-variant.mjs example
```

构建器把基础版与覆盖层合成到系统临时目录里再执行 `vue-tsc -b && vite build`，
输出目录用绝对路径指到 `backend/internal/web/dist/<name>/`。临时目录里会还原
"`frontend/` 与 `docs/` 同级"的仓库布局，因为源码里存在 `../../../../docs/legal/*.md?raw`
这类跨出 `frontend/` 的构建期导入。合成时不带 `node_modules`（改用软链复用基础版依赖）、
`dist/`、构建缓存和 `*.tsbuildinfo`。

镜像构建可以用 build-arg 限定变体来瘦身，默认构建全部：

```bash
docker build --build-arg FRONTEND_VARIANTS=default,example .
```

## 运行时选择

```bash
SERVER_FRONTEND_VARIANT=example
```

- 选中的变体在启动日志里打一次：`frontend variant selected frontend.variant=example available=[default example]`。
- 指向一个镜像里没有的变体 → **启动失败**，错误里列出可用变体。不会静默回落到 `default`：
  回落会把"变量配错了"伪装成"改的东西没生效"。
- `/api/v1/settings/public` 的 `frontend_variant` 字段返回当前进程正在服务的变体，便于排查副本之间的差异。
- 运行期静态覆盖目录 `<DATA_DIR>/public` 与变体机制正交：先选出整套变体产物，
  再让覆盖目录逐文件盖在它上面（品牌 logo、favicon 之类）。

## 示例

`example/` 是一个可运行的最小变体：替换 `index.html`（加一条 `<link>` 和一个右下角角标），
并新增 `public/variant-example.css`。它同时演示了覆盖层的两条规则——同路径替换、新文件追加。
