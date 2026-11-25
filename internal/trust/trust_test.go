package trust_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"simple-agent/internal/trust"
)

const (
	fingerprintA = "a23ef46fc2dd8dc5efdff066cecd160abb20936fa83081dcfe804532b376199a"
	fingerprintB = "3d368bf8d94f25eed8e63c647bbec19fca0df58466e74b67039705ec9664950e"
)

// 首次见到的对端必须报"未知"——调用方据此询问用户。
//
// 这条是 TOFU 的基础：如果这里返回"已知"，就等于**自动信任任何对端**，
// 校验完全失效。
func TestUnknownPeerNeedsConfirmation(t *testing.T) {
	store, err := trust.Open(filepath.Join(t.TempDir(), "known_hosts"))
	if err != nil {
		t.Fatal(err)
	}
	status, err := store.Check(trust.RoleRelay, "1.2.3.4:7900", fingerprintA)
	if err != nil {
		t.Fatalf("未知对端不该报错：%v", err)
	}
	if status != trust.StatusUnknown {
		t.Fatalf("应报未知，实际 %v", status)
	}
}

// 信任之后，同样的指纹静默通过。
func TestTrustedPeerPasses(t *testing.T) {
	store, err := trust.Open(filepath.Join(t.TempDir(), "known_hosts"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Trust(trust.RoleRelay, "1.2.3.4:7900", fingerprintA); err != nil {
		t.Fatal(err)
	}
	status, err := store.Check(trust.RoleRelay, "1.2.3.4:7900", fingerprintA)
	if err != nil {
		t.Fatal(err)
	}
	if status != trust.StatusKnown {
		t.Fatalf("应通过，实际 %v", status)
	}
}

// **指纹变了必须报错**，而不是静默接受。
//
// 这是防中间人的核心：如果这里放行，攻击者第一次拦截之后就一直"合法"。
func TestChangedFingerprintIsError(t *testing.T) {
	store, err := trust.Open(filepath.Join(t.TempDir(), "known_hosts"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Trust(trust.RoleRelay, "1.2.3.4:7900", fingerprintA); err != nil {
		t.Fatal(err)
	}
	status, err := store.Check(trust.RoleRelay, "1.2.3.4:7900", fingerprintB)
	if status != trust.StatusChanged {
		t.Fatalf("应报「变了」，实际 %v", status)
	}
	var changed *trust.ChangedError
	if !asChanged(err, &changed) {
		t.Fatalf("应返回 ChangedError，实际 %T", err)
	}
	if changed.Known != fingerprintA || changed.Got != fingerprintB {
		t.Fatalf("错误里应同时带旧指纹和新指纹：%+v", changed)
	}
}

// 不同角色分开记：同一地址既可能是 relay 也可能是执行端，
// 它们的证书不同，不能互相覆盖。
func TestRolesAreIndependent(t *testing.T) {
	store, err := trust.Open(filepath.Join(t.TempDir(), "known_hosts"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Trust(trust.RoleRelay, "10.0.0.1:7900", fingerprintA); err != nil {
		t.Fatal(err)
	}
	// 同一地址、不同角色 → 仍是未知
	status, _ := store.Check(trust.RoleExecutor, "10.0.0.1:7900", fingerprintA)
	if status != trust.StatusUnknown {
		t.Fatalf("不同角色应独立判断，实际 %v", status)
	}
}

// 落盘后重新打开要能读回来（否则每次启动都要重新确认一遍）。
func TestPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	store, err := trust.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Trust(trust.RoleExecutor, "10.0.0.5:8221", fingerprintA); err != nil {
		t.Fatal(err)
	}

	reopened, err := trust.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	status, err := reopened.Check(trust.RoleExecutor, "10.0.0.5:8221", fingerprintA)
	if err != nil {
		t.Fatal(err)
	}
	if status != trust.StatusKnown {
		t.Fatal("重新打开后应仍然信任")
	}
}

// 冒号格式的指纹要和规范化后的等价（用户可能手抄带冒号的）。
func TestFingerprintNormalization(t *testing.T) {
	store, err := trust.Open(filepath.Join(t.TempDir(), "known_hosts"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Trust(trust.RoleRelay, "1.2.3.4:7900", fingerprintA); err != nil {
		t.Fatal(err)
	}
	withColons := trust.Format(fingerprintA)
	if !strings.Contains(withColons, ":") {
		t.Fatal("Format 应该加冒号")
	}
	status, err := store.Check(trust.RoleRelay, "1.2.3.4:7900", withColons)
	if err != nil || status != trust.StatusKnown {
		t.Fatalf("带冒号的写法应等价，实际 %v %v", status, err)
	}
}

// 文件损坏不该让程序起不来（最坏情况是重新问一遍用户）。
func TestCorruptFileIsTolerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	// 三种坏行：纯文本、字段不足、空指纹
	content := "这行是垃圾\n" +
		"relay\t只有两个字段\n" +
		"executor\t10.0.0.1:8221\t" + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := trust.Open(path)
	if err != nil {
		t.Fatalf("坏文件不该报错：%v", err)
	}
	// "relay\t只有两个字段" 只有 2 个字段 → 跳过；
	// 纯文本行只有 1 个 → 跳过；
	// 空指纹那行有 3 个字段但指纹为空 → 也算坏（不同版本可能行为不同，
	// 这里只要求"不 panic、不产生可信条目"）
	for _, entry := range store.Entries() {
		if entry.Fingerprint == "" {
			t.Fatalf("空指纹不该成为可信条目：%+v", entry)
		}
	}
}

func asChanged(err error, target **trust.ChangedError) bool {
	changed, ok := err.(*trust.ChangedError)
	if ok {
		*target = changed
	}
	return ok
}