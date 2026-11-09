//go:build !windows

package locale

import (
	"os"
	"strings"
)

// Detect 返回跟随系统时的界面语言：非 Windows 只看 LANG/LC_ALL 前缀。
func Detect() Lang {
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
