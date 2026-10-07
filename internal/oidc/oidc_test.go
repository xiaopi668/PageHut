package oidc

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testClientID     = "test-client"
	testClientSecret = "test-secret"
	testRedirectURL  = "https://dash.example.com/oidc/callback"
)

var (
	testKeyOnce sync.Once
	testKeys    [2]*rsa.PrivateKey
	testKeyErr  error
)

// testRSAKey 返回第 i 把测试用 RSA 私钥；进程内只生成一次，避免测试变慢。
func testRSAKey(t *testing.T, i int) *rsa.PrivateKey {
	t.Helper()
	testKeyOnce.Do(func() {
		for j := range testKeys {
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				testKeyErr = err
				return
			}
			testKeys[j] = key
		}
	})
	if testKeyErr != nil {
		t.Fatalf("生成测试 RSA 私钥失败: %v", testKeyErr)
	}
	return testKeys[i]
}

// publishedKey 是假 IdP 在 JWKS 中发布的一把公钥。
// kty / use / alg 为空时分别按 RSA / sig / RS256 输出。
type publishedKey struct {
	kid string
	pub *rsa.PublicKey
	kty string
	use string
	alg string
}

// fakeIDP 是用 httptest 搭起来的假 IdP：提供 discovery、JWKS 与令牌端点。
type fakeIDP struct {
	t      *testing.T
	server *httptest.Server

	mu sync.Mutex

	// issuerOverride 非空时 discovery 返回该 issuer（用于测试 issuer 校验）。
	issuerOverride string
	// authMethods 是 discovery 暴露的 token_endpoint_auth_methods_supported。
	authMethods []string
	// published 是当前 JWKS 发布的公钥集合。
	published []publishedKey
	// signKey / signKid 是当前用来签 ID Token 的私钥。
	signKey *rsa.PrivateKey
	signKid string
	// alg 为空时按 RS256 签名；可设为 none / HS256 / RS512 制造异常令牌。
	alg string
	// claimsMutate 在默认声明上做修改，nil 表示使用默认声明。
	claimsMutate func(map[string]any)
	// nonce 模拟假 IdP 在授权请求中记住的 nonce。
	nonce string
	// tokenHandler 非 nil 时接管令牌端点的响应。
	tokenHandler func(w http.ResponseWriter, r *http.Request)

	// jwksRequests / tokenRequests 统计端点被调用的次数。
	jwksRequests  int
	tokenRequests int
	// lastForm / lastBasic 记录最近一次令牌请求的细节。
	lastForm  url.Values
	lastBasic bool
	lastUser  string
	lastPass  string
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	key := testRSAKey(t, 0)
	f := &fakeIDP{
		t:         t,
		signKey:   key,
		signKid:   "kid-a",
		published: []publishedKey{{kid: "kid-a", pub: &key.PublicKey}},
		nonce:     "test-nonce",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", f.handleDiscovery)
	mux.HandleFunc("/jwks", f.handleJWKS)
	mux.HandleFunc("/token", f.handleToken)
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// issuer 返回当前 discovery 声明的 issuer。
func (f *fakeIDP) issuer() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.issuerOverride != "" {
		return f.issuerOverride
	}
	return f.server.URL
}

// config 返回指向本假 IdP 的默认配置。
func (f *fakeIDP) config() Config {
	return Config{
		Issuer:       f.issuer(),
		ClientID:     testClientID,
		ClientSecret: testClientSecret,
		RedirectURL:  testRedirectURL,
	}
}

func (f *fakeIDP) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	issuer := f.issuerOverride
	methods := append([]string(nil), f.authMethods...)
	f.mu.Unlock()
	if issuer == "" {
		issuer = f.server.URL
	}
	doc := map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                f.server.URL + "/authorize",
		"token_endpoint":                        f.server.URL + "/token",
		"jwks_uri":                              f.server.URL + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	}
	if methods != nil {
		doc["token_endpoint_auth_methods_supported"] = methods
	}
	writeJSON(f.t, w, doc)
}

