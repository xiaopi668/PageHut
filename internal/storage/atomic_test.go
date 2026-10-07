package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sitesRoot 返回临时数据目录下的 sites 根。
func sitesRoot(dataDir string) string { return filepath.Join(dataDir, "sites") }

// assertNoTempDirs 断言没有残留的 .upload-* / .old-* 临时目录。
func assertNoTempDirs(t *testing.T, dataDir string) {
	t.Helper()
	entries, err := os.ReadDir(sitesRoot(dataDir))
	if err != nil {
		t.Fatalf("读取 sites 目录失败: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".upload-") || strings.HasPrefix(e.Name(), ".old-") {
			t.Errorf("残留临时目录: %s", e.Name())
		}
	}
}

// TestExtractZipKeepsOldContentOnFailure 验证解压失败时旧内容原样保留：
// 旧实现会先删光旧文件再逐个搬新文件，一旦中途出错项目就空了。
func TestExtractZipKeepsOldContentOnFailure(t *testing.T) {
	dataDir := t.TempDir()
	s := New(dataDir)
	if err := s.Create(1); err != nil {
		t.Fatalf("创建项目目录失败: %v", err)
	}
	if err := s.WriteFile(1, "index.html", []byte("OLD-CONTENT")); err != nil {
		t.Fatalf("写入旧内容失败: %v", err)
	}

	// 含 zip-slip 路径的压缩包，必然在搬移之前就被拒绝
	bad := buildZip(t, map[string]string{"../evil.txt": "x"})
	if _, _, err := s.ExtractZip(1, bytes.NewReader(bad), 1<<20); err == nil {
		t.Fatal("非法路径的 zip 应当解压失败")
	}

	got, err := s.ReadFile(1, "index.html")
	if err != nil {
		t.Fatalf("旧内容应仍可读: %v", err)
	}
	if string(got) != "OLD-CONTENT" {
		t.Fatalf("旧内容被破坏: %q", got)
	}
	assertNoTempDirs(t, dataDir)
}

// TestExtractZipSwapsWholeDirectory 验证成功解压后是整体替换。
func TestExtractZipSwapsWholeDirectory(t *testing.T) {
	dataDir := t.TempDir()
	s := New(dataDir)
	if err := s.Create(1); err != nil {
		t.Fatalf("创建项目目录失败: %v", err)
	}
	if err := s.WriteFile(1, "stale.html", []byte("STALE")); err != nil {
		t.Fatalf("写入旧内容失败: %v", err)
	}

	z := buildZip(t, map[string]string{"index.html": "NEW", "sub/page.html": "SUB"})
	n, size, err := s.ExtractZip(1, bytes.NewReader(z), 1<<20)
	if err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	if n != 2 || size != int64(len("NEW")+len("SUB")) {
		t.Fatalf("统计异常: files=%d size=%d", n, size)
	}

	if _, err := s.ReadFile(1, "stale.html"); err == nil {
		t.Error("旧文件应当被整体替换掉")
	}
	for _, name := range []string{"index.html", "sub/page.html"} {
		if _, err := s.ReadFile(1, name); err != nil {
			t.Errorf("新文件 %s 缺失: %v", name, err)
		}
	}
	assertNoTempDirs(t, dataDir)
}

// TestCleanupTemp 验证启动时能清掉上次崩溃残留的临时目录，且不误删项目目录。
func TestCleanupTemp(t *testing.T) {
	dataDir := t.TempDir()
	s := New(dataDir)
	if err := s.Create(7); err != nil {
		t.Fatalf("创建项目目录失败: %v", err)
	}
	for _, name := range []string{".upload-abc", ".old-xyz"} {
		if err := os.MkdirAll(filepath.Join(sitesRoot(dataDir), name), 0o755); err != nil {
			t.Fatalf("构造残留目录失败: %v", err)
		}
	}

	n, err := s.CleanupTemp()
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("应清理 2 个残留目录，实际 %d", n)
	}
	if _, err := os.Stat(s.Dir(7)); err != nil {
		t.Errorf("项目目录不应被清理: %v", err)
	}
	assertNoTempDirs(t, dataDir)
}
