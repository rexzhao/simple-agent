package markdown

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// LRU 是带容量上限的 LRU 缓存（泛型：渲染块与高亮行各用一份）。
//
// 为什么必须有：视图是状态的纯函数，每帧都会把全部可见消息重新渲染一遍。
// 流式时如果每帧都重新解析所有可见消息，长会话会明显卡顿。键是
// (messageID, partIndex, 内容哈希)：内容没变就直接复用，正在流式的那一条
// 只有它自己的哈希会变。
type LRU[T any] struct {
	limit int
	order *list.List
	items map[string]*list.Element
	// hits/misses 便于测试与诊断
	hits, misses int
}

// Cache 是渲染中间表示的 LRU 缓存。
type Cache = LRU[[]Block]

// entry 是缓存项。
type entry[T any] struct {
	key   string
	value T
}

// NewCache 构造渲染块缓存（limit 为最多保留多少条）。
func NewCache(limit int) *Cache { return NewLRU[[]Block](limit) }

// NewLRU 构造任意类型的 LRU 缓存。
func NewLRU[T any](limit int) *LRU[T] {
	if limit <= 0 {
		limit = 256
	}
	return &LRU[T]{limit: limit, order: list.New(), items: map[string]*list.Element{}}
}

// Key 生成缓存键。
func Key(messageID string, partIndex int, content string) string {
	sum := sha256.Sum256([]byte(content))
	return messageID + "/" + strconv.Itoa(partIndex) + "/" + hex.EncodeToString(sum[:8])
}

// Get 取缓存；第二个返回值表示是否命中。
func (c *LRU[T]) Get(key string) (T, bool) {
	if element, ok := c.items[key]; ok {
		c.order.MoveToFront(element)
		c.hits++
		return element.Value.(*entry[T]).value, true
	}
	var zero T
	c.misses++
	return zero, false
}

// Put 写入缓存（超出上限时淘汰最久未用的）。
func (c *LRU[T]) Put(key string, value T) {
	if c.limit <= 0 {
		return
	}
	if element, ok := c.items[key]; ok {
		element.Value.(*entry[T]).value = value
		c.order.MoveToFront(element)
		return
	}
	element := c.order.PushFront(&entry[T]{key: key, value: value})
	c.items[key] = element
	for c.order.Len() > c.limit {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*entry[T]).key)
	}
}

// Len 返回当前缓存条数。
func (c *LRU[T]) Len() int { return len(c.items) }

// Stats 返回命中/未命中次数。
func (c *LRU[T]) Stats() (hits, misses int) { return c.hits, c.misses }
