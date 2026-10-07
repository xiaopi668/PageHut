package web

import (
	"context"
	"crypto/subtle"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"pagehut/internal/oidc"
	"pagehut/internal/store"
)

// oidcStateCookie 保存 OIDC 流程的 state/nonce/verifier（一次性）。
const oidcStateCookie = "pp_oidc"

// oidcFlowTTL 一次 OIDC 登录流程的有效期。
const oidcFlowTTL = 10 * time.Minute

// oidcRedirectURL 由当前请求推导回调地址（兼容反代）。
func (w *Web) oidcRedirectURL(r *http.Request) string {
	scheme := "http"
	if isSecureRequest(r) {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = hostOnly(r.Host)
	}
	return scheme + "://" + host + "/oidc/callback"
}

// oidcProvider 按当前设置构造 OIDC 客户端（每次登录重新 discovery，
// 便于管理员改配置后立即生效；discovery 结果不做进程级缓存）。
func (w *Web) oidcProvider(ctx context.Context, st *store.Settings, r *http.Request) (*oidc.Provider, error) {
	return oidc.Discover(ctx, oidc.Config{
		Issuer:       st.OIDCIssuer,
		ClientID:     st.OIDCClientID,
		ClientSecret: st.OIDCClientSecret,
		Scopes:       strings.Fields(st.OIDCScopes),
		RedirectURL:  w.oidcRedirectURL(r),
	})
}

// oidcLogin 跳转到 IdP 授权端点。
func (w *Web) oidcLogin(rw http.ResponseWriter, r *http.Request) {
	st := w.mustSettings()
	if !st.OIDCReady() {
		w.errorPage(rw, r, http.StatusNotFound, "本站未启用 OIDC 登录")
		return
	}
	state, err := oidc.NewState()
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	nonce, err := oidc.NewState()
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	verifier, err := oidc.NewCodeVerifier()
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	provider, err := w.oidcProvider(ctx, st, r)
	if err != nil {
		log.Printf("[web] OIDC discovery 失败: %v", err)
		w.errorPage(rw, r, http.StatusBadGateway, "无法连接身份提供方，请检查后台 OIDC 配置。")
		return
	}

	// state/nonce/verifier 放在一次性 cookie 里，回调时比对（SameSite=Lax
	// 允许 IdP 通过顶层跳转带回来）。
	payload := state + "." + nonce + "." + verifier
	setCookie(rw, oidcStateCookie, payload, int(oidcFlowTTL/time.Second), true, isSecureRequest(r))
	http.Redirect(rw, r, provider.AuthCodeURL(state, nonce, verifier), http.StatusSeeOther)
}

// oidcCallback 处理 IdP 回调：校验 state → 换 token → 校验 ID Token →
// 关联或创建本地用户 → 建立会话。
func (w *Web) oidcCallback(rw http.ResponseWriter, r *http.Request) {
	st := w.mustSettings()
	if !st.OIDCReady() {
		w.errorPage(rw, r, http.StatusNotFound, "本站未启用 OIDC 登录")
		return
	}
	// 一次性使用：无论成功失败都清掉流程 cookie
	flowCookie, _ := r.Cookie(oidcStateCookie)
	setCookie(rw, oidcStateCookie, "", -1, true, isSecureRequest(r))

	fail := func(msg string) {
		w.render(rw, r, http.StatusBadRequest, "login", "登录", map[string]any{
			"Next": "/", "Error": msg,
			"OIDCReady": st.OIDCReady(), "OIDCLabel": st.OIDCButtonLabel,
			"EmailEnabled": st.EmailEnabled(),
		})
	}

	if e := r.URL.Query().Get("error"); e != "" {
		fail("身份提供方返回错误：" + e)
		return
	}
	if flowCookie == nil || flowCookie.Value == "" {
		fail("登录会话已过期，请重新发起 OIDC 登录。")
		return
	}
	parts := strings.SplitN(flowCookie.Value, ".", 3)
	if len(parts) != 3 {
		fail("登录状态异常，请重新发起 OIDC 登录。")
		return
	}
	state, nonce, verifier := parts[0], parts[1], parts[2]
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(state)) != 1 {
		fail("state 校验失败，请重新发起 OIDC 登录。")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		fail("身份提供方未返回授权码。")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	provider, err := w.oidcProvider(ctx, st, r)
	if err != nil {
		log.Printf("[web] OIDC discovery 失败: %v", err)
		fail("无法连接身份提供方，请稍后重试。")
		return
	}
	claims, err := provider.Exchange(ctx, code, verifier, nonce)
	if err != nil {
		log.Printf("[web] OIDC 换取令牌失败: %v", err)
		fail("OIDC 登录校验失败：" + err.Error())
		return
	}

	u, err := w.resolveOIDCUser(st, provider.Issuer(), claims)
	if err != nil {
		log.Printf("[web] OIDC 关联用户失败: %v", err)
		fail(err.Error())
		return
	}
	if u.Status != "active" {
		fail("该账号已被禁用，请联系管理员。")
		return
	}

	token := randHex(32)
	if err := w.st.CreateSession(token, u.ID, time.Now().Add(sessionTTL).Unix()); err != nil {
		log.Printf("[web] 创建会话失败: %v", err)
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	setCookie(rw, cookieSession, token, int(sessionTTL/time.Second), true, isSecureRequest(r))
	w.st.Audit(&u.ID, u.Username, "user.login", "oidc:"+provider.Issuer())
	http.Redirect(rw, r, "/", http.StatusSeeOther)
}

// resolveOIDCUser 把 OIDC 身份映射到本地用户：
//  1. (issuer, subject) 已绑定 → 直接登录；
//  2. 邮箱已验证且本地已有同邮箱账号 → 建立绑定并登录；
//  3. 否则自动建号（用户名由 preferred_username / 邮箱前缀推导，必要时加后缀）。
func (w *Web) resolveOIDCUser(st *store.Settings, issuer string, claims *oidc.Claims) (*store.User, error) {
	if claims.Subject == "" {
		return nil, errOIDC("身份提供方未返回 sub。")
	}
	if it, err := w.st.GetOIDCIdentity(issuer, claims.Subject); err != nil {
		return nil, errOIDC("服务器错误，请稍后重试。")
	} else if it != nil {
		u, err := w.st.GetUserByID(it.UserID)
		if err != nil {
			return nil, errOIDC("服务器错误，请稍后重试。")
		}
		if u == nil {
			return nil, errOIDC("该 OIDC 身份绑定的账号已不存在。")
		}
		return u, nil
	}

	email := store.NormalizeEmail(claims.Email)
	// 按邮箱关联本地账号：只有 IdP 明确声明邮箱已验证时才允许，
	// 否则任何人都能用一个「未验证的同名邮箱」接管本地账号。
	if email != "" && claims.EmailVerified {
		if u, err := w.st.GetUserByEmail(email); err != nil {
			return nil, errOIDC("服务器错误，请稍后重试。")
		} else if u != nil {
			if err := w.st.LinkOIDCIdentity(issuer, claims.Subject, u.ID, email); err != nil {
				return nil, errOIDC("服务器错误，请稍后重试。")
			}
			return u, nil
		}
	}

	if !st.OIDCEnabled {
		return nil, errOIDC("本站未启用 OIDC 登录。")
	}
	// 自动建号
	username := oidcUsername(claims)
	uid, err := w.st.CreateUser(username, "", st.OIDCDefaultRole)
	if err != nil {
		if store.IsUniqueErr(err) {
			username = uniqueUsername(w.st, username)
			uid, err = w.st.CreateUser(username, "", st.OIDCDefaultRole)
		}
		if err != nil {
			log.Printf("[web] OIDC 自动建号失败: %v", err)
			return nil, errOIDC("自动创建账号失败，请联系管理员。")
		}
	}
	if err := w.st.LinkOIDCIdentity(issuer, claims.Subject, uid, email); err != nil {
		return nil, errOIDC("服务器错误，请稍后重试。")
	}
	if email != "" && claims.EmailVerified {
		if err := w.st.SetUserEmail(uid, email, time.Now().Unix()); err != nil {
			log.Printf("[web] OIDC 写入邮箱失败: %v", err)
		}
	}
	u, err := w.st.GetUserByID(uid)
	if err != nil || u == nil {
		return nil, errOIDC("服务器错误，请稍后重试。")
	}
	w.st.Audit(&u.ID, u.Username, "user.register", "oidc:"+issuer)
	w.notifyAdmins("新用户注册", "用户 "+u.Username+" 通过 OIDC 注册。")
	return u, nil
}

// oidcUsername 由 preferred_username / 邮箱前缀推导合法用户名。
func oidcUsername(claims *oidc.Claims) string {
	candidates := []string{claims.PreferredUsername}
	if at := strings.IndexByte(claims.Email, '@'); at > 0 {
		candidates = append(candidates, claims.Email[:at])
	}
	candidates = append(candidates, claims.Subject)
	for _, c := range candidates {
		if u := sanitizeUsername(c); u != "" {
			return u
		}
	}
	return "oidc-user"
}

// sanitizeUsername 把任意字符串规整成合法用户名（2-32 位字母数字下划线短横线）。
func sanitizeUsername(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
			b.WriteRune(c)
		case c == '.' || c == '+' || c == ' ':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	for len(out) < 2 {
		out += "-u"
	}
	if len(out) > 32 {
		out = out[:32]
	}
	if !validUsername(out) {
		return ""
	}
	return out
}

// uniqueUsername 在用户名冲突时追加数字后缀。
func uniqueUsername(st *store.Store, base string) string {
	for i := 2; i < 1000; i++ {
		suffix := "-" + strconv.Itoa(i)
		cand := base
		if len(cand)+len(suffix) > 32 {
			cand = cand[:32-len(suffix)]
		}
		cand += suffix
		if u, err := st.GetUserByUsername(cand); err == nil && u == nil {
			return cand
		}
	}
	return base + "-" + randHex(3)
}

// errOIDC 是给用户看的 OIDC 错误。
type oidcError string

func (e oidcError) Error() string { return string(e) }

func errOIDC(msg string) error { return oidcError(msg) }
