package web

import (
	"encoding/hex"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// csrfKeyFile 是持久化 CSRF 派生密钥的文件名（位于数据目录，0600）。
const csrfKeyFile = ".csrf-key"

// loadCSRFKey 读取（必要时生成并落盘）CSRF 派生密钥。
//
// 密钥必须跨进程重启保持稳定：会话存在数据库里、重启后依然有效，而
// 如果派生密钥每次启动都重新随机，重启前打开的页面里所有 POST（包括
// 「退出」按钮）都会因令牌不匹配而 403——表现为「无法退出登录」。
func loadCSRFKey(dataDir string) []byte {
	path := filepath.Join(dataDir, csrfKeyFile)
	if raw, err := os.ReadFile(path); err == nil {
		if key, derr := hex.DecodeString(strings.TrimSpace(string(raw))); derr == nil && len(key) >= 32 {
			return key
		}
		log.Printf("[web] %s 内容不可用，将重新生成", path)
	}
	key := randBytes(32)
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		// 落盘失败只影响「重启后旧页面仍可用」，不应阻断启动。
		log.Printf("[web] 无法写入 CSRF 密钥 %s: %v（重启后旧页面需刷新）", path, err)
	}
	return key
}
