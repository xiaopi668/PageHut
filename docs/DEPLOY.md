# PageHut 部署指南

PageHut 有两种 TLS 模式，决定了你的网络与反代怎么搭：

| 模式 | `-tls` 参数 | 适用场景 | 端口要求 |
|---|---|---|---|
| **外部反代**（默认 manual） | `manual` | 服务器上已有 Nginx/Caddy 统一管理证书 | 只需暴露一个 HTTP 端口 |
| **内置 ACME** | `auto` | PageHut 独占 80/443，自动签发续期 Let's Encrypt 证书 | 需直连 80 + 443 |

---

## 一、Docker Compose 部署（推荐）

```yaml
services:
  pagehut:
    image: pagehut:latest   # 或 build: .
    container_name: pagehut
    restart: unless-stopped
    ports:
      - "8080:8080"
    volumes:
      - ./data:/data
```

```bash
docker compose up -d
docker compose logs pagehut | grep 初始管理员
```

> 容器以非 root 用户（UID 1000）运行。使用 bind mount（`./data:/data`）时，请先在宿主机执行 `mkdir -p data && chown -R 1000:1000 data`，否则无写入权限。

### 内置自动 HTTPS（auto 模式）

前提：`面板域名`、`子域名后缀` 与各自定义域名都已把 DNS 解析（A 记录或泛解析）指到本服务器，且 80/443 未被其他程序占用。

```yaml
    ports:
      - "80:80"
      - "443:443"
    command: ["-data", "/data", "-tls", "auto", "-http", ":80", "-https", ":443", "-acme-email", "you@example.com"]
```

auto 模式下证书按需签发：面板域名、每个站点子域名、每个已开通的自定义域名首次被访问时自动申请，缓存于 `数据目录/acme`。

## 二、裸二进制 + systemd

```bash
sudo install -m 755 pagehut-linux-amd64 /usr/local/bin/pagehut
sudo useradd -r -s /usr/sbin/nologin pagehut
sudo mkdir -p /var/lib/pagehut && sudo chown pagehut:pagehut /var/lib/pagehut
```

`/etc/systemd/system/pagehut.service`：

```ini
[Unit]
Description=PageHut static hosting
After=network-online.target
Wants=network-online.target

[Service]
User=pagehut
ExecStart=/usr/local/bin/pagehut -data /var/lib/pagehut -http 127.0.0.1:8080
Restart=on-failure
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/var/lib/pagehut
# 仅 -tls auto 模式需要：以非 root 身份绑定 80/443
#AmbientCapabilities=CAP_NET_BIND_SERVICE
#CapabilityBoundingSet=CAP_NET_BIND_SERVICE
# 反代在另一台机器时，把它的出口地址填进来（默认只信任回环）：
#Environment=PAGEHUT_TRUSTED_PROXIES=127.0.0.1/8,::1/128,10.0.0.5/32

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now pagehut
# 首次启动的初始管理员密码在日志里：
sudo journalctl -u pagehut | grep 初始管理员
```

## 三、反向代理示例（manual 模式）

反代时**必须把原始 Host 头传给 PageHut**（站点按 Host 路由）。

### Nginx

```nginx
# 面板
server {
    listen 443 ssl http2;
    server_name dash.example.com;
    # ssl_certificate ...;（由 certbot 等管理）

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto https;
        # 下面两行让 PageHut 拿到真实客户端 IP（登录限流、日志都依赖它）
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        client_max_body_size 200m;   # ≥ 后台配置的单项目大小上限（应用侧还会再留 1MB 表单开销）
        proxy_read_timeout 600s;     # 大文件上传 / 下载
        proxy_request_buffering off; # 边收边转发，避免大 zip 先落盘到反代
    }
}

# 站点子域名（泛解析）
server {
    listen 443 ssl http2;
    server_name *.sites.example.com;
    # ssl_certificate 泛域名证书（或使用 Caddy 按需签发）
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto https;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        client_max_body_size 200m;
        proxy_read_timeout 600s;
    }
}
```

> 反代与 PageHut **不在同一台机器**时，用 `-trusted-proxies`（或 `PAGEHUT_TRUSTED_PROXIES`）
> 把反代的出口地址段加进去；默认只信任回环地址，其余来源的 `X-Forwarded-For` 一律忽略。

### Caddy（自动 HTTPS，含泛域名按需签发）

```caddy
{
    on_demand_tls {
        ask http://127.0.0.1:8080/healthz   # 见下方警告，勿用 /login
    }
}

dash.example.com {
    reverse_proxy 127.0.0.1:8080
}

*.sites.example.com, www.mydemo.cn {
    tls {
        on_demand
    }
    reverse_proxy 127.0.0.1:8080
}
```

