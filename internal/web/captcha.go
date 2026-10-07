package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"pagehut/internal/store"
)

// 人机验证形态（对应 settings.captcha_provider）。
const (
	captchaOff       = "off"
	captchaTurnstile = "turnstile"
	captchaImage     = "image"
	captchaSlider    = "slider"
)

const (
	// captchaTTL 一次挑战的有效期。
	captchaTTL = 10 * time.Minute
	// captchaMaxTries 同一挑战最多可校验几次（防止暴力试答案）。
	captchaMaxTries = 5
	// captchaImageLen 图形验证码位数。
	captchaImageLen = 5
	// sliderTolerance 滑动验证允许的像素误差。
	sliderTolerance = 8
	// captchaCookie 保存当前挑战 id 的 cookie 名。
	captchaCookie = "pp_captcha"
	// turnstileScriptURL 前端 widget 脚本来源（用于 CSP 放行）。
	turnstileScriptURL = "https://challenges.cloudflare.com"
)

// turnstileVerifyURL 是 Cloudflare 官方 siteverify 端点。
// 声明为变量是为了让测试可以替换成假服务端。
var turnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

// captchaChallenge 是服务端保存的一次挑战。
// 图形码用 answer 存答案；滑动码用 answer 存目标横坐标。
type captchaChallenge struct {
	answer    string
	expiresAt time.Time
	tries     int
}

// captchaStore 是进程内的挑战存储。PageHut 是单实例部署（SQLite），
// 重启后未完成的验证自然失效，用户刷新页面即可，不需要持久化。
type captchaStore struct {
	mu    sync.Mutex
	items map[string]*captchaChallenge
	last  time.Time
}

var captchas = &captchaStore{items: map[string]*captchaChallenge{}}

func (s *captchaStore) put(answer string) string {
	id := randHex(16)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.items[id] = &captchaChallenge{answer: answer, expiresAt: time.Now().Add(captchaTTL)}
	return id
}

// take 取出挑战并计数；返回 (答案, 是否仍有效)。校验失败时挑战保留，
// 超过尝试上限或已过期则删除。
func (s *captchaStore) take(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[id]
	if !ok {
		return "", false
	}
	if time.Now().After(item.expiresAt) {
		delete(s.items, id)
		return "", false
	}
	item.tries++
	if item.tries > captchaMaxTries {
		delete(s.items, id)
		return "", false
	}
	return item.answer, true
}

func (s *captchaStore) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, id)
}

// sweepLocked 定期清理过期挑战，避免字典无限增长。
func (s *captchaStore) sweepLocked() {
	now := time.Now()
	if now.Sub(s.last) < time.Minute {
		return
	}
	s.last = now
	for k, v := range s.items {
		if now.After(v.expiresAt) {
			delete(s.items, k)
		}
	}
}

// captchaView 是渲染到页面的验证码信息。
type captchaView struct {
	Provider string // turnstile | image | slider
	SiteKey  string // Turnstile sitekey
	Action   string // Turnstile action（同时用于日志/排查）
}

// captchaViewFor 构造页面所需的验证码信息；返回 nil 表示该页不需要人机验证。
func (w *Web) captchaViewFor(st *store.Settings, action string) *captchaView {
	if action == "" || !st.CaptchaEnabled() {
		return nil
	}
	v := &captchaView{Provider: st.CaptchaProvider, Action: action}
	if st.CaptchaProvider == captchaTurnstile {
		v.SiteKey = st.TurnstileSiteKey
	}
	return v
}

// captchaActionFor 按页面模板名决定需要哪种 action：
// 注册、绑定邮箱、重置密码始终需要；登录页按开关决定。
func captchaActionFor(tpl string, st *store.Settings) string {
	if !st.CaptchaEnabled() {
		return ""
	}
	switch tpl {
	case "login":
		if st.CaptchaOnLogin {
			return "login"
		}
	case "register":
		return "register"
	case "account":
		return "bind_email"
	case "forgot":
		return "reset"
	}
	return ""
}

// verifyCaptcha 校验一次人机验证。返回 (是否通过, 给用户看的错误提示)。
// 未配置人机验证时直接通过。
func (w *Web) verifyCaptcha(r *http.Request, st *store.Settings, action string) (bool, string) {
	if !st.CaptchaEnabled() {
		return true, ""
	}
	switch st.CaptchaProvider {
	case captchaTurnstile:
		return w.verifyTurnstile(r, st, action)
	case captchaImage:
		return w.verifyImageCaptcha(r)
	case captchaSlider:
		return w.verifySliderCaptcha(r)
	}
	return true, ""
}

// verifyImageCaptcha 校验图形验证码（表单字段 captcha_code）。
func (w *Web) verifyImageCaptcha(r *http.Request) (bool, string) {
	id := captchaIDFrom(r)
	if id == "" {
		return false, "验证码已过期，请点击图片刷新后重试。"
	}
	answer, ok := captchas.take(id)
	if !ok {
		return false, "验证码已过期或尝试次数过多，请点击图片刷新后重试。"
	}
	got := strings.TrimSpace(r.FormValue("captcha_code"))
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(answer)) != 1 {
		return false, "验证码不正确。"
	}
	captchas.remove(id)
	return true, ""
}

// verifySliderCaptcha 校验滑动验证码（表单字段 captcha_x，单位像素）。
func (w *Web) verifySliderCaptcha(r *http.Request) (bool, string) {
	id := captchaIDFrom(r)
	if id == "" {
		return false, "验证已过期，请刷新页面重试。"
	}
	answer, ok := captchas.take(id)
	if !ok {
		return false, "验证已过期或尝试次数过多，请刷新页面重试。"
	}
	target, err := strconv.Atoi(answer)
	if err != nil {
		return false, "验证状态异常，请刷新页面重试。"
	}
	got, err := strconv.Atoi(strings.TrimSpace(r.FormValue("captcha_x")))
	if err != nil {
		return false, "请拖动滑块完成验证。"
	}
	diff := got - target
	if diff < 0 {
		diff = -diff
	}
	if diff > sliderTolerance {
		return false, "拼图位置不正确，请再试一次。"
	}
	captchas.remove(id)
	return true, ""
}

// verifyTurnstile 按 Cloudflare 官方约定校验 Turnstile 令牌：
// 服务端调用 siteverify，要求 success、action 与预期一致、hostname 在白名单内，
// 任何异常都按不通过处理（fail closed）。令牌是一次性的。
func (w *Web) verifyTurnstile(r *http.Request, st *store.Settings, action string) (bool, string) {
	token := strings.TrimSpace(r.FormValue("cf-turnstile-response"))
	if token == "" || len(token) > 2048 {
		return false, "请先完成人机验证。"
	}

	form := url.Values{
		"secret":   {st.TurnstileSecret},
		"response": {token},
		"remoteip": {w.clientIP(r)},
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, turnstileVerifyURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		log.Printf("[web] 构造 siteverify 请求失败: %v", err)
		return false, "人机验证失败，请重试。"
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := turnstileClient.Do(req)
	if err != nil {
		log.Printf("[web] siteverify 请求失败: %v", err)
		return false, "人机验证服务暂时不可用，请稍后重试。"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[web] siteverify 返回 %d", resp.StatusCode)
		return false, "人机验证服务暂时不可用，请稍后重试。"
	}

	var out struct {
		Success    bool     `json:"success"`
		Action     string   `json:"action"`
		Hostname   string   `json:"hostname"`
		ErrorCodes []string `json:"error-codes"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		log.Printf("[web] siteverify 响应解析失败: %v", err)
		return false, "人机验证服务返回异常，请稍后重试。"
	}
	if !out.Success {
		log.Printf("[web] Turnstile 校验未通过: %v", out.ErrorCodes)
		return false, "人机验证未通过，请重试。"
	}
	if out.Action != action {
		log.Printf("[web] Turnstile action 不匹配: got=%q want=%q", out.Action, action)
		return false, "人机验证校验失败，请刷新页面重试。"
	}
	if !w.turnstileHostAllowed(r, st, out.Hostname) {
		log.Printf("[web] Turnstile hostname 不在允许列表: %q", out.Hostname)
		return false, "人机验证校验失败，请刷新页面重试。"
	}
	return true, ""
}

// turnstileHostAllowed 校验 siteverify 返回的 hostname：
// 默认只允许当前请求的 Host（widget 就渲染在面板域名上），
// 如需额外域名可在后台填写 turnstile_hostnames。
func (w *Web) turnstileHostAllowed(r *http.Request, st *store.Settings, hostname string) bool {
	got := strings.ToLower(strings.TrimSpace(hostname))
	if got == "" {
		return false
	}
	if got == hostOnly(r.Host) {
		return true
	}
	for _, item := range strings.Split(st.TurnstileHostnames, ",") {
		if h := strings.ToLower(strings.TrimSpace(item)); h != "" && h == got {
			return true
		}
	}
	return false
}

// captchaIDFrom 从 cookie 取当前挑战 id。
func captchaIDFrom(r *http.Request) string {
	c, err := r.Cookie(captchaCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// turnstileClient 是校验 Turnstile 用的 HTTP 客户端（带超时，禁跟随跳转）。
var turnstileClient = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}
