package tlsx_test

import (
	"crypto/tls"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"simple-agent/internal/tlsx"
)

// 零值配置必须完全等价于明文：这是"默认不开启"的核心保证。
func TestZeroConfigIsPlaintext(t *testing.T) {
	cfg := tlsx.Config{}
	if cfg.Enabled() || cfg.ServerTLS() || cfg.ClientTLS() {
		t.Fatal("零值配置不该启用任何 TLS")
	}

	// 用零值配置监听 + 拨号，必须是**明文**（用普通 TCP 能连上）
	listener, err := cfg.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	defer listener.Close()

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("plain"))
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("明文拨号失败：%v", err)
	}
	defer conn.Close()
	buf := make([]byte, 5)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("读失败：%v", err)
	}
	if string(buf) != "plain" {
		t.Fatalf("期望明文 plain，实际 %q", buf)
	}
}

// 自签证书生成：指纹要能稳定复现，且第二次调用复用已有文件。
func TestEnsureCertGeneratesAndReuses(t *testing.T) {
	dir := t.TempDir()
	cfg := tlsx.Config{
		CertFile: filepath.Join(dir, "cert.pem"),
		KeyFile:  filepath.Join(dir, "key.pem"),
	}
	first, err := cfg.EnsureCert()
	if err != nil {
		t.Fatalf("生成证书失败：%v", err)
	}
	if len(first) != 64 {
		t.Fatalf("指纹应是 64 个十六进制字符，实际 %d", len(first))
	}

	// 再调一次：读到的是同一份证书，指纹必须一致
	second, err := cfg.EnsureCert()
	if err != nil {
		t.Fatalf("复用证书失败：%v", err)
	}
	if first != second {
		t.Fatalf("复用证书时指纹变了：%s -> %s", first, second)
	}

	// 从文件算出来的指纹也要一致
	fromFile, err := tlsx.CertFingerprintFile(cfg.CertFile)
	if err != nil {
		t.Fatalf("读指纹失败：%v", err)
	}
	if fromFile != first {
		t.Fatalf("文件指纹不一致：%s vs %s", fromFile, first)
	}
}

// 证书能真的用于 TLS 握手，且客户端用正确指纹能通过校验。
func TestTLSHandshakeWithMatchingFingerprint(t *testing.T) {
	dir := t.TempDir()
	serverCfg := tlsx.Config{
		CertFile: filepath.Join(dir, "cert.pem"),
		KeyFile:  filepath.Join(dir, "key.pem"),
	}
	fingerprint, err := serverCfg.EnsureCert()
	if err != nil {
		t.Fatalf("生成证书失败：%v", err)
	}

	listener, err := serverCfg.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("TLS 监听失败：%v", err)
	}
	defer listener.Close()

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("secret"))
	}()

	clientCfg := tlsx.Config{Fingerprint: fingerprint}
	conn, err := clientCfg.Dial(listener.Addr().String())
	if err != nil {
		t.Fatalf("TLS 拨号失败（指纹应当匹配）：%v", err)
	}
	defer conn.Close()

	buf := make([]byte, 6)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("读失败：%v", err)
	}
	if string(buf) != "secret" {
		t.Fatalf("期望 secret，实际 %q", buf)
	}
}

// 指纹不对必须**握手失败**，不能悄悄降级成明文。
func TestTLSHandshakeRejectsWrongFingerprint(t *testing.T) {
	dir := t.TempDir()
	serverCfg := tlsx.Config{
		CertFile: filepath.Join(dir, "cert.pem"),
		KeyFile:  filepath.Join(dir, "key.pem"),
	}
	if _, err := serverCfg.EnsureCert(); err != nil {
		t.Fatalf("生成证书失败：%v", err)
	}
	listener, err := serverCfg.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("TLS 监听失败：%v", err)
	}
	defer listener.Close()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = conn.Close()
		}
	}()

	wrong := strings.Repeat("ab", 32)
	clientCfg := tlsx.Config{Fingerprint: wrong}
	if _, err := clientCfg.Dial(listener.Addr().String()); err == nil {
		t.Fatal("指纹不匹配却连上了")
	} else if !strings.Contains(err.Error(), "指纹") && !strings.Contains(err.Error(), "certificate") {
		// 错误信息应当让用户能看出是证书问题
		t.Logf("错误信息（可接受）：%v", err)
	}
}

// 指纹格式容错：用户可能带冒号、空格、大写。
func TestFingerprintNormalization(t *testing.T) {
	dir := t.TempDir()
	cfg := tlsx.Config{
		CertFile: filepath.Join(dir, "cert.pem"),
		KeyFile:  filepath.Join(dir, "key.pem"),
	}
	fingerprint, err := cfg.EnsureCert()
	if err != nil {
		t.Fatalf("生成证书失败：%v", err)
	}

	listener, err := cfg.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	defer listener.Close()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			_, _ = conn.Write([]byte("ok"))
			_ = conn.Close()
		}
	}()

	// 大写 + 冒号分组，应当照样能通过
	formatted := tlsx.FormatFingerprint(fingerprint)
	upper := strings.ToUpper(formatted)
	clientCfg := tlsx.Config{Fingerprint: upper}
	conn, err := clientCfg.Dial(listener.Addr().String())
	if err != nil {
		t.Fatalf("带冒号/大写的指纹应当可用：%v", err)
	}
	_ = conn.Close()
}

// 客户端配置校验：指纹长度不对要直接报错，而不是连上去才失败。
func TestClientConfigValidatesFingerprint(t *testing.T) {
	cfg := tlsx.Config{Fingerprint: "abcd"}
	if _, err := cfg.ClientConfig(); err == nil {
		t.Fatal("短指纹应当被拒绝")
	}
	valid := tlsx.Config{Fingerprint: strings.Repeat("0", 64)}
	if _, err := valid.ClientConfig(); err != nil {
		t.Fatalf("64 字符指纹应当接受：%v", err)
	}
}

// TLS 监听器产出的连接必须是 *tls.Conn（证明真的加密了）。
func TestListenProducesTLSConn(t *testing.T) {
	dir := t.TempDir()
	cfg := tlsx.Config{
		CertFile: filepath.Join(dir, "cert.pem"),
		KeyFile:  filepath.Join(dir, "key.pem"),
	}
	if _, err := cfg.EnsureCert(); err != nil {
		t.Fatalf("生成证书失败：%v", err)
	}
	listener, err := cfg.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	defer listener.Close()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("裸 TCP 连接失败：%v", err)
	}
	defer conn.Close()

	accepted, err := listener.Accept()
	if err != nil {
		t.Fatalf("Accept 失败：%v", err)
	}
	defer accepted.Close()
	if _, ok := accepted.(*tls.Conn); !ok {
		t.Fatalf("Accept 返回的不是 *tls.Conn，而是 %T", accepted)
	}
}
