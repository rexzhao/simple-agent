package driver_test

import (
	"os"
	"testing"

	"simple-agent/internal/locale"
)

// 驱动测试固定中文：通知文案按"当前界面语言"写入，默认跟随系统（CI 上可能是英文）。
func TestMain(m *testing.M) {
	locale.SetCurrent(locale.ZH)
	os.Exit(m.Run())
}