> ⚠️ **`ask` 端点决定谁能触发证书签发**。`/login`、`/healthz` 这类无条件返回 200 的端点
> 等于对任意域名放行，攻击者可以用 `随机.sites.example.com` 消耗你的 ACME 配额，
> 导致正常站点签不出证书。请让 `ask` 指向一个真正校验「该域名归本站管」的端点
> （例如仅当域名在 `domains` 表中处于 `active` 或属于已存在的项目子域名时才返回 200），
> 或在反代层维护显式域名白名单。上面示例用的是最简形态，仅适合域名完全自控的场景。

## 四、DNS 配置

| 用途 | 记录 | 示例 |
|---|---|---|
| 面板 | A 记录 `dash` → 服务器 IP | `dash.example.com` |
| 站点子域名 | A 记录 `*.sites` → 服务器 IP（泛解析） | `demo.sites.example.com` |
| 自定义域名（用户操作） | CNAME `www` → `demo.sites.example.com`，或 A 记录指向服务器 IP | — |

后台「系统设置」中：

- **子域名后缀**：填 `sites.example.com`，用户项目即获得 `前缀.sites.example.com`；留空则不启用子域名访问（仍可用预览与自定义域名）
- **面板域名**：填 `dash.example.com`（**建议填写**）。留空时只有「IP / localhost」以及子域名后缀顶点能进面板，其他域名一律返回 404 —— 按域名访问面板会打不开，请先填好这一项再配反代

## 五、自定义域名流程（给管理员看）

1. 用户在项目「域名」页提交域名
2. 用户完成 DNS 解析（CNAME 指向其项目的子域名地址，或 A 记录指向服务器）
3. 所有权验证二选一，自动完成：
   - 用户点击「立即检查」，服务端核对 CNAME；
   - 或用户访问验证链接 `http://域名/.well-known/pagehut-verify/<令牌>`（域名解析到本服务器时自动通过）
4. 域名进入「已验证待开通」，管理员在「域名管理」中开通
5. 开通后该域名即按 Host 路由到对应项目；HTTPS 证书由所选 TLS 模式处理（auto 模式自动签发；manual 模式需你在反代上为该域名配证书，推荐让用户用 CNAME 接入泛域名）

## 六、额度体系

| 设置项 | 作用 |
|---|---|
| 单个项目大小上限（全站硬上限） | 对**所有人**生效（含管理员），防止单项目撑爆磁盘 |
| 免费用户项目数量上限 | 普通用户与审核员可创建的项目个数 |
| 免费用户单项目大小上限 | 普通用户每个项目的内容总量；不可超过全站硬上限 |
| 用户级覆盖 | 「用户管理 → 编辑」中可按用户覆盖以上两项免费额度，留空用系统默认 |

管理员自身不受数量限制；zip 解压后与文件管理器上传均受额度约束。

## 七、备份与恢复

PageHut 的全部状态都在数据目录中：

```
data/
├── pagehut.db      # SQLite（用户/项目/设置/审核/域名/日志）
├── pagehut.db-wal  # WAL：未 checkpoint 的最近写入（备份时必须一起处理）
├── pagehut.db-shm  # WAL 共享内存索引
├── sites/<项目ID>/  # 各项目站点文件
└── acme/            # auto 模式的证书缓存（可不备份）
```

> SQLite 运行在 WAL 模式下。**只拷贝 `pagehut.db` 会丢掉最近的写入**，
> 因此不要用 `cp` / `tar` 直接抓主库文件。

备份（推荐 `.backup`，在线且一致）：

```bash
sqlite3 data/pagehut.db ".backup '/backup/pagehut-$(date +%F).db'"
tar czf backup-sites.tgz data/sites/
```

如果宿主机没有 `sqlite3`（例如只有 Docker 环境），两种办法：

```bash
# 1) 用容器里的二进制 + 挂载数据目录
docker run --rm -v "$PWD/data:/data" keinos/sqlite3 \
  sqlite3 /data/pagehut.db ".backup '/data/backup.db'"
# 2) 停服后连 WAL 一起打包（先停服，让进程优雅退出并 checkpoint）
docker compose stop
tar czf backup.tgz data/pagehut.db data/pagehut.db-wal data/pagehut.db-shm data/sites/
docker compose start
```

恢复：停服 → 还原 `pagehut.db` 与 `sites/` → **删除旧的 `pagehut.db-wal` / `pagehut.db-shm`**
（残留的 WAL 可能回放陈旧数据）→ 启动。

## 八、升级

1. 备份数据目录（见上）
2. 替换二进制/镜像
3. 重启服务

> 表结构目前只做「新增表 / 新增索引」的幂等创建（`CREATE TABLE IF NOT EXISTS`），
> **给已有表加列不会自动生效**，版本化迁移机制见 ROADMAP 的 v0.2 计划。
