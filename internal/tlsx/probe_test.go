package tlsx_test

import (
	"crypto/tls"
	"net"
	"testing"
	"time"

	"simple-agent/internal/tlsx"
)

// 对端是 TLS 服务：探测应拿到**与证书一致**的指纹，plain=false。
func TestProbeFingerprintOrPlainTLS(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := dir+"/cert.pem", dir+"/key.pem"
	cfg0 := tlsx.Config{CertFile: certFile, KeyFile: keyFile}
	want, err := cfg0.EnsureCert()
	if err != nil {
		t.Fatal(err)
	}
	cfg := tlsx.Config{CertFile: certFile, KeyFile: keyFile}
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
			// 握手完就关：探测只关心证书
			if tlsConn, ok := conn.(*tls.Conn); ok {
				_ = tlsConn.Handshake()
			}
			_ = conn.Close()
		}
	}()

	got, plain, err := tlsx.ProbeFingerprintOrPlain(listener.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("探测失败：%v", err)
	}
	if plain {
		t.Fatal("对端是 TLS 服务，不该判成明文")
	}
	// EnsureCert 返回带冒号格式的可读形式，比较时统一规范化
	if normalized(got) != normalized(want) {
		t.Fatalf("指纹不一致：拿到 %s，期望 %s", got, want)
	}
}

// 对端是**明文**服务：应判成 plain=true、不报错。
//
// 这是老用法（明文 relay）不能被打断的关键：它不该被当成"连接失败"。
func TestProbeFingerprintOrPlainPlaintext(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
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
			// 明文服务收到 TLS ClientHello：直接断开（真实 relay 也大致如此）
			_ = conn.Close()
		}
	}()

	_, plain, err := tlsx.ProbeFingerprintOrPlain(listener.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("明文对端不该报错：%v", err)
	}
	if !plain {
		t.Fatal("应判成明文服务")
	}
}

// 连不上的地址：报超时错误，**不能**误判成明文。
//
// 判错的后果很坑：会把"网络不通"当成"明文 relay"，然后走明文连接，
// 用户看到的错误变成"连接被拒"，根本猜不到是地址写错了。
func TestProbeFingerprintOrPlainUnreachable(t *testing.T) {
	// 10.255.255.1 是保留地址，正常网络里不会有回应
	_, plain, err := tlsx.ProbeFingerprintOrPlain("10.255.255.1:7900", 500*time.Millisecond)
	if err == nil {
		t.Fatal("连不上的地址应报错")
	}
	if plain {
		t.Fatal("连不上不该被判成明文服务")
	}
}

func normalized(value string) string {
	out := make([]rune, 0, len(value))
	for _, r := range value {
		switch r {
		case ':', ' ', '-':
			continue
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		out = append(out, r)
	}
	return string(out)
}