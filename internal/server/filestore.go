package server

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// fileTTL 上传文件的保留时长。
//
// 画布链路是「上传参考图 → 立刻提交生成」的秒级动作；但用户可能失败重试、
// 或同时开多个生成任务复用同一张图，故留 2h 余量。过期即回收，避免内存常驻。
const fileTTL = 2 * time.Hour

// fileStoreMaxEntries 条数上限：超出时淘汰「最早到期」的一条，防无界增长。
// 单张参考图按 10MB 估，512 条封顶约 5GB —— 实际远达不到（画布对 >2.8MB 的图会先压缩）。
const fileStoreMaxEntries = 512

// storedFile 一份上传文件的元数据与字节。
type storedFile struct {
	data    []byte
	mime    string
	expires time.Time
}

// fileStore 进程内上传文件存储。
//
// 存在的原因：画布类前端在提交生成前，必须先把参考图换成**一个 https 链接**
// （见 files.go 顶部注释）。我们不做对象存储，就把字节放在内存里，回一个指向
// 本网关自己的 URL；重启即清空——参考图是短生命周期数据，不需要持久化。
//
// 并发：put/get 都由 mu 保护；业务量（人工点几下生成）远达不到锁竞争。
type fileStore struct {
	mu    sync.Mutex
	items map[string]storedFile
}

func newFileStore() *fileStore {
	return &fileStore{items: make(map[string]storedFile)}
}

// randomFileID 生成 32 位十六进制 id（128bit 随机，不可枚举）。
func randomFileID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 在本平台不可失败；真失败时用时间戳兜底，宁可 id 弱也不能 panic。
		return hex.EncodeToString([]byte(time.Now().Format("20060102150405.000000000")))
	}
	return hex.EncodeToString(b[:])
}

// put 存入一份文件并返回其 id。
func (s *fileStore) put(data []byte, mimeType string) string {
	id := randomFileID()
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(now)
	if len(s.items) >= fileStoreMaxEntries {
		s.evictOldestLocked()
	}
	s.items[id] = storedFile{data: data, mime: mimeType, expires: now.Add(fileTTL)}
	return id
}

// get 取出文件；过期视为不存在并顺手删除。
func (s *fileStore) get(id string) (storedFile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	if !ok {
		return storedFile{}, false
	}
	if time.Now().After(it.expires) {
		delete(s.items, id)
		return storedFile{}, false
	}
	return it, true
}

func (s *fileStore) gcLocked(now time.Time) {
	for id, it := range s.items {
		if now.After(it.expires) {
			delete(s.items, id)
		}
	}
}

func (s *fileStore) evictOldestLocked() {
	oldestID := ""
	var oldest time.Time
	for id, it := range s.items {
		if oldestID == "" || it.expires.Before(oldest) {
			oldestID, oldest = id, it.expires
		}
	}
	if oldestID != "" {
		delete(s.items, oldestID)
	}
}
