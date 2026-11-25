// Package trust 管理"已知对端指纹"（相当于 SSH 的 known_hosts）。
//
// # 为什么需要
//
// 自签证书没有 CA 背书，"这个证书是不是对的那台机器"只能靠**第一次见过
// 之后记住它**。这就是 TOFU（trust on first use）：
//
//  1. 第一次连某个对端：拿到指纹，**问用户**（界面弹窗）是否信任
//  2. 用户确认后记进本文件
//  3. 以后连同一个对端：指纹一致就静默通过；**不一致就报错**
//     （要么对端换了证书，要么有人在中间冒充 —— 两种都必须让用户知道）
//
// # 为什么不自动更新指纹
//
// 自动更新等于没有校验：中间人第一次拦截时"新指纹"会被自动接受，
// 之后攻击者就一直是"合法"的。所以指纹变化必须由用户**手动确认**
// （界面上明确提示"和上次不一样了"）。
//
// # 文件格式
//
// 一行一条，制表符分隔，方便人眼查看和手工删除（与 known_hosts 同理）：
//
//	<角色>\t<地址>\t<sha256 指纹>\t<首次信任时间>
//
// 角色是 viewer / executor / relay —— 同一台机器可能既当执行端又被
// 别人当展示端，分开记录才不会互相覆盖。
package trust

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Role 是对端扮演的角色（同一地址可能有不同角色，指纹也不同）。
type Role string

const (
	RoleRelay    Role = "relay"
	RoleExecutor Role = "executor"
	RoleViewer   Role = "viewer"
)

// Entry 是一条信任记录。
type Entry struct {
	Role Role
	Addr string
	// Fingerprint 是规范化后的 SHA-256（小写十六进制、无冒号）。
	Fingerprint string
	// Since 是首次信任的时间（指纹变化后重新信任会更新它）。
	Since time.Time
}

// Key 是条目的唯一标识（角色 + 地址）。
func (e Entry) Key() string { return string(e.Role) + "\t" + e.Addr }

// Store 是信任列表（内存 + 落盘）。
//
// 零值不可用，用 [Open] 构造。并发安全由调用方保证（本项目的用法是
// 单一界面线程 + 拨号前查询，没有并发写）。
type Store struct {
	path    string
	entries map[string]Entry
}

// Open 读取信任列表（文件不存在时返回空列表，不报错）。
//
// path 为空时返回一个"不落盘"的内存 Store：用于测试和"用户明确禁用
// 指纹记忆"的场景。
func Open(path string) (*Store, error) {
	store := &Store{path: path, entries: map[string]Entry{}}
	if path == "" {
		return store, nil
	}
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			// 坏行直接跳过：信任列表损坏不该让程序起不来
			// （最坏情况是重新问一遍用户）
			continue
		}
		// **空指纹或长度不对的也要跳过**：一条"指纹为空"的记录会让
		// Check 在比对时永远不匹配（对端给的是真实指纹），等于这条
		// 记录没用；更糟的是它看起来"已信任"，排查时会误导。
		fingerprint := normalize(fields[2])
		if len(fingerprint) != sha256HexLen {
			continue
		}
		entry := Entry{
			Role:        Role(fields[0]),
			Addr:        fields[1],
			Fingerprint: fingerprint,
		}
		if len(fields) >= 4 {
			if at, err := time.Parse(time.RFC3339, fields[3]); err == nil {
				entry.Since = at
			}
		}
		store.entries[entry.Key()] = entry
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return store, nil
}

// Lookup 查一条信任记录。
func (s *Store) Lookup(role Role, addr string) (Entry, bool) {
	entry, ok := s.entries[string(role)+"\t"+addr]
	return entry, ok
}

// Check 判断对端指纹是否可信。
//
// 返回三种结果：
//   - (Known, nil)：指纹与记录一致，可以静默通过
//   - (Unknown, nil)：从没见过这个对端，调用方应**询问用户**
//   - (Changed, *ChangedError)：见过但指纹变了，必须让用户明确确认
func (s *Store) Check(role Role, addr, fingerprint string) (Status, error) {
	want := normalize(fingerprint)
	entry, ok := s.Lookup(role, addr)
	if !ok {
		return StatusUnknown, nil
	}
	if entry.Fingerprint == want {
		return StatusKnown, nil
	}
	return StatusChanged, &ChangedError{
		Role: role, Addr: addr,
		Known: entry.Fingerprint, Got: want, Since: entry.Since,
	}
}

