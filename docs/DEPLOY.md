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

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now pagehut
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
        client_max_body_size 200m;   # ≥ 后台配置的单项目大小上限
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
        client_max_body_size 200m;
    }
}
```

### Caddy（自动 HTTPS，含泛域名按需签发）

```caddy
{
    on_demand_tls {
        ask http://127.0.0.1:8080/login   # 存在性检查端点（200 即放行）
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

> 使用 Caddy on-demand 时，请把 `ask` 指向一个能代表「该域名归我管」的检查端点；`/login` 返回 200 可作为最简单的放行策略。更严格的做法是在反代层维护域名白名单。

## 四、DNS 配置

| 用途 | 记录 | 示例 |
|---|---|---|
| 面板 | A 记录 `dash` → 服务器 IP | `dash.example.com` |
| 站点子域名 | A 记录 `*.sites` → 服务器 IP（泛解析） | `demo.sites.example.com` |
| 自定义域名（用户操作） | CNAME `www` → `demo.sites.example.com`，或 A 记录指向服务器 IP | — |

后台「系统设置」中：

- **子域名后缀**：填 `sites.example.com`，用户项目即获得 `前缀.sites.example.com`；留空则不启用子域名访问（仍可用预览与自定义域名）
- **面板域名**：填 `dash.example.com`；留空表示「未匹配到任何站点的一律进面板」（适合刚开始只有面板域名的场景）。IP 与 localhost 访问始终进面板

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
├── sites/<项目ID>/  # 各项目站点文件
└── acme/            # auto 模式的证书缓存（可不备份）
```

备份：

```bash
sqlite3 data/pagehut.db ".backup '/backup/pagehut-$(date +%F).db'"
tar czf backup.tgz data/pagehut.db data/sites/
```

恢复：停服 → 还原两个路径 → 启动。建议先停服再拷贝 SQLite 主文件，或使用 `.backup` 在线备份。

## 八、升级

1. 备份数据目录
2. 替换二进制/镜像
3. 重启服务（数据库结构自动迁移，只增不改）
