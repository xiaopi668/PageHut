package sitefs

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newSite(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>home</h1>"), 0o644)
	os.WriteFile(filepath.Join(dir, "404.html"), []byte("<h1>custom404</h1>"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "index.html"), []byte("<h1>subpage</h1>"), 0o644)
	os.WriteFile(filepath.Join(dir, "data.json"), []byte(`{"a":1}`), 0o644)
	return dir
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestServeIndex(t *testing.T) {
	h := Handler(newSite(t))
	rec := get(h, "/")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "home") {
		t.Fatalf("首页服务异常: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("Content-Type 错误: %s", ct)
	}
}

func TestServeSubDirIndexAndRedirect(t *testing.T) {
	h := Handler(newSite(t))
	rec := get(h, "/sub/")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "subpage") {
		t.Fatalf("子目录 index 服务异常: %d", rec.Code)
	}
	rec = get(h, "/sub")
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/sub/" {
		t.Fatalf("目录应 301 补斜杠: %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

func TestCustom404(t *testing.T) {
	h := Handler(newSite(t))
	rec := get(h, "/missing.html")
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "custom404") {
		t.Fatalf("自定义 404 异常: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMIME(t *testing.T) {
	h := Handler(newSite(t))
	rec := get(h, "/data.json")
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("json MIME 错误: %s", ct)
	}
}

func TestNoDirectoryListing(t *testing.T) {
	h := Handler(newSite(t))
	rec := get(h, "/empty-dir/")
	if rec.Code != 404 {
		t.Fatalf("空目录应 404（不做列表）: %d", rec.Code)
	}
}

func TestTraversalSafe(t *testing.T) {
	// 在站点目录外放一个秘密文件
	base := t.TempDir()
	dir := filepath.Join(base, "site")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(base, "secret.txt"), []byte("top secret"), 0o644)
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("ok"), 0o644)

	h := Handler(dir)
	for _, p := range []string{"/../secret.txt", "/..%2fsecret.txt", "/%2e%2e/secret.txt"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if strings.Contains(rec.Body.String(), "top secret") {
			t.Fatalf("路径穿越未被拦截: %s", p)
		}
	}
}
