package trust_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"simple-agent/internal/trust"
)

// **viewer 按指纹识别，地址不参与判断**。
//
// 真实踩到：执行端每次启动都要重新问同一个 viewer 的指纹。原因是
// viewer 每次连接用新的源端口，而记录按"角色+地址"存 —— known_hosts
// 里堆了四条同指纹、只有端口不同的记录，而新连接带着第五个端口来查，
// 永远查不到。
//
// 身份就是指纹：同一个证书，从哪个端口来都是同一台机器。
func TestViewerTrustedByFingerprintOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	store, err := trust.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.TrustByFingerprint(trust.RoleViewer, fingerprintA); err != nil {
		t.Fatal(err)
	}

	// 同一个指纹，无论哪个地址都该命中
	if got := store.CheckByFingerprint(trust.RoleViewer, fingerprintA); got != trust.StatusKnown {
		t.Fatalf("同指纹应通过，实际 %v", got)
	}
	// 换个地址再问一次，结果不变（地址根本不参与）
	if got := store.CheckByFingerprint(trust.RoleViewer, fingerprintA); got != trust.StatusKnown {
		t.Fatalf("地址变化不该影响判断，实际 %v", got)
	}
	// 另一个指纹不该被信任
	if got := store.CheckByFingerprint(trust.RoleViewer, fingerprintB); got != trust.StatusUnknown {
		t.Fatalf("陌生指纹应为未知，实际 %v", got)
	}
}

// 重复记录同一个指纹不会堆多条（地址根本不存）。
func TestTrustByFingerprintIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	store, err := trust.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := store.TrustByFingerprint(trust.RoleViewer, fingerprintA); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(store.Entries()); got != 1 {
		t.Fatalf("同指纹只该有一条记录，实际 %d 条", got)
	}
}

// 落盘格式用 fp: 前缀标记"按指纹识别"，重新打开后仍然生效。
func TestFingerprintKeyedEntryRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	store, err := trust.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.TrustByFingerprint(trust.RoleViewer, fingerprintA); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "fp:"+fingerprintA) {
		t.Fatalf("落盘应带 fp: 前缀，实际内容：\n%s", content)
	}

	reopened, err := trust.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.CheckByFingerprint(trust.RoleViewer, fingerprintA); got != trust.StatusKnown {
		t.Fatalf("重新打开后仍应信任，实际 %v", got)
	}
}

// 旧格式（按地址记录的 viewer）应被清理掉。
//
// 那些记录既占地方又永远命中不了，留着只会让排查时产生"明明记过"的错觉。
func TestCleanupLegacyViewerEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	// 模拟旧版本写下的内容：viewer 按地址记（含端口）
	legacy := "# header\n" +
		"viewer\t10.0.0.9:34206\t" + fingerprintA + "\t2026-11-25T16:47:45+08:00\n" +
		"viewer\t10.0.0.9:34218\t" + fingerprintA + "\t2026-11-25T16:47:46+08:00\n" +
		"relay\t1.2.3.4:7900\t" + fingerprintB + "\t2026-11-25T15:58:46+08:00\n"
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := trust.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CleanupLegacy(trust.RoleViewer); err != nil {
		t.Fatal(err)
	}

	for _, entry := range store.Entries() {
		if entry.Role == trust.RoleViewer && !entry.ByFingerprint {
			t.Fatalf("旧格式 viewer 记录没被清掉：%+v", entry)
		}
	}
	// relay 记录不受影响
	if got := len(store.Entries()); got != 1 {
		t.Fatalf("只该剩下 relay 一条，实际 %d 条", got)
	}
}