func (f *fakeIDP) handleJWKS(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.jwksRequests++
	keys := make([]map[string]any, 0, len(f.published))
	for _, k := range f.published {
		kty, use, alg := k.kty, k.use, k.alg
		if kty == "" {
			kty = "RSA"
		}
		if use == "" {
			use = "sig"
		}
		if alg == "" {
			alg = "RS256"
		}
		keys = append(keys, map[string]any{
			"kty": kty,
			"use": use,
			"alg": alg,
			"kid": k.kid,
			"n":   b64url(k.pub.N.Bytes()),
			"e":   b64url(bigEndianE(k.pub.E)),
		})
	}
	f.mu.Unlock()
	writeJSON(f.t, w, map[string]any{"keys": keys})
}

func (f *fakeIDP) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	user, pass, hasBasic := r.BasicAuth()

	f.mu.Lock()
	f.tokenRequests++
	f.lastForm = r.PostForm
	f.lastBasic = hasBasic
	f.lastUser, f.lastPass = user, pass
	handler := f.tokenHandler
	signKey, signKid, alg := f.signKey, f.signKid, f.alg
	mutate := f.claimsMutate
	nonce := f.nonce
	f.mu.Unlock()

	if handler != nil {
		handler(w, r)
		return
	}

	claims := defaultClaims(f.server.URL, nonce)
	if mutate != nil {
		mutate(claims)
	}
	token, err := signToken(signKey, signKid, alg, claims)
	if err != nil {
		// 处理函数跑在服务端 goroutine 上，只能用 Errorf。
		f.t.Errorf("签发 id_token 失败: %v", err)
		http.Error(w, "sign failed", http.StatusInternalServerError)
		return
	}
	writeJSON(f.t, w, map[string]any{
		"access_token": "fake-access-token",
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     token,
	})
}

func (f *fakeIDP) formValue(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastForm.Get(key)
}

func (f *fakeIDP) basicAuth() (user, pass string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastUser, f.lastPass, f.lastBasic
}

func (f *fakeIDP) jwksFetchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jwksRequests
}

func (f *fakeIDP) setNonce(nonce string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nonce = nonce
}

func (f *fakeIDP) setAlg(alg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alg = alg
}

func (f *fakeIDP) setSigner(key *rsa.PrivateKey, kid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signKey, f.signKid = key, kid
}

func (f *fakeIDP) setPublished(keys ...publishedKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = keys
}

func (f *fakeIDP) setClaimsMutate(fn func(map[string]any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimsMutate = fn
}

func (f *fakeIDP) setAuthMethods(methods ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authMethods = methods
}

func (f *fakeIDP) setIssuerOverride(issuer string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issuerOverride = issuer
}

func (f *fakeIDP) setTokenHandler(fn func(w http.ResponseWriter, r *http.Request)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenHandler = fn
}

// mustDiscover 针对假 IdP 做一次 discovery，失败即终止测试。
func mustDiscover(t *testing.T, f *fakeIDP) *Provider {
	t.Helper()
	p, err := Discover(context.Background(), f.config())
	if err != nil {
		t.Fatalf("Discover 失败: %v", err)
	}
	return p
}

// defaultClaims 构造一份合法的 ID Token 载荷。
func defaultClaims(issuer, nonce string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":                issuer,
		"sub":                "user-123",
		"aud":                testClientID,
		"exp":                now.Add(time.Hour).Unix(),
		"iat":                now.Unix(),
		"nonce":              nonce,
		"email":              "alice@example.com",
		"email_verified":     true,
		"preferred_username": "alice",
		"name":               "Alice Example",
	}
}

// signToken 按 alg 签发 JWT；alg 为空时按 RS256 签名。
func signToken(key *rsa.PrivateKey, kid, alg string, claims map[string]any) (string, error) {
	if alg == "" {
		alg = rs256
	}
	headerBytes, err := json.Marshal(map[string]any{"alg": alg, "typ": "JWT", "kid": kid})
	if err != nil {
		return "", err
	}
	claimBytes, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := b64url(headerBytes) + "." + b64url(claimBytes)

	var signature []byte
	switch alg {
	case rs256, "RS512":
		// RS512 分支故意仍用 SHA-256 签名：本包只应看 header.alg 就拒绝它。
		digest := sha256.Sum256([]byte(signingInput))
		signature, err = rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
		if err != nil {
			return "", err
		}
	case "none":
		signature = nil
	case "HS256":
		// 算法混淆攻击：用 JWKS 里的公钥当 HMAC 密钥。
		mac := hmac.New(sha256.New, key.PublicKey.N.Bytes())
		mac.Write([]byte(signingInput))
		signature = mac.Sum(nil)
	default:
		return "", fmt.Errorf("测试不支持的算法 %q", alg)
	}
	return signingInput + "." + b64url(signature), nil
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// bigEndianE 把 RSA 公开指数转成大端字节。
func bigEndianE(e int) []byte {
	var out []byte
	for e > 0 {
		out = append([]byte{byte(e & 0xff)}, out...)
		e >>= 8
	}
	if len(out) == 0 {
		out = []byte{0}
	}
	return out
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("写 JSON 响应失败: %v", err)
	}
}

