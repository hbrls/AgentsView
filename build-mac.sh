# build-mac.sh — macOS 桌面端（Tauri）完整构建步骤，纯注释版
#
# 本文件暂时不含任何可执行代码。每个步骤独立、按序执行，
# 后续将据此清理 desktop/scripts/prepare-sidecar.sh 并落地实现。
#
# 前置（提前、手动、独立完成；构建步骤与 make 均不处理）
# 定价快照：确认 internal/pricing/snapshot/litellm_snapshot.json.gz
# 存在（正常已随仓库提交）。缺失时手动恢复：
#   go run ./internal/pricing/cmd/litellm-snapshot -restore
# （幂等：本地文件 SHA256 命中 pinned ref 时自动跳过）
# make 的 pricing-snapshot 目标只 print 提醒，不做这件事

# 1. 构建前端（仓库根目录执行），产出 frontend/dist
pnpm --filter agentsview-frontend run build

# 2. 同步 Go embed 目录（仓库根目录执行）
#    注意：internal/web/dist 是全工程共享的 embed 输入，本步骤
#    之后任何 go build（含纯 server 构建）都会嵌入这份前端产物
rm -rf internal/web/dist
cp -r frontend/dist internal/web/dist
printf '%s\n' 'keep embed dir for generated frontend assets' > internal/web/dist/.keep

# 3. 编译 Go 二进制并放置 sidecar 与版本元数据
#    - 真实命令（仓库根目录执行）：
#      make build-release
#    - 步骤 1/2 的产物 internal/web/dist 是本步骤的输入
#    - 产出：
#      ./agentsview（根目录，本机构建可供 dev 复用）
#      desktop/src-tauri/binaries/agentsview-<target-triple>（sidecar）
#      desktop/src-tauri/binaries/version.json（原始元数据，冒烟验证用）
#      desktop/src-tauri/tauri.macos.version.conf.json（Tauri 分层配置
#      覆盖，仅一个键 version；非 tag 输入落 0.0.0-dev 兜底）

# 4. Tauri 打包
#    - 真实命令（仓库根目录执行）：
#      pnpm --filter agentsview-desktop exec tauri build --bundles app --config src-tauri/tauri.macos.version.conf.json
#    - 唯一构建目标是单文件 .app，不存在 dmg，不存在签名
#    - 消费：步骤 3 的 sidecar（externalBin）、desktop/ui 自托管 webview UI
#      （与 Go 嵌入的 Svelte 产物是两份独立资源，互不影响）
#    - 产出 bundle/macos/AgentsView.app

# 5. 收集产物
#     - 校验 AgentsView.app 存在，缺失即失败
#     - 拷贝到分发目录 dist/desktop/macos/