// Status 是 [Store.Check] 的结论。
type Status int

const (
	// StatusUnknown：首次见到这个对端。
	StatusUnknown Status = iota
	// StatusKnown：指纹与已记录的一致。
	StatusKnown
	// StatusChanged：见过，但指纹变了。
	StatusChanged
)

// ChangedError 描述"指纹变了"。
//
// 单独一个错误类型而不是一句字符串：调用方要靠它区分"变了"和"连不上"，
// 前者需要弹"这可能是冒充"的警告，后者只是网络问题 —— 提示语气完全不同。
type ChangedError struct {
	Role        Role
	Addr        string
	Known       string
	Got         string
	Since       time.Time
}

func (e *ChangedError) Error() string {
	return fmt.Sprintf(
		"%s %s 的证书指纹和上次不一样了（可能是换了证书，也可能是有人在冒充）：\n"+
			"  之前：%s\n  现在：%s",
		e.Role, e.Addr, Format(e.Known), Format(e.Got))
}

// Trust 记录（或更新）一条信任。
//
// 调用方必须**先让用户确认**再调用这里 —— 这样"自动信任"在代码结构上
// 就是不可能的，而不是靠纪律保证。
func (s *Store) Trust(role Role, addr, fingerprint string) error {
	entry := Entry{
		Role:        role,
		Addr:        addr,
		Fingerprint: normalize(fingerprint),
		Since:       time.Now(),
	}
	s.entries[entry.Key()] = entry
	return s.save()
}

// Forget 删除一条信任（用户手动清理用）。
func (s *Store) Forget(role Role, addr string) error {
	delete(s.entries, string(role)+"\t"+addr)
	return s.save()
}

// Entries 返回全部记录（按角色/地址排序，供界面展示）。
func (s *Store) Entries() []Entry {
	out := make([]Entry, 0, len(s.entries))
	for _, entry := range s.entries {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Role != out[j].Role {
			return out[i].Role < out[j].Role
		}
		return out[i].Addr < out[j].Addr
	})
	return out
}

// save 全量重写文件（条目很少，增量写没必要且容易写坏）。
func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	var builder strings.Builder
	builder.WriteString("# simple-agent 信任的对端指纹（首次连接时由用户确认）\n")
	builder.WriteString("# 角色\t地址\tSHA-256\t首次信任时间\n")
	for _, entry := range s.Entries() {
		builder.WriteString(string(entry.Role))
		builder.WriteByte('\t')
		builder.WriteString(entry.Addr)
		builder.WriteByte('\t')
		builder.WriteString(entry.Fingerprint)
		builder.WriteByte('\t')
		builder.WriteString(entry.Since.Format(time.RFC3339))
		builder.WriteByte('\n')
	}
	// 0600：指纹本身不是秘密，但这份文件决定"信任谁"，
	// 被别人改写就等于被冒充 —— 按凭据对待。
	return os.WriteFile(s.path, []byte(builder.String()), 0o600)
}

// sha256HexLen 是 SHA-256 十六进制表示的长度。
const sha256HexLen = 64

// normalize 规范化指纹（去冒号/空格、转小写）。
func normalize(value string) string {
	return strings.ToLower(strings.NewReplacer(":", "", " ", "", "-", "").Replace(strings.TrimSpace(value)))
}

// Format 把指纹按 32 字节分组加冒号（人眼核对用）。
func Format(fingerprint string) string {
	clean := normalize(fingerprint)
	if len(clean) != 64 {
		return clean
	}
	parts := make([]string, 0, 32)
	for i := 0; i < 64; i += 2 {
		parts = append(parts, clean[i:i+2])
	}
	return strings.Join(parts, ":")
}

// DefaultPath 返回信任列表的默认位置（数据目录下）。
func DefaultPath(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "known_hosts")
}