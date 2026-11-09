package host

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"simple-agent/internal/wire"
)

// 展示端截图/粘贴的图片要落到**执行端**的 blob 库，模型才能读；执行端的图片
// 展示端也要按需拉取。协议：
//
//	展示端 → 执行端：blob.put（分块，kind=blob.put）+ blob.commit{hash}
//	展示端 → 执行端：blob.get{hash}
//	执行端 → 展示端：blob.data（分块，kind=blob.data，MsgID=hash）或 blob.error{hash}
const blobCacheBudget = 512 << 20 // 512MB，配置项见 viewer.blob_cache_mb

// serverBlobs 实现执行端的 blob 收发（挂在 execServer 上）。
func (s *execServer) handleBlobPutChunk(payload []byte) {
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	s.putDone[hash] = payload
}

func (s *execServer) handleBlobCommit(body []byte) {
	var request struct {
		Hash string `json:"hash"`
	}
	if err := wire.Decode(body, &request); err != nil {
		return
	}
	data, ok := s.putDone[request.Hash]
	delete(s.putDone, request.Hash)
	if !ok {
		_ = s.send(msgBlobError, map[string]any{"hash": request.Hash, "error": "上传未完成"})
		return
	}
	if s.local.Blobs() == nil {
		_ = s.send(msgBlobError, map[string]any{"hash": request.Hash, "error": "执行端未启用图片存储"})
		return
	}
	hash, err := s.local.Blobs().Put(data)
	if err != nil {
		_ = s.send(msgBlobError, map[string]any{"hash": request.Hash, "error": err.Error()})
		return
	}
	_ = s.send(msgBlobOK, map[string]any{"hash": hash})
}

func (s *execServer) handleBlobGet(body []byte) {
	var request struct {
		Hash string `json:"hash"`
	}
	if err := wire.Decode(body, &request); err != nil {
		return
	}
	if s.local.Blobs() == nil {
		_ = s.send(msgBlobError, map[string]any{"hash": request.Hash, "error": "执行端未启用图片存储"})
		return
	}
	data, err := s.local.Blobs().Open(request.Hash)
	if err != nil {
		_ = s.send(msgBlobError, map[string]any{"hash": request.Hash, "error": "图片不存在"})
		return
	}
	// 一个逻辑项入队，写 goroutine 切块（大图不会撑爆队列）
	if err := s.enqueueLarge(msgBlobData, request.Hash, data); err != nil {
		// 调试：入队失败时必须让展示端知道，否则它会一直等到超时（或拿到空数据）
		_ = s.send(msgBlobError, map[string]any{"hash": request.Hash, "error": "图片传输失败：" + err.Error()})
		return
	}
}

// ---- 展示端 ----

// remoteBlobs 是展示端的 blob 服务：写入转发给执行端，读取按需拉取 + LRU 缓存。
type remoteBlobs struct {
	mgr  *connManager
	mu   sync.Mutex
	data map[string][]byte
	used map[string]int64
	size int64
	max  int64
	// waiting 里每条请求的结果由连接管理器直接写入（**不经过界面线程**，
	// 否则在界面线程上调 Open 会自己等自己 30 秒）。
	waiting map[string][]*blobWait
	clock   int64
}

type blobWait struct {
	hash string
	ch   chan blobResult
}

// blobTimeout 按数据量估算等待上限：基础 30s，每 MB 加 5s。
func blobTimeout(size int) time.Duration {
	extra := time.Duration(size>>20) * 5 * time.Second
	if extra > 5*time.Minute {
		extra = 5 * time.Minute
	}
	return 30*time.Second + extra
}

type blobResult struct {
	data []byte
	err  error
}

func newRemoteBlobs(mgr *connManager, max int64) *remoteBlobs {
	return &remoteBlobs{mgr: mgr, data: map[string][]byte{}, used: map[string]int64{},
		max: max, waiting: map[string][]*blobWait{}}
}

func (b *remoteBlobs) Has(hash string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.data[hash]
	return ok
}

// Get 返回本地缓存的图片；没有就报错（渲染路径用 Has 判断是否需要 Open）。
func (b *remoteBlobs) Get(hash string) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.data[hash]
	if ok {
		b.clock++
		b.used[hash] = b.clock
	}
	return data, ok
}

