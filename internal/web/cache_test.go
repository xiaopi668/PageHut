package web

import (
	"net/http"
	"testing"
)

// TestStaticAssetsAreCacheSafe 覆盖一个真实事故：/static/app.css 没有任何
// 缓存头，浏览器在面板升级后继续用旧样式渲染新页面（注册页验证码行错位）。
// 现在静态资源必须带 ETag + no-cache，面板页面必须 no-store。
func TestStaticAssetsAreCacheSafe(t *testing.T) {
	env := newTestEnv(t)
	cl := newClient(t)

	resp := env.get(cl, "/static/app.css")
	body(t, resp) // 读完并关闭
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/static/app.css 应 200，实际 %d", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Error("静态资源必须带 ETag，否则升级后浏览器会一直复用旧 CSS")
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("静态资源 Cache-Control 应为 no-cache，实际 %q", cc)
	}

	// 带 If-None-Match 重新校验应返回 304（内容没变时不重传）
	req, err := http.NewRequest(http.MethodGet, env.srv.URL+"/static/app.css", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("If-None-Match", etag)
	again, err := cl.Do(req)
	if err != nil {
		t.Fatalf("重校验请求失败: %v", err)
	}
	defer again.Body.Close()
	if again.StatusCode != http.StatusNotModified {
		t.Errorf("带 If-None-Match 应 304，实际 %d", again.StatusCode)
	}

	// 面板页面不缓存
	page := env.get(cl, "/login")
	defer page.Body.Close()
	if cc := page.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("面板页面 Cache-Control 应为 no-store，实际 %q", cc)
	}
}