// TestDiscoverAuthCodeURLAndExchange 覆盖正常流程：
// discovery → AuthCodeURL → 假令牌端点 → Exchange 返回 Claims。
func TestDiscoverAuthCodeURLAndExchange(t *testing.T) {
	idp := newFakeIDP(t)
	ctx := context.Background()

	// 配置里故意给 issuer 带一个末尾斜杠，验证 TrimSuffix 处理。
	cfg := idp.config()
	cfg.Issuer += "/"
	p, err := Discover(ctx, cfg)
	if err != nil {
		t.Fatalf("Discover 失败: %v", err)
	}
	if p.issuer != idp.server.URL {
		t.Errorf("issuer = %q, 期望 %q", p.issuer, idp.server.URL)
	}
	if got := p.Issuer(); got != idp.server.URL {
		t.Errorf("Issuer() = %q, 期望 %q", got, idp.server.URL)
	}

	verifier, err := NewCodeVerifier()
	if err != nil {
		t.Fatalf("NewCodeVerifier 失败: %v", err)
	}
	state, err := NewState()
	if err != nil {
		t.Fatalf("NewState 失败: %v", err)
	}
	nonce, err := NewState()
	if err != nil {
		t.Fatalf("NewState 失败: %v", err)
	}
	// 假 IdP "记住" 授权请求里的 nonce，换令牌时发回同样的 nonce。
	idp.setNonce(nonce)

	raw := p.AuthCodeURL(state, nonce, verifier)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("AuthCodeURL 结果不是合法 URL: %v", err)
	}
	if u.Path != "/authorize" {
		t.Errorf("授权端点路径 = %q, 期望 /authorize", u.Path)
	}
	want := map[string]string{
		"response_type":         "code",
		"client_id":             testClientID,
		"redirect_uri":          testRedirectURL,
		"scope":                 "openid email profile",
		"state":                 state,
		"nonce":                 nonce,
		"code_challenge":        CodeChallengeS256(verifier),
		"code_challenge_method": "S256",
	}
	q := u.Query()
	for key, value := range want {
		if got := q.Get(key); got != value {
			t.Errorf("授权地址参数 %s = %q, 期望 %q", key, got, value)
		}
	}

	claims, err := p.Exchange(ctx, "the-code", verifier, nonce)
	if err != nil {
		t.Fatalf("Exchange 失败: %v", err)
	}
	if claims.Subject != "user-123" {
		t.Errorf("Subject = %q", claims.Subject)
	}
	if claims.Email != "alice@example.com" {
		t.Errorf("Email = %q", claims.Email)
	}
	if !claims.EmailVerified {
		t.Error("EmailVerified 应为 true")
	}
	if claims.PreferredUsername != "alice" {
		t.Errorf("PreferredUsername = %q", claims.PreferredUsername)
	}
	if claims.Name != "Alice Example" {
		t.Errorf("Name = %q", claims.Name)
	}
	if got, _ := claims.Raw["nonce"].(string); got != nonce {
		t.Errorf("Raw[nonce] = %q, 期望 %q", got, nonce)
	}

	// 令牌端点收到的表单与客户端认证方式。
	if got := idp.formValue("grant_type"); got != "authorization_code" {
		t.Errorf("grant_type = %q", got)
	}
	if got := idp.formValue("code"); got != "the-code" {
		t.Errorf("code = %q", got)
	}
	if got := idp.formValue("code_verifier"); got != verifier {
		t.Errorf("code_verifier = %q, 期望 %q", got, verifier)
	}
	if got := idp.formValue("redirect_uri"); got != testRedirectURL {
		t.Errorf("redirect_uri = %q", got)
	}
	user, pass, ok := idp.basicAuth()
	if !ok || user != testClientID || pass != testClientSecret {
		t.Errorf("应使用 client_secret_basic 认证，实际 basic=%v user=%q", ok, user)
	}
	if got := idp.formValue("client_secret"); got != "" {
		t.Error("使用 basic 认证时不应把 client_secret 放进表单")
	}

	// 公钥缓存命中后不应重复拉取 JWKS。
	if got := idp.jwksFetchCount(); got != 1 {
		t.Errorf("JWKS 拉取次数 = %d, 期望 1", got)
	}
}

