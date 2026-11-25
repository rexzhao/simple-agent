package tlsx_test

import (
	"strings"
	"testing"

	"simple-agent/internal/tlsx"
)

// **配了证书/指纹但没开 ClientTLSEnabled 时必须报错，不能静默走明文**。
//
// 这是真实踩到的坑：直连执行端时只配了 DataDir（自己的证书）和超时，
// 忘了 ClientTLSEnabled。ClientTLS() 为 false，于是发了一条**明文**连接
// 过去 —— 执行端只看到"没有出示客户端证书"就拒绝，而调用方完全不知道
// 自己发的是明文，日志里也看不出。
//
// 静默降级是加密功能最危险的失败模式：看起来配了，实际没加密。
func TestDialRefusesPlaintextWhenTLSConfigured(t *testing.T) {
	cfg := tlsx.Config{
		DataDir: t.TempDir(), // 配了证书目录，但没开 ClientTLSEnabled
	}
	_, err := cfg.Dial("127.0.0.1:1")
	if err == nil {
		t.Fatal("应报错而不是降级成明文")
	}
	if !strings.Contains(err.Error(), "明文") {
		t.Fatalf("错误里应说明是明文问题，实际：%v", err)
	}
}

// 真正想用明文时（所有 TLS 字段为空）仍应放行 —— 老用法不能被破坏。
//
// 这里连一个必然失败的地址，只要错误是"连不上"而不是"拒绝明文"就说明
// 走的是 net.Dial 分支。
func TestDialAllowsPlaintextWhenNothingConfigured(t *testing.T) {
	cfg := tlsx.Config{}
	_, err := cfg.Dial("127.0.0.1:1")
	if err == nil {
		t.Fatal("连一个关闭的端口应该失败")
	}
	if strings.Contains(err.Error(), "拒绝以明文") {
		t.Fatalf("零配置应允许明文，实际：%v", err)
	}
}