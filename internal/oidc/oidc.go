// Package oidc 实现 PageHut 需要的最小 OpenID Connect 依赖方（RP）能力：
// discovery、JWKS 公钥缓存、授权跳转地址生成、授权码换令牌以及 ID Token 校验。
//
// 本包只依赖 Go 标准库。当前支持 authorization_code 流程、PKCE(S256) 与
// RS256 签名的 ID Token；不支持加密 ID Token（JWE）、ES256/PS256、
// refresh token、UserInfo 与注销等流程。
package oidc

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// defaultHTTPTimeout 是未提供 HTTPClient 时默认客户端的单次请求超时。
	defaultHTTPTimeout = 10 * time.Second
	// maxResponseBytes 限制单个出站响应体的读取上限（1 MiB）。
	maxResponseBytes = 1 << 20
	// jwksCacheTTL 是 JWKS 公钥缓存的存活时间，超时后按需重新拉取。
	jwksCacheTTL = time.Hour
	// clockSkew 是校验 exp/nbf/iat 时允许的时钟偏移。
	clockSkew = 60 * time.Second
	// minRSABits 是接受的签名公钥最小位数（RFC 7518 要求 RS256 至少 2048 位）。
	minRSABits = 2048
	// codeVerifierBytes 是 PKCE code_verifier 的随机字节数，
	// base64url 编码后为 43 字符，落在 RFC 7636 要求的 43-128 区间内。
	codeVerifierBytes = 32
	// randomTokenBytes 是 state / nonce 的随机字节数，base64url 编码后为 43 字符。
	randomTokenBytes = 32
	// authMethodBasic 是客户端密钥通过 Authorization: Basic 发送的认证方式。
	authMethodBasic = "client_secret_basic"
	// rs256 是本包唯一接受的 JWS 签名算法。
	rs256 = "RS256"
)

// Config 是一次 OIDC 客户端配置。
type Config struct {
	// Issuer 是 IdP 的 issuer，例如 https://accounts.example.com 或 https://idp/realms/x。
	Issuer string
	// ClientID 是 IdP 侧登记的客户端 ID。
	ClientID string
	// ClientSecret 是客户端密钥；公共客户端（仅 PKCE）留空。
	ClientSecret string
	// Scopes 为空时使用 ["openid", "email", "profile"]。
	Scopes []string
	// RedirectURL 必须与 IdP 侧登记的回调地址完全一致。
	RedirectURL string
	// HTTPClient 可为 nil，nil 时使用带 10s 超时的默认客户端。
	HTTPClient *http.Client
}