// TestExchangeRejectsWrongSigningKey 覆盖用另一把私钥签名的情况。
func TestExchangeRejectsWrongSigningKey(t *testing.T) {
	ctx := context.Background()

	t.Run("同一 kid 但换了私钥", func(t *testing.T) {
		idp := newFakeIDP(t)
		idp.setNonce("n1")
		// JWKS 仍发布 kid-a 的公钥，但签名换成了另一把私钥。
		idp.setSigner(testRSAKey(t, 1), "kid-a")
		p := mustDiscover(t, idp)

		_, err := p.Exchange(ctx, "code", "verifier", "n1")
		if err == nil {
			t.Fatal("签名与公钥不匹配时必须失败")
		}
		if !strings.Contains(err.Error(), "签名") {
			t.Errorf("错误信息 %q 未说明签名校验失败", err)
		}
	})

	t.Run("kid 不在 JWKS 中", func(t *testing.T) {
		idp := newFakeIDP(t)
		idp.setNonce("n1")
		idp.setSigner(testRSAKey(t, 1), "kid-unknown")
		p := mustDiscover(t, idp)

		_, err := p.Exchange(ctx, "code", "verifier", "n1")
		if err == nil {
			t.Fatal("kid 未在 JWKS 中时必须失败")
		}
		if !strings.Contains(err.Error(), "kid-unknown") {
			t.Errorf("错误信息 %q 未指出未知 kid", err)
		}
	})
}

// TestExchangeRejectsUnsupportedAlgorithms 覆盖 alg=none / HS256 / RS512。
func TestExchangeRejectsUnsupportedAlgorithms(t *testing.T) {
	for _, alg := range []string{"none", "HS256", "RS512"} {
		t.Run(alg, func(t *testing.T) {
			idp := newFakeIDP(t)
			idp.setNonce("n1")
			idp.setAlg(alg)
			p := mustDiscover(t, idp)

			_, err := p.Exchange(context.Background(), "code", "verifier", "n1")
			if err == nil {
				t.Fatalf("alg=%s 的 id_token 必须被拒绝", alg)
			}
			if !strings.Contains(err.Error(), "算法") {
				t.Errorf("错误信息 %q 未指出算法不支持", err)
			}
		})
	}
}

