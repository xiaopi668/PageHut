package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"pagehut/internal/mail"
	"pagehut/internal/oidc"
	"pagehut/internal/store"
)

// ---------- 人机验证 ----------

// TestCaptchaImageFlow 覆盖图形验证码：图片可取、错误码被拒、正确码放行。
func TestCaptchaImageFlow(t *testing.T) {
	env := newTestEnv(t)
	env.addUser("admin", "admin-pass-123", "admin")
	set := func(kv map[string]string) {
		if err := env.st.UpdateSettings(kv); err != nil {
			t.Fatalf("更新设置失败: %v", err)
		}
		env.web.invalidateSettings()
	}
	set(map[string]string{"captcha_provider": "image", "captcha_on_login": "1"})

	cl := newClient(t)
	// 取图：应返回 PNG 并下发挑战 cookie
	resp := env.get(cl, "/captcha/image")
	png := body(t, resp)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("取图形验证码失败: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.HasPrefix(png, "\x89PNG") {
		t.Fatal("返回的不是 PNG")
	}
	id := env.cookie(cl, captchaCookie)
	if id == "" {
		t.Fatal("未下发验证码 cookie")
	}

	// 登录页应渲染验证码输入框
	page := body(t, env.get(cl, "/login"))
	if !strings.Contains(page, `name="captcha_code"`) || !strings.Contains(page, "/captcha/image") {
		t.Error("登录页未渲染图形验证码")
	}

	// 错误验证码 → 被拒
	resp = env.post(cl, "/login", url.Values{
		"username": {"admin"}, "password": {"admin-pass-123"},
		"captcha_code": {"00000"}, "_csrf": {env.cookie(cl, cookieCSRF)},
	})
	if got := body(t, resp); !strings.Contains(got, "验证码不正确") {
		t.Errorf("错误验证码应被拒，实际响应未包含提示")
	}

	// 正确验证码 → 通过（随后因密码正确应登录成功）
	answer := ""
	captchas.mu.Lock()
	if c, ok := captchas.items[id]; ok {
		answer = c.answer
	}
	captchas.mu.Unlock()
	if answer == "" {
		t.Fatal("服务端未保存验证码答案")
	}
	resp = env.post(cl, "/login", url.Values{
		"username": {"admin"}, "password": {"admin-pass-123"},
		"captcha_code": {answer}, "_csrf": {env.cookie(cl, cookieCSRF)}, "next": {"/"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("正确验证码应放行登录，实际 %d", resp.StatusCode)
	}
}

// TestCaptchaSliderFlow 覆盖滑动验证码：数据可取、位置错误被拒、位置正确放行。
func TestCaptchaSliderFlow(t *testing.T) {
	env := newTestEnv(t)
	env.addUser("admin", "admin-pass-123", "admin")
	if err := env.st.UpdateSettings(map[string]string{"captcha_provider": "slider", "captcha_on_login": "1"}); err != nil {
		t.Fatalf("更新设置失败: %v", err)
	}
	env.web.invalidateSettings()

	cl := newClient(t)
	resp := env.get(cl, "/captcha/slider")
	raw := body(t, resp)
	var payload struct {
		Bg, Piece string
		Y, Width  int
		Size      int
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("滑动验证码响应不是合法 JSON: %v", err)
	}
	if !strings.HasPrefix(payload.Bg, "data:image/png;base64,") || !strings.HasPrefix(payload.Piece, "data:image/png;base64,") {
		t.Fatal("滑动验证码未返回图片数据")
	}
	id := env.cookie(cl, captchaCookie)

	// 位置错误
	resp = env.post(cl, "/login", url.Values{
		"username": {"admin"}, "password": {"admin-pass-123"}, "captcha_x": {"0"},
		"_csrf": {env.cookie(cl, cookieCSRF)},
	})
	if got := body(t, resp); !strings.Contains(got, "拼图位置不正确") {
		t.Errorf("错误位置应被拒，实际未包含提示")
	}

	// 位置正确
	target := ""
	captchas.mu.Lock()
	if c, ok := captchas.items[id]; ok {
		target = c.answer
	}
	captchas.mu.Unlock()
	resp = env.post(cl, "/login", url.Values{
		"username": {"admin"}, "password": {"admin-pass-123"}, "captcha_x": {target},
		"_csrf": {env.cookie(cl, cookieCSRF)}, "next": {"/"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("正确位置应放行登录，实际 %d", resp.StatusCode)
	}
}

// TestTurnstileVerifyContract 覆盖 Turnstile 服务端校验契约：
// success、action、hostname 三者任一不符都必须拒绝（fail closed）。
func TestTurnstileVerifyContract(t *testing.T) {
	var reply string
	fake := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("siteverify 必须用 POST，实际 %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			t.Errorf("siteverify 必须表单编码，实际 %s", ct)
		}
		_ = r.ParseForm()
		if r.PostFormValue("secret") != "test-secret" {
			t.Errorf("secret 未正确传递")
		}
		rw.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(rw, reply)
	}))
	defer fake.Close()

	oldURL := turnstileVerifyURL
	turnstileVerifyURL = fake.URL
	defer func() { turnstileVerifyURL = oldURL }()

	env := newTestEnv(t)
	env.addUser("admin", "admin-pass-123", "admin")
	if err := env.st.UpdateSettings(map[string]string{
		"captcha_provider": "turnstile", "captcha_on_login": "1",
		"turnstile_site_key": "test-sitekey", "turnstile_secret": "test-secret",
	}); err != nil {
		t.Fatalf("更新设置失败: %v", err)
	}
	env.web.invalidateSettings()

	login := func(token string) (int, string) {
		cl := newClient(t)
		cl.Jar.SetCookies(mustURL(env.srv.URL), []*http.Cookie{{Name: cookieCSRF, Value: "x"}})
		// 用带 cookie 的请求拿 csrf
		env.get(cl, "/login").Body.Close()
		resp := env.post(cl, "/login", url.Values{
			"username": {"admin"}, "password": {"admin-pass-123"},
			"cf-turnstile-response": {token}, "_csrf": {env.cookie(cl, cookieCSRF)},
		})
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// 1) 正常通过 → 登录成功
	reply = `{"success":true,"action":"login","hostname":"127.0.0.1"}`
	if code, _ := login("good-token"); code != http.StatusSeeOther {
		t.Fatalf("校验通过时应登录成功，实际 %d", code)
	}
	// 2) success=false
	reply = `{"success":false,"error-codes":["invalid-input-response"]}`
	if code, b := login("bad"); code == http.StatusSeeOther || !strings.Contains(b, "人机验证未通过") {
		t.Fatalf("success=false 应被拒，实际 %d", code)
	}
	// 3) action 不符
	reply = `{"success":true,"action":"signup","hostname":"127.0.0.1"}`
	if code, b := login("bad"); code == http.StatusSeeOther || !strings.Contains(b, "校验失败") {
		t.Fatalf("action 不符应被拒，实际 %d", code)
	}
	// 4) hostname 不符
	reply = `{"success":true,"action":"login","hostname":"evil.example.com"}`
	if code, b := login("bad"); code == http.StatusSeeOther || !strings.Contains(b, "校验失败") {
		t.Fatalf("hostname 不符应被拒，实际 %d", code)
	}
	// 5) 空令牌
	reply = `{"success":true,"action":"login","hostname":"127.0.0.1"}`
	if code, b := login(""); code == http.StatusSeeOther || !strings.Contains(b, "请先完成人机验证") {
		t.Fatalf("空令牌应被拒，实际 %d", code)
	}
}

func mustURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

// ---------- 邮箱验证 ----------

// fakeMailer 记录发出的验证码邮件内容。
type fakeMailer struct {
	to   string
	body string
	fail bool
}

func (f *fakeMailer) send(_ context.Context, _ mail.Config, to, _, body string) error {
	if f.fail {
		return fmt.Errorf("smtp 故障")
	}
	f.to = to
	f.body = body
	return nil
}

var codeRe = regexp.MustCompile(`验证码是：(\d{6})`)

// TestRegisterRequiresEmailVerification 覆盖「先过人机验证 → 发邮箱验证码 → 注册」。
func TestRegisterRequiresEmailVerification(t *testing.T) {
	env := newTestEnv(t)
	fake := &fakeMailer{}
	oldSend := sendMailFunc
	sendMailFunc = fake.send
	defer func() { sendMailFunc = oldSend }()

	if err := env.st.UpdateSettings(map[string]string{
		"registration_mode": "public",
		"captcha_provider":  "image",
		"smtp_host":         "smtp.example.com", "smtp_from": "noreply@example.com",
		"email_verify_required": "1",
	}); err != nil {
		t.Fatalf("更新设置失败: %v", err)
	}
	env.web.invalidateSettings()

	cl := newClient(t)
	// 注册页应显示邮箱与验证码字段
	page := body(t, env.get(cl, "/register"))
	for _, want := range []string{`name="email"`, `name="code"`, "/register/send-code"} {
		if !strings.Contains(page, want) {
			t.Errorf("注册页缺少 %s", want)
		}
	}

	// 取图形验证码答案
	env.get(cl, "/captcha/image").Body.Close()
	id := env.cookie(cl, captchaCookie)
	captchas.mu.Lock()
	answer := captchas.items[id].answer
	captchas.mu.Unlock()

	// 发送验证码（带正确的人机验证）
	resp := env.post(cl, "/register/send-code", url.Values{
		"username": {"newbie"}, "email": {"Newbie@Example.com "},
		"captcha_code": {answer}, "_csrf": {env.cookie(cl, cookieCSRF)},
	})
	got := body(t, resp)
	if !strings.Contains(got, "验证码已发送") {
		t.Fatalf("发送验证码失败：%s", firstLines(got, 3))
	}
	if fake.to != "newbie@example.com" {
		t.Errorf("收件人应为规范化后的邮箱，实际 %q", fake.to)
	}
	m := codeRe.FindStringSubmatch(fake.body)
	if m == nil {
		t.Fatalf("邮件里没有验证码：%q", fake.body)
	}
	code := m[1]

	// 验证码错误 → 拒绝
	resp = env.post(cl, "/register", url.Values{
		"username": {"newbie"}, "email": {"newbie@example.com"},
		"password": {"newbie-pass-1"}, "password2": {"newbie-pass-1"},
		"code": {"000000"}, "_csrf": {env.cookie(cl, cookieCSRF)},
	})
	if b := body(t, resp); !strings.Contains(b, "验证码不正确") {
		t.Error("错误邮箱验证码应被拒")
	}
	// 验证码正确 → 注册成功且邮箱已绑定
	resp = env.post(cl, "/register", url.Values{
		"username": {"newbie"}, "email": {"newbie@example.com"},
		"password": {"newbie-pass-1"}, "password2": {"newbie-pass-1"},
		"code": {code}, "_csrf": {env.cookie(cl, cookieCSRF)},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("注册应成功（303），实际 %d", resp.StatusCode)
	}
	u, err := env.st.GetUserByUsername("newbie")
	if err != nil || u == nil {
		t.Fatal("用户未创建")
	}
	email, verifiedAt, err := env.st.GetUserEmail(u.ID)
	if err != nil || email != "newbie@example.com" || verifiedAt == 0 {
		t.Errorf("邮箱未正确绑定: email=%q verified=%d err=%v", email, verifiedAt, err)
	}
	// 邮箱验证码是一次性的
	if c, _ := env.st.LatestEmailCode("newbie@example.com", store.EmailPurposeRegister); c != nil {
		t.Error("注册成功后验证码应被清除")
	}
}

// TestForgotPasswordByEmail 覆盖邮箱找回密码。
func TestForgotPasswordByEmail(t *testing.T) {
	env := newTestEnv(t)
	fake := &fakeMailer{}
	oldSend := sendMailFunc
	sendMailFunc = fake.send
	defer func() { sendMailFunc = oldSend }()

	u := env.addUser("alice", "old-pass-1234", "user")
	if err := env.st.SetUserEmail(u.ID, "alice@example.com", 1); err != nil {
		t.Fatalf("绑定邮箱失败: %v", err)
	}
	if err := env.st.UpdateSettings(map[string]string{
		"smtp_host": "smtp.example.com", "smtp_from": "noreply@example.com",
	}); err != nil {
		t.Fatalf("更新设置失败: %v", err)
	}
	env.web.invalidateSettings()

	cl := newClient(t)
	env.get(cl, "/forgot").Body.Close()
	resp := env.post(cl, "/forgot/send-code", url.Values{
		"email": {"alice@example.com"}, "_csrf": {env.cookie(cl, cookieCSRF)},
	})
	if b := body(t, resp); !strings.Contains(b, "验证码已发送") {
		t.Fatalf("找回密码发码失败：%s", firstLines(b, 3))
	}
	m := codeRe.FindStringSubmatch(fake.body)
	if m == nil {
		t.Fatal("邮件里没有验证码")
	}
	resp = env.post(cl, "/forgot", url.Values{
		"email": {"alice@example.com"}, "code": {m[1]},
		"password": {"brand-new-pass"}, "password2": {"brand-new-pass"},
		"_csrf": {env.cookie(cl, cookieCSRF)},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("重置密码应 303，实际 %d", resp.StatusCode)
	}
	// 新密码可登录，旧密码不可
	cl2 := newClient(t)
	if r := env.login(cl2, "alice", "brand-new-pass"); r.StatusCode != http.StatusSeeOther {
		t.Errorf("新密码应可登录，实际 %d", r.StatusCode)
	}
	cl3 := newClient(t)
	if r := env.login(cl3, "alice", "old-pass-1234"); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("旧密码应失效，实际 %d", r.StatusCode)
	}
}

// ---------- OIDC 用户映射 ----------

func TestResolveOIDCUser(t *testing.T) {
	env := newTestEnv(t)
	if err := env.st.UpdateSettings(map[string]string{
		"oidc_enabled": "1", "oidc_issuer": "https://idp.example.com",
		"oidc_client_id": "cid", "oidc_default_role": "user",
	}); err != nil {
		t.Fatalf("更新设置失败: %v", err)
	}
	env.web.invalidateSettings()
	st := env.web.mustSettings()
	const issuer = "https://idp.example.com"

	// 1) 自动建号（用户名由 preferred_username 推导）
	u1, err := env.web.resolveOIDCUser(st, issuer, &oidc.Claims{
		Subject: "sub-1", PreferredUsername: "Alice", Email: "alice@example.com", EmailVerified: true,
	})
	if err != nil {
		t.Fatalf("自动建号失败: %v", err)
	}
	if u1.Username != "alice" {
		t.Errorf("用户名应由 preferred_username 推导，实际 %q", u1.Username)
	}
	// 2) 同一 (issuer, sub) 再次登录 → 同一个用户
	u2, err := env.web.resolveOIDCUser(st, issuer, &oidc.Claims{Subject: "sub-1", PreferredUsername: "Alice"})
	if err != nil || u2.ID != u1.ID {
		t.Fatalf("同一 OIDC 身份应映射到同一用户: %v", err)
	}
	// 3) 邮箱已验证 + 本地已有同邮箱账号 → 关联而不是新建
	local := env.addUser("bob", "bob-pass-1234", "user")
	if err := env.st.SetUserEmail(local.ID, "bob@example.com", 1); err != nil {
		t.Fatalf("绑定邮箱失败: %v", err)
	}
	u3, err := env.web.resolveOIDCUser(st, issuer, &oidc.Claims{
		Subject: "sub-2", Email: "bob@example.com", EmailVerified: true,
	})
	if err != nil {
		t.Fatalf("按邮箱关联失败: %v", err)
	}
	if u3.ID != local.ID {
		t.Errorf("已验证邮箱应关联到本地账号 %d，实际 %d", local.ID, u3.ID)
	}
	// 4) 邮箱未验证 → 不允许接管同邮箱账号，只能新建
	u4, err := env.web.resolveOIDCUser(st, issuer, &oidc.Claims{
		Subject: "sub-3", PreferredUsername: "bob2", Email: "bob@example.com", EmailVerified: false,
	})
	if err != nil {
		t.Fatalf("未验证邮箱应可建号: %v", err)
	}
	if u4.ID == local.ID {
		t.Error("邮箱未验证时绝不能关联到已有账号（账号接管风险）")
	}
	// 5) 用户名冲突时自动加后缀
	u5, err := env.web.resolveOIDCUser(st, issuer, &oidc.Claims{
		Subject: "sub-4", PreferredUsername: "alice",
	})
	if err != nil {
		t.Fatalf("冲突用户名应可建号: %v", err)
	}
	if u5.Username == u1.Username {
		t.Errorf("用户名冲突时应加后缀，实际仍为 %q", u5.Username)
	}
}

func TestOIDCCallbackRejectsBadState(t *testing.T) {
	env := newTestEnv(t)
	if err := env.st.UpdateSettings(map[string]string{
		"oidc_enabled": "1", "oidc_issuer": "https://idp.example.com", "oidc_client_id": "cid",
	}); err != nil {
		t.Fatalf("更新设置失败: %v", err)
	}
	env.web.invalidateSettings()

	cl := newClient(t)
	cl.Jar.SetCookies(mustURL(env.srv.URL), []*http.Cookie{
		{Name: oidcStateCookie, Value: "state-abc.nonce-abc.verifier-abc"},
	})
	resp := env.get(cl, "/oidc/callback?state=WRONG&code=xyz")
	got := body(t, resp)
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("state 不匹配时必须拒绝")
	}
	if !strings.Contains(got, "state 校验失败") {
		t.Errorf("应提示 state 校验失败，实际响应未包含")
	}
}

// firstLines 取前 n 行，便于断言失败时输出精简信息。
func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