// Put 把图片上传到执行端，返回内容 hash。
func (b *remoteBlobs) Put(data []byte) (string, error) {
	if b.mgr == nil {
		return "", errors.New("未连接执行端")
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	b.mu.Lock()
	b.data[hash] = data
	b.used[hash] = b.clock
	b.clock++
	b.size += int64(len(data))
	b.evictLocked()
	b.mu.Unlock()

	// 先登记等待，再入队：执行端回 blob.ok 才算"写成功"，这条规则跨进程也不能绕过
	// （否则断线/磁盘满时事件引用了执行端并不存在的 blob，模型只看到"[图片丢失]"）。
	wait := b.watch(hash)
	if b.mgr == nil || b.mgr.submitWait(mItem{large: &largePayload{kind: msgBlobPut, msgID: hash, payload: data}}) != nil {
		b.forget(wait)
		return "", errTransportBusy
	}
	body, _ := wire.Encode(map[string]any{"hash": hash})
	if b.mgr.submitWait(mItem{packet: wire.Packet{Kind: msgBlobCommit, Body: body}}) != nil {
		b.forget(wait)
		return "", errTransportBusy
	}
	select {
	case result := <-wait.ch:
		if result.err != nil {
			return "", result.err
		}
		return hash, nil
	case <-time.After(blobTimeout(len(data))):
		b.forget(wait)
		return "", errBlobTimeout
	}
}

// watch 登记一个等待回包的请求。
func (b *remoteBlobs) watch(hash string) *blobWait {
	wait := &blobWait{hash: hash, ch: make(chan blobResult, 1)}
	b.mu.Lock()
	b.waiting[hash] = append(b.waiting[hash], wait)
	b.mu.Unlock()
	return wait
}

// FailAll 唤醒所有等待中的 blob 请求（断线、关闭连接时调用）。
//
// 不这么做的话，断线前发出的 Put/Open 会永远挂着，界面一直是"正在加载"。
func (b *remoteBlobs) FailAll(err error) {
	if b == nil {
		return
	}
	b.mu.Lock()
	waiting := b.waiting
	b.waiting = map[string][]*blobWait{}
	b.mu.Unlock()
	for _, waits := range waiting {
		for _, wait := range waits {
			wait.ch <- blobResult{err: err}
		}
	}
}

// Open 向执行端要图片。**只能在后台 goroutine 调用**：它会阻塞等回包。
func (b *remoteBlobs) Open(hash string) ([]byte, error) {
	if data, ok := b.Get(hash); ok {
		return data, nil
	}
	wait := b.watch(hash)
	if b.mgr == nil || b.mgr.submitWait(mItem{packet: wire.Packet{Kind: msgBlobGet, Body: mustEncode(map[string]any{"hash": hash})}}) != nil {
		b.forget(wait)
		return nil, errTransportBusy
	}
	select {
	case result := <-wait.ch:
		return result.data, result.err
	case <-time.After(blobTimeout(0)):
		b.forget(wait)
		return nil, errBlobTimeout
	}
}

func (b *remoteBlobs) forget(wait *blobWait) {
	b.mu.Lock()
	defer b.mu.Unlock()
	waits := b.waiting[wait.hash]
	for i, w := range waits {
		if w == wait {
			waits = append(waits[:i], waits[i+1:]...)
			break
		}
	}
	if len(waits) == 0 {
		delete(b.waiting, wait.hash)
	} else {
		b.waiting[wait.hash] = waits
	}
}

// deliverBlob 由连接管理器调用（不经过界面线程）。
//
// data 为 nil 表示"这次回包不携带内容"（目前就是 blob.ok：上传成功后执行端只说
// "收到了"，并不回数据）。这种情况下**必须保留已有缓存**，不能把 nil 写进去——
// 否则刚上传的图片会被自己覆盖成空，界面显示一张坏图（修过的一个真实 bug）。
func (b *remoteBlobs) deliverBlob(hash string, data []byte, err error) {
	b.mu.Lock()
	waits := b.waiting[hash]
	delete(b.waiting, hash)
	if err == nil && data != nil {
		b.data[hash] = data
		b.clock++
		b.used[hash] = b.clock
		b.size += int64(len(data))
		b.evictLocked()
	}
	b.mu.Unlock()
	for _, w := range waits {
		w.ch <- blobResult{data: data, err: err}
	}
}

var errBlobTimeout = errors.New("图片传输超时")

func (b *remoteBlobs) evictLocked() {
	for b.max > 0 && b.size > b.max && len(b.used) > 0 {
		var oldest string
		var stamp int64
		for hash, used := range b.used {
			if oldest == "" || used < stamp {
				oldest, stamp = hash, used
			}
		}
		b.size -= int64(len(b.data[oldest]))
		delete(b.data, oldest)
		delete(b.used, oldest)
	}
}

var errTransportBusy = errors.New("连接繁忙，图片请求未发送")