// TestExchangeValidatesClaims 覆盖 iss / aud / exp / nbf / iat / nonce / sub。
func TestExchangeValidatesClaims(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		mutate  func(map[string]any)
		wantSub string
	}{
		{
			name:    "aud 不含 client_id",
			mutate:  func(c map[string]any) { c["aud"] = "other-client" },
			wantSub: "aud",
		},
		{
			name:    "aud 数组不含 client_id",
			mutate:  func(c map[string]any) { c["aud"] = []string{"a", "b"} },
			wantSub: "aud",
		},
		{
			name:    "aud 数组含 client_id 时接受",
			mutate:  func(c map[string]any) { c["aud"] = []any{"other", testClientID} },
			wantSub: "",
		},
		{
			name:    "iss 不匹配",
			mutate:  func(c map[string]any) { c["iss"] = "https://evil.example.com" },
			wantSub: "iss",
		},
		{
			name:    "已过期 exp",
			mutate:  func(c map[string]any) { c["exp"] = now.Add(-time.Hour).Unix() },
			wantSub: "过期",
		},
		{
			name:    "缺少 exp",
			mutate:  func(c map[string]any) { delete(c, "exp") },
			wantSub: "exp",
		},
		{
			name:    "nbf 在未来",
			mutate:  func(c map[string]any) { c["nbf"] = now.Add(time.Hour).Unix() },
			wantSub: "nbf",
		},
		{
			name:    "iat 在未来",
			mutate:  func(c map[string]any) { c["iat"] = now.Add(time.Hour).Unix() },
			wantSub: "iat",
		},
		{
			name:    "clock skew 内的 exp 仍接受",
			mutate:  func(c map[string]any) { c["exp"] = now.Add(-30 * time.Second).Unix() },
			wantSub: "",
		},
		{
			name:    "nonce 不匹配",
			mutate:  func(c map[string]any) { c["nonce"] = "another-nonce" },
			wantSub: "nonce",
		},
		{
			name:    "缺少 nonce",
			mutate:  func(c map[string]any) { delete(c, "nonce") },
			wantSub: "nonce",
		},
		{
			name:    "缺少 sub",
			mutate:  func(c map[string]any) { delete(c, "sub") },
			wantSub: "sub",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idp := newFakeIDP(t)
			idp.setNonce("n1")
			idp.setClaimsMutate(tc.mutate)
			p := mustDiscover(t, idp)

			_, err := p.Exchange(context.Background(), "code", "verifier", "n1")
			if tc.wantSub == "" {
				if err != nil {
					t.Fatalf("期望通过校验，实际失败: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("期望校验失败，但 Exchange 成功了")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("错误信息 %q 未包含 %q", err, tc.wantSub)
			}
		})
	}
}

// TestExchangeWithoutExpectedNonce 覆盖 expectedNonce 为空时跳过 nonce 校验。
func TestExchangeWithoutExpectedNonce(t *testing.T) {
	idp := newFakeIDP(t)
	idp.setNonce("idp-side-nonce")
	p := mustDiscover(t, idp)

	claims, err := p.Exchange(context.Background(), "code", "verifier", "")
	if err != nil {
		t.Fatalf("expectedNonce 为空时不应校验 nonce: %v", err)
	}
	if got, _ := claims.Raw["nonce"].(string); got != "idp-side-nonce" {
		t.Errorf("Raw[nonce] = %q", got)
	}
}

// TestJWKSRotationAndUnknownKidRefresh 覆盖未知 kid 触发刷新与缓存命中不重复拉取。
func TestJWKSRotationAndUnknownKidRefresh(t *testing.T) {
	ctx := context.Background()
	idp := newFakeIDP(t)
	idp.setNonce("n1")
	p := mustDiscover(t, idp)

	// 第一次交换：缓存为空，拉取一次 JWKS。
	if _, err := p.Exchange(ctx, "c1", "verifier", "n1"); err != nil {
		t.Fatalf("首次 Exchange 失败: %v", err)
	}
	if got := idp.jwksFetchCount(); got != 1 {
		t.Fatalf("JWKS 拉取次数 = %d, 期望 1", got)
	}

	// 第二次交换：kid 命中缓存且在 TTL 内，不应重复拉取。
	if _, err := p.Exchange(ctx, "c2", "verifier", "n1"); err != nil {
		t.Fatalf("第二次 Exchange 失败: %v", err)
	}
	if got := idp.jwksFetchCount(); got != 1 {
		t.Errorf("缓存命中时不应重复拉取 JWKS，实际 %d 次", got)
	}

	// 轮换：签名换成 kid-b，JWKS 也只发布 kid-b。
	keyB := testRSAKey(t, 1)
	idp.setSigner(keyB, "kid-b")
	idp.setPublished(publishedKey{kid: "kid-b", pub: &keyB.PublicKey})

	// 旧缓存中没有 kid-b（未知 kid）→ 触发一次刷新 → 成功。
	if _, err := p.Exchange(ctx, "c3", "verifier", "n1"); err != nil {
		t.Fatalf("轮换后应能通过刷新 JWKS 恢复: %v", err)
	}
	if got := idp.jwksFetchCount(); got != 2 {
		t.Errorf("未知 kid 应触发一次刷新，JWKS 拉取次数 = %d, 期望 2", got)
	}

	// 刷新后的缓存生效，后续交换不再拉取。
	if _, err := p.Exchange(ctx, "c4", "verifier", "n1"); err != nil {
		t.Fatalf("刷新后 Exchange 失败: %v", err)
	}
	if got := idp.jwksFetchCount(); got != 2 {
		t.Errorf("刷新后应命中缓存，JWKS 拉取次数 = %d, 期望 2", got)
	}
}

