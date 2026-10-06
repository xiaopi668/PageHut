package storage

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStorage(t *testing.T) (*Storage, int64) {
	t.Helper()
	root := t.TempDir()
	s := New(root)
	if err := s.Create(1); err != nil {
		t.Fatalf("创建项目目录: %v", err)
	}
	return s, 1
}

func buildZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("写入 zip 条目 %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractZipOK(t *testing.T) {
	s, id := newTestStorage(t)
	data := buildZip(t, map[string]string{
		"index.html":          "<h1>hi</h1>",
		"assets/css/main.css": "body{}",
	})
	files, size, err := s.ExtractZip(id, bytes.NewReader(data), 1<<20)
	if err != nil {
		t.Fatalf("正常解压失败: %v", err)
	}
	if files != 2 || size != int64(len("<h1>hi</h1>")+len("body{}")) {
		t.Fatalf("统计不对: files=%d size=%d", files, size)
	}
	for _, p := range []string{"index.html", "assets/css/main.css"} {
		if _, err := os.Stat(filepath.Join(s.Dir(id), p)); err != nil {
			t.Fatalf("缺少文件 %s: %v", p, err)
		}
	}
}

func TestExtractZipReplacesContent(t *testing.T) {
	s, id := newTestStorage(t)
	first := buildZip(t, map[string]string{"old.html": "old"})
	if _, _, err := s.ExtractZip(id, bytes.NewReader(first), 1<<20); err != nil {
		t.Fatal(err)
	}
	second := buildZip(t, map[string]string{"index.html": "new"})
	if _, _, err := s.ExtractZip(id, bytes.NewReader(second), 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(id), "old.html")); !os.IsNotExist(err) {
		t.Fatal("旧内容应被整体替换")
	}
	if _, err := os.Stat(filepath.Join(s.Dir(id), "index.html")); err != nil {
		t.Fatal("新内容缺失")
	}
}

func TestZipSlip(t *testing.T) {
	s, id := newTestStorage(t)
	data := buildZip(t, map[string]string{"../evil.txt": "boom"})
	_, _, err := s.ExtractZip(id, bytes.NewReader(data), 1<<20)
	if err == nil {
		t.Fatal("包含 ../ 的 zip 应被拒绝")
	}
	escaped := filepath.Join(s.Root, "evil.txt")
	if _, err := os.Stat(escaped); !os.IsNotExist(err) {
		t.Fatal("不应在项目目录外写入文件")
	}
}

func TestZipSymlinkSkipped(t *testing.T) {
	s, id := newTestStorage(t)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	fh := &zip.FileHeader{Name: "link"}
	fh.SetMode(os.ModeSymlink | 0o777)
	w, _ := zw.CreateHeader(fh)
	w.Write([]byte("/etc/passwd"))
	zw.Close()
	if _, _, err := s.ExtractZip(id, bytes.NewReader(buf.Bytes()), 1<<20); err != nil {
		t.Fatalf("符号链接应被跳过而不报错: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(s.Dir(id), "link")); !os.IsNotExist(err) {
		t.Fatal("符号链接条目不应落盘")
	}
}

func TestZipAbsolutePathRejected(t *testing.T) {
	s, id := newTestStorage(t)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("/abs.txt")
	w.Write([]byte("x"))
	zw.Close()
	if _, _, err := s.ExtractZip(id, bytes.NewReader(buf.Bytes()), 1<<20); err == nil {
		t.Fatal("绝对路径条目应导致整个 zip 被拒绝")
	}
}

func TestZipTooLarge(t *testing.T) {
	s, id := newTestStorage(t)
	data := buildZip(t, map[string]string{"big.bin": strings.Repeat("A", 4096)})
	_, _, err := s.ExtractZip(id, bytes.NewReader(data), 1024)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("应返回 ErrTooLarge，实际: %v", err)
	}
	entries, _ := os.ReadDir(s.Dir(id))
	if len(entries) != 0 {
		t.Fatal("失败后项目内容应保持为空（原子替换）")
	}
}

func TestPathTraversalProtection(t *testing.T) {
	s, id := newTestStorage(t)
	if err := s.WriteFile(id, "../outside.txt", []byte("x")); !errors.Is(err, ErrBadPath) {
		t.Fatalf("越界写入应被拒绝: %v", err)
	}
	if err := s.Remove(id, "."); !errors.Is(err, ErrForbidden) {
		t.Fatalf("删除根目录应被拒绝: %v", err)
	}
	if err := s.WriteFile(id, "a/../../b.txt", []byte("x")); !errors.Is(err, ErrBadPath) {
		t.Fatalf("嵌套越界应被拒绝: %v", err)
	}
	// 正常写读删
	if err := s.WriteFile(id, "dir/sub/file.txt", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadFile(id, "dir/sub/file.txt")
	if err != nil || string(got) != "hello" {
		t.Fatalf("读回不一致: %s %v", got, err)
	}
	if err := s.Remove(id, "dir/sub/file.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestUsage(t *testing.T) {
	s, id := newTestStorage(t)
	data := buildZip(t, map[string]string{"a.txt": "12345", "b/c.txt": "123"})
	if _, _, err := s.ExtractZip(id, bytes.NewReader(data), 1<<20); err != nil {
		t.Fatal(err)
	}
	files, size, err := s.Usage(id)
	if err != nil {
		t.Fatal(err)
	}
	if files != 2 || size != 8 {
		t.Fatalf("Usage 统计错误: files=%d size=%d", files, size)
	}
}
