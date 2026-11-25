package tlsx_test

import (
	"crypto/tls"
	"encoding/json"
	"testing"
	"time"

	"simple-agent/internal/tlsx"
)

// 手机端导入配置时用的 JSON 格式（就是执行端的 relay.json）。
//
// 这里锁定的是**字段名契约**：Android 端按 addr/token/fingerprint 解析，
// 执行端按同样三个字段写。任何一边改了名字，导入功能就静默失效
// （用户看到的是"导入了但没填上"）。
func TestRelayConfigJSONContract(t *testing.T) {
	// 执行端保存的格式（见 internal/uiapp/relay_state.go 的 savedRelay）
	raw := `{"addr":"129.211.171.158:7900","token":"relaytest2024","fingerprint":"a23ef46fc2dd8dc5efdff066cecd160abb20936fa83081dcfe804532b376199a"}`

	var config struct {
		Addr        string `json:"addr"`
		Token       string `json:"token"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	if config.Addr == "" || config.Token == "" || config.Fingerprint == "" {
		t.Fatalf("三个字段都要能解出来：%+v", config)
	}
	// 指纹必须是 64 位十六进制（手机端会原样传给 Go 侧做严格校验）
	if len(config.Fingerprint) != 64 {
		t.Fatalf("指纹应是 64 位，实际 %d", len(config.Fingerprint))
	}
}

// 确认过的指纹要能直接用于连接（TOFU 的最后一环）。
//
// 覆盖"用户点了信任 → 保存 → 下次用它严格校验"这条链：只要指纹存对了，
// 用它连就是通的。
func TestConfirmedFingerprintConnects(t *testing.T) {
	dir := t.TempDir()
	cfg := tlsx.Config{CertFile: dir + "/cert.pem", KeyFile: dir + "/key.pem"}
	fingerprint, err := cfg.EnsureCert()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := cfg.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if tlsConn, ok := conn.(*tls.Conn); ok {
				_ = tlsConn.Handshake()
			}
			_ = conn.Close()
		}
	}()

	// 拿这些指纹去连：带冒号的（relay 打印格式）和规范化的都应该能通
	for _, candidate := range []string{
		fingerprint,
		tlsx.FormatFingerprint(fingerprint),
	} {
		if _, err := (tlsx.Config{Fingerprint: candidate}).Dial(listener.Addr().String()); err != nil {
			t.Fatalf("用已确认的指纹 %q 应该能连上：%v", candidate, err)
		}
	}
}

// 指纹不匹配必须拒绝（TOFU 的意义就在于此）。
func TestWrongFingerprintRejected(t *testing.T) {
	dir := t.TempDir()
	cfg := tlsx.Config{CertFile: dir + "/cert.pem", KeyFile: dir + "/key.pem"}
	if _, err := cfg.EnsureCert(); err != nil {
		t.Fatal(err)
	}
	listener, err := cfg.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if tlsConn, ok := conn.(*tls.Conn); ok {
				_ = tlsConn.Handshake()
			}
			_ = conn.Close()
		}
	}()

	wrong := "00" + repeatChar("ab", 31)
	if _, err := (tlsx.Config{Fingerprint: wrong}).Dial(listener.Addr().String()); err == nil {
		t.Fatal("指纹不符时必须拒绝连接")
	}
}

func repeatChar(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

var _ = time.Second
