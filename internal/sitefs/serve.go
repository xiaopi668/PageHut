// Package sitefs 把磁盘上的项目目录作为静态网站对外服务。
package sitefs

import (
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// builtinMime 保证在精简容器（缺 /etc/mime.types）下也能返回正确的 Content-Type。
var builtinMime = map[string]string{
	".html":        "text/html; charset=utf-8",
	".htm":         "text/html; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".json":        "application/json; charset=utf-8",
	".map":         "application/json; charset=utf-8",
	".txt":         "text/plain; charset=utf-8",
	".md":          "text/plain; charset=utf-8",
	".xml":         "application/xml; charset=utf-8",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".gif":         "image/gif",
	".webp":        "image/webp",
	".avif":        "image/avif",
	".ico":         "image/x-icon",
	".bmp":         "image/bmp",
	".woff":        "font/woff",
	".woff2":       "font/woff2",
	".ttf":         "font/ttf",
	".otf":         "font/otf",
	".eot":         "application/vnd.ms-fontobject",
	".wasm":        "application/wasm",
	".mp3":         "audio/mpeg",
	".ogg":         "audio/ogg",
	".wav":         "audio/wav",
	".mp4":         "video/mp4",
	".webm":        "video/webm",
	".pdf":         "application/pdf",
	".zip":         "application/zip",
	".csv":         "text/csv; charset=utf-8",
	".webmanifest": "application/manifest+json; charset=utf-8",
	".rss":         "application/rss+xml",
	".atom":        "application/atom+xml",
}

// ContentType 根据文件名推断 Content-Type。
func ContentType(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if ct, ok := builtinMime[ext]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// textExts 是允许在线编辑的文本扩展名。
var textExts = map[string]bool{
	".html": true, ".htm": true, ".css": true, ".js": true, ".mjs": true,
	".json": true, ".map": true, ".txt": true, ".md": true, ".xml": true,
	".svg": true, ".csv": true, ".yml": true, ".yaml": true, ".toml": true,
	".ini": true, ".conf": true, ".webmanifest": true, ".gitignore": true,
	".htaccess": true, ".env": true, ".log": true, ".ts": true, ".jsx": true,
	".tsx": true, ".scss": true, ".less": true,
}

// IsTextFile 判断是否为可在线编辑的文本文件。
func IsTextFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	if ext == "" {
		return strings.HasPrefix(strings.ToLower(path.Base(name)), ".")
	}
	return textExts[ext]
}

// Handler 返回以 dir 为根的静态站点处理器。
// 规则：目录请求自动补全 index.html；支持项目自定义 404.html；不做目录列表。
func Handler(dir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := path.Clean("/" + r.URL.Path) // 强制 rooted，天然去除 ..
		target := filepath.Join(dir, filepath.FromSlash(rel))

		fi, err := os.Stat(target)
		if err == nil && fi.IsDir() {
			if !strings.HasSuffix(r.URL.Path, "/") {
				http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
				return
			}
			target = filepath.Join(target, "index.html")
			fi, err = os.Stat(target)
		}
		if err != nil {
			serveNotFound(w, dir)
			return
		}

		f, err := os.Open(target)
		if err != nil {
			serveNotFound(w, dir)
			return
		}
		defer f.Close()

		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Type", ContentType(target))
		http.ServeContent(w, r, path.Base(target), fi.ModTime(), f)
	})
}

func serveNotFound(w http.ResponseWriter, dir string) {
	data, err := os.ReadFile(filepath.Join(dir, "404.html"))
	if err == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		w.Write(data)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	io.WriteString(w, "404 page not found\n")
}
