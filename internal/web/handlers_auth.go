package web

import (
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"pagehut/internal/store"
)

// ---------- 登录限流（内存版，按 IP） ----------

type loginLimiter struct {
	mu    sync.Mutex
	fails map[string]*failInfo
}

type failInfo struct {
	count int
	until time.Time
}

var limiter = &loginLimiter{fails: map[string]*failInfo{}}

const (
	maxFails     = 8
	failCooldown = 15 * time.Minute
)

func (l *loginLimiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	fi, ok := l.fails[key]
	if !ok {
		return false
	}
	if time.Now().After(fi.until) {
		delete(l.fails, key)
		return false
	}
	return fi.count >= maxFails
}

func (l *loginLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fi, ok := l.fails[key]
	if !ok || time.Now().After(fi.until) {
		l.fails[key] = &failInfo{count: 1, until: time.Now().Add(failCooldown)}
		return
	}
	fi.count++
}

func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------- 登录 / 注册 / 登出 ----------

func (w *Web) loginPage(rw http.ResponseWriter, r *http.Request) {
	if userFrom(r) != nil {
		http.Redirect(rw, r, "/", http.StatusSeeOther)
		return
	}
	w.render(rw, r, http.StatusOK, "login", "登录", map[string]any{
		"Next":  r.URL.Query().Get("next"),
		"Error": "",
	})
}

