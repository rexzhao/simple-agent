package peerverify_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"simple-agent/internal/peerverify"
	"simple-agent/internal/trust"
)

const (
	fingerprintA = "a23ef46fc2dd8dc5efdff066cecd160abb20936fa83081dcfe804532b376199a"
	fingerprintB = "3d368bf8d94f25eed8e63c647bbec19fca0df58466e74b67039705ec9664950e"
)

func newStore(t *testing.T) *trust.Store {
	t.Helper()
	store, err := trust.Open(filepath.Join(t.TempDir(), "known_hosts"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// 已信任的指纹直接通过，**不问用户**（确认过就不再问）。
func TestKnownPeerPassesWithoutAsking(t *testing.T) {
	store := newStore(t)
	if err := store.Trust(trust.RoleRelay, "1.2.3.4:7900", fingerprintA); err != nil {
		t.Fatal(err)
	}
	asked := false
	verifier := &peerverify.Verifier{
		Store: store,
		Role:  trust.RoleRelay,
		Ask: func(peerverify.Request) (bool, error) {
			asked = true
			return true, nil
		},
	}
	if err := verifier.VerifyFingerprint("1.2.3.4:7900", fingerprintA); err != nil {
		t.Fatalf("已信任的对端应通过：%v", err)
	}
	if asked {
		t.Fatal("已信任的对端不该再询问用户")
	}
}

// 首次见到：**必须问用户**，用户同意才通过。
//
// 这是 TOFU 的核心。以前的实现是"自动放行 + 记进信任列表"，
// 等于自动信任任何对端 —— 中间人第一次拦截就永久生效。
func TestUnknownPeerAsksUser(t *testing.T) {
	store := newStore(t)
	var got peerverify.Request
	verifier := &peerverify.Verifier{
		Store: store,
		Role:  trust.RoleRelay,
		Ask: func(req peerverify.Request) (bool, error) {
			got = req
			return true, nil
		},
	}
	if err := verifier.VerifyFingerprint("1.2.3.4:7900", fingerprintA); err != nil {
		t.Fatalf("用户同意后应通过：%v", err)
	}
	if got.Addr != "1.2.3.4:7900" || got.Fingerprint != fingerprintA {
		t.Fatalf("询问内容不对：%+v", got)
	}
	if got.Changed {
		t.Fatal("首次连接不该标记为「变了」")
	}
	// 同意之后应该记住
	status, err := store.Check(trust.RoleRelay, "1.2.3.4:7900", fingerprintA)
	if err != nil || status != trust.StatusKnown {
		t.Fatalf("用户同意后应记入信任列表：%v %v", status, err)
	}
}

// **用户取消 = 连接失败**（Verify 返回错误，握手中止）。
func TestUserRejectsPeer(t *testing.T) {
	store := newStore(t)
	verifier := &peerverify.Verifier{
		Store: store,
		Role:  trust.RoleRelay,
		Ask:   func(peerverify.Request) (bool, error) { return false, nil },
	}
	if err := verifier.VerifyFingerprint("1.2.3.4:7900", fingerprintA); err == nil {
		t.Fatal("用户拒绝时 Verify 必须返回错误（中止握手）")
	}
	// 拒绝的不该被记住
	status, _ := store.Check(trust.RoleRelay, "1.2.3.4:7900", fingerprintA)
	if status != trust.StatusUnknown {
		t.Fatal("用户拒绝的对端不该记入信任列表")
	}
}

// 指纹变了：必须问，且请求里带上 Changed/Known（界面据此换措辞）。
func TestChangedFingerprintAsksWithContext(t *testing.T) {
	store := newStore(t)
	if err := store.Trust(trust.RoleRelay, "1.2.3.4:7900", fingerprintA); err != nil {
		t.Fatal(err)
	}
	var got peerverify.Request
	verifier := &peerverify.Verifier{
		Store: store,
		Role:  trust.RoleRelay,
		Ask: func(req peerverify.Request) (bool, error) {
			got = req
			return true, nil
		},
	}
	if err := verifier.VerifyFingerprint("1.2.3.4:7900", fingerprintB); err != nil {
		t.Fatalf("用户同意后应通过：%v", err)
	}
	if !got.Changed {
		t.Fatal("指纹变了应标记 Changed")
	}
	if got.Known != fingerprintA {
		t.Fatalf("应带上旧指纹：%q", got.Known)
	}
}

// **等待用户确认不计入超时**。
//
// 这是明确的设计要求：网络 IO 有自己的超时，但人看弹窗的速度不是网络
// 速度。给"等用户"设上限的话，用户去倒杯水回来就被判成取消了。
//
// 用 3 秒验证（远远超过任何"合理的网络超时"）：如果实现里偷偷加了
// 短超时，这条会失败。
func TestAskHasNoTimeout(t *testing.T) {
	store := newStore(t)
	gate := &peerverify.Gate{}
	done := make(chan error, 1)
	verifier := &peerverify.Verifier{
		Store: store,
		Role:  trust.RoleRelay,
		Ask:   gate.Ask,
	}
	go func() {
		done <- verifier.VerifyFingerprint("1.2.3.4:7900", fingerprintA)
	}()

	// 等 3 秒（模拟用户慢慢看弹窗），期间不该有任何结论
	select {
	case err := <-done:
		t.Fatalf("等待用户期间不该返回（那样等于加了超时）：%v", err)
	case <-time.After(3 * time.Second):
	}

	// 用户这时才确认
	pending := gate.Pending()
	if len(pending) != 1 {
		t.Fatalf("应有一条待确认，实际 %d", len(pending))
	}
	gate.Resolve(pending[0], true)
	if err := <-done; err != nil {
		t.Fatalf("用户同意后应通过：%v", err)
	}
}

// 命令行给了指纹时严格校验，不符直接拒绝（**不弹窗**）。
//
// 用户明确指定了"就该是这台"，不符说明有问题，不该给"要不要信"的选择。
func TestPinnedFingerprintIsStrict(t *testing.T) {
	store := newStore(t)
	asked := false
	verifier := &peerverify.Verifier{
		Store:  store,
		Role:   trust.RoleRelay,
		Pinned: fingerprintA,
		Ask: func(peerverify.Request) (bool, error) {
			asked = true
			return true, nil
		},
	}
	if err := verifier.VerifyFingerprint("1.2.3.4:7900", fingerprintB); err == nil {
		t.Fatal("与 --tls-fingerprint 不符时必须拒绝")
	}
	if asked {
		t.Fatal("显式指定指纹时不该询问用户")
	}
}

// **界面未就绪不能被当成「用户拒绝」**。
//
// 这是真实踩到的 bug：viewer 首次连接发生在窗口创建之前，那时
// Gate.Ask 没有界面可问，直接返回 false —— 上层把它理解成「用户拒绝」，
// 于是弹出「配置错误：用户拒绝了对端的证书」并退出。用户什么都没点。
//
// 同一个对端的多条连接**只该问用户一次**。
//
// 用户报告"执行端弹了两次确认，可能跟我点得慢有关系" —— 机制正是如此：
// viewer 那边等不及又发起了一次拨号，执行端就挂着两条未知指纹的连接，
// 界面上弹两次。
//
// 指纹相同 = 同一台机器 = 同一个决定，后来的连接等同一个结论即可。
func TestDuplicateFingerprintAsksOnce(t *testing.T) {
	store := newStore(t)
	gate := &peerverify.Gate{}
	verifier := &peerverify.Verifier{
		Store: store,
		Role:  trust.RoleViewer,
		Ask:   gate.Ask,
	}

	results := make(chan error, 3)
	// 三条连接，指纹相同、地址不同（重试时源端口会变）
	for i := 0; i < 3; i++ {
		addr := "10.0.0.9:1000" + string(rune('0'+i))
		go func(a string) {
			results <- verifier.VerifyFingerprint(a, fingerprintA)
		}(addr)
	}

	// 等三条都登记完
	deadline := time.Now().Add(2 * time.Second)
	for {
		if len(gate.Pending()) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// 只该有一条待确认
	if got := len(gate.Pending()); got != 1 {
		t.Fatalf("同指纹的多条连接只该问一次，实际登记了 %d 条", got)
	}

	// 用户点一次"允许"，三条都该通过
	gate.Resolve(gate.Pending()[0], true)
	for i := 0; i < 3; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("用户同意后应通过：%v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("有连接没被放行")
		}
	}
}

// 现在返回 ErrNotReady，调用方据此稍后重试。
func TestNotReadyIsNotRejection(t *testing.T) {
	store := newStore(t)
	gate := &peerverify.Gate{
		Ready: func() bool { return false }, // 界面还没起来
	}
	verifier := &peerverify.Verifier{
		Store: store,
		Role:  trust.RoleRelay,
		Ask:   gate.Ask,
	}
	err := verifier.VerifyFingerprint("1.2.3.4:7900", fingerprintA)
	if err == nil {
		t.Fatal("界面未就绪时不该放行")
	}
	if !errors.Is(err, peerverify.ErrNotReady) {
		t.Fatalf("应返回 ErrNotReady（可重试），实际：%v", err)
	}
	status, _ := store.Check(trust.RoleRelay, "1.2.3.4:7900", fingerprintA)
	if status != trust.StatusUnknown {
		t.Fatal("未确认的对端不该记入信任列表")
	}
	if len(gate.Pending()) != 0 {
		t.Fatal("界面未就绪时不该登记待确认项")
	}
}