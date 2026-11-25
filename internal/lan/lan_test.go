package lan_test

import (
	"net"
	"testing"

	"simple-agent/internal/lan"
)

// 端口没监听时不该上报候选：没有端口就直连不了，展示端据此判断
// "这台执行端不支持直连"。
func TestAddressesWithoutPort(t *testing.T) {
	if got := lan.Addresses(0, "ab"); got != nil {
		t.Fatalf("port=0 应返回空，实际 %v", got)
	}
}

// 枚举出来的地址必须都是可拨的（IPv4、非回环、非链路本地）。
func TestLocalAddressesAreDialable(t *testing.T) {
	// LocalAddresses 返回的地址**不带端口**（端口由 Addresses 统一填），
	// 所以这里验的是 IP 本身合法，可拨性要用 Addresses 验。
	for _, addr := range lan.LocalAddresses() {
		ip := net.ParseIP(addr.IP)
		if ip == nil || ip.To4() == nil {
			t.Fatalf("只该上报 IPv4：%+v", addr)
		}
		if ip.IsLoopback() {
			t.Fatalf("不该上报回环地址：%+v", addr)
		}
		if ip.IsLinkLocalUnicast() {
			t.Fatalf("不该上报链路本地地址（169.254/APIPA）：%+v", addr)
		}
	}
}

// Addresses 填上端口后必须全部可拨（展示端直接拿去拨号）。
func TestAddressesAreDialable(t *testing.T) {
	for _, addr := range lan.Addresses(7900, "cd") {
		if !addr.Dialable() {
			t.Fatalf("Addresses 的结果必须可拨：%+v", addr)
		}
		if addr.Port != 7900 {
			t.Fatalf("端口应统一填成 7900，实际 %d", addr.Port)
		}
	}
}

// 虚拟网卡要过滤掉：别的机器连不到 WSL/VMware/docker 那些地址，
// 上报它们只会让展示端多试几个必然失败的地址、拖慢连接。
func TestVirtualInterfacesFiltered(t *testing.T) {
	for _, addr := range lan.LocalAddresses() {
		for _, marker := range []string{"vethernet", "vmware", "docker", "vbox", "tailscale"} {
			if containsFold(addr.Name, marker) {
				t.Fatalf("虚拟网卡 %q 不该上报（地址 %s）", addr.Name, addr.IP)
			}
		}
	}
}

// ListenPort 要能从 :0 的监听器反查出真实端口（执行端用随机端口，
// 不反查就没法上报给 relay）。
func TestListenPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := lan.ListenPort(listener)
	if port <= 0 {
		t.Fatalf("应拿到真实端口，实际 %d", port)
	}
	if lan.ListenPort(nil) != 0 {
		t.Fatal("nil 监听器应返回 0")
	}
}

// Address.String 要能直接用于拨号。
func TestAddressString(t *testing.T) {
	addr := lan.Address{IP: "10.10.110.196", Port: 7900, TLSFingerprint: "ab"}
	if got := addr.String(); got != "10.10.110.196:7900" {
		t.Fatalf("host:port 格式不对：%q", got)
	}
}

func containsFold(haystack, needle string) bool {
	return len(haystack) >= len(needle) && containsLower(lower(haystack), lower(needle))
}

func lower(s string) string {
	out := []byte(s)
	for i := range out {
		if out[i] >= 'A' && out[i] <= 'Z' {
			out[i] += 'a' - 'A'
		}
	}
	return string(out)
}

func containsLower(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}