func (w *Web) loginSubmit(rw http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if limiter.blocked(ip) {
		w.errorPage(rw, r, http.StatusTooManyRequests, "尝试次数过多，请 15 分钟后再试。")
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	next := safeNext(r.FormValue("next"))

	fail := func(msg string) {
		limiter.fail(ip)
		w.render(rw, r, http.StatusUnauthorized, "login", "登录", map[string]any{
			"Next": next, "Error": msg, "Username": username,
		})
	}

	u, err := w.st.GetUserByUsername(username)
	if err != nil {
		log.Printf("[web] 查询用户失败: %v", err)
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	if u == nil || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		fail("用户名或密码错误。")
		return
	}
	if u.Status != "active" {
		w.render(rw, r, http.StatusForbidden, "login", "登录", map[string]any{
			"Next": next, "Error": "该账号已被禁用，请联系管理员。", "Username": username,
		})
		return
	}

	token := randHex(32)
	if err := w.st.CreateSession(token, u.ID, time.Now().Add(sessionTTL).Unix()); err != nil {
		log.Printf("[web] 创建会话失败: %v", err)
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	setCookie(rw, cookieSession, token, int(sessionTTL/time.Second), true, isSecureRequest(r))
	limiter.reset(ip)
	w.st.Audit(&u.ID, u.Username, "user.login", "")
	http.Redirect(rw, r, next, http.StatusSeeOther)
}

func (w *Web) registerPage(rw http.ResponseWriter, r *http.Request) {
	if userFrom(r) != nil {
		http.Redirect(rw, r, "/", http.StatusSeeOther)
		return
	}
	st, err := w.st.GetSettings()
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	if st.RegistrationMode == "closed" {
		w.errorPage(rw, r, http.StatusForbidden, "本站未开放自助注册，请联系管理员创建账号。")
		return
	}
	w.render(rw, r, http.StatusOK, "register", "注册", map[string]any{
		"Mode":  st.RegistrationMode,
		"Error": "",
	})
}

func (w *Web) registerSubmit(rw http.ResponseWriter, r *http.Request) {
	st, err := w.st.GetSettings()
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	if st.RegistrationMode == "closed" {
		w.errorPage(rw, r, http.StatusForbidden, "本站未开放自助注册，请联系管理员创建账号。")
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	password2 := r.FormValue("password2")
	invite := strings.TrimSpace(r.FormValue("invite"))

	fail := func(msg string) {
		w.render(rw, r, http.StatusBadRequest, "register", "注册", map[string]any{
			"Mode": st.RegistrationMode, "Error": msg, "Username": username,
		})
	}

	if !validUsername(username) {
		fail("用户名需为 2-32 位字母、数字、下划线或短横线。")
		return
	}
	if len(password) < 8 {
		fail("密码至少 8 位。")
		return
	}
	if password != password2 {
		fail("两次输入的密码不一致。")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	uid, err := w.st.CreateUser(username, string(hash), "user")
	if err != nil {
		if store.IsUniqueErr(err) {
			fail("用户名已被占用。")
			return
		}
		log.Printf("[web] 创建用户失败: %v", err)
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}

	if st.RegistrationMode == "invite" {
		ok, err := w.st.ConsumeInvite(invite, uid)
		if err != nil || !ok {
			w.st.DeleteUser(uid) // 邀请码无效，回滚注册
			fail("邀请码无效或已被使用。")
			return
		}
	}

	token := randHex(32)
	if err := w.st.CreateSession(token, uid, time.Now().Add(sessionTTL).Unix()); err != nil {
		log.Printf("[web] 创建会话失败: %v", err)
	}
	setCookie(rw, cookieSession, token, int(sessionTTL/time.Second), true, isSecureRequest(r))
	w.st.Audit(&uid, username, "user.register", "注册模式: "+st.RegistrationMode)
	w.notifyAdmins("新用户注册", "用户 "+username+" 刚刚注册。")
	flash(rw, r, "注册成功，欢迎加入！")
	http.Redirect(rw, r, "/", http.StatusSeeOther)
}

func (w *Web) logoutSubmit(rw http.ResponseWriter, r *http.Request) {
	if u := userFrom(r); u != nil {
		w.st.Audit(&u.ID, u.Username, "user.logout", "")
	}
	if c, err := r.Cookie(cookieSession); err == nil {
		w.st.DeleteSession(c.Value)
	}
	setCookie(rw, cookieSession, "", -1, true, isSecureRequest(r))
	http.Redirect(rw, r, "/login", http.StatusSeeOther)
}

// ---------- 账号设置 ----------

func (w *Web) accountPage(rw http.ResponseWriter, r *http.Request) {
	u, ok := w.auth(rw, r)
	if !ok {
		return
	}
	w.render(rw, r, http.StatusOK, "account", "账号设置", map[string]any{
		"User":  u,
		"Error": "",
	})
}

func (w *Web) accountPasswordSubmit(rw http.ResponseWriter, r *http.Request) {
	u, ok := w.auth(rw, r)
	if !ok {
		return
	}
	current := r.FormValue("current")
	newPass := r.FormValue("new")
	confirm := r.FormValue("confirm")

	fail := func(msg string) {
		w.render(rw, r, http.StatusBadRequest, "account", "账号设置", map[string]any{
			"User": u, "Error": msg,
		})
	}

	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(current)) != nil {
		fail("当前密码不正确。")
		return
	}
	if len(newPass) < 8 {
		fail("新密码至少 8 位。")
		return
	}
	if newPass != confirm {
		fail("两次输入的新密码不一致。")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPass), bcrypt.DefaultCost)
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	if err := w.st.UpdateUserPassword(u.ID, string(hash)); err != nil {
		log.Printf("[web] 更新密码失败: %v", err)
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	if c, err := r.Cookie(cookieSession); err == nil {
		w.st.DeleteOtherSessions(u.ID, c.Value)
	}
	w.st.Audit(&u.ID, u.Username, "user.password", "修改密码")
	flash(rw, r, "密码已修改。")
	http.Redirect(rw, r, "/account", http.StatusSeeOther)
}

// ---------- 通知 ----------

func (w *Web) notificationsPage(rw http.ResponseWriter, r *http.Request) {
	u, ok := w.auth(rw, r)
	if !ok {
		return
	}
	list, err := w.st.ListNotifications(u.ID)
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	w.render(rw, r, http.StatusOK, "notifications", "通知", map[string]any{
		"List": list,
	})
}

func (w *Web) notificationsRead(rw http.ResponseWriter, r *http.Request) {
	u, ok := w.auth(rw, r)
	if !ok {
		return
	}
	w.st.MarkAllNotificationsRead(u.ID)
	http.Redirect(rw, r, "/notifications", http.StatusSeeOther)
}

// ---------- 内部辅助 ----------

// notifyAdmins 给所有可用管理员发通知。
func (w *Web) notifyAdmins(title, body string) {
	users, err := w.st.ListUsers()
	if err != nil {
		return
	}
	for _, u := range users {
		if u.IsAdmin() && u.Status == "active" {
			w.st.Notify(u.ID, title, body)
		}
	}
}

// notifyReviewers 给所有可用审核员与管理员发通知。
func (w *Web) notifyReviewers(title, body string) {
	users, err := w.st.ListUsers()
	if err != nil {
		return
	}
	for _, u := range users {
		if u.IsReviewer() && u.Status == "active" {
			w.st.Notify(u.ID, title, body)
		}
	}
}
