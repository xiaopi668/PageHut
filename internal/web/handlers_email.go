package web

import (
	"context"
	"crypto/subtle"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"pagehut/internal/mail"
	"pagehut/internal/store"
)

const (
	// emailCodeTTL 邮箱验证码有效期。
	emailCodeTTL = 10 * time.Minute
	// emailCodeMaxTries 同一验证码最多尝试次数。
	emailCodeMaxTries = 5
	// emailSendInterval 同一邮箱最短重发间隔。
	emailSendInterval = 60 * time.Second
	// emailSendPerHour 同一邮箱每小时发信上限。
	emailSendPerHour = 5
)

// emailLimiter 按邮箱限制发信频率（进程内，单实例部署足够）。
type emailLimiter struct {
	mu    sync.Mutex
	items map[string]*emailSendInfo
}

type emailSendInfo struct {
	last      time.Time
	hourStart time.Time
	count     int
}

var emailSends = &emailLimiter{items: map[string]*emailSendInfo{}}

// allow 判断现在能否给该邮箱发信，并返回拒绝原因。
func (l *emailLimiter) allow(email string) (bool, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	info, ok := l.items[email]
	if !ok {
		l.items[email] = &emailSendInfo{last: now, hourStart: now, count: 1}
		l.sweepLocked(now)
		return true, ""
	}
	if now.Sub(info.last) < emailSendInterval {
		wait := int((emailSendInterval - now.Sub(info.last)).Seconds())
		if wait < 1 {
			wait = 1
		}
		return false, "发送太频繁，请 " + strconv.Itoa(wait) + " 秒后再试。"
	}
	if now.Sub(info.hourStart) > time.Hour {
		info.hourStart = now
		info.count = 0
	}
	if info.count >= emailSendPerHour {
		return false, "该邮箱一小时内发送次数过多，请稍后再试。"
	}
	info.last = now
	info.count++
	return true, ""
}

// sweepLocked 清理长期未使用的记录。
func (l *emailLimiter) sweepLocked(now time.Time) {
	if len(l.items) < 512 {
		return
	}
	for k, v := range l.items {
		if now.Sub(v.last) > 2*time.Hour {
			delete(l.items, k)
		}
	}
}

// mailConfig 由设置构造 SMTP 配置。
func (w *Web) mailConfig(st *store.Settings) mail.Config {
	return mail.Config{
		Host: st.SMTPHost,
		Port: st.SMTPPort,
		User: st.SMTPUser,
		Pass: st.SMTPPass,
		From: st.SMTPFrom,
		TLS:  st.SMTPTLS,
	}
}

// sendEmailCode 生成验证码、落库并同步发信。返回 (是否成功, 提示语)。
func (w *Web) sendEmailCode(st *store.Settings, email, purpose string, userID *int64) (bool, string) {
	if !st.EmailEnabled() {
		return false, "本站尚未配置邮件服务，无法发送验证码。"
	}
	if ok, msg := emailSends.allow(email); !ok {
		return false, msg
	}
	code := randDigits(6)
	hash := store.HashEmailCode(email, purpose, code)
	if err := w.st.CreateEmailCode(email, purpose, userID, hash, time.Now().Add(emailCodeTTL).Unix()); err != nil {
		log.Printf("[web] 写入邮箱验证码失败: %v", err)
		return false, "验证码发送失败，请稍后重试。"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	subject, body := emailCodeMessage(st, purpose, code)
	if err := sendMailFunc(ctx, w.mailConfig(st), email, subject, body); err != nil {
		log.Printf("[web] 发送验证码邮件失败: %v", err)
		_ = w.st.DeleteEmailCodes(email, purpose)
		return false, "验证码邮件发送失败，请检查后台 SMTP 配置。"
	}
	return true, ""
}

// sendMailFunc 是可替换的发信实现（测试里替换为假发送器）。
var sendMailFunc = mail.Send

// emailCodeMessage 生成验证码邮件内容。
func emailCodeMessage(st *store.Settings, purpose, code string) (string, string) {
	action := "完成验证"
	switch purpose {
	case store.EmailPurposeRegister:
		action = "完成注册"
	case store.EmailPurposeBind:
		action = "绑定邮箱"
	case store.EmailPurposeReset:
		action = "重置密码"
	}
	subject := "【" + st.SiteName + "】邮箱验证码"
	body := "你的验证码是：" + code + "\n\n" +
		"用于" + action + "，10 分钟内有效。\n" +
		"如果这不是你本人的操作，请忽略本邮件。\n\n" +
		"-- " + st.SiteName + "（PageHut）\n"
	return subject, body
}

// verifyEmailCode 校验邮箱验证码；通过后立即作废该验证码。
func (w *Web) verifyEmailCode(email, purpose, code string) (bool, string) {
	code = strings.TrimSpace(code)
	if code == "" {
		return false, "请填写邮箱验证码。"
	}
	c, err := w.st.LatestEmailCode(email, purpose)
	if err != nil {
		log.Printf("[web] 读取邮箱验证码失败: %v", err)
		return false, "服务器错误，请稍后重试。"
	}
	if c == nil {
		return false, "请先获取邮箱验证码。"
	}
	if time.Now().Unix() > c.ExpiresAt {
		return false, "验证码已过期，请重新获取。"
	}
	if c.Attempts >= emailCodeMaxTries {
		return false, "验证码尝试次数过多，请重新获取。"
	}
	want := store.HashEmailCode(email, purpose, code)
	if subtle.ConstantTimeCompare([]byte(want), []byte(c.CodeHash)) != 1 {
		_ = w.st.IncrEmailCodeAttempts(c.ID)
		return false, "邮箱验证码不正确。"
	}
	_ = w.st.DeleteEmailCodes(email, purpose)
	return true, ""
}

// validEmail 做一个基本的邮箱格式校验（真正的验证靠验证码）。
func validEmail(s string) bool {
	if len(s) < 5 || len(s) > 254 {
		return false
	}
	at := strings.LastIndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return false
	}
	domain := s[at+1:]
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return false
	}
	return !strings.ContainsAny(s, " \t\r\n<>,;\"")
}

