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
	if c.DataDir == "" {
		return c.CertFile, c.KeyFile
	}
	dir := filepath.Join(c.DataDir, "tls")
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
	return &tls.Config{
		// 关掉 CA 链校验：我们不用 CA，只认下面的指纹。
		// 这不是"跳过校验"——VerifyPeerCertificate 里做了更严格的检查。
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("对端没有提供证书")
			}
			sum := sha256.Sum256(rawCerts[0])
			got := hex.EncodeToString(sum[:])
			if got != want {
				return fmt.Errorf("证书指纹不匹配：收到 %s，期望 %s", got, want)
			}
			return nil
		},
	}, nil
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

// Listen 用当前生效配置监听。
func Listen(addr string) (net.Listener, error) { return Active().Listen(addr) }
