<div align="center">

<img src="assets/logo.png" alt="PageHut logo" width="140">

# PageHut

**可自托管的静态网页托管平台** —— 单二进制、低内存、自带审核与额度体系。

自建你的「迷你 Netlify」：用户注册 → 上传 zip → 审核发布 → 绑定自定义域名。

</div>

## 特性

- **静态托管**：zip 上传整体部署 + 在线文件管理（上传 / 编辑 / 删除 / 新建目录），支持项目自定义 `404.html`
- **三级角色**：管理员 / 审核员 / 普通用户；管理员可将任意用户设为审核员
- **审核体系**：审核总开关（开启时项目过审才能访问，驳回需填写理由；关闭时上传即发布）；已发布项目内容变更自动回到待审核；站内通知贯穿全程
- **额度体系**（全部后台可配，支持按用户覆盖）：
  - 全站「单个项目大小」硬上限
  - 免费用户项目数量上限、免费用户单项目大小上限
- **三种注册模式**（后台随时切换）：公开注册 / 邀请码注册 / 关闭注册（仅管理员建号）
- **域名体系**：自动分配 `前缀.子域名后缀` 访问地址；用户可申请绑定自定义域名（CNAME 或令牌文件验证所有权，管理员开通后生效）
- **HTTPS 双模式**：内置 ACME 自动签发续期证书（`-tls auto`），或由你自己的 Nginx / Caddy 反代终结 TLS
- **安全**：CSRF 防护、bcrypt 密码、登录限流、zip 解压路径穿越防护（zip-slip / 符号链接 / zip 炸弹）、操作审计日志
- **轻量**：Go 单二进制 + SQLite（纯 Go 驱动，无 CGO）+ 本地磁盘存储，面板内存占用约 20~40MB

## 快速开始

### Docker Compose（推荐）

```bash
mkdir pagehut && cd pagehut
# 把 docker-compose.yml 与 Dockerfile 放入当前目录（或直接克隆本仓库）
mkdir -p data && chown -R 1000:1000 data   # 容器以 UID 1000 运行，bind mount 必须先授权
docker compose up -d
docker compose logs | grep 初始管理员   # 首次启动会打印初始管理员密码
```

面板地址：`http://127.0.0.1:8080`（compose 默认只绑定回环，请用 Nginx / Caddy 反代对外提供服务，**不要**把 8080 直接开到公网）。用日志中的 `admin` 账号登录，**立即修改密码**，然后进入「系统设置」配置站点名称、注册模式、额度和子域名后缀。

### 裸二进制

从 [Releases](https://github.com/xiaopi668/PageHut/releases) 下载对应平台二进制（linux/darwin，amd64/arm64），或自行构建（见下）：

```bash
./pagehut -data ./data -http :8080
```

首次启动日志会输出初始管理员密码：

```
初始管理员账号: admin
初始管理员密码: XXXXXXXX
```

> Docker 镜像：每次发布版本（打 `v*` 标签）会自动构建多架构镜像并推送到 `ghcr.io/xiaopi668/pagehut`（amd64/arm64）。

## 启动参数

| 参数 | 环境变量 | 默认 | 说明 |
|---|---|---|---|
| `-data` | `PAGEHUT_DATA` | `./data` | 数据目录（数据库、站点文件、证书缓存） |
| `-http` | `PAGEHUT_HTTP` | `:8080` | HTTP 监听地址；auto 模式下为 ACME/跳转端口（通常 `:80`） |
| `-https` | `PAGEHUT_HTTPS` | `:443` | HTTPS 监听地址（仅 auto 模式） |
| `-tls` | `PAGEHUT_TLS` | `manual` | `manual`：外部反代终结 TLS；`auto`：内置 ACME 自动签发 |
| `-acme-email` | `PAGEHUT_ACME_EMAIL` | 空 | ACME 注册邮箱（auto 模式建议填写） |
| `-acme-cache` | `PAGEHUT_ACME_CACHE` | `数据目录/acme` | 证书缓存目录 |
| `-trusted-proxies` | `PAGEHUT_TRUSTED_PROXIES` | `127.0.0.1/8,::1/128` | 可信反代的 IP/CIDR（逗号分隔）。只有来自这些地址的 `X-Forwarded-For` / `X-Real-IP` 才会被采信，用于登录限流与日志取真实客户端 IP |

> 反代与 PageHut 不在同一台机器时，必须把反代的出口地址填进 `-trusted-proxies`，
> 否则所有请求会被当成同一个来源（登录限流会误伤全站，日志也只有代理 IP）。

运行期设置（注册模式、审核开关、额度、子域名后缀、面板域名等）全部在**管理后台 → 系统设置**中修改，实时生效。

## 部署

详见 [docs/DEPLOY.md](docs/DEPLOY.md)，包含：

- Docker Compose 与 systemd 两种部署形态
- 反向代理（Nginx / Caddy）完整示例
- 两种 HTTPS 模式的选择依据
- 子域名泛解析与自定义域名的 DNS 配置指引
- 数据备份与恢复

## 开发

```bash
go build ./...        # 编译
go test ./...         # 单元测试（zip 安全、路径防护、站点服务等）
bash scripts/e2e.sh   # 端到端冒烟测试（自动起服务跑全流程）
bash scripts/build.sh # 交叉编译 linux/amd64 + linux/arm64 到 dist/
```

要求 Go ≥ 1.26。无 CGO，可任意交叉编译。

## 安全说明

- 所有面板 POST 均有 CSRF 校验；已登录用户的令牌由**会话派生**（不采信 cookie 里的值），可抵御父域 cookie 投放
- 会话 Cookie 为 HttpOnly + SameSite=Lax；面板响应带 CSP、`X-Frame-Options: DENY` 等安全头
- 审核预览在 **CSP sandbox（不透明源）** 中运行：站点 JS 照常执行，但读不到面板 DOM / Cookie，也调不动面板接口
- zip 解压在临时目录完成并通过校验后**整目录交换**（失败自动回滚）：拒绝路径穿越（`../`）、绝对路径与符号链接条目，限制解压总大小与文件数
- 用户站点服务不做目录列表，自动继承项目自定义 404 页；自定义域名同样受项目状态约束（下架 / 待审内容不会对外服务）
- 登录限流按「来源 IP」与「IP + 账号」双维度计数，反代后不会因单点试错锁死全站
- 域名所有权验证的 HTTP 探测拒绝连接回环 / 链路本地地址（防 SSRF 打云元数据），最多跟随 2 次跳转
- 审计日志记录登录、上传、审核、设置变更等全部敏感操作；`GET /healthz` 供容器探针使用
- 上线请务必：修改初始密码、启用 HTTPS、配置合理的额度，并把面板放在反代之后

## License

[AGPL-3.0](LICENSE)
