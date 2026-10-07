package storage

import (
	"os"
	"path/filepath"
	"strings"
)

// CleanupTemp 清理上次运行残留的临时目录（.upload-* 解压暂存、.old-* 交换备份）。
// 服务启动时调用：解压 / 交换中途崩溃会留下这些目录，它们不在项目目录内、
// 不参与配额统计，长期堆积会白占磁盘。
func (s *Storage) CleanupTemp() (int, error) {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() {
			continue
		}
		if !strings.HasPrefix(name, ".upload-") && !strings.HasPrefix(name, ".old-") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.Root, name)); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