// discoveryDocument 是 /.well-known/openid-configuration 中本包关心的字段。
type discoveryDocument struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	JWKSURI                           string   `json:"jwks_uri"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
}

// tokenResponse 是令牌端点的响应；本包只使用 id_token，
// access_token / refresh_token 不落库也不返回给调用方。
type tokenResponse struct {
	IDToken string `json:"id_token"`
}

// jwksDocument 是 JWKS 文档。
type jwksDocument struct {
	Keys []jwk `json:"keys"`
}

// jwk 是 JWKS 中的单个 RSA 公钥。
type jwk struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// Provider 保存 discovery 结果与 JWKS 缓存，可并发使用。
// 内部含有互斥锁，Provider 值不可复制（请始终使用 *Provider）。
type Provider struct {
	// issuer 是 discovery 校验通过后的 issuer（已去掉末尾斜杠）。
	issuer string

	// tokenEndpoint / jwksURI 来自 discovery 文档，构造跳转地址用的是 authBase/authParams。
	tokenEndpoint string
	jwksURI       string

	clientID     string
	clientSecret string
	redirectURL  string
	scopes       []string
	// useBasicAuth 表示令牌端点用 client_secret_basic 认证；
	// 无 clientSecret（公共客户端）时该字段不起作用。
	useBasicAuth bool

	httpClient *http.Client
	// now 便于测试注入时钟，生产环境固定为 time.Now。
	now func() time.Time

	// authBase 与 authParams 在 Discover 阶段算好且此后只读，
	// AuthCodeURL 只做克隆，因此可以并发调用。
	authBase   *url.URL
	authParams url.Values

	// mu 保护 keys 与 keysFetchedAt。
	mu            sync.RWMutex
	keys          map[string]*rsa.PublicKey
	keysFetchedAt time.Time

	// refreshMu 串行化 JWKS 刷新，避免大量请求遇到未知 kid 时重复拉取。
	refreshMu sync.Mutex
}

// Discover 拉取 <issuer>/.well-known/openid-configuration 并校验 issuer 一致。
// 它不会预取 JWKS：公钥在第一次校验 ID Token 时按需拉取并缓存。
func Discover(ctx context.Context, cfg Config) (*Provider, error) {
	issuer := strings.TrimSuffix(strings.TrimSpace(cfg.Issuer), "/")
	if issuer == "" {
		return nil, errors.New("oidc: Config.Issuer 不能为空")
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, errors.New("oidc: Config.ClientID 不能为空")
	}
	if strings.TrimSpace(cfg.RedirectURL) == "" {
		return nil, errors.New("oidc: Config.RedirectURL 不能为空")
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}

	p := &Provider{
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		redirectURL:  cfg.RedirectURL,
		scopes:       normalizeScopes(cfg.Scopes),
		useBasicAuth: true,
		httpClient:   client,
		now:          time.Now,
	}

	var doc discoveryDocument
	if err := p.getJSON(ctx, issuer+"/.well-known/openid-configuration", &doc); err != nil {
		return nil, err
	}
	// 校验 discovery 返回的 issuer 与配置一致，避免被指向另一个 IdP（混淆攻击）。
	if strings.TrimSuffix(strings.TrimSpace(doc.Issuer), "/") != issuer {
		return nil, errors.New("oidc: discovery 文档中的 issuer 与配置不一致")
	}

	authEndpoint, err := checkEndpoint("authorization_endpoint", doc.AuthorizationEndpoint)
	if err != nil {
		return nil, err
	}
	tokenEndpoint, err := checkEndpoint("token_endpoint", doc.TokenEndpoint)
	if err != nil {
		return nil, err
	}
	jwksURI, err := checkEndpoint("jwks_uri", doc.JWKSURI)
	if err != nil {
		return nil, err
	}

	authBase, err := url.Parse(authEndpoint)
	if err != nil {
		return nil, fmt.Errorf("oidc: 解析 authorization_endpoint 失败: %w", err)
	}

	p.issuer = issuer
	p.tokenEndpoint = tokenEndpoint
	p.jwksURI = jwksURI
	p.authBase = authBase
	p.authParams = url.Values{
		"response_type": {"code"},
		"client_id":     {cfg.ClientID},
		"redirect_uri":  {cfg.RedirectURL},
		"scope":         {strings.Join(p.scopes, " ")},
	}

	// 客户端认证方式：默认优先 client_secret_basic；
	// 只有当 discovery 明确列出了支持的方式且其中不含 basic 时，才退回 client_secret_post。
	if len(doc.TokenEndpointAuthMethodsSupported) > 0 {
		p.useBasicAuth = false
		for _, m := range doc.TokenEndpointAuthMethodsSupported {
			if strings.EqualFold(strings.TrimSpace(m), authMethodBasic) {
				p.useBasicAuth = true
				break
			}
		}
	}
	return p, nil
}

// Issuer 返回 discovery 校验通过的 issuer。
func (p *Provider) Issuer() string { return p.issuer }

// AuthCodeURL 生成授权跳转地址，必须带 state、nonce、PKCE(S256) 参数。
func (p *Provider) AuthCodeURL(state, nonce, codeVerifier string) string {
	if p.authBase == nil {
		return ""
	}
	u := *p.authBase
	q := make(url.Values, len(p.authParams)+4)
	// authParams 在 Discover 之后只读，这里按值共享切片是安全的。
	for k, vs := range p.authParams {
		q[k] = vs
	}
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", CodeChallengeS256(codeVerifier))
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String()
}

// Claims 是校验通过的 ID Token 载荷。
type Claims struct {
	Subject           string
	Email             string
	EmailVerified     bool
	PreferredUsername string
	Name              string
	Raw               map[string]any
}

// Exchange 用授权码换取令牌，校验 ID Token 签名与声明后返回 Claims。
// 必须校验：签名（仅 RS256）、iss、aud（包含 ClientID）、exp/nbf/iat、
// nonce（expectedNonce 非空时必须匹配）。
func (p *Provider) Exchange(ctx context.Context, code, codeVerifier, expectedNonce string) (*Claims, error) {
	if strings.TrimSpace(code) == "" {
		return nil, errors.New("oidc: 授权码不能为空")
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", p.redirectURL)
	if codeVerifier != "" {
		form.Set("code_verifier", codeVerifier)
	}
	switch {
	case p.clientSecret == "":
		// 公共客户端 + PKCE：不带任何客户端认证，只声明 client_id。
		form.Set("client_id", p.clientID)
	case p.useBasicAuth:
		// 凭据走 Authorization: Basic 头，表单里不放 secret。
	default:
		// discovery 说明该 IdP 不支持 basic，退回 client_secret_post。
		form.Set("client_id", p.clientID)
		form.Set("client_secret", p.clientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("oidc: 构造令牌请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if p.clientSecret != "" && p.useBasicAuth {
		// 按 RFC 6749 §2.3.1，Basic 凭据先做表单转义；IdP 签发的密钥通常
		// 只含 URL 安全字符，转义在这里是空操作。
		req.SetBasicAuth(url.QueryEscape(p.clientID), url.QueryEscape(p.clientSecret))
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc: 请求令牌端点失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := readLimited(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, tokenEndpointError(resp.StatusCode, body)
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("oidc: 解析令牌端点响应失败: %w", err)
	}
	if tr.IDToken == "" {
		return nil, errors.New("oidc: 令牌端点响应中没有 id_token（请确认 scope 含 openid）")
	}
	return p.verifyIDToken(ctx, tr.IDToken, expectedNonce)
}

// verifyIDToken 校验 ID Token 的签名与声明。
func (p *Provider) verifyIDToken(ctx context.Context, rawToken, expectedNonce string) (*Claims, error) {
	// JWT 必须恰好三段：header.payload.signature。
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return nil, errors.New("oidc: id_token 不是合法的三段式 JWT")
	}

	headerBytes, err := decodeBase64URL(parts[0])
	if err != nil {
		return nil, errors.New("oidc: 无法解析 id_token 头部")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, errors.New("oidc: 无法解析 id_token 头部")
	}
	// 只接受 RS256：显式拒绝 none / HS256 / ES256 等，防止算法混淆攻击。
	if header.Alg != rs256 {
		return nil, fmt.Errorf("oidc: id_token 使用了不支持的签名算法 %q，仅接受 %s", header.Alg, rs256)
	}

	signature, err := decodeBase64URL(parts[2])
	if err != nil {
		return nil, errors.New("oidc: id_token 签名不是合法的 base64url")
	}
	pub, err := p.verificationKey(ctx, header.Kid)
	if err != nil {
		return nil, err
	}
	// RS256 的签名输入是 ASCII 的 "header.payload"。
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], signature); err != nil {
		return nil, errors.New("oidc: id_token 签名校验失败")
	}

	payloadBytes, err := decodeBase64URL(parts[1])
	if err != nil {
		return nil, errors.New("oidc: id_token 载荷不是合法的 base64url")
	}
	claims, err := parseClaims(payloadBytes)
	if err != nil {
		return nil, err
	}
	if err := p.validateClaims(claims, expectedNonce); err != nil {
		return nil, err
	}
	return claims, nil
}

// parseClaims 解析 ID Token 载荷；数字保留原文以避免浮点精度问题。
func parseClaims(payload []byte) (*Claims, error) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return nil, errors.New("oidc: 无法解析 id_token 载荷")
	}
	return &Claims{
		Subject:           stringClaim(raw, "sub"),
		Email:             stringClaim(raw, "email"),
		EmailVerified:     boolClaim(raw, "email_verified"),
		PreferredUsername: stringClaim(raw, "preferred_username"),
		Name:              stringClaim(raw, "name"),
		Raw:               raw,
	}, nil
}

// validateClaims 校验 iss / aud / exp / nbf / iat / nonce。
// 错误信息只说明失败原因，不回显声明原文，避免把令牌内容带进日志。
func (p *Provider) validateClaims(claims *Claims, expectedNonce string) error {
	if claims.Subject == "" {
		return errors.New("oidc: id_token 缺少 sub 声明")
	}
	iss := stringClaim(claims.Raw, "iss")
	if strings.TrimSuffix(iss, "/") != p.issuer {
		return errors.New("oidc: id_token 的 iss 与配置的 issuer 不一致")
	}
	if !audienceContains(claims.Raw["aud"], p.clientID) {
		return errors.New("oidc: id_token 的 aud 不包含本应用的 client_id")
	}

	now := p.now().UTC()
	exp, ok := numericDate(claims.Raw["exp"])
	if !ok {
		return errors.New("oidc: id_token 缺少 exp 声明")
	}
	if !now.Before(exp.Add(clockSkew)) {
		return errors.New("oidc: id_token 已过期")
	}
	if nbf, ok := numericDate(claims.Raw["nbf"]); ok && now.Add(clockSkew).Before(nbf) {
		return errors.New("oidc: id_token 的 nbf 尚未生效")
	}
	if iat, ok := numericDate(claims.Raw["iat"]); ok && now.Add(clockSkew).Before(iat) {
		return errors.New("oidc: id_token 的 iat 在未来")
	}

	if expectedNonce != "" {
		if stringClaim(claims.Raw, "nonce") != expectedNonce {
			return errors.New("oidc: id_token 的 nonce 与本次登录不一致")
		}
	}
	return nil
}

// verificationKey 按 kid 取签名公钥；缓存未命中或已过期时刷新一次 JWKS 再试。
func (p *Provider) verificationKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	if pub := p.cachedKey(kid); pub != nil {
		return pub, nil
	}
	// 未知 kid（或缓存过期）时刷新一次；串行化避免惊群。
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	if pub := p.cachedKey(kid); pub != nil {
		return pub, nil
	}
	if err := p.fetchJWKS(ctx); err != nil {
		return nil, err
	}
	if pub := p.cachedKey(kid); pub != nil {
		return pub, nil
	}
	return nil, fmt.Errorf("oidc: JWKS 中没有 kid=%q 对应的签名公钥", kid)
}

// cachedKey 返回缓存中的公钥；缓存为空、超过 TTL 或缺少该 kid 时返回 nil。
// 头部没有 kid 时，只有公钥唯一才敢使用。
func (p *Provider) cachedKey(kid string) *rsa.PublicKey {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.keys == nil || p.now().Sub(p.keysFetchedAt) > jwksCacheTTL {
		return nil
	}
	if kid == "" {
		if len(p.keys) != 1 {
			return nil
		}
		for _, pub := range p.keys {
			return pub
		}
	}
	return p.keys[kid]
}

// fetchJWKS 拉取并替换公钥缓存。
func (p *Provider) fetchJWKS(ctx context.Context) error {
	var doc jwksDocument
	if err := p.getJSON(ctx, p.jwksURI, &doc); err != nil {
		return err
	}
	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		pub, err := k.rsaPublicKey()
		if err != nil {
			// 单个坏 key 不影响其它 key，跳过即可。
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return fmt.Errorf("oidc: JWKS 中没有可用的 RSA 签名公钥（共 %d 个 key）", len(doc.Keys))
	}
	p.mu.Lock()
	p.keys = keys
	p.keysFetchedAt = p.now()
	p.mu.Unlock()
	return nil
}

// rsaPublicKey 把 JWK 转成 *rsa.PublicKey；非 RSA 签名密钥返回错误。
func (k jwk) rsaPublicKey() (*rsa.PublicKey, error) {
	if !strings.EqualFold(k.Kty, "RSA") {
		return nil, fmt.Errorf("oidc: JWK 的 kty=%q 不是 RSA", k.Kty)
	}
	if k.Use != "" && !strings.EqualFold(k.Use, "sig") {
		return nil, fmt.Errorf("oidc: JWK 的 use=%q 不是签名用途", k.Use)
	}
	// alg 缺失视为可用于签名；显式声明了 PS*/RSA-OAEP 等本包不支持族的密钥直接跳过。
	if k.Alg != "" && !isRS256Family(k.Alg) {
		return nil, fmt.Errorf("oidc: JWK 的 alg=%q 不在支持范围内", k.Alg)
	}
	return parseRSAPublicKey(k.N, k.E)
}

// isRS256Family 判断 JWK 声明的算法是否属于 RSASSA-PKCS1-v1_5 签名族。
func isRS256Family(alg string) bool {
	switch strings.ToUpper(strings.TrimSpace(alg)) {
	case "RS256", "RS384", "RS512":
		return true
	default:
		return false
	}
}

// parseRSAPublicKey 用 JWK 的 n / e 构造 RSA 公钥。
func parseRSAPublicKey(nB64, eB64 string) (*rsa.PublicKey, error) {
	if nB64 == "" || eB64 == "" {
		return nil, errors.New("oidc: JWK 缺少 n 或 e")
	}
	nBytes, err := decodeBase64URL(nB64)
	if err != nil {
		return nil, errors.New("oidc: JWK 的 n 不是合法的 base64url")
	}
	eBytes, err := decodeBase64URL(eB64)
	if err != nil {
		return nil, errors.New("oidc: JWK 的 e 不是合法的 base64url")
	}
	if len(eBytes) == 0 || len(eBytes) > 8 {
		return nil, errors.New("oidc: JWK 的 e 长度不合法")
	}
	e := 0
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	if e < 3 || e%2 == 0 {
		return nil, errors.New("oidc: JWK 的 e 不合法")
	}
	n := new(big.Int).SetBytes(nBytes)
	if n.Sign() <= 0 {
		return nil, errors.New("oidc: JWK 的 n 不合法")
	}
	if n.BitLen() < minRSABits {
		return nil, fmt.Errorf("oidc: JWK 的 RSA 公钥只有 %d 位，低于 %d 位下限", n.BitLen(), minRSABits)
	}
	return &rsa.PublicKey{N: n, E: e}, nil
}

// getJSON 用 GET 拉取一个 JSON 文档，响应体大小受限。
func (p *Provider) getJSON(ctx context.Context, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("oidc: 构造请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("oidc: 请求 %s 失败: %w", rawURL, err)
	}
	defer resp.Body.Close()

	body, err := readLimited(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oidc: 请求 %s 返回 HTTP %d", rawURL, resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("oidc: 解析 %s 的响应失败: %w", rawURL, err)
	}
	return nil
}

// readLimited 读取响应体，超过 maxResponseBytes 直接报错。
func readLimited(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("oidc: 读取响应体失败: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("oidc: 响应体超过 %d 字节上限", maxResponseBytes)
	}
	return body, nil
}

// tokenEndpointError 把令牌端点的错误响应转成中文错误。
// 只保留 error / error_description，不回显响应体全文，避免带出令牌。
func tokenEndpointError(status int, body []byte) error {
	var payload struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &payload)
	if payload.Error == "" {
		return fmt.Errorf("oidc: 令牌端点返回 HTTP %d", status)
	}
	desc := strings.TrimSpace(payload.ErrorDescription)
	if len(desc) > 200 {
		desc = desc[:200]
	}
	if desc == "" {
		return fmt.Errorf("oidc: 令牌端点返回 HTTP %d，错误码 %q", status, payload.Error)
	}
	return fmt.Errorf("oidc: 令牌端点返回 HTTP %d，错误码 %q：%s", status, payload.Error, desc)
}

// checkEndpoint 校验 discovery 给出的端点必须是 http/https 绝对地址。
func checkEndpoint(name, raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("oidc: discovery 文档缺少 %s", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("oidc: discovery 文档中的 %s 不是合法 URL: %w", name, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("oidc: discovery 文档中的 %s 必须是 http/https 绝对地址", name)
	}
	return u.String(), nil
}

// normalizeScopes 去掉空白项；为空时返回默认 scope。
func normalizeScopes(scopes []string) []string {
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return []string{"openid", "email", "profile"}
	}
	return out
}

// audienceContains 判断 aud（字符串或数组）是否包含 clientID。
func audienceContains(aud any, clientID string) bool {
	switch v := aud.(type) {
	case string:
		return v == clientID
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s == clientID {
				return true
			}
		}
	case []string:
		for _, s := range v {
			if s == clientID {
				return true
			}
		}
	}
	return false
}

// stringClaim 读取字符串声明；缺失或类型不符时返回空串。
func stringClaim(raw map[string]any, key string) string {
	if s, ok := raw[key].(string); ok {
		return s
	}
	return ""
}

// boolClaim 读取布尔声明；部分 IdP 会发 "true"/"false" 字符串，这里一并容忍。
func boolClaim(raw map[string]any, key string) bool {
	switch v := raw[key].(type) {
	case bool:
		return v
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		return err == nil && b
	default:
		return false
	}
}

// numericDate 解析 NumericDate 声明，容忍数字与纯数字字符串两种形式。
func numericDate(v any) (time.Time, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return time.Time{}, false
		}
		return unixFromFloat(f), true
	case float64:
		return unixFromFloat(t), true
	case int64:
		return time.Unix(t, 0).UTC(), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return time.Time{}, false
		}
		return unixFromFloat(f), true
	default:
		return time.Time{}, false
	}
}

// unixFromFloat 把秒（可含小数）转成 time.Time。
func unixFromFloat(f float64) time.Time {
	sec := int64(f)
	nsec := int64((f - float64(sec)) * float64(time.Second))
	return time.Unix(sec, nsec).UTC()
}

// decodeBase64URL 解码 base64url；同时容忍带 padding 的实现。
func decodeBase64URL(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

// NewCodeVerifier 生成 PKCE code_verifier（43-128 字符，RFC 7636）。
func NewCodeVerifier() (string, error) {
	b := make([]byte, codeVerifierBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oidc: 生成 PKCE code_verifier 失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// CodeChallengeS256 返回 BASE64URL(SHA256(verifier))，不带 padding。
func CodeChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// NewState 生成随机 state / nonce（32 字节，base64url）。
func NewState() (string, error) {
	b := make([]byte, randomTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oidc: 生成随机 state/nonce 失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
