# build-win.ps1 — Windows 桌面端（Tauri）完整构建步骤，纯注释版
#
# 本文件是文档：不含可执行代码，逐条手工执行。步骤按序，失败即中止。
# 使用两个 shell：Go 后端（步骤 3）在 Git Bash（MINGW64）下跑 make，
# 其余步骤（前端、embed 同步、Tauri、收集）在 PowerShell 7 下执行
# 也可全程留在 MINGW64，命令用各自的等价形式。
# 依赖已提前安装（pnpm workspace 模式），本文件不含任何安装步骤。
#
# 最终产物：
#   .\agentsview（根目录，dev 复用；Windows 下无扩展名，见步骤 3 备注）
#   desktop\src-tauri\binaries\agentsview-x86_64-pc-windows-msvc.exe
#   desktop\src-tauri\binaries\version.json
#   desktop\src-tauri\tauri.macos.version.conf.json
#   desktop\src-tauri\target\release\AgentsView.exe
#   dist\desktop\windows\AgentsView.exe（+ 同目录 sidecar exe）
#
# 与 mac 版的差异（先读再动手）：
#   - Windows 无法产出 .app；单文件目标改为裸 Tauri exe
#     （tauri build --no-bundle，无 NSIS 安装包，无签名）
#   - agentsview:// 深链接的协议注册通常由 NSIS 安装器完成，
#     裸 exe 运行时深链接不生效，属预期缺口，冒烟不算失败
#   - tauri.macos.version.conf.json 的分层版本覆盖在本流程是
#     可选项（不发安装器时 tauri.conf.json 固定的版本号够用）
#
# 环境前置（提前、手动、独立完成）：
#   - 定价快照：确认 internal/pricing/snapshot/litellm_snapshot.json.gz
#     存在（正常已随仓库提交）。缺失时手动恢复：
#       go run ./internal/pricing/cmd/litellm-snapshot -restore
#     （幂等：本地文件 SHA256 命中 pinned ref 时自动跳过）
#     make 的 pricing-snapshot 目标只 print 提醒，不做这件事
#   - GNU make：仓库 Makefile 的配方是 sh 语法，必须在 MINGW64/
#     Git Bash 下运行（make 会在 PATH 里找到 sh 并用它执行配方；
#     从 cmd/PowerShell 直接跑会因 cmd 语义失败）。MSYS2 的
#     mingw32-make 与独立 GNU make 均可
#   - mingw gcc 在 PATH：CGO_ENABLED=1 需要（mattn/go-sqlite3
#     是 cgo 驱动，internal/db 无条件导入）
#   - rustc 在 PATH：make build-release 用它探测 host 三元组
#     决定 sidecar 文件名与 GOOS/GOARCH（Makefile:86-93）。
#     也可用 TAURI_ENV_TARGET_TRIPLE / CARGO_BUILD_TARGET 覆盖

# 1. 构建前端（仓库根目录执行），产出 frontend/dist
#      pnpm --filter agentsview-frontend run build

# 2. 同步 Go embed 目录（仓库根目录，Git Bash / MINGW64 执行）
#    注意：internal/web/dist 是全工程共享的 embed 输入，本步骤
#    之后任何 go build（含纯 server 构建）都会嵌入这份前端产物
#      rm -rf internal/web/dist && cp -r frontend/dist internal/web/dist
#      printf '%s\n' 'keep embed dir for generated frontend assets' > internal/web/dist/.keep
#    （与 build-mac.sh:28-30 一致；PowerShell 等价：Remove-Item -Recurse
#      -Force internal/web/dist; Copy-Item -Recurse frontend/dist
#      internal/web/dist; Set-Content internal/web/dist/.keep
#      'keep embed dir for generated frontend assets'）

# 3. 编译 Go 二进制并放置 sidecar 与版本元数据（在 MINGW64 下执行）
#      mingw32-make build-release
#    一条命令覆盖：
#      - sqlite-vec-header（Makefile:62-67）：staging
#        mattn/go-sqlite3 的 sqlite3-binding.h 到 .sqlite-include，
#        并经 CGO_CFLAGS（Makefile:41）指给 cgo——sqlite-vec 的 cgo
#        绑定 #include "sqlite3.h"，须与驱动静态链接的 amalgamation
#        同版本，避免系统头漂移（Makefile:31-39 注释说明原因）
#      - go build（CGO_ENABLED=1，tags fts5，-trimpath，ldflags 含
#        -s -w）输出 .\agentsview
#      - sidecar staging：探测 rustc host 三元组（默认
#        x86_64-pc-windows-msvc），Windows 自动加 .exe 后缀，
#        输出 desktop\src-tauri\binaries\agentsview-<triple>.exe
#      - version.json 与 tauri.macos.version.conf.json
#    备注：根目录产物名为 agentsview（无扩展名）。bash 里
#      ./agentsview 可直接运行；cmd/PowerShell 需要扩展名，
#      重命名为 agentsview.exe 即可
#    版本元数据与 ldflags 的计算规则见 Makefile:3-11（唯一信息源）

# 4. Tauri 编译（单文件 exe，不打包安装器；PowerShell 或 bash 均可）
#    - 真实命令（仓库根目录执行）：
#      pnpm --filter agentsview-desktop exec tauri build --no-bundle
#    - --no-bundle 是 mac 版 --bundles app 的 Windows 等价：
#      只编译产物 exe，无 NSIS、无签名
#    - 不跑 desktop/scripts/prepare-sidecar.sh：它是 bash 全流程，
#      会重复步骤 1-3 并 sed 修改 tauri.conf.json（跟踪文件污染）
#    - frontendDist 指向 ../ui（仓库内静态文件），无需构建
#    - 消费：步骤 3 的 sidecar（externalBin）、desktop/ui 自托管
#      webview UI（与 Go 嵌入的 Svelte 产物是两份独立资源）
#    - 产出 desktop\src-tauri\target\release\AgentsView.exe
#    - 校验 sidecar 是否被 Tauri 复制到 target\release\ 旁边；
#      若没有，手动把 binaries\agentsview-<triple>.exe 拷到
#      AgentsView.exe 同目录（运行时按自身路径解析，不在旁边
#      就起不来）

# 5. 收集产物
#     - 校验 AgentsView.exe 存在，缺失即失败
#     - 拷贝到分发目录 dist\desktop\windows\，sidecar exe 一并放置

# 6. 冒烟验证
#     - 运行 sidecar 的 version 子命令：输出不得是 dev、
#       不得缺 commit、不得缺 build date，且与
#       desktop\src-tauri\binaries\version.json 一致
#     - 启动 AgentsView.exe，观察窗口与托盘
#     - agentsview:// 深链接：Windows 裸 exe 无协议注册，
#       预期不生效，跳过此项（见顶部差异说明）

# 顺序依赖总览：
#   1 前端 → 2 embed → 3 make build-release → 4 tauri →
#   5 收集 → 6 冒烟
#   各步严格按序执行，失败即中止；除步骤 3 的产物目录外，
#   不再修改任何跟踪文件
