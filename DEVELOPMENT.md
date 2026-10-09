# Tauri 桌面端发布构建

## 目标

在本地为 macOS 构建单文件 `AgentsView.app`：Go 后端 + Svelte 前端
编译为 Tauri sidecar，再用 Tauri 打包。不存在 dmg，不存在签名，
不存在 CI。

完整步骤见根目录 `build-mac.sh`（纯注释版流程说明）。每步独立、
按序执行：

```sh
# 1. 构建前端（依赖已提前通过 pnpm workspace 安装）
pnpm --filter agentsview-frontend run build

# 2. 同步 Go embed 目录
rm -rf internal/web/dist
cp -r frontend/dist internal/web/dist
printf '%s\n' 'keep embed dir for generated frontend assets' > internal/web/dist/.keep

# 3. 编译 Go 二进制并放置 sidecar 与版本元数据
make build-release

# 4. Tauri 打包（单文件 .app）
pnpm --filter agentsview-desktop exec tauri build --bundles app --config src-tauri/tauri.macos.version.conf.json

# 5. 收集产物到 dist/desktop/macos/
# 6. 冒烟验证（见下）
```

## 前置（提前、手动完成；构建步骤与 make 均不处理）

- **定价快照**：`internal/pricing/snapshot/litellm_snapshot.json.gz`
  已随仓库提交，正常无需动作。意外缺失时手动恢复：
  `go run ./internal/pricing/cmd/litellm-snapshot -restore`
  （幂等：本地文件 SHA256 命中 pinned ref 时自动跳过）。
- **依赖安装**：pnpm workspace 模式，仓库根目录一次性安装。

## 职责划分

- **make 只负责 Go**：版本元数据唯一信息源在 `Makefile` 顶部
  （`VERSION` / `COMMIT` / `BUILD_DATE`），ldflags 注入二进制。
  `make build-release` 产出四件：
  - `./agentsview`（根目录，本机构建可供 dev 复用）
  - `desktop/src-tauri/binaries/agentsview-<target-triple>`
    （目标三元组动态解析，Windows 添加 `.exe`）
  - `desktop/src-tauri/binaries/version.json`（原始元数据，
    冒烟验证对照用；`target` 与编译目标和 sidecar 文件名一致）
  - `desktop/src-tauri/tauri.macos.version.conf.json`（Tauri
    分层配置覆盖，仅 `version` 一个键，semver 化由 make 完成）
- **前端与快照是调用方前置**：make 的 `frontend` /
  `pricing-snapshot` 目标不构建前端、不恢复快照；后者仍准备 SQLite 头文件。
- **目标计算在 Makefile 内联完成**：依次使用 `TAURI_ENV_TARGET_TRIPLE`、
  `CARGO_BUILD_TARGET`、`rustc -vV` 的 host，并映射为 Go 的
  `GOOS` / `GOARCH`。未知目标直接报错。交叉编译需要匹配的 CGO 编译器；
  Tauri 打包时需通过 `--target` 选择同一三元组，bundle 输出也会移至
  `target/<target-triple>/release/` 下。本机 macOS 构建无需指定目标。
- **`tauri.conf.json` 零修改**：版本经 `--config` 分层合并注入，
  跟踪文件从不被补丁或还原。文件名遵循生态模式
  `tauri.<platform>.conf.json`，但不是 `tauri.macos.conf.json`
  精确名，不会被 CLI 自动加载。
- **无 updater 工件**：`createUpdaterArtifacts: false`，不生成
  `.app.tar.gz`，不要求 `TAURI_SIGNING_PRIVATE_KEY`。App 内
  "Check for Updates" 菜单保留原功能：会调用接口、能发现新版本，
  但 pubkey 是占位符，签名校验必然失败，不会自动更新。

## 前置依赖

一次性安装：

- **Go 1.27+**，启用 **CGO**（macOS 用系统自带 `clang`）。
  `encoding/json/v2` 已自带，无需实验开关。
- **Node.js ≥ 24.11**（锁在 `frontend/package.json#engines`）。
- **pnpm**：workspace 模式管理 `frontend` 与 `desktop` 两个包，
  每包独立 lockfile。所有命令从仓库根目录以 `--filter` 执行。
- **Rust (stable)**：`rustc` / `cargo` 需在 `PATH` 上。
- **Tauri CLI v2**：作为 `desktop` 的 devDependency，经
  `pnpm --filter agentsview-desktop exec tauri` 调用。
- **Git**：解析版本元数据。

## 为什么要单独构建 Svelte 再走 Tauri

`desktop/src-tauri/tauri.conf.json` 中 `frontendDist` 指向
`desktop/ui`，那是 Tauri 自托管 webview 用的 UI，与 Go 嵌入的
Svelte 构建产物是**两份独立资源**。对 `frontend/` 的修改只能通过
拷贝到 `internal/web/dist` 流入 Go sidecar，不会自动进入 Tauri。

## 本仓库特有的坑

- **`go.kenn.io/kit` 版本必须能解析**：`go.mod` 里曾登记过一个
  不存在的 pseudo-version。改成本地缓存里验证过的 tag（如
  `v0.32.2`），再跑 `go mod tidy` 同步
  `go.sum`。
- **HTTP 代理**：本仓库 `github.com` 走代理时 `git` 与 `go` 都
  需要 `HTTPS_PROXY` / `HTTP_PROXY`。`go mod tidy` 也要带。
- **`rustc` 不在 PATH 上**：Tauri 打包依赖 `cargo`，缺失时无法
  产出 bundle。

## 验证产物

`make build-release` 成功后：

- `./agentsview`
- `desktop/src-tauri/binaries/agentsview-<target-triple>`
- `desktop/src-tauri/binaries/version.json`
- `desktop/src-tauri/tauri.macos.version.conf.json`

Tauri 打包成功后：

- `desktop/src-tauri/target/release/bundle/macos/AgentsView.app`

冒烟测试：

```sh
# 本机 macOS 构建：
target_triple="$(rustc -vV | awk '/^host: /{print $2}')"
"desktop/src-tauri/binaries/agentsview-$target_triple" version
# 输出不得是 dev、不得缺 commit / build date，
# 且与 binaries/version.json 一致

open desktop/src-tauri/target/release/bundle/macos/AgentsView.app
# 观察窗口、托盘、agentsview:// 深链接
```

## 参考

- `build-mac.sh`（完整构建步骤，纯注释版流程说明）
- `Makefile`（`build-release`：Go 编译、sidecar 放置、版本元数据
  与 tauri.macos.version.conf.json 生成）
- `desktop/src-tauri/tauri.conf.json`（Tauri 打包配置）
- `frontend/AGENTS.md`（Vite+ 工具链）
- `docs/agents/build.md`（CGO / fts5 / kit-ui 固定规则）
