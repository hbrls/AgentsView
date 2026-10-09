# Docbank 调查结果

调查日期：2026-10-09。本文记录调查时工作区源码的行为及 Go 后端体积实验，
不代表所有平台或所有发布版本的结果。

## 它在应用中做什么

Docbank 是应用内部的文件内容仓库。AgentsView 用它保存、读取和校验内容对象，
并使用其去重和压缩能力。这里的“对象”是有内容摘要和长度的文件内容，
例如导出的会话数据或上传的原始会话文件。

当前查到两个实际调用入口：

| 场景 | 保存什么 | 目的 |
| --- | --- | --- |
| 文件夹会话同步 | 从 SQLite 导出的会话、消息、清单和检查点 | 让多台机器交换会话，并导入各自的 SQLite |
| 托管原始文件上传的服务端 | 客户端上传的原始会话文件内容 | 在服务器留存原始内容，供后续解析使用 |

调用证据分别是 [artifact.Sync](../internal/artifact/sync.go) 和
[pgRawSyncCustody.openService](../cmd/agentsview/pg_raw_sync.go)。原始文件的
存取适配见 [object_store_artifact.go](../internal/rawsync/object_store_artifact.go)。

## 多机同步具体怎么使用

1. 电脑 A 执行 `agentsview sync --target /path/to/shared-folder`。
2. 应用从 A 的 SQLite 导出会话内容，保存到本地 Docbank 仓库，
   再将可交换的内容发布到共享目录。
3. 电脑 B 对同一个共享目录执行相同命令。
4. B 接收内容，校验后将支持的会话数据导入 B 自己的 SQLite。

共享目录可以是挂载的 NAS、外置硬盘或其他可信文件系统目录。
Docbank 负责内容存储；导出、目录交换、进度记录和导入由 AgentsView 实现。
该流程不是直接复制或合并两个正在使用的 SQLite 数据库。

这是显式启用的功能。普通 `agentsview sync` 不带 `--target` 时，
不会为这个流程创建 Docbank 仓库。当前文件夹交换没有自动监听或调度。
完整用法和边界以 [Artifact Folder Sync](../docs/artifact-sync.md) 为准。

## 是不是为了备份

文件夹同步的目标是交换可浏览的会话内容，不能作为完整备份：

- 不复制整个 SQLite 数据库。
- 不交换原始 provider JSONL 文件。
- 不交换用户整理信息等可变元数据。
- 不复制独立图片资源文件；导入时相关引用会替换为可读描述。

因此，不能据此承诺恢复完整应用状态或恢复原始会话文件。
这些限制见 [文件夹同步范围](../docs/artifact-sync.md#what-the-folder-contains)
和 [图片资源说明](../docs/data.md)。

托管原始文件上传有在服务器额外保存一份原始内容的效果，
但目前没有完整的灾难恢复流程。管理员仍须另外备份原始文件仓库和
PostgreSQL 元数据。见 [Hosted Raw Sync](../docs/hosted-raw-sync.md)。

## 有没有连接云端数据库

当前 AgentsView 对 Docbank 的配置是本地目录、SQLite 元数据和内容压缩。
文件夹同步的仓库位于 `{dataDir}/artifacts`；托管上传服务端使用
`{dataDir}/raw-sync/artifacts`。服务端即使部署在云服务器上，
这里仍使用服务端文件系统。

配置证据见 [repositoryDocbankConfig](../internal/artifact/repository.go)。
托管上传流程使用的 PostgreSQL 是另外的数据库连接，不是 Docbank 的 S3 后端。

Docbank 库本身支持 S3 对象存储，但当前配置没有设置 `StoreBindings`，
没有使用该能力。S3 也不是数据库。

## 为什么它会增加 Go 二进制体积

调查使用的依赖版本是 `go.kenn.io/docbank v0.14.0`。
虽然应用只配置本地存储，库的统一入口仍引用 S3 实现：

```text
AgentsView internal/artifact
  -> Docbank internal/blob
     -> AWS config + Kit packstore/s3store
  -> Docbank internal/config
     -> internal/storenamespace
        -> AWS S3 SDK
```

本地与 S3 的选择发生在运行时，Go 链接器没有根据应用传入的配置
排除这些远程存储分支。仅不配置 S3，不会移除二进制中的 AWS SDK。

应用自身的 S3 会话读取另用 MinIO 客户端，与这条 Docbank AWS 依赖链独立。

在 macOS ARM64、Go 1.27、CGO、`fts5`、`jsonv2`、`-trimpath` 和
`-ldflags='-s -w'` 条件下，临时对照构建结果如下。MB 使用十进制单位。

| 构建 | 字节数 | MB |
| --- | ---: | ---: |
| 原始 Go 后端 | 135,674,066 | 135.67 |
| 实验排除 Docbank S3 依赖链 | 128,381,602 | 128.38 |
| 减少 | 7,292,464 | 7.29 |

实验修改了临时 Docbank 副本的 S3 构造分支、能力探测及端点规范化代码。
构建后的模块信息确认 AWS SDK 消失，而 MinIO 仍保留。
这测量的是 S3 依赖链的增量，不是整个 Docbank 的体积。

实验版本改变了库能力及端点处理，仅用于归因；没有验证为生产替代版本。
实验没有修改仓库实现或模块缓存，也没有运行到实际数据目录。

## 后续可以考虑的优化

可以让 Docbank 提供独立的本地存储入口，或通过注入方式引入 S3 后端，
使本地调用不必链接 AWS SDK。需要同时处理存储命名空间代码中的 AWS 引用。

这个方向可以保留本地 Docbank、多机文件夹交换和服务端本地文件留存。
它是待评估的优化建议，本次没有实现，也没有批准删除任何现有功能。