// ---------- 注册：发送验证码 ----------

func (w *Web) registerSendCode(rw http.ResponseWriter, r *http.Request) {
	st, err := w.settings()
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	if st.RegistrationMode == "closed" {
		w.errorPage(rw, r, http.StatusForbidden, "本站未开放自助注册，请联系管理员创建账号。")
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	email := store.NormalizeEmail(r.FormValue("email"))
	invite := strings.TrimSpace(r.FormValue("invite"))

	render := func(msg string, ok bool) {
		data := map[string]any{
			"Mode": st.RegistrationMode, "Error": "", "Notice": "",
			"Username": username, "Email": email, "Invite": invite,
		}
		if ok {
			data["Notice"] = msg
		} else {
			data["Error"] = msg
		}
		w.render(rw, r, http.StatusOK, "register", "注册", data)
	}

	if !validEmail(email) {
		render("请先填写正确的邮箱地址。", false)
		return
	}
	if taken, err := w.st.EmailTaken(email); err != nil {
		render("服务器错误，请稍后重试。", false)
		return
	} else if taken {
		render("该邮箱已被使用。", false)
		return
	}
	if ok, msg := w.verifyCaptcha(r, st, "register"); !ok {
		render(msg, false)
		return
	}
	ok, msg := w.sendEmailCode(st, email, store.EmailPurposeRegister, nil)
	if !ok {
		render(msg, false)
		return
	}
	render("验证码已发送到 "+email+"，10 分钟内有效。", true)
}

// ---------- 找回密码 ----------

func (w *Web) forgotPage(rw http.ResponseWriter, r *http.Request) {
	if userFrom(r) != nil {
		http.Redirect(rw, r, "/", http.StatusSeeOther)
		return
	}
	st := w.mustSettings()
	w.render(rw, r, http.StatusOK, "forgot", "找回密码", map[string]any{
		"Error": "", "Email": "", "Enabled": st.EmailEnabled(),
	})
}

func (w *Web) forgotSendCode(rw http.ResponseWriter, r *http.Request) {
	st, err := w.settings()
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	email := store.NormalizeEmail(r.FormValue("email"))
	render := func(msg string) {
		w.render(rw, r, http.StatusOK, "forgot", "找回密码", map[string]any{
			"Error": msg, "Email": email, "Enabled": st.EmailEnabled(),
		})
	}
	if !st.EmailEnabled() {
		render("本站未配置邮件服务，无法自助找回密码，请联系管理员。")
		return
	}
	if !validEmail(email) {
		render("请先填写正确的邮箱地址。")
		return
	}
	if ok, msg := w.verifyCaptcha(r, st, "reset"); !ok {
		render(msg)
		return
	}
	// 不暴露邮箱是否存在：无论是否注册都提示已发送
	user, err := w.st.GetUserByEmail(email)
	if err != nil {
		render("服务器错误，请稍后重试。")
		return
	}
	if user != nil && user.Status == "active" {
		if ok, msg := w.sendEmailCode(st, email, store.EmailPurposeReset, &user.ID); !ok {
			render(msg)
			return
		}
	}
	render("如果该邮箱已注册，验证码已发送，10 分钟内有效。")
}

func (w *Web) forgotSubmit(rw http.ResponseWriter, r *http.Request) {
	st, err := w.settings()
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	email := store.NormalizeEmail(r.FormValue("email"))
	code := r.FormValue("code")
	newPass := r.FormValue("password")
	confirm := r.FormValue("password2")

	render := func(msg string) {
		w.render(rw, r, http.StatusBadRequest, "forgot", "找回密码", map[string]any{
			"Error": msg, "Email": email, "Enabled": st.EmailEnabled(),
		})
	}
	if !st.EmailEnabled() {
		render("本站未配置邮件服务，无法自助找回密码。")
		return
	}
	if len(newPass) < 8 {
		render("新密码至少 8 位。")
		return
	}
	if len(newPass) > maxPasswordLen {
		render("新密码过长：bcrypt 上限为 72 字节（中文约 24 个字）。")
		return
	}
	if newPass != confirm {
		render("两次输入的密码不一致。")
		return
	}
	if ok, msg := w.verifyEmailCode(email, store.EmailPurposeReset, code); !ok {
		render(msg)
		return
	}
	user, err := w.st.GetUserByEmail(email)
	if err != nil || user == nil {
		render("该邮箱尚未注册。")
		return
	}
	hash, err := bcryptHash(newPass)
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	if err := w.st.UpdateUserPassword(user.ID, hash); err != nil {
		log.Printf("[web] 重置密码失败: %v", err)
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	_ = w.st.DeleteUserSessions(user.ID) // 重置后强制所有设备重新登录
	w.st.Audit(&user.ID, user.Username, "user.password", "邮箱验证码重置密码")
	flash(rw, r, "密码已重置，请用新密码登录。")
	http.Redirect(rw, r, "/login", http.StatusSeeOther)
}

// ---------- 账号设置：绑定 / 换绑邮箱 ----------

func (w *Web) accountEmailSendCode(rw http.ResponseWriter, r *http.Request) {
	u, ok := w.auth(rw, r)
	if !ok {
		return
	}
	st := w.mustSettings()
	email := store.NormalizeEmail(r.FormValue("email"))
	current, _, _ := w.st.GetUserEmail(u.ID)

	render := func(msg string, bad bool) {
		code := http.StatusOK
		if bad {
			code = http.StatusBadRequest
		}
		w.render(rw, r, code, "account", "账号设置", map[string]any{
			"User": u, "Error": msg, "Email": email, "CurrentEmail": current,
			"EmailEnabled": st.EmailEnabled(),
		})
	}
	if !st.EmailEnabled() {
		render("本站未配置邮件服务，无法绑定邮箱。", true)
		return
	}
	if !validEmail(email) {
		render("请先填写正确的邮箱地址。", true)
		return
	}
	if taken, err := w.st.EmailTaken(email); err != nil {
		render("服务器错误，请稍后重试。", true)
		return
	} else if taken {
		render("该邮箱已被其他账号使用。", true)
		return
	}
	if ok, msg := w.verifyCaptcha(r, st, "bind_email"); !ok {
		render(msg, true)
		return
	}
	if ok, msg := w.sendEmailCode(st, email, store.EmailPurposeBind, &u.ID); !ok {
		render(msg, true)
		return
	}
	render("验证码已发送到 "+email+"，10 分钟内有效。", false)
}

func (w *Web) accountEmailSubmit(rw http.ResponseWriter, r *http.Request) {
	u, ok := w.auth(rw, r)
	if !ok {
		return
	}
	st := w.mustSettings()
	email := store.NormalizeEmail(r.FormValue("email"))
	code := r.FormValue("code")
	current, _, _ := w.st.GetUserEmail(u.ID)

	render := func(msg string, bad bool) {
		status := http.StatusOK
		if bad {
			status = http.StatusBadRequest
		}
		w.render(rw, r, status, "account", "账号设置", map[string]any{
			"User": u, "Error": msg, "Email": email, "CurrentEmail": current,
			"EmailEnabled": st.EmailEnabled(),
		})
	}
	if !st.EmailEnabled() {
		render("本站未配置邮件服务，无法绑定邮箱。", true)
		return
	}
	if !validEmail(email) {
		render("请先填写正确的邮箱地址。", true)
		return
	}
	if taken, err := w.st.EmailTaken(email); err != nil {
		render("服务器错误，请稍后重试。", true)
		return
	} else if taken && email != current {
		render("该邮箱已被其他账号使用。", true)
		return
	}
	if ok, msg := w.verifyEmailCode(email, store.EmailPurposeBind, code); !ok {
		render(msg, true)
		return
	}
	if err := w.st.SetUserEmail(u.ID, email, time.Now().Unix()); err != nil {
		log.Printf("[web] 绑定邮箱失败: %v", err)
		render("保存失败，请稍后重试。", true)
		return
	}
	w.st.Audit(&u.ID, u.Username, "user.email", email)
	flash(rw, r, "邮箱已绑定："+email)
	http.Redirect(rw, r, "/account", http.StatusSeeOther)
}
