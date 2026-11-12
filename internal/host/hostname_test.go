package host

import (
	"os"
	"testing"
)

// 执行端握手要报告**机器名**，而不是写死的 "local"。
//
// 以前写死 "local" 时，展示端连两台机器看到的都是 "local"，完全分不清连的是谁。
func TestDisplayNameUsesHostname(t *testing.T) {
	local := NewLocal(Config{})
	t.Cleanup(func() { _ = local.Close() })

	got := local.displayName()
	want, err := os.Hostname()
	if err != nil || want == "" {
		t.Skip("拿不到机器名")
	}
	if got != want {
		t.Fatalf("未配置 Name 时应回落到机器名 %q，实际 %q", want, got)
	}
	if got == "local" {
		t.Fatal("不该再返回写死的 local")
	}
}

// 配置了 Name 时优先用它（`--name` / 注册 relay 时的 host_id）。
func TestDisplayNamePrefersConfig(t *testing.T) {
	local := NewLocal(Config{Name: "my-pc"})
	t.Cleanup(func() { _ = local.Close() })

	if got := local.displayName(); got != "my-pc" {
		t.Fatalf("期望配置里的名字 my-pc，实际 %q", got)
	}
}
