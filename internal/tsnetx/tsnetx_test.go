package tsnetx_test

import (
	"os"
	"path/filepath"
	"testing"

	"simple-agent/internal/tsnetx"
)

// 身份目录必须按角色分开：执行端与 relay 共用目录会互相覆盖身份，
// tailnet 里就会出现"陌生设备"，而不是"重启还是同一台"。
func TestStateDirIsPerRole(t *testing.T) {
	dir := t.TempDir()
	roles := []tsnetx.Role{tsnetx.RoleExecutor, tsnetx.RoleViewer, tsnetx.RoleRelay}
	seen := map[string]bool{}
	for _, role := range roles {
		path := tsnetx.StateDirFor(tsnetx.Config{Role: role, DataDir: dir})
		if path == "" {
			t.Fatalf("%s 的状态目录不该为空", role)
		}
		if seen[path] {
			t.Fatalf("不同角色复用了同一个状态目录：%s", path)
		}
		seen[path] = true
		if filepath.Dir(path) != filepath.Join(dir, "tailscale") {
			t.Fatalf("状态目录应在 <data_dir>/tailscale 下：%s", path)
		}
	}
}

// 显式指定 StateDir 必须原样使用（用户可能想放到别处持久化，跨 data_dir 变更保留）。
func TestStateDirOverride(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "my-ts")
	cfg := tsnetx.Config{Role: tsnetx.RoleExecutor, StateDir: custom, DataDir: "/should/not/matter"}
	if got := tsnetx.StateDirFor(cfg); got != custom {
		t.Fatalf("显式 StateDir 应原样使用，实际 %s", got)
	}
}

// 节点名必须 DNS 安全（tsnet 会拒绝非法主机名），且默认带角色后缀。
func TestHostname(t *testing.T) {
	cases := []struct {
		name string
		cfg  tsnetx.Config
		want string
	}{
		{"大写转小写", tsnetx.Config{Hostname: "My-PC"}, "my-pc"},
		// 空格→'-'，非 ASCII 的"的"被丢弃，"_"→'-'："Rex 的 PC_x" → "rex--pc-x"
		{"非法字符", tsnetx.Config{Hostname: "Rex 的 PC_x"}, "rex--pc-x"},
		{"下划线", tsnetx.Config{Hostname: "a_b"}, "a-b"},
		{"首尾连字符被去掉", tsnetx.Config{Hostname: "-abc-"}, "abc"},
		{"空名兜底", tsnetx.Config{Hostname: "的"}, "simple-agent"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := tsnetx.HostnameFor(item.cfg); got != item.want {
				t.Fatalf("期望 %q，实际 %q", item.want, got)
			}
		})
	}

	// 默认名带角色后缀：同一台机器上的执行端与 relay 不能同名
	exec := tsnetx.HostnameFor(tsnetx.Config{Role: tsnetx.RoleExecutor})
	relayStart := tsnetx.HostnameFor(tsnetx.Config{Role: tsnetx.RoleRelay})
	if exec == relayStart {
		t.Fatalf("执行端与 relay 的默认节点名不能相同：%s", exec)
	}
}

// 没有状态、也没有 authkey 时，New 不该 panic（只提示"可能要人工授权"），
// 并且必须真的把状态目录建出来。
func TestNewCreatesStateDir(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	server, err := tsnetx.New(tsnetx.Config{
		Role:     tsnetx.RoleViewer,
		StateDir: stateDir,
		Hostname: "test-node",
		// 故意不给 AuthKey：验证"提示而不是 panic"
	})
	if err != nil {
		t.Fatalf("New 失败：%v", err)
	}
	defer func() { _ = server.Close() }()

	if _, statErr := os.Stat(stateDir); statErr != nil {
		t.Fatalf("状态目录没被创建：%v", statErr)
	}
	if server.Dir() != stateDir {
		t.Fatalf("Dir() 不对：%s", server.Dir())
	}
	// Close 必须幂等（退出路径会调两次）
	if err := server.Close(); err != nil {
		t.Fatalf("Close 报错：%v", err)
	}
}

// 没有 data_dir 时要落到用户配置目录，而不是当前工作目录
// （否则程序在哪个目录启动就把状态写到哪儿，身份会丢）。
func TestStateDirFallsBackToUserConfig(t *testing.T) {
	cfg := tsnetx.Config{Role: tsnetx.RoleExecutor}
	got := tsnetx.StateDirFor(cfg)
	base, err := os.UserConfigDir()
	if err != nil {
		t.Skip("拿不到 UserConfigDir")
	}
	want := filepath.Join(base, "simple-agent", "tailscale", "executor")
	if got != want {
		t.Fatalf("期望回退到 %s，实际 %s", want, got)
	}
}
