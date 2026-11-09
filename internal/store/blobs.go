package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Blob 是内容寻址的二进制存储（图片、以及以后的大文件都走这里）。
//
// 设计要点（方案 §12 第 26 条）：
//   - **公共目录**：`<data_dir>/blobs/`，所有会话共用，同一份内容只存一次；
//   - **用 hash 命名**：路径 = `blobs/<sha256 前两位>/<完整 hex>`，两级分片避免
//     单目录塞几万个文件；不带扩展名——MIME 记在引用里（part 的 mediaType），
//     存储层不认识图片；
//   - **原子写**：先写临时文件、fsync，再 rename；目标已存在就直接复用（内容相同）；
//   - **先 Put 成功、再写引用它的事件**：否则崩溃后日志里会留下指向不存在文件的
//     引用（调用方负责顺序，本包只提供能力）。
type Blobs struct {
	root string
}

// ErrBadHash 表示 blob 引用里的 hash 不合法。
var ErrBadHash = errors.New("blob hash 不合法")

// NewBlobs 打开（必要时创建）`<dataDir>/blobs`。
func NewBlobs(dataDir string) (*Blobs, error) {
	root := filepath.Join(dataDir, "blobs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("创建 blob 目录失败：%w", err)
	}
	return &Blobs{root: root}, nil
}

// Root 返回 blob 目录（测试与诊断用）。
func (b *Blobs) Root() string { return b.root }

// Put 存入一段字节，返回它的 sha256（小写 hex）。已存在则直接复用。
func (b *Blobs) Put(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	path := b.Path(hash)
	if _, err := os.Stat(path); err == nil {
		return hash, nil
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建 blob 分片目录失败：%w", err)
	}
	temp, err := os.CreateTemp(b.root, ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("创建临时文件失败：%w", err)
	}
	tempName := temp.Name()
	defer func() {
		// 成功路径已经 rename 掉；失败路径在这里清理
		_ = temp.Close()
		_ = os.Remove(tempName)
	}()
	if _, err := temp.Write(data); err != nil {
		return "", fmt.Errorf("写入 blob 失败：%w", err)
	}
	if err := temp.Sync(); err != nil {
		return "", fmt.Errorf("同步 blob 失败：%w", err)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("关闭 blob 临时文件失败：%w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return "", fmt.Errorf("提交 blob 失败：%w", err)
	}
	return hash, nil
}

// Open 读取 blob 内容。
//
// hash 必须是 **64 位小写 hex**：引用可能来自被改过的日志或模型输出，
// 放任 `../` 之类就能读到 data_dir 之外的任意文件。读出来之后再校验一次内容
// 哈希，不一致就当"丢失"处理（损坏的 blob 不能静默当成正确内容发出去）。
func (b *Blobs) Open(hash string) ([]byte, error) {
	path, err := b.checkedPath(hash)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != hash {
		return nil, fmt.Errorf("blob 内容与 hash 不一致：%s", hash)
	}
	return data, nil
}

// Stat 返回 blob 的大小。
func (b *Blobs) Stat(hash string) (int64, error) {
	path, err := b.checkedPath(hash)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// Has 报告 blob 是否存在。
func (b *Blobs) Has(hash string) bool {
	path, err := b.checkedPath(hash)
	if err != nil {
		return false
	}
	if _, err := os.Stat(path); err != nil {
		return false
	}
	return true
}

// Path 返回 blob 的落盘路径（调用方保证 hash 合法时才该用它）。
func (b *Blobs) Path(hash string) string {
	return filepath.Join(b.root, hash[:2], hash)
}

// checkedPath 校验 hash 并返回路径。
func (b *Blobs) checkedPath(hash string) (string, error) {
	if !ValidBlobHash(hash) {
		return "", fmt.Errorf("%w：%q", ErrBadHash, hash)
	}
	return b.Path(hash), nil
}

// ValidBlobHash 报告 hash 是否是 64 位小写 hex。
func ValidBlobHash(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	for index := 0; index < len(hash); index++ {
		ch := hash[index]
		if ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' {
			continue
		}
		return false
	}
	return true
}

// BlobHash 从 `blob:sha256:<hex>` 引用里取出 hash。
//
// 只认这个前缀：markdown 里的图片仍然只允许 http(s)，blob 引用只在 part 里生效。
func BlobHash(url string) (string, bool) {
	const prefix = "blob:sha256:"
	if !strings.HasPrefix(url, prefix) {
		return "", false
	}
	hash := url[len(prefix):]
	if !ValidBlobHash(hash) {
		return "", false
	}
	return hash, true
}

// BlobURL 构造 `blob:sha256:<hex>` 引用。
func BlobURL(hash string) string { return "blob:sha256:" + hash }