// TestJWKSRefreshAfterTTL 覆盖缓存过期后的重新拉取。
func TestJWKSRefreshAfterTTL(t *testing.T) {
	ctx := context.Background()
	idp := newFakeIDP(t)
	idp.setNonce("n1")
	p := mustDiscover(t, idp)

	if _, err := p.Exchange(ctx, "c1", "verifier", "n1"); err != nil {
		t.Fatalf("首次 Exchange 失败: %v", err)
	}
	if got := idp.jwksFetchCount(); got != 1 {
		t.Fatalf("JWKS 拉取次数 = %d, 期望 1", got)
	}

	// 把缓存时间人为推回到 TTL 之前，并轮换到 kid-b。
	p.mu.Lock()
	p.keysFetchedAt = time.Now().Add(-2 * jwksCacheTTL)
	p.mu.Unlock()
	keyB := testRSAKey(t, 1)
	idp.setSigner(keyB, "kid-b")
	idp.setPublished(publishedKey{kid: "kid-b", pub: &keyB.PublicKey})

	if _, err := p.Exchange(ctx, "c2", "verifier", "n1"); err != nil {
		t.Fatalf("缓存过期后应重新拉取 JWKS: %v", err)
	}
	if got := idp.jwksFetchCount(); got != 2 {
		t.Errorf("缓存过期后 JWKS 拉取次数 = %d, 期望 2", got)
	}
}

// TestJWKSSkipsNonSigningKeys 覆盖 JWKS 中混入非签名密钥的情况。
func TestJWKSSkipsNonSigningKeys(t *testing.T) {
	ctx := context.Background()

	t.Run("混杂 use=enc 的密钥仍能命中签名密钥", func(t *testing.T) {
		idp := newFakeIDP(t)
		idp.setNonce("n1")
		keyA := testRSAKey(t, 0)
		idp.setPublished(
			publishedKey{kid: "kid-enc", pub: &testRSAKey(t, 1).PublicKey, use: "enc"},
			publishedKey{kid: "kid-oaep", pub: &testRSAKey(t, 1).PublicKey, alg: "RSA-OAEP"},
			publishedKey{kid: "kid-a", pub: &keyA.PublicKey},
		)
		p := mustDiscover(t, idp)

		if _, err := p.Exchange(ctx, "c1", "verifier", "n1"); err != nil {
			t.Fatalf("JWKS 中混有非签名密钥时仍应按 kid 命中: %v", err)
		}
	})

	t.Run("只有非签名密钥时报错", func(t *testing.T) {
		idp := newFakeIDP(t)
		idp.setNonce("n1")
		idp.setPublished(
			publishedKey{kid: "kid-enc", pub: &testRSAKey(t, 1).PublicKey, use: "enc"},
		)
		p := mustDiscover(t, idp)

		_, err := p.Exchange(ctx, "c1", "verifier", "n1")
		if err == nil {
			t.Fatal("JWKS 中没有签名公钥时必须失败")
		}
		if !strings.Contains(err.Error(), "签名公钥") {
			t.Errorf("错误信息 %q 未说明缺少签名公钥", err)
		}
	})
}

// TestDiscoverRejectsIssuerMismatch 覆盖 discovery 的 issuer 混淆防护。
func TestDiscoverRejectsIssuerMismatch(t *testing.T) {
	idp := newFakeIDP(t)
	// 配置指向真实地址，但 discovery 文档谎称自己是另一个 issuer。
	idp.setIssuerOverride("https://evil.example.com")

	cfg := Config{
		Issuer:       idp.server.URL,
		ClientID:     testClientID,
		ClientSecret: testClientSecret,
		RedirectURL:  testRedirectURL,
	}
	_, err := Discover(context.Background(), cfg)
	if err == nil {
		t.Fatal("discovery 返回的 issuer 与配置不一致时必须失败")
	}
	if !strings.Contains(err.Error(), "issuer") {
		t.Errorf("错误信息 %q 未指出 issuer 不一致", err)
	}
}

