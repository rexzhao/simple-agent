package providers_test

import (
	"os"
	"testing"

	"simple-agent/internal/locale"
)

// 测试固定中文：模拟提供商的引导文案随界面语言。
func TestMain(m *testing.M) {
	locale.SetCurrent(locale.ZH)
	os.Exit(m.Run())
}
