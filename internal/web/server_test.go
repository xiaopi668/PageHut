package web

import (
	"archive/zip"
	"bytes"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"pagehut/internal/config"
	"pagehut/internal/db"
	"pagehut/internal/storage"
	"pagehut/internal/store"
)

// ---------- 测试脚手架 ----------

type testEnv struct {
	t    *testing.T
	srv  *httptest.Server
	st   *store.Store
	disk *storage.Storage
	web  *Web
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(dir)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	st := store.New(d)
	disk := storage.New(dir)
	_, loop, _ := net.ParseCIDR("127.0.0.1/8")
	cfg := &config.Config{
		DataDir:        dir,
		HTTPAddr:       "127.0.0.1:0",
		TLS:            config.TLSManual,
		TrustedProxies: []*net.IPNet{loop},
	}
	w, err := Build(cfg, d, st, disk)
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	srv := httptest.NewServer(w)
	t.Cleanup(srv.Close)
	// 限流器是包级单例，逐个用例清零，避免互相干扰。
	limiter.mu.Lock()
	limiter.fails = map[string]*failInfo{}
	limiter.mu.Unlock()
	return &testEnv{t: t, srv: srv, st: st, disk: disk, web: w}
}

func (e *testEnv) addUser(username, password, role string) *store.User {
	e.t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		e.t.Fatalf("生成密码失败: %v", err)
	}
	id, err := e.st.CreateUser(username, string(hash), role)
	if err != nil {
		e.t.Fatalf("创建用户失败: %v", err)
	}
	u, err := e.st.GetUserByID(id)
	if err != nil || u == nil {
		e.t.Fatalf("读取用户失败: %v", err)
	}
	return u
}

func (e *testEnv) addProject(owner *store.User, slug string) *store.Project {
	e.t.Helper()
	id, err := e.st.CreateProject(owner.ID, slug, "项目 "+slug)
	if err != nil {
		e.t.Fatalf("创建项目失败: %v", err)
	}
	if err := e.disk.Create(id); err != nil {
		e.t.Fatalf("创建项目目录失败: %v", err)
	}
	p, err := e.st.GetProject(id)
	if err != nil || p == nil {
		e.t.Fatalf("读取项目失败: %v", err)
	}
	return p
}

func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("创建 cookie jar 失败: %v", err)
	}
	return &http.Client{
		Jar: jar,
		// 不自动跟随跳转，便于断言 303 / 403。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (e *testEnv) cookie(cl *http.Client, name string) string {
	e.t.Helper()
	u, _ := url.Parse(e.srv.URL)
	for _, c := range cl.Jar.Cookies(u) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func (e *testEnv) get(cl *http.Client, path string) *http.Response {
	e.t.Helper()
	resp, err := cl.Get(e.srv.URL + path)
	if err != nil {
		e.t.Fatalf("GET %s 失败: %v", path, err)
	}
	return resp
}

func (e *testEnv) post(cl *http.Client, path string, form url.Values) *http.Response {
	e.t.Helper()
	resp, err := cl.PostForm(e.srv.URL+path, form)
	if err != nil {
		e.t.Fatalf("POST %s 失败: %v", path, err)
	}
	return resp
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应体失败: %v", err)
	}
	return string(b)
}

// login 用「先 GET 取令牌、再 POST」的方式登录。
func (e *testEnv) login(cl *http.Client, username, password string) *http.Response {
	e.t.Helper()
	e.get(cl, "/login").Body.Close()
	csrf := e.cookie(cl, cookieCSRF)
	if csrf == "" {
		e.t.Fatal("登录页未下发 CSRF cookie")
	}
	return e.post(cl, "/login", url.Values{
		"username": {username}, "password": {password}, "_csrf": {csrf}, "next": {"/"},
	})
}

func (e *testEnv) loginOK(cl *http.Client, username, password string) {
	e.t.Helper()
	resp := e.login(cl, username, password)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		e.t.Fatalf("登录 %s 失败: HTTP %d", username, resp.StatusCode)
	}
}

