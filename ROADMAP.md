<div align="center">

# PageHut 路线图

</div>

## 当前状态（v0.1.0）

PageHut 是一个可自托管的开源静态网页托管平台：单 Go 二进制 + SQLite（纯 Go 驱动）+ 本地磁盘存储，运行内存约 6MB。

已具备：zip 部署与在线文件管理 · 管理员/审核员/用户三级角色 · 可开关的审核流程 · 额度体系（后台可配、按用户覆盖）· 三种注册模式 · 自定义域名（所有权验证 + 管理员开通）· HTTPS 双模式（内置 ACME / 外部反代）· 操作审计与站内通知。

## 定位原则

- **默认形态保持轻量单机**：单二进制 + SQLite 开箱即用是 PageHut 的核心优势，不动摇。
- **新能力以「可选驱动」加入**：构建管线、远程存储等放宽项做成可配置后端/可关闭开关，不用则零开销。
- **大功能先 RFC**：影响架构的提案先开 Discussion 讨论，达成共识再进里程碑。

## v0.2 —— 生态与自动化

> 主线：让用户「push 即部署」，把 PageHut 接进开发者的现有工作流。

| 方向 | 条目 | Issue |
|---|---|---|
| infra | 数据库 Schema 版本化迁移机制 | #1 |
| deploy-api | REST API 与 API Token 管理 | #2 |
| deploy-api | Git 仓库对接部署（GitHub / Gitee webhook） | #3 |
| deploy-api | 命令行工具 pagehut-cli | #4 |
| site-features | SPA 路由回退（history 模式 fallback） | #5 |
| ops-ux | 暗色主题 | #6 |

依赖关系：#2（API）是 #3（Git 对接）与 #4（CLI）的地基，#1（迁移机制）先行。

## v0.3 —— 站点能力与运营

> 主线：把「托管」做深——访问控制、可观测性、触达渠道与账号安全。

| 方向 | 条目 | Issue |
|---|---|---|
| site-features | 站点密码保护（HTTP Basic Auth） | #7 |
| site-features | `_redirects` / `_headers` 支持 + 缓存策略 | #8 |
| ops-ux | 站点访问统计 | #9 |
| ops-ux | 通知渠道扩展：邮件与 Webhook | #10 |
| account-collab | 两步验证（TOTP） | #11 |
| account-collab | 项目转让 | #12 |
| ops-ux | 英文界面（i18n） | #13 |

## 远期候选（v0.4+，讨论后立项）

以下为已识别但未排期的方向，**欢迎开 Discussion 推动 RFC**：

- **构建管线**：静态生成器（Hugo / Astro 等）服务端构建。涉及构建沙箱与资源隔离，需要先出 RFC。
- **S3 兼容存储后端**：站点文件存储层抽象为 local / s3 双驱动，默认仍为本地磁盘。
- **多人协作**：项目级 ACL，一个项目多个管理者（项目转让是其简化前置）。
- **站点内容自动安全扫描**：上传后的恶意内容/钓鱼特征检测，辅助审核员。

## 参与

- 想做某个 Issue：在 Issue 下留言认领，避免重复劳动。
- 有新想法：先开 [Discussion](https://github.com/xiaopi668/PageHut/discussions) 或 Issue 描述场景与方案。
- 大架构变更走 RFC：开 Discussion 附设计文档，讨论定稿后再排里程碑。
