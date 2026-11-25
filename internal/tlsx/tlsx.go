// Package tlsx 给 wire 连接加一层可选的 TLS。
//
// # 默认关闭
//
// 零值 [Config] 表示**明文**，行为与没有这个包完全一样。只有显式设置
// `--tls-cert` / `--tls-key`（服务端）或 `--tls-fingerprint`（客户端）才会启用。
//
// # 为什么用证书指纹而不是 CA
//
// 自用场景没有域名、也不想去买证书。客户端**写死 relay 证书的 SHA-256 指纹**，
// 只有指纹完全一致才接受连接——这比 CA 链更严格：CA 签发的任何证书都不认，
// 只认这一台 relay。
//
// 代价是首次连接要人工确认指纹（把服务端打印的指纹填到客户端配置里）。
// 这跟 SSH 的 known_hosts 是同一个思路。
//
// # 加密只是纵深防御
//
// 它保护"不被监听、不被中间人"，但**不替代 token**：能连上端口的人如果不带
// token 仍然会被拒。两者的顺序是"先 TLS 握手（验指纹），再在加密通道内校验 token"。
package tlsx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Config 是 TLS 的启用配置。零值 = 明文。
type Config struct {
	// CertFile / KeyFile 是服务端证书与私钥的 PEM 路径。
	//
	// 文件不存在时会自动生成自签证书（见 [Config.EnsureCert]）。
	// 给了 [Config.DataDir] 而路径为空时，落在 `<DataDir>/tls/cert.pem` 与 `key.pem`，
	// 这样 `--tls` 一个开关就能用，不必每次传两个路径。
	CertFile string
	// KeyFile 见 CertFile。
	KeyFile string
	// DataDir 用来推导默认证书位置（仅当 CertFile/KeyFile 为空时使用）。
	DataDir string
	// CertDir 覆盖证书目录（默认 `<DataDir>/tls`）。
	//
	// 用途：**同一台机器上的不同角色要有不同身份**。执行端和 viewer 都从
	// DataDir 派生证书的话，两者会共用同一张证书 —— 那意味着"能证明我是
	// 这台机器"，而不是"能证明我是哪个角色"，mTLS 里就分不清谁是谁了。
	//
	// 所以 viewer 用 `<DataDir>/tls-viewer`，执行端保持 `<DataDir>/tls`。
	CertDir string
	// Fingerprint 是客户端侧要校验的服务端证书 SHA-256 指纹（十六进制，可带冒号）。
	//
	// 为空表示不启用客户端 TLS。
	Fingerprint string
}

// resolvedCertPaths 返回实际使用的证书路径（未显式指定时落到 DataDir 下）。
func (c Config) resolvedCertPaths() (certFile, keyFile string) {
	if c.CertFile != "" && c.KeyFile != "" {
		return c.CertFile, c.KeyFile
	}
	dir := c.CertDir
	if dir == "" {
		if c.DataDir == "" {
			return c.CertFile, c.KeyFile
		}
		dir = filepath.Join(c.DataDir, "tls")
	}
	return filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
}

// Enabled 报告是否配置了任意一侧的 TLS。
func (c Config) Enabled() bool {
	return (c.CertFile != "" && c.KeyFile != "") || c.Fingerprint != ""
}

// ServerTLS 报告是否应该用 TLS 监听。
// ServerTLS 报告是否应该用 TLS 监听。
//
// 显式给了证书路径、或者给了 DataDir（用默认路径）都算启用。
func (c Config) ServerTLS() bool {
	certFile, keyFile := c.resolvedCertPaths()
	return certFile != "" && keyFile != ""
}

// ClientTLS 报告是否应该用 TLS 拨号。
func (c Config) ClientTLS() bool { return c.Fingerprint != "" }

// normalizedFingerprint 去掉冒号/空格并转小写，便于人工输入。
func normalizedFingerprint(value string) string {
	replacer := strings.NewReplacer(":", "", " ", "", "-", "")
	return strings.ToLower(replacer.Replace(strings.TrimSpace(value)))
}

