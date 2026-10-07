#!/usr/bin/env bash
# PageHut 端到端冒烟测试：从零启动服务并走通核心流程。
# 依赖：curl、python3（生成测试 zip）
set -euo pipefail

# 固定到仓库根目录，保证从任何 CWD 调用都能 go build .
cd "$(dirname "$0")/.."

PORT="${PORT:-8099}"
BASE="http://127.0.0.1:${PORT}"
WORK="$(mktemp -d)"
DATA="${WORK}/data"
LOG="${WORK}/server.log"
JAR_ADMIN="${WORK}/admin.jar"
JAR_USER="${WORK}/user.jar"
mkdir -p "${DATA}"

pass() { echo "  ✓ $1"; }
fail() { echo "  ✗ $1"; exit 1; }
check() { # check <描述> <期望片段> <实际内容>
  if [[ "$3" == *"$2"* ]]; then pass "$1"; else echo "    期望包含: $2"; echo "    实际: $3"; fail "$1"; fi
}

echo "══ PageHut E2E (${WORK}) ══"

# 1. 启动
go build -o "${WORK}/pagehut" . 2>/dev/null || (go build -o "${WORK}/pagehut" .)
"${WORK}/pagehut" -data "${DATA}" -http ":${PORT}" > "${LOG}" 2>&1 &
SRV_PID=$!
cleanup() {
  kill "${SRV_PID}" 2>/dev/null || true
  wait "${SRV_PID}" 2>/dev/null || true
  rm -rf "${WORK}"
}
trap cleanup EXIT
for i in $(seq 1 50); do curl -s -o /dev/null "${BASE}/login" && break; sleep 0.2; done

ADMIN_PW="$(grep -oP '初始管理员密码: \K\S+' "${LOG}" || true)"
[[ -n "${ADMIN_PW}" ]] && pass "服务启动，初始管理员密码已生成" || fail "未找到初始管理员密码（服务可能未启动，见 ${LOG}）"

# 取 CSRF 令牌：先 GET 一次面板页，让服务端把 cookie 同步成当前会话对应的
# 令牌（已登录用户用的是「会话派生值」，只在响应里下发），再从 jar 读取。
csrf() {
  curl -s -b "$1" -c "$1" -o /dev/null "${BASE}/" || true
  grep pp_csrf "$1" | awk '{print $7}' | tail -1
}

# 2. 管理员登录
curl -s -c "${JAR_ADMIN}" -o /dev/null "${BASE}/login"
code=$(curl -s -b "${JAR_ADMIN}" -c "${JAR_ADMIN}" -o /dev/null -w '%{http_code}' \
  -d "_csrf=$(csrf ${JAR_ADMIN})&username=admin&password=${ADMIN_PW}&next=/" "${BASE}/login")
[[ "$code" == 303 ]] && pass "管理员登录" || fail "管理员登录 (HTTP ${code})"

# 3. 修改系统设置：公开注册 + 开启审核 + 子域名后缀
code=$(curl -s -b "${JAR_ADMIN}" -c "${JAR_ADMIN}" -o /dev/null -w '%{http_code}' \
  --data-urlencode "_csrf=$(csrf ${JAR_ADMIN})" \
  --data-urlencode "site_name=PageHut测试站" \
  --data-urlencode "registration_mode=public" \
  --data-urlencode "review_enabled=1" \
  --data-urlencode "max_project_size_mb=10" \
  --data-urlencode "free_project_count=2" \
  --data-urlencode "free_project_size_mb=1" \
  --data-urlencode "sites_host=sites.example.com" \
  --data-urlencode "panel_host=" \
  "${BASE}/admin/settings")
[[ "$code" == 303 ]] && pass "保存系统设置" || fail "保存系统设置 (HTTP ${code})"
body=$(curl -s -b "${JAR_ADMIN}" "${BASE}/admin/settings")
check "设置已生效（站点名）" "PageHut测试站" "$body"

