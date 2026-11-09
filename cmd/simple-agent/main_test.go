package main

import (
	"bytes"
	"strings"
	"testing"

	"simple-agent/internal/config"
)

// --version / --help 必须在拿单实例锁之前处理：已经有一个实例在跑时，
// 它们仍然应该立刻打印并退出，而不是弹「已在运行」。
func TestHandleEarlyArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"version", []string{"--version"}, "simple-agent " + version},
		{"version 短参数", []string{"-v"}, "simple-agent " + version},
		{"help", []string{"--help"}, "用法：simple-agent"},
		{"help 短参数", []string{"-h"}, "用法：simple-agent"},
		{"版本在其它参数之后", []string{"--workspace", ".", "--version"}, "simple-agent " + version},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buffer bytes.Buffer
			if !handleEarlyArgs(c.args, &buffer) {
				t.Fatalf("%v 应被提前处理", c.args)
			}
			if !strings.Contains(buffer.String(), c.want) {
				t.Fatalf("输出应包含 %q，实际 %q", c.want, buffer.String())
			}
		})
	}

	// 普通启动参数不触发提前退出
	var buffer bytes.Buffer
	if handleEarlyArgs([]string{"--allow-shell", "--workspace", "."}, &buffer) {
		t.Fatalf("普通参数不应提前退出，输出 %q", buffer.String())
	}

	// 帮助信息里要有版本/配置说明，避免打印空内容
	if !strings.Contains(config.Help(), "--allow-shell") {
		t.Fatal("帮助信息应包含参数说明")
	}
}