// ServerConfig 构造服务端 TLS 配置（证书从文件加载）。
func (c Config) ServerConfig() (*tls.Config, error) {
	certFile, keyFile := c.resolvedCertPaths()
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("加载 TLS 证书失败（%s / %s）：%w", certFile, keyFile, err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ClientConfig 构造客户端 TLS 配置：**只认写死的指纹**，不走 CA。
func (c Config) ClientConfig() (*tls.Config, error) {
	want := normalizedFingerprint(c.Fingerprint)
	if want == "" {
		return nil, errors.New("没有配置 TLS 指纹")
	}
	if len(want) != sha256.Size*2 {
		return nil, fmt.Errorf("TLS 指纹长度不对（应为 %d 个十六进制字符，实际 %d）",
			sha256.Size*2, len(want))
	}
	cfg := &tls.Config{
		// 关掉 CA 链校验：我们不用 CA，只认下面的指纹。
		// 这不是"跳过校验"——VerifyPeerCertificate 里做了更严格的检查。
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
		VerifyPeerCertificate: verifyFingerprint(want),
	}
	// **自己的证书**：双向认证时要出示给对端（对端会校验我们的指纹）。
	// 没有自己的证书时保持单向（只验对端）—— 明文/旧配置仍然能用。
	if cert, err := c.ownCertificate(); err == nil && len(cert.Certificate) > 0 {
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// verifyFingerprint 返回"只认这个指纹"的校验函数。
func verifyFingerprint(want string) func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("对端没有提供证书")
		}
		sum := sha256.Sum256(rawCerts[0])
		got := hex.EncodeToString(sum[:])
		if got != want {
			return fmt.Errorf("证书指纹不匹配：收到 %s，期望 %s", got, want)
		}
		return nil
	}
}

// ownCertificate 加载本机证书（用于向对端出示）。没有证书时返回空。
func (c Config) ownCertificate() (tls.Certificate, error) {
	certFile, keyFile := c.resolvedCertPaths()
	if certFile == "" || keyFile == "" {
		return tls.Certificate{}, errors.New("没有配置本机证书")
	}
	return tls.LoadX509KeyPair(certFile, keyFile)
}

// Listen 按配置监听（未启用服务端 TLS 时就是 net.Listen）。
func (c Config) Listen(addr string) (net.Listener, error) {
	if !c.ServerTLS() {
		return net.Listen("tcp", addr)
	}
	cfg, err := c.ServerConfig()
	if err != nil {
		return nil, err
	}
	return tls.Listen("tcp", addr, cfg)
}

// Dial 按配置拨号（未启用客户端 TLS 时就是 net.Dial）。
func (c Config) Dial(addr string) (net.Conn, error) {
	if !c.ClientTLS() {
		return net.Dial("tcp", addr)
	}
	cfg, err := c.ClientConfig()
	if err != nil {
		return nil, err
	}
	conn, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		// 指纹不匹配时底层可能是 "connection aborted" 之类的通用错误，
		// 用户看不出是证书问题。这里统一包一层可读的说明。
		return nil, fmt.Errorf("TLS 连接 %s 失败（可能是证书指纹不匹配，或对端没开 TLS）：%w", addr, err)
	}
	return conn, nil
}

// ProbeFingerprint 连一次对端，**只取证书指纹、不做任何校验**。
//
// 用途：首次连接时的 TOFU（trust on first use）——先拿到指纹展示给用户确认，
// 确认过之后才保存下来，以后用它做严格校验（见 ClientConfig）。
//
// 也用来判断"对端到底是不是 TLS 服务"：明文服务收到 TLS 握手包会直接断开
// 或回垃圾数据，两种都表现为 error，调用方据此退回明文连接。
//
// 为什么单独一个函数而不是"拨号失败时顺便拿"：探测阶段必须**不校验**，
// 而正常拨号必须**严格校验**，两者的 tls.Config 不同，混在一起很容易
// 写出"校验形同虚设"的实现。
//
// 注意它只完成握手就断开，不发任何业务数据。返回的指纹是规范化后的
// 小写十六进制（无冒号），可直接存进配置。
func ProbeFingerprint(addr string, timeout time.Duration) (string, error) {
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
		// 探测阶段无法校验（还没指纹），这里必须跳过。
		// **它不会被用在正式连接上** —— 那一侧由 ClientConfig 严格校验。
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
	if err != nil {
		return "", fmt.Errorf("连不上 %s：%w", addr, err)
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", fmt.Errorf("对端 %s 没有提供证书", addr)
	}
	sum := sha256.Sum256(certs[0].Raw)
	return hex.EncodeToString(sum[:]), nil
}

// ProbeFingerprintOrPlain 探测指纹；对端是明文时返回 (\"\", plain=true)。
//
// 与 ProbeFingerprint 的区别：把"对端不是 TLS"和"连不上"区分开。
// 前者是正常情况（明文 relay），后者才是错误 —— 调用方要能分辨，
// 否则明文 relay 会被当成连接失败。
//
// 判据：明文服务收到 TLS 握手（ClientHello）后，要么立刻断开（EOF/reset），
// 要么回一段不是 TLS 的数据。两种情况都归为"明文"。
// 超时不算明文 —— 那更像是网络不通，交给调用方报错。
func ProbeFingerprintOrPlain(addr string, timeout time.Duration) (fingerprint string, plain bool, err error) {
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
	if err == nil {
		defer conn.Close()
		certs := conn.ConnectionState().PeerCertificates
		if len(certs) == 0 {
			return "", true, nil
		}
		sum := sha256.Sum256(certs[0].Raw)
		return hex.EncodeToString(sum[:]), false, nil
	}
	// 握手失败：分清"对端是明文"和"真的连不上"。
	//
	// 明文服务的行为是**立刻**断开或回非 TLS 数据，所以出错很快；
	// 若是超时，说明包发出去没回应，那是网络问题。
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "", false, fmt.Errorf("连不上 %s：%w", addr, err)
	}
	// 非超时的握手失败 = 对端不是 TLS（或证书都不能用），按明文处理。
	return "", true, nil
}