# 4. 创建项目
curl -s -b "${JAR_ADMIN}" -c "${JAR_ADMIN}" -o /dev/null "${BASE}/projects/new"
code=$(curl -s -b "${JAR_ADMIN}" -c "${JAR_ADMIN}" -o /dev/null -w '%{http_code}' \
  -d "_csrf=$(csrf ${JAR_ADMIN})&name=演示站点&slug=demo" "${BASE}/projects/new")
[[ "$code" == 303 ]] && pass "创建项目" || fail "创建项目 (HTTP ${code})"

# 找到项目 ID（grep 无匹配时不要让 pipefail 静默中止）
pid=$(curl -s -b "${JAR_ADMIN}" "${BASE}/" | grep -oP '/projects/\K[0-9]+' | head -1 || true)
[[ -n "${pid}" ]] && pass "项目出现在仪表盘 (id=${pid})" || fail "未找到项目"

# 5. 打包并上传 zip（审核开启 → 应进入待审核）
SITE_DIR="${WORK}/site"; mkdir -p "${SITE_DIR}"
echo '<h1>Hello PageHut</h1><p>第一个静态站</p>' > "${SITE_DIR}/index.html"
mkdir -p "${SITE_DIR}/assets"
echo 'body{color:red}' > "${SITE_DIR}/assets/style.css"
python3 - "${SITE_DIR}" "${WORK}/site.zip" <<'EOF'
import sys, os, zipfile
src, dst = sys.argv[1], sys.argv[2]
with zipfile.ZipFile(dst, 'w', zipfile.ZIP_DEFLATED) as z:
    for root, _, files in os.walk(src):
        for f in files:
            full = os.path.join(root, f)
            z.write(full, os.path.relpath(full, src))
EOF
code=$(curl -s -b "${JAR_ADMIN}" -c "${JAR_ADMIN}" -o /dev/null -w '%{http_code}' \
  -F "_csrf=$(csrf ${JAR_ADMIN})" -F "zip=@${WORK}/site.zip" \
  "${BASE}/projects/${pid}/upload")
[[ "$code" == 303 ]] && pass "上传 zip" || fail "上传 zip (HTTP ${code})"

body=$(curl -s -b "${JAR_ADMIN}" "${BASE}/projects/${pid}")
check "上传后进入待审核" "待审核" "$body"

# 6. 站点 Host 访问：未发布 → 403 占位页
code=$(curl -s -o "${WORK}/host.html" -w '%{http_code}' -H "Host: demo.sites.example.com" "${BASE}/index.html")
[[ "$code" == 403 ]] && pass "未发布站点返回 403 占位页" || fail "未发布站点应 403（实际 ${code}）"

# 7. 预览可访问（同源带会话）
body=$(curl -s -b "${JAR_ADMIN}" "${BASE}/preview/${pid}/")
check "审核预览可访问" "Hello PageHut" "$body"

# 8. 审核通过
code=$(curl -s -b "${JAR_ADMIN}" -c "${JAR_ADMIN}" -o /dev/null -w '%{http_code}' \
  -d "_csrf=$(csrf ${JAR_ADMIN})&action=approve" "${BASE}/review/${pid}")
[[ "$code" == 303 ]] && pass "审核通过" || fail "审核通过 (HTTP ${code})"

body=$(curl -s -H "Host: demo.sites.example.com" "${BASE}/index.html")
check "发布后站点可访问" "Hello PageHut" "$body"
body=$(curl -s -H "Host: demo.sites.example.com" "${BASE}/assets/style.css")
check "静态资源 MIME/内容正常" "color:red" "$body"
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Host: demo.sites.example.com" "${BASE}/no-such-page.html")
[[ "$code" == 404 ]] && pass "404 正常" || fail "404 应为 404（实际 ${code}）"

# 9. 公开注册新用户
curl -s -c "${JAR_USER}" -o /dev/null "${BASE}/register"
code=$(curl -s -b "${JAR_USER}" -c "${JAR_USER}" -o /dev/null -w '%{http_code}' \
  -d "_csrf=$(csrf ${JAR_USER})&username=e2euser&password=pass12345&password2=pass12345" \
  "${BASE}/register")
