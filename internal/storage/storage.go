// Package storage 管理项目站点文件在磁盘上的存储。
// 目录布局：数据目录/sites/<项目ID>/...
package storage

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

var (
	ErrTooLarge  = errors.New("超出项目大小限制")
	ErrTooMany   = errors.New("文件数量超出限制")
	ErrBadPath   = errors.New("非法路径")
	ErrForbidden = errors.New("不允许该操作")
)

// MaxFiles 单个项目允许的文件数上限（防 zip 炸弹的兜底）。
const MaxFiles = 20000

// MaxEditSize 在线编辑允许打开的最大文本文件。
const MaxEditSize = 4 << 20 // 4MB

type Storage struct {
	Root string
}

func New(dataDir string) *Storage {
	return &Storage{Root: filepath.Join(dataDir, "sites")}
}

func (s *Storage) Dir(id int64) string {
	return filepath.Join(s.Root, strconv.FormatInt(id, 10))
}

func (s *Storage) Create(id int64) error {
	return os.MkdirAll(s.Dir(id), 0o755)
}

func (s *Storage) Destroy(id int64) error {
	return os.RemoveAll(s.Dir(id))
}

// resolve 将项目内相对路径映射为磁盘绝对路径，拒绝越界访问。
func (s *Storage) resolve(id int64, rel string) (string, error) {
	root := s.Dir(id)
	rel = strings.Trim(filepath.FromSlash(rel), "/")
	if rel == "" {
		return root, nil
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return "", ErrBadPath
		}
	}
	target := filepath.Join(root, filepath.FromSlash(rel))
	// 双保险：归一化后必须仍位于项目目录内
	if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
		return "", ErrBadPath
	}
	return target, nil
}

// ExtractZip 用 zip 内容整体替换项目内容，返回（文件数, 总字节数）。
// 解压前先写入临时目录，全部校验通过后再原子替换，失败不留半成品。
func (s *Storage) ExtractZip(id int64, r io.Reader, maxTotal int64) (int, int64, error) {
	if maxTotal <= 0 {
		return 0, 0, ErrTooLarge
	}
	tmp, err := os.MkdirTemp(s.Root, ".upload-*")
	if err != nil {
		return 0, 0, err
	}
	defer os.RemoveAll(tmp)

	zipPath := filepath.Join(tmp, "archive.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		return 0, 0, err
	}
	// 多读 1 字节用于判断超限
	n, err := io.Copy(f, io.LimitReader(r, maxTotal+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, 0, fmt.Errorf("读取上传内容失败: %w", err)
	}
	if n > maxTotal {
		return 0, 0, ErrTooLarge
	}
	if n == 0 {
		return 0, 0, errors.New("上传内容为空")
	}

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return 0, 0, fmt.Errorf("无法读取 zip 文件: %w", err)
	}
	defer zr.Close()

	outDir := filepath.Join(tmp, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return 0, 0, err
	}

	totalFiles, totalSize := 0, int64(0)
	for _, zf := range zr.File {
		name := decodeZipName(zf.FileHeader)
		clean := path.Clean(name)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
			return 0, 0, fmt.Errorf("zip 内包含非法路径 %q", name)
		}
		if clean == "__MACOSX" || strings.HasPrefix(clean, "__MACOSX/") ||
			clean == ".DS_Store" || strings.HasSuffix(clean, "/.DS_Store") {
			continue
		}
		if zf.Mode()&os.ModeSymlink != 0 {
			continue // 拒绝符号链接，防逃逸
		}
		target := filepath.Join(outDir, filepath.FromSlash(clean))
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return 0, 0, err
			}
			continue
		}
		if totalFiles >= MaxFiles {
			return 0, 0, ErrTooMany
		}
		totalFiles++
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return 0, 0, err
		}
		// 先落盘到临时文件（顺带计数限流），再改名到目标位置
		rc, err := zf.Open()
		if err != nil {
			return 0, 0, err
		}
		st, err := os.CreateTemp(tmp, "entry-*")
		if err != nil {
			rc.Close()
			return 0, 0, err
		}
		remaining := maxTotal - totalSize
		n, err := io.Copy(st, io.LimitReader(rc, remaining+1))
		rc.Close()
		st.Close()
		if err != nil {
			return 0, 0, err
		}
		if n > remaining {
			return 0, 0, ErrTooLarge
		}
		if err := os.Chmod(st.Name(), 0o644); err != nil {
			return 0, 0, err
		}
		if err := os.Rename(st.Name(), target); err != nil {
			return 0, 0, err
		}
		totalSize += n
	}

	pdir := s.Dir(id)
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		return 0, 0, err
	}
	old, err := os.ReadDir(pdir)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range old {
		if err := os.RemoveAll(filepath.Join(pdir, e.Name())); err != nil {
			return 0, 0, err
		}
	}
	outEntries, err := os.ReadDir(outDir)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range outEntries {
		if err := os.Rename(filepath.Join(outDir, e.Name()), filepath.Join(pdir, e.Name())); err != nil {
			return 0, 0, err
		}
	}
	return totalFiles, totalSize, nil
}

