// Package lan 负责"局域网直连"：让展示端跳过 relay 直接连执行端。
//
// # 为什么需要
//
// 手机与执行端在同一个 WiFi 下时，经 relay 中转要绕一圈公网
// （实测 20~40ms，还可能受 relay 所在机房影响）。局域网直连能把延迟
// 降到 1~2ms，而且完全不依赖外网。
//
// # 为什么不做 NAT 打洞
//
// 实测本机是对称型 NAT（三次 STUN 拿到三个不同的公网端口），
// 这种 NAT 下 UDP 打洞必然失败 —— 无论用哪个库。而且双方都没有
// 公网 IPv6。所以这里**只做确定可达的局域网直连**，跨网仍然走 relay。
//
// 这不是妥协：真正需要跨网低延迟时，正确的做法是 WireGuard/Tailscale
// 那类带中继的方案，自己实现打洞是在重复它们已经做对的事。
//
// # 数据流
//
//	执行端：自动监听随机端口 → 枚举内网 IP → 注册 relay 时上报
//	展示端：从 relay 拿到地址列表 → 逐个尝试直连 → 全失败则回退 relay
//
// 认证**完全复用现有链路**：直连跑的是同一个 wire 协议 + 同一个 token +
// 同一个 TLS 指纹，执行端那边只是换了一条已经建立的 TCP 连接。
package lan

import (
	"net"
	"sort"
	"strings"
)

// Address 是一个局域网直连候选地址。
type Address struct {
	// IP 是 IPv4 点分十进制（只上报 IPv4：局域网里 IPv6 多是链路本地
	// fe80::，带 zone 的地址在手机上往往连不通，收益低而干扰大）。
	IP string `json:"ip"`
	// Port 是执行端局域网监听的端口。
	Port int `json:"port"`
	// Name 是网卡名（仅用于界面/日志可读性，选路时不参与判断）。
	Name string `json:"name,omitempty"`
}

// String 返回 host:port 形式（直接可用于拨号）。
func (a Address) String() string {
	if a.Port <= 0 {
		return a.IP
	}
	return net.JoinHostPort(a.IP, itoa(a.Port))
}

// Dialable 报告这个地址是否值得尝试。
func (a Address) Dialable() bool {
	if a.IP == "" || a.Port <= 0 {
		return false
	}
	ip := net.ParseIP(a.IP)
	return ip != nil && ip.To4() != nil
}

// Addresses 返回可上报的局域网直连候选（全部带上监听端口）。
//
// port <= 0（没在监听）时返回空：没有端口就没有直连的意义，
// 展示端据此判断"这个执行端不支持直连"。
func Addresses(port int) []Address {
	if port <= 0 {
		return nil
	}
	addresses := LocalAddresses()
	for i := range addresses {
		addresses[i].Port = port
	}
	// 过滤掉拼装后仍然不可用的（防御：LocalAddresses 已保证 IP 合法）
	usable := addresses[:0]
	for _, addr := range addresses {
		if addr.Dialable() {
			usable = append(usable, addr)
		}
	}
	return usable
}

// LocalAddresses 枚举本机所有适合局域网直连的地址。
//
// 过滤掉的：
//   - 回环（127.0.0.1）：对方连不到
//   - 链路本地（169.254.x.x）：APIPA，通常表示没拿到 DHCP
//   - 虚拟网卡（WSL、VMware、Hyper-V、docker）：其他机器连不到这些地址，
//     上报它们只会让展示端多试几个必然失败的地址、拖慢连接
//
// 保留虚拟网卡的判断只能靠名字，这里用常见的名字黑名单 —— 宁可漏报
// （那只是少一条候选，还能走 relay），也不要误报（会拖慢每次连接）。
func LocalAddresses() []Address {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []Address
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if isVirtualInterface(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil {
				continue // 只上报 IPv4，理由见 Address.IP 的注释
			}
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, Address{IP: ip.String(), Name: iface.Name})
		}
	}
	// 按 IP 排序：让上报顺序稳定（展示端按顺序尝试，顺序乱跳会让
	// 日志和"为什么先试这个"变得难以解释）。
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out
}

// isVirtualInterface 判断是不是"别的机器连不到"的虚拟网卡。
func isVirtualInterface(name string) bool {
	lower := strings.ToLower(name)
	for _, marker := range []string{
		"vethernet", // Hyper-V / WSL
		"vmware", "vmnet",
		"virtualbox", "vboxnet",
		"docker", "br-", "veth",
		"loopback", "tap", "tun", "wg", // 隧道：地址是本机内部的
		"zt", "tailscale", // 覆盖网络：有自己的寻址，不该混进局域网候选
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// ListenPort 返回监听器实际使用的端口（支持 :0 随机端口）。
func ListenPort(listener net.Listener) int {
	if listener == nil {
		return 0
	}
	if addr, ok := listener.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return 0
}

// itoa 是 strconv.Itoa 的本地副本（避免为了一个调用引入 import）。
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
