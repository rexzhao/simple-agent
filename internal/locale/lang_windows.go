//go:build windows

package locale

import (
	"os"
	"strings"
	"syscall"
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procGetUserDefaultUILanguage = kernel32.NewProc("GetUserDefaultUILanguage")
)

// Detect 返回跟随系统时的界面语言。
//
// Windows 上用 GetUserDefaultUILanguage 的 LANGID：低 10 位（primary language id）
// 等于 0x04 就是中文 —— 覆盖 0x0804（简中）、0x0404（繁中）、0x0C04（港繁）、
// 0x1004（新加坡简中）。拿不到时退回 LANG 环境变量，再退回英文。
func Detect() Lang {
	if langID, _, _ := procGetUserDefaultUILanguage.Call(); langID != 0 {
		if uint32(langID)&0x3FF == 0x04 {
			return ZH
		}
		return EN
	}
	return detectFromEnv()
}

// detectFromEnv 看 LANG/LC_ALL 前缀（兜底路径，也方便测试与 CI）。
func detectFromEnv() Lang {
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if value := strings.ToLower(strings.TrimSpace(os.Getenv(key))); value != "" {
			if strings.HasPrefix(value, "zh") {
				return ZH
			}
			return EN
		}
	}
	return EN
}