// decodeZipName 处理 Windows 下打包产生的 GBK 文件名。
func decodeZipName(h zip.FileHeader) string {
	if !utf8.ValidString(h.Name) {
		if dec, err := simplifiedchinese.GBK.NewDecoder().String(h.Name); err == nil {
			return dec
		}
	}
	return h.Name
}

// Entry 文件管理器列表项。
type Entry struct {
	Name    string
	Size    int64
	ModTime int64
	IsDir   bool
}

// ListDir 列出项目内目录，目录在前、按名称排序。
func (s *Storage) ListDir(id int64, rel string) ([]*Entry, error) {
	p, err := s.resolve(id, rel)
	if err != nil {
		return nil, err
	}
	items, err := os.ReadDir(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fs.ErrNotExist
		}
		return nil, err
	}
	out := make([]*Entry, 0, len(items))
	for _, it := range items {
		info, err := it.Info()
		if err != nil {
			continue
		}
		out = append(out, &Entry{
			Name:    it.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime().Unix(),
			IsDir:   it.IsDir(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// OpenFile 打开项目内文件供流式读取（静态站点服务用）。
func (s *Storage) OpenFile(id int64, rel string) (*os.File, fs.FileInfo, error) {
	p, err := s.resolve(id, rel)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		f.Close()
		return nil, nil, fs.ErrNotExist
	}
	return f, fi, nil
}

// Stat 返回项目内路径的文件信息。
func (s *Storage) Stat(id int64, rel string) (fs.FileInfo, error) {
	p, err := s.resolve(id, rel)
	if err != nil {
		return nil, err
	}
	return os.Stat(p)
}

// ReadFile 读取项目内文件（受 MaxEditSize 限制，供在线编辑）。
func (s *Storage) ReadFile(id int64, rel string) ([]byte, error) {
	fi, err := s.Stat(id, rel)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, ErrForbidden
	}
	if fi.Size() > MaxEditSize {
		return nil, errors.New("文件过大，不支持在线编辑")
	}
	p, err := s.resolve(id, rel)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// WriteFile 写入项目内文件（自动创建父目录，覆盖同名文件）。
func (s *Storage) WriteFile(id int64, rel string, data []byte) error {
	p, err := s.resolve(id, rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// Mkdir 在项目内创建目录（含父目录）。
func (s *Storage) Mkdir(id int64, rel string) error {
	p, err := s.resolve(id, rel)
	if err != nil {
		return err
	}
	if p == s.Dir(id) {
		return ErrForbidden
	}
	return os.MkdirAll(p, 0o755)
}

// Remove 删除项目内文件或目录（不允许删除根目录）。
func (s *Storage) Remove(id int64, rel string) error {
	if rel == "" || rel == "." || rel == "/" {
		return ErrForbidden
	}
	p, err := s.resolve(id, rel)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); err != nil {
		return fs.ErrNotExist
	}
	return os.RemoveAll(p)
}

// Usage 统计项目文件的个数与总字节数。
func (s *Storage) Usage(id int64) (int, int64, error) {
	root := s.Dir(id)
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	var files int
	var size int64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		files++
		info, err := d.Info()
		if err != nil {
			return err
		}
		size += info.Size()
		return nil
	})
	return files, size, err
}