// ---- 自签证书生成 ----

// EnsureCert 确保证书文件存在，不存在就用自签方式生成。
//
// 返回证书的 SHA-256 指纹（客户端要填的那个）。
//
// 生成的是自签证书（自己签发自己），对"指纹校验"来说完全够用——
// 我们不需要任何 CA 信任链。
func (c Config) EnsureCert() (string, error) {
	certFile, keyFile := c.resolvedCertPaths()
	if certFile == "" || keyFile == "" {
		return "", errors.New("需要证书与私钥路径（或指定 DataDir 用默认位置）")
	}
	if fileExists(certFile) && fileExists(keyFile) {
		return CertFingerprintFile(certFile)
	}
	if err := os.MkdirAll(filepath.Dir(certFile), 0o700); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		return "", err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return "", err
	}
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "simple-agent"
	}
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: hostname, Organization: []string{"simple-agent"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{hostname, "localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return "", err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	if err := writeFileExclusive(certFile, certPEM); err != nil {
		return "", err
	}
	if err := writeFileExclusive(keyFile, keyPEM); err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

// CertPaths 返回实际使用的证书与私钥路径（日志/打印用）。
func (c Config) CertPaths() (certFile, keyFile string) { return c.resolvedCertPaths() }

// CertFingerprintFile 计算证书文件的 SHA-256 指纹（十六进制）。
//
// 也可以用命令行打印：
//
//	simple-agent --tls-print-fingerprint --tls-cert ... --tls-key ...
func CertFingerprintFile(certFile string) (string, error) {
	content, err := os.ReadFile(certFile)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(content)
	if block == nil {
		return "", errors.New("证书文件不是 PEM 格式")
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:]), nil
}

// FormatFingerprint 把指纹按 32 字节分组加冒号，便于人眼核对。
func FormatFingerprint(fingerprint string) string {
	value := normalizedFingerprint(fingerprint)
	if len(value) != sha256.Size*2 {
		return fingerprint
	}
	parts := make([]string, 0, sha256.Size)
	for i := 0; i < len(value); i += 2 {
		parts = append(parts, value[i:i+2])
	}
	return strings.Join(parts, ":")
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Size() > 0
}

// writeFileExclusive 写文件，权限 0600（私钥必须只有自己能读）。
func writeFileExclusive(path string, content []byte) error {
	return os.WriteFile(path, content, 0o600)
}

// ---- 进程级配置（避免把 tls 参数穿过所有调用栈） ----

var (
	activeMu  sync.RWMutex
	activeCfg Config
)

// SetActive 设置全局 TLS 配置（进程启动时调用一次）。
//
// 用包级状态是为了避免把 TLS 配置加进 wire.Dial、relay 各函数、
// host 的每一条签名——那些函数的调用点很多，而 TLS 是"全进程一个开关"。
func SetActive(cfg Config) {
	activeMu.Lock()
	activeCfg = cfg
	activeMu.Unlock()
}

// Active 返回当前生效的 TLS 配置。
func Active() Config {
	activeMu.RLock()
	defer activeMu.RUnlock()
	return activeCfg
}

// Dial 用当前生效配置拨号。
func Dial(addr string) (net.Conn, error) { return Active().Dial(addr) }

// ActiveServerFingerprint 返回**当前生效的服务端证书**指纹（小写十六进制）。
//
// 用途：执行端把局域网直连地址上报给 relay 时，要一并带上自己的证书指纹
// —— 展示端直连时校验的是执行端的证书，而连 relay 时校验的是 relay 的，
// 两者是不同的自签名证书。不带这一项，直连必然握手失败。
//
// 没配服务端证书（明文模式）时返回空串。
func ActiveServerFingerprint() string {
	cfg := Active()
	if !cfg.ServerTLS() {
		return ""
	}
	certFile, _ := cfg.resolvedCertPaths()
	fingerprint, err := CertFingerprintFile(certFile)
	if err != nil {
		return ""
	}
	return normalizedFingerprint(fingerprint)
}

// Listen 用当前生效配置监听。
func Listen(addr string) (net.Listener, error) { return Active().Listen(addr) }