// zipBody 构造一个 multipart 表单，内含指定文件内容的 zip。
func zipBody(t *testing.T, files map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	for name, content := range files {
		fw, err := zw.Create(name)
		if err != nil {
			t.Fatalf("构造 zip 失败: %v", err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatalf("写入 zip 失败: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 zip 失败: %v", err)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("zip", "site.zip")
	if err != nil {
		t.Fatalf("构造表单失败: %v", err)
	}
	if _, err := fw.Write(zbuf.Bytes()); err != nil {
		t.Fatalf("写入表单失败: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("关闭表单失败: %v", err)
	}
	return &body, mw.FormDataContentType()
}

func (e *testEnv) uploadZip(cl *http.Client, csrf string, projectID int64, files map[string]string) *http.Response {
	e.t.Helper()
	bodyBuf, contentType := zipBody(e.t, files)
	req, err := http.NewRequest("POST", e.srv.URL+"/projects/"+itoa(projectID)+"/upload", bodyBuf)
	if err != nil {
		e.t.Fatalf("构造上传请求失败: %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.AddCookie(&http.Cookie{Name: cookieCSRF, Value: csrf})
	u, _ := url.Parse(e.srv.URL)
	for _, c := range cl.Jar.Cookies(u) {
		req.AddCookie(c)
	}
	resp, err := cl.Do(req)
	if err != nil {
		e.t.Fatalf("上传请求失败: %v", err)
	}
	return resp
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// ---------- P0：预览必须运行在不透明源里 ----------

func TestPreviewIsSandboxed(t *testing.T) {
	env := newTestEnv(t)
	admin := env.addUser("admin", "admin-pass-123", "admin")
	p := env.addProject(admin, "demo")
	// 上传一个带脚本的站点
	if err := env.disk.WriteFile(p.ID, "index.html", []byte(`<script>parent.document.title="pwned"</script>`)); err != nil {
		t.Fatalf("写入站点文件失败: %v", err)
	}

	cl := newClient(t)
	env.loginOK(cl, "admin", "admin-pass-123")
	resp := env.get(cl, "/preview/"+itoa(p.ID)+"/")
	defer resp.Body.Close()
	got := body(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("预览应 200，实际 %d", resp.StatusCode)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox") {
		t.Errorf("预览响应必须带 CSP sandbox，实际 CSP=%q", csp)
	}
	if strings.Contains(csp, "allow-same-origin") {
		t.Errorf("预览响应不能放行 allow-same-origin，实际 CSP=%q", csp)
	}
	if resp.Header.Get("X-Frame-Options") != "" {
		t.Error("预览响应不应设置 X-Frame-Options，否则审核页 iframe 无法加载")
	}
	if !strings.Contains(got, "parent.document.title") {
		t.Error("预览应原样返回站点内容（隔离靠 CSP，而不是改写内容）")
	}
}

func TestPanelSecurityHeaders(t *testing.T) {
	env := newTestEnv(t)
	resp := env.get(newClient(t), "/login")
	defer resp.Body.Close()
	if xfo := resp.Header.Get("X-Frame-Options"); xfo != "DENY" {
		t.Errorf("面板应设置 X-Frame-Options: DENY，实际 %q", xfo)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("面板 CSP 应含 frame-ancestors 'none'，实际 %q", csp)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("面板应设置 X-Content-Type-Options: nosniff")
	}
}

// ---------- P1：CSRF 令牌绑定会话，父域投放的 cookie 无效 ----------

func TestCSRFRejectsPlantedCookie(t *testing.T) {
	env := newTestEnv(t)
	env.addUser("admin", "admin-pass-123", "admin")

	cl := newClient(t)
	env.loginOK(cl, "admin", "admin-pass-123")
	// 已登录请求后，服务端会把 cookie 同步成「会话派生令牌」
	env.get(cl, "/account").Body.Close()
	derived := env.cookie(cl, cookieCSRF)
	session := env.cookie(cl, cookieSession)
	if derived == "" || session == "" {
		t.Fatal("缺少会话或 CSRF cookie")
	}

	form := url.Values{
		"current": {"admin-pass-123"}, "new": {"new-pass-4567"}, "confirm": {"new-pass-4567"},
	}

	// 1) 攻击者投放一个自己知道的 pp_csrf，并让表单带上同一个值 → 必须被拒
	planted := strings.Repeat("a", 64)
	req, err := http.NewRequest("POST", env.srv.URL+"/account/password", strings.NewReader(
		url.Values{"_csrf": {planted}, "current": {form.Get("current")},
			"new": {form.Get("new")}, "confirm": {form.Get("confirm")}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: cookieSession, Value: session})
	req.AddCookie(&http.Cookie{Name: cookieCSRF, Value: planted})
	raw := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := raw.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("投放 cookie 的伪造请求应 403，实际 %d", resp.StatusCode)
	}

	// 2) 用页面里真实令牌 → 正常通过
	form.Set("_csrf", derived)
	resp2 := env.post(cl, "/account/password", form)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Fatalf("使用真实令牌应 303，实际 %d", resp2.StatusCode)
	}
}

// ---------- P1：限流不再因单个 IP 锁死全站 ----------

func TestLoginLimiterDoesNotLockOutEveryone(t *testing.T) {
	env := newTestEnv(t)
	env.addUser("victim", "victim-pass-123", "user")
	env.addUser("other", "other-pass-123", "user")

	cl := newClient(t)
	for i := 0; i < maxFailsPerUser; i++ {
		resp := env.login(cl, "victim", "wrong-password")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("第 %d 次错误密码应 401，实际 %d", i+1, resp.StatusCode)
		}
	}
	// 同一 IP 下的另一个账号仍能正常登录（旧实现会返回 429）
	other := newClient(t)
	resp := env.login(other, "other", "other-pass-123")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("同 IP 的其他用户应能登录（不应被连坐），实际 %d", resp.StatusCode)
	}
	// 而被针对的账号已被限流
	resp2 := env.login(newClient(t), "victim", "victim-pass-123")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("被针对账号应 429，实际 %d", resp2.StatusCode)
	}
}

func TestClientIPTrustsOnlyTrustedProxies(t *testing.T) {
	_, loop, _ := net.ParseCIDR("127.0.0.1/8")
	cases := []struct {
		name    string
		trusted []*net.IPNet
		peer    string
		headers map[string]string
		want    string
	}{
		{"非可信来源忽略转发头", nil, "203.0.113.5:1234", map[string]string{"X-Forwarded-For": "1.2.3.4"}, "203.0.113.5"},
		{"可信代理采信 XFF", []*net.IPNet{loop}, "127.0.0.1:1234", map[string]string{"X-Forwarded-For": "1.2.3.4"}, "1.2.3.4"},
		{"从右往左跳过可信代理", []*net.IPNet{loop}, "127.0.0.1:1234",
			map[string]string{"X-Forwarded-For": "9.9.9.9, 127.0.0.1"}, "9.9.9.9"},
		{"无 XFF 时退回 X-Real-IP", []*net.IPNet{loop}, "127.0.0.1:1234", map[string]string{"X-Real-IP": "5.6.7.8"}, "5.6.7.8"},
		{"可信代理但无任何转发头", []*net.IPNet{loop}, "127.0.0.1:1234", nil, "127.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &Web{cfg: &config.Config{TrustedProxies: tc.trusted}}
			r := httptest.NewRequest("POST", "http://panel.example.com/login", nil)
			r.RemoteAddr = tc.peer
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := w.clientIP(r); got != tc.want {
				t.Errorf("clientIP = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// ---------- P1：下架状态不可被覆盖 ----------

func TestSuspendedProjectRejectsUpload(t *testing.T) {
	env := newTestEnv(t)
	admin := env.addUser("admin", "admin-pass-123", "admin")
	p := env.addProject(admin, "demo")
	if err := env.st.UpdateProjectStatus(p.ID, store.StatusSuspended, "违规", &admin.ID); err != nil {
		t.Fatalf("下架失败: %v", err)
	}

	cl := newClient(t)
	env.loginOK(cl, "admin", "admin-pass-123")
	env.get(cl, "/projects/"+itoa(p.ID)).Body.Close()
	csrf := env.cookie(cl, cookieCSRF)

	resp := env.uploadZip(cl, csrf, p.ID, map[string]string{"index.html": "hello"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("已下架项目的上传应 403，实际 %d", resp.StatusCode)
	}
	// 状态与内容都不应被改动
	after, err := env.st.GetProject(p.ID)
	if err != nil || after == nil {
		t.Fatalf("读取项目失败: %v", err)
	}
	if after.Status != store.StatusSuspended {
		t.Errorf("下架状态被覆盖为 %q", after.Status)
	}
	if _, _, err := env.disk.Usage(p.ID); err != nil {
		t.Errorf("读取项目内容失败: %v", err)
	}
}

// ---------- P1：自定义域名同样遵守项目状态 ----------

func TestCustomDomainRespectsProjectStatus(t *testing.T) {
	env := newTestEnv(t)
	admin := env.addUser("admin", "admin-pass-123", "admin")
	p := env.addProject(admin, "demo") // 默认 pending
	if err := env.disk.WriteFile(p.ID, "index.html", []byte("SECRET-CONTENT")); err != nil {
		t.Fatalf("写入站点文件失败: %v", err)
	}
	if err := env.st.CreateDomain(p.ID, "www.example.org", "tok"); err != nil {
		t.Fatalf("创建域名失败: %v", err)
	}
	d, err := env.st.GetDomainByHost("www.example.org")
	if err != nil || d == nil {
		t.Fatalf("读取域名失败: %v", err)
	}
	if err := env.st.UpdateDomainStatus(d.ID, "active", &admin.ID); err != nil {
		t.Fatalf("开通域名失败: %v", err)
	}

	req := httptest.NewRequest("GET", "http://www.example.org/index.html", nil)
	req.Host = "www.example.org"
	rec := httptest.NewRecorder()
	env.web.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("未发布项目不应通过自定义域名对外服务（实际 200）")
	}
	if strings.Contains(rec.Body.String(), "SECRET-CONTENT") {
		t.Fatal("未发布项目的内容通过自定义域名泄露了")
	}
}

// ---------- P2：密码长度 / 健康检查 / 静态目录列表 ----------

func TestPasswordLengthLimit(t *testing.T) {
	env := newTestEnv(t)
	env.addUser("admin", "admin-pass-123", "admin")
	long := strings.Repeat("A", 100)

	// 管理员创建用户路径
	cl := newClient(t)
	env.loginOK(cl, "admin", "admin-pass-123")
	env.get(cl, "/admin/users/new").Body.Close()
	resp := env.post(cl, "/admin/users/new", url.Values{
		"username": {"longpw"}, "password": {long}, "role": {"user"},
		"_csrf": {env.cookie(cl, cookieCSRF)},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("超长密码应 400（旧实现 500），实际 %d", resp.StatusCode)
	}
	if u, _ := env.st.GetUserByUsername("longpw"); u != nil {
		t.Error("超长密码不应创建出用户")
	}

	// 公开注册路径
	if err := env.st.UpdateSettings(map[string]string{"registration_mode": "public"}); err != nil {
		t.Fatalf("更新设置失败: %v", err)
	}
	env.web.invalidateSettings()
	anon := newClient(t)
	env.get(anon, "/register").Body.Close()
	resp2 := env.post(anon, "/register", url.Values{
		"username": {"longpw2"}, "password": {long}, "password2": {long},
		"_csrf": {env.cookie(anon, cookieCSRF)},
	})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("注册超长密码应 400，实际 %d", resp2.StatusCode)
	}
}

func TestHealthz(t *testing.T) {
	env := newTestEnv(t)
	resp := env.get(newClient(t), "/healthz")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz 应 200，实际 %d", resp.StatusCode)
	}
	if got := body(t, resp); !strings.Contains(got, "ok") {
		t.Errorf("/healthz 响应异常: %q", got)
	}
}

func TestStaticNoDirectoryListing(t *testing.T) {
	env := newTestEnv(t)
	resp := env.get(newClient(t), "/static/")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/static/ 应 404，实际 %d", resp.StatusCode)
	}
	// 正常文件仍可访问
	resp2 := env.get(newClient(t), "/static/app.css")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("/static/app.css 应 200，实际 %d", resp2.StatusCode)
	}
}

// ---------- P2：审核员不能自审 ----------

func TestReviewerCannotSelfApprove(t *testing.T) {
	env := newTestEnv(t)
	reviewer := env.addUser("rev", "rev-pass-1234", "reviewer")
	p := env.addProject(reviewer, "own") // 审核员自己的项目，pending

	cl := newClient(t)
	env.loginOK(cl, "rev", "rev-pass-1234")
	env.get(cl, "/review/"+itoa(p.ID)).Body.Close()
	resp := env.post(cl, "/review/"+itoa(p.ID), url.Values{
		"action": {"approve"}, "reason": {""}, "_csrf": {env.cookie(cl, cookieCSRF)},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("审核员自审应 403，实际 %d", resp.StatusCode)
	}
	after, _ := env.st.GetProject(p.ID)
	if after != nil && after.Status == store.StatusPublished {
		t.Error("项目不应被自己审核通过")
	}
}