[[ "$code" == 303 ]] && pass "公开注册" || fail "公开注册 (HTTP ${code})"

# 10. 免费额度：默认项目数上限 2，第 3 个应被拒
for n in a b; do
  curl -s -b "${JAR_USER}" -c "${JAR_USER}" -o /dev/null \
    -d "_csrf=$(csrf ${JAR_USER})&name=项目${n}&slug=u-${n}" "${BASE}/projects/new"
done
code=$(curl -s -b "${JAR_USER}" -o /dev/null -w '%{http_code}' \
  -d "_csrf=$(csrf ${JAR_USER})&name=项目c&slug=u-c" "${BASE}/projects/new")
[[ "$code" == 403 ]] && pass "项目数量额度拦截（第 3 个被拒）" || fail "额度拦截应 403（实际 ${code}）"

# 11. 大小额度：免费单项目 1MB，上传 2MB 随机 zip 应被拒
BIG="${WORK}/big.zip"
python3 - "$BIG" <<'EOF'
import sys, zipfile, zlib, os
path = sys.argv[1]
with zipfile.ZipFile(path, 'w', zipfile.ZIP_DEFLATED) as z:
    data = os.urandom(2 * 1024 * 1024)  # 2MB 随机数据（不可压缩）
    z.writestr('big.bin', data)
EOF
upid=$(curl -s -b "${JAR_USER}" "${BASE}/" | grep -oP '/projects/\K[0-9]+' | head -1 || true)
code=$(curl -s -b "${JAR_USER}" -o "${WORK}/up_resp.html" -w '%{http_code}' \
  -F "_csrf=$(csrf ${JAR_USER})" -F "zip=@${BIG}" \
  "${BASE}/projects/${upid}/upload")
[[ "$code" == 400 ]] && pass "项目大小额度拦截（2MB > 1MB 上限）" || fail "大小拦截应 400（实际 ${code}）"

# 12. 自定义域名：申请 → 令牌验证 → 管理员开通 → 域名访问
curl -s -b "${JAR_ADMIN}" -c "${JAR_ADMIN}" "${BASE}/projects/${pid}/domains" -o /dev/null
code=$(curl -s -b "${JAR_ADMIN}" -c "${JAR_ADMIN}" -o /dev/null -w '%{http_code}' \
  -d "_csrf=$(csrf ${JAR_ADMIN})&domain=www.mydemo.cn" \
  "${BASE}/projects/${pid}/domains")
[[ "$code" == 303 ]] && pass "提交自定义域名申请" || fail "域名申请 (HTTP ${code})"

# 12.1 回归：域名页必须给出真实可用的 CNAME 目标（曾渲染成 "demo."）
body=$(curl -s -b "${JAR_ADMIN}" "${BASE}/projects/${pid}/domains")
check "域名页给出 CNAME 目标" "demo.sites.example.com" "$body"

# 12.2 回归：文件管理器根目录（曾经 400 非法路径）
code=$(curl -s -b "${JAR_ADMIN}" -o "${WORK}/files.html" -w '%{http_code}' "${BASE}/projects/${pid}/files")
[[ "$code" == 200 ]] && pass "文件标签页可打开（根目录）" || fail "文件标签页应 200（实际 ${code}）"
check "根目录列出站点文件" "index.html" "$(cat "${WORK}/files.html")"

token=$(curl -s -b "${JAR_ADMIN}" "${BASE}/projects/${pid}/domains" | grep -oP 'pagehut-verify/\K[a-f0-9]+' | head -1 || true)
[[ -n "${token}" ]] && pass "验证令牌已生成" || fail "未找到验证令牌"
body=$(curl -s -H "Host: www.mydemo.cn" "${BASE}/.well-known/pagehut-verify/${token}")
check "令牌验证成功" "验证成功" "$body"

did=$(curl -s -b "${JAR_ADMIN}" "${BASE}/admin/domains" | grep -oP '/admin/domains/\K[0-9]+' | head -1 || true)
code=$(curl -s -b "${JAR_ADMIN}" -o /dev/null -w '%{http_code}' \
  -d "_csrf=$(csrf ${JAR_ADMIN})&action=activate" "${BASE}/admin/domains/${did}")
