package bridge

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"strings"
	"testing"
	"time"
)

// **bridge 的指纹确认接口必须成对可用**：PendingPeers 给出待确认项，
// ResolvePeer 能做决定。
//
// 这条只测接口契约（不需要真连 relay）：Android 侧靠这两个方法弹窗，
// 名字或格式一变界面就静默失效 —— 那是很难从手机上排查的故障。
func TestPendingPeersEmptyWhenNothingWaiting(t *testing.T) {
	client := NewClient(nil, "zh", t.TempDir())
	defer client.Close()

	if got := client.PendingPeers(); got != "" {
		t.Fatalf("没有待确认项时应返回空串，实际 %q", got)
	}
	// 没有待确认项时回答也不该崩
	if got := client.ResolvePeer("1.2.3.4:7900", true); !strings.Contains(got, "没有待确认") {
		t.Fatalf("应提示没有待确认的对端，实际 %q", got)
	}
}

// NewClient 必须把信任列表和证书目录准备好（在 cacheDir 下）。
//
// 少了任何一样：信任列表不落盘 → 每次启动都重新问；
// 证书目录为空 → mTLS 拿不出自己的证书 → 执行端拒绝每个连接。
func TestNewClientPreparesTrustStoreAndDataDir(t *testing.T) {
	dir := t.TempDir()
	client := NewClient(nil, "zh", dir)
	defer client.Close()

	if client.dataDir != dir {
		t.Fatalf("dataDir 应为 %q，实际 %q", dir, client.dataDir)
	}
	if client.trustStore == nil {
		t.Fatal("信任列表没有初始化（每次连接都会重新问用户）")
	}
	if client.peerGate == nil {
		t.Fatal("指纹确认队列没有初始化（弹窗永远不会出现）")
	}
}

// 两个待确认项按指纹去重：同一台机器只问一次。
//
// 用户点得慢时 Android 会重复拨号（重连/重试），执行端或 relay 那边
// 就会同时挂着几条"未知指纹"的连接 —— 不去重的话手机上弹好几个框。
func TestPeerGateDeduplicatesByFingerprint(t *testing.T) {
	client := NewClient(nil, "zh", t.TempDir())
	defer client.Close()

	const fingerprint = "a23ef46fc2dd8dc5efdff066cecd160abb20936fa83081dcfe804532b376199a"
	// **三条连接用同一张证书**：去重是按指纹的，证书必须一样
	certDER := certificateWithFingerprint(t, fingerprint)
	results := make(chan error, 3)
	for i := 0; i < 3; i++ {
		addr := "10.0.0.1:1000" + string(rune('0'+i))
		go func(a string) {
			results <- client.verifyPeer(a, certDER)
		}(addr)
	}

	// 等登记完成
	deadline := waitForPending(client, 1)
	if !deadline {
		t.Fatal("待确认项没有登记")
	}
	if got := len(client.peerGate.Pending()); got != 1 {
		t.Fatalf("同指纹的多条连接只该登记一次，实际 %d 条", got)
	}

	// 用户点一次"信任"，三条都该通过
	pending := client.peerGate.Pending()[0]
	client.peerGate.Resolve(pending, true)
	for i := 0; i < 3; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("用户同意后应通过：%v", err)
			}
		}
	}
}

// waitForPending 等至少有一条待确认项出现。
func waitForPending(client *Client, want int) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(client.peerGate.Pending()) >= want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// certificateWithFingerprint 造一张自签证书（DER）。
//
// 调用方要验证的是"同指纹去重"，指纹具体值不重要 —— 这里生成真证书
// 再算出它的实际指纹即可。
func certificateWithFingerprint(t *testing.T, want string) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "test-peer"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	_ = want
	return der
}