// TestDiscoverRejectsOversizedResponse 覆盖 discovery 响应体大小限制。
func TestDiscoverRejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issuer":"http://` + r.Host + `"`))
		_, _ = w.Write(bytes.Repeat([]byte(" "), 2*maxResponseBytes))
	}))
	defer srv.Close()

	_, err := Discover(context.Background(), Config{
		Issuer:      srv.URL,
		ClientID:    testClientID,
		RedirectURL: testRedirectURL,
	})
	if err == nil {
		t.Fatal("超过 1 MiB 的 discovery 响应必须被拒绝")
	}
	if !strings.Contains(err.Error(), "超过") {
		t.Errorf("错误信息 %q 未说明响应体过大", err)
	}
}

// TestTokenEndpointAuthStyles 覆盖 client_secret_post 与公共客户端（仅 PKCE）。
func TestTokenEndpointAuthStyles(t *testing.T) {
	ctx := context.Background()

	t.Run("discovery 不支持 basic 时用 client_secret_post", func(t *testing.T) {
		idp := newFakeIDP(t)
		idp.setNonce("n1")
		idp.setAuthMethods("client_secret_post")
		p := mustDiscover(t, idp)

		if _, err := p.Exchange(ctx, "code", "verifier", "n1"); err != nil {
			t.Fatalf("Exchange 失败: %v", err)
		}
		if _, _, ok := idp.basicAuth(); ok {
			t.Error("discovery 不支持 basic 时不应发送 Authorization 头")
		}
		if got := idp.formValue("client_id"); got != testClientID {
			t.Errorf("表单 client_id = %q", got)
		}
		if got := idp.formValue("client_secret"); got != testClientSecret {
			t.Errorf("表单 client_secret 不正确")
		}
	})

	t.Run("公共客户端不带任何客户端认证", func(t *testing.T) {
		idp := newFakeIDP(t)
		idp.setNonce("n1")
		cfg := idp.config()
		cfg.ClientSecret = ""
		p, err := Discover(ctx, cfg)
		if err != nil {
			t.Fatalf("Discover 失败: %v", err)
		}

		if _, err := p.Exchange(ctx, "code", "verifier", "n1"); err != nil {
			t.Fatalf("Exchange 失败: %v", err)
		}
		if _, _, ok := idp.basicAuth(); ok {
			t.Error("公共客户端不应发送 Authorization 头")
		}
		if got := idp.formValue("client_secret"); got != "" {
			t.Error("公共客户端不应发送 client_secret")
		}
		if got := idp.formValue("client_id"); got != testClientID {
			t.Errorf("表单 client_id = %q", got)
		}
		if got := idp.formValue("code_verifier"); got != "verifier" {
			t.Errorf("公共客户端必须带 code_verifier，实际 %q", got)
		}
	})
}

// TestExchangeErrorDoesNotLeakSecret 覆盖错误信息不泄露密钥。
func TestExchangeErrorDoesNotLeakSecret(t *testing.T) {
	idp := newFakeIDP(t)
	idp.setNonce("n1")
	idp.setTokenHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"authorization code not found"}`))
	})
	p := mustDiscover(t, idp)

	_, err := p.Exchange(context.Background(), "code", "verifier", "n1")
	if err == nil {
		t.Fatal("令牌端点报错时 Exchange 必须失败")
	}
	if strings.Contains(err.Error(), testClientSecret) {
		t.Errorf("错误信息不应包含 client_secret: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("错误信息应包含 IdP 返回的错误码: %v", err)
	}
}

// TestExchangeWithoutIDToken 覆盖响应缺少 id_token 的情况。
func TestExchangeWithoutIDToken(t *testing.T) {
	idp := newFakeIDP(t)
	idp.setNonce("n1")
	idp.setTokenHandler(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"access_token": "at", "token_type": "Bearer"})
	})
	p := mustDiscover(t, idp)

	if _, err := p.Exchange(context.Background(), "code", "verifier", "n1"); err == nil {
		t.Fatal("响应缺少 id_token 时必须失败")
	}
}

// TestExchangeHonorsContextTimeout 覆盖出站请求使用 context 超时。
func TestExchangeHonorsContextTimeout(t *testing.T) {
	idp := newFakeIDP(t)
	idp.setNonce("n1")
	idp.setTokenHandler(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // 一直等到客户端放弃
	})
	p := mustDiscover(t, idp)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := p.Exchange(ctx, "code", "verifier", "n1"); err == nil {
		t.Fatal("context 超时后 Exchange 必须失败")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("超时未被及时中断，耗时 %v", elapsed)
	}
}

// TestConcurrentJWKSAccess 覆盖并发使用 Provider（需配合 -race 运行）。
func TestConcurrentJWKSAccess(t *testing.T) {
	ctx := context.Background()
	idp := newFakeIDP(t)
	idp.setNonce("n1")
	p := mustDiscover(t, idp)
	if _, err := p.Exchange(ctx, "warmup", "verifier", "n1"); err != nil {
		t.Fatalf("预热 Exchange 失败: %v", err)
	}

	const (
		workers  = 16
		rounds   = 4
		maxError = workers * rounds
	)
	errCh := make(chan error, maxError)
	start := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < rounds; j++ {
				if _, err := p.Exchange(ctx, "code", "verifier", "n1"); err != nil {
					errCh <- err
				}
				if got := p.AuthCodeURL("state", "nonce", "verifier"); got == "" {
					errCh <- fmt.Errorf("AuthCodeURL 返回空串")
				}
				// 人为让缓存过期，制造并发刷新 JWKS 的场景。
				p.mu.Lock()
				p.keysFetchedAt = time.Now().Add(-2 * jwksCacheTTL)
				p.mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("并发 Exchange 失败: %v", err)
	}
	if got := idp.jwksFetchCount(); got == 0 {
		t.Error("并发场景下应至少拉取过一次 JWKS")
	}
}

// TestCodeChallengeS256RFC7636Vector 用 RFC 7636 附录 B 的官方向量校验。
func TestCodeChallengeS256RFC7636Vector(t *testing.T) {
	const (
		verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
		want     = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	)
	if got := CodeChallengeS256(verifier); got != want {
		t.Errorf("CodeChallengeS256(%q) = %q, 期望 %q", verifier, got, want)
	}
	if strings.ContainsAny(CodeChallengeS256(verifier), "=+/") {
		t.Error("code_challenge 必须是去掉 padding 的 base64url")
	}
}

// TestRandomHelpers 校验 code_verifier / state / nonce 的字符集与长度。
func TestRandomHelpers(t *testing.T) {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"

	verifier, err := NewCodeVerifier()
	if err != nil {
		t.Fatalf("NewCodeVerifier 失败: %v", err)
	}
	if n := len(verifier); n < 43 || n > 128 {
		t.Errorf("code_verifier 长度 %d 不在 RFC 7636 要求的 43-128 之间", n)
	}
	if strings.Trim(verifier, unreserved) != "" {
		t.Errorf("code_verifier 含非 unreserved 字符: %q", verifier)
	}
	other, err := NewCodeVerifier()
	if err != nil {
		t.Fatalf("NewCodeVerifier 失败: %v", err)
	}
	if verifier == other {
		t.Error("两次生成的 code_verifier 不应相同")
	}

	state, err := NewState()
	if err != nil {
		t.Fatalf("NewState 失败: %v", err)
	}
	nonce, err := NewState()
	if err != nil {
		t.Fatalf("NewState 失败: %v", err)
	}
	if state == nonce {
		t.Error("两次生成的随机值不应相同")
	}
	for name, v := range map[string]string{"state": state, "nonce": nonce} {
		if len(v) != 43 {
			t.Errorf("%s 长度 = %d, 期望 43（32 字节 base64url）", name, len(v))
		}
		if _, err := base64.RawURLEncoding.DecodeString(v); err != nil {
			t.Errorf("%s 不是合法的 base64url: %v", name, err)
		}
	}

	// 未配置 scope 时使用默认值。
	p := mustDiscover(t, newFakeIDP(t))
	if got := strings.Join(p.scopes, " "); got != "openid email profile" {
		t.Errorf("默认 scope = %q", got)
	}
}