[[ "$code" == 303 ]] && pass "管理员开通域名" || fail "开通域名 (HTTP ${code})"

body=$(curl -s -H "Host: www.mydemo.cn" "${BASE}/index.html")
check "自定义域名可访问站点" "Hello PageHut" "$body"

# 13. 未托管域名
body=$(curl -s -H "Host: nothing.here.example" "${BASE}/")
check "未托管域名提示页" "尚未托管" "$body"

# 14. CSRF 防护：不带令牌的 POST 应 403
code=$(curl -s -b "${JAR_ADMIN}" -o /dev/null -w '%{http_code}' -d "username=x&password=y" "${BASE}/login")
[[ "$code" == 403 ]] && pass "CSRF 防护生效" || fail "CSRF 应 403（实际 ${code}）"

# 15. 健康检查
code=$(curl -s -o "${WORK}/health.txt" -w '%{http_code}' "${BASE}/healthz")
[[ "$code" == 200 ]] && pass "健康检查 /healthz 可用" || fail "/healthz 应 200（实际 ${code}）"

# 16. 面板安全响应头
hdr=$(curl -s -D - -o /dev/null "${BASE}/login")
check "面板带 X-Frame-Options: DENY" "X-Frame-Options: DENY" "$hdr"
check "面板 CSP 禁止被嵌套" "frame-ancestors 'none'" "$hdr"

# 17. 预览必须运行在 CSP sandbox（不透明源）里
hdr=$(curl -s -D - -o /dev/null -b "${JAR_ADMIN}" "${BASE}/preview/${pid}/")
check "预览响应带 CSP sandbox" "sandbox" "$hdr"
[[ "$hdr" == *"allow-same-origin"* ]] && fail "预览 CSP 不应放行 allow-same-origin" || pass "预览未放行 allow-same-origin"

# 18. 静态资源不做目录列表
code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE}/static/")
[[ "$code" == 404 ]] && pass "静态目录不做列表（/static/ → 404）" || fail "/static/ 应 404（实际 ${code}）"

# 19. 下架不可被覆盖：子域名与自定义域名都必须停止服务
code=$(curl -s -b "${JAR_ADMIN}" -o /dev/null -w '%{http_code}' \
  -d "_csrf=$(csrf ${JAR_ADMIN})&action=suspend&reason=e2e" "${BASE}/review/${pid}")
[[ "$code" == 303 ]] && pass "管理员下架项目" || fail "下架应 303（实际 ${code}）"

code=$(curl -s -o /dev/null -w '%{http_code}' -H "Host: demo.sites.example.com" "${BASE}/index.html")
[[ "$code" == 403 ]] && pass "下架后子域名不再服务" || fail "下架后子域名应 403（实际 ${code}）"
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Host: www.mydemo.cn" "${BASE}/index.html")
[[ "$code" == 403 ]] && pass "下架后自定义域名不再服务" || fail "下架后自定义域名应 403（实际 ${code}）"

code=$(curl -s -b "${JAR_ADMIN}" -o /dev/null -w '%{http_code}' \
  -F "_csrf=$(csrf ${JAR_ADMIN})" -F "zip=@${WORK}/site.zip" \
  "${BASE}/projects/${pid}/upload" || true)
[[ "$code" == 403 ]] && pass "下架项目拒绝重新上传" || fail "下架后上传应 403（实际 ${code}）"

code=$(curl -s -b "${JAR_ADMIN}" -o /dev/null -w '%{http_code}' \
  -d "_csrf=$(csrf ${JAR_ADMIN})&action=publish" "${BASE}/review/${pid}")
[[ "$code" == 303 ]] && pass "管理员强制发布恢复" || fail "发布应 303（实际 ${code}）"
body=$(curl -s -H "Host: demo.sites.example.com" "${BASE}/index.html")
check "恢复后站点可访问" "Hello PageHut" "$body"

echo "══ 全部通过 ══"
