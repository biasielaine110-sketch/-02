// Pool 账号池核心：结构定义、构造（New/Set* 注入）、在途租约（Acquire/Release）
// 与账号增删（Add/SyncToDir/upsertLocked）。选号/冷却/状态/持久化见同包其他文件。
package pool

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
	dirty   atomic.Bool // 内存有变更待落盘
	// store 池状态快照镜像（redisstore.Store）；nil = 无需镜像（未配置 Redis / Noop 之外也可能 nil）。
	// SaveState/LoadState 经它接线，与本地 state.json 并存作启动恢复备份。
	store StoreSnapshotter
	// 熔断器调优（SetBreaker 注入；默认值见 defaultBreaker*）。
	breakerThreshold   int
	breakerCooldown    time.Duration
	breakerCooldownMax time.Duration
	// softRateMax 软冷却指数退避的封顶（SetSoftRateMax 注入；默认 defaultSoftRateMax）。
	softRateMax time.Duration
	// degradeThreshold / degradeCooldown / degradeCooldownMax 连败降权参数
	// （SetDegrade 注入；默认值见 defaultDegrade*，issue #114）。
	degradeThreshold   int
	degradeCooldown    time.Duration
	degradeCooldownMax time.Duration
	// 三因子加权调优（SetWeights 注入；默认值见 defaultIdle*）。
	idleWeightPerHour float64
	idleWeightMax     float64
	// maxInFlight 单账号最大在途请求数；0 = 不限（租约关闭）。
	maxInFlight int
	// maxInFlightGlobal global 域单账号在途上限分档（WAF 403 修复 P1-1：global 域
	// WAF 风控更紧，压低并发）；0 = 未设置，回落 maxInFlight（不分档，零回归）。
	maxInFlightGlobal int
	// maxInFlightMedia 媒体（生图/生视频）专用单账号在途上限：0 = 未设置，回落
	// inFlightLimit（零回归）。显式设 1 = 「一任务一账号」：媒体任务只在账号无在途
	// 请求时才能占用，于是 N 条并行媒体任务自动落到 N 个不同账号。
	//
	// 语义要点：媒体与 chat 共用 entry.inFlight 这同一枚计数，所以媒体档低于 chat 档时，
	// 媒体需要账号「当前完全空闲」才启动。
	//   - 好处：单账号总上游并发不超过已验证的 max_in_flight，对上游 WAF 友好（媒体是
	//     长任务，若与 chat 各占一份，单号瞬时并发会翻倍，而 3 这个值是照 chat 实测定的）；
	//   - 代价：chat 繁忙时媒体暂时占不到该号（选号会自动轮换到池内其他账号，14 个号的
	//     池子里基本无感）。
	maxInFlightMedia int
	// randInt64N 仅供测试注入确定性随机源；nil 时用 math/rand/v2 全局源。
	// 生产代码不应设置此字段。
	randInt64N func(n int64) int64
	// persistFails 本地 state.json 连续落盘失败计数（仅 saveLocked 在持锁下读写，无需 atomic）。
	// 用于落盘失败的日志节流：首败/每 N 次提醒/恢复各打一条，避免磁盘满时刷屏。
	persistFails int
	// pickSeq 单调递增的选号序号：每次 pick 选中账号时自增并记到 entry.usedSeq，
	// 为 LRU 兜底/防惊群提供与 time.Now() 精度无关的严格全序（Windows ~0.5ms 精度下
	// lastUsed 墙钟会全等）。仅 pick 写锁路径读写，无需 atomic。
	pickSeq uint64
	// stopCh 关闭信号：Close 关闭它使 startFlusher 的后台 goroutine 退出。
	// nil = 未启动 flusher（stateFp 为空时 New 不起 flusher）。
	stopCh chan struct{}
	// closeOnce 保证 Close 幂等（多次调用不重复 close channel）。
	closeOnce sync.Once
}

// defaultBreaker* 熔断器默认参数（FreeBuff2API 参考口径）。
func New(stateFp string) *Pool {
	p := &Pool{
		byUID:              map[string]*entry{},
		stateFp:            stateFp,
		breakerThreshold:   defaultBreakerThreshold,
		breakerCooldown:    defaultBreakerCooldown,
		breakerCooldownMax: defaultBreakerCooldownMax,
		idleWeightPerHour:  defaultIdleWeightPerHour,
		idleWeightMax:      defaultIdleWeightMax,
		degradeThreshold:   defaultDegradeThreshold,
		degradeCooldown:    defaultDegradeCooldown,
		degradeCooldownMax: defaultDegradeCooldownMax,
	}
	if stateFp != "" {
		p.load()
		p.startFlusher()
	}
	return p
}

// Close 停止后台落盘 goroutine 并做最后一次落盘（幂等）。
// 进程退出前调用，消除 startFlusher 的 goroutine 泄漏；不调用也不影响正确性
// （进程退出即回收），仅是生命周期卫生。
func (p *Pool) Close() {
	if p.stopCh == nil {
		return
	}
	p.closeOnce.Do(func() {
		close(p.stopCh)
	})
	p.Flush()
}

// SetBreaker 注入熔断器参数（main 从 config 解析后调用）。非正值保留原值（用默认）。
func (p *Pool) SetBreaker(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.breakerThreshold = threshold
	}
	if cooldown > 0 {
		p.breakerCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.breakerCooldownMax = cooldownMax
	}
}

// SetSoftRateMax 注入软冷却指数退避的封顶时长（main 从 config 解析后调用）。
// 非正值保留原值（用默认 2h），风格同 SetBreaker。
func (p *Pool) SetSoftRateMax(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d > 0 {
		p.softRateMax = d
	}
}

// SetWeights 注入三因子加权的闲置补偿参数。非正值保留原值（用默认）。
func (p *Pool) SetWeights(idlePerHour, idleMax float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idlePerHour > 0 {
		p.idleWeightPerHour = idlePerHour
	}
	if idleMax > 0 {
		p.idleWeightMax = idleMax
	}
}

// SetDegrade 注入连败降权参数（main 从 config 解析后调用，issue #114）。
// 非正值保留原值（用默认，见 defaultDegrade*），风格同 SetBreaker/SetSoftRateMax。
func (p *Pool) SetDegrade(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.degradeThreshold = threshold
	}
	if cooldown > 0 {
		p.degradeCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.degradeCooldownMax = cooldownMax
	}
}

// SetMaxInFlight 注入单账号最大在途请求数；0 = 不限。负值保留原值。
func (p *Pool) SetMaxInFlight(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlight = n
	}
}

// SetMaxInFlightGlobal 注入 global 域单账号在途上限（WAF 403 修复 P1-1 分档）；
// 0 = 未设置，global 账号回落 maxInFlight（不分档）。负值保留原值。
func (p *Pool) SetMaxInFlightGlobal(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlightGlobal = n
	}
}

// SetMaxInFlightMedia 注入媒体（生图/生视频）专用单账号在途上限；
// 0 = 未设置，回落 maxInFlightGlobal/maxInFlight（零回归，风格同 SetMaxInFlightGlobal）。
// 设 1 即「一任务一账号」。负值保留原值。
func (p *Pool) SetMaxInFlightMedia(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlightMedia = n
	}
}

// inFlightLimit 报告账号的生效在途上限（global 分档优先，回落 maxInFlight）；
// 0 = 不限。调用方需已持 p.mu（或快照过 limit，见 Acquire）。
func (p *Pool) inFlightLimit(e *entry) int {
	if p.maxInFlightGlobal > 0 && e.a.Realm() == "global" {
		return p.maxInFlightGlobal
	}
	return p.maxInFlight
}

// inFlightLimitFor 报告账号在**指定用途**下的生效在途上限：
// media=true 且 maxInFlightMedia>0 时走媒体专用档，否则回落 inFlightLimit（realm 分档）。
// 只给媒体路径用（生图/生视频），chat 路径仍走 inFlightLimit，互不影响。
// 调用方需已持 p.mu（同 inFlightLimit）。
func (p *Pool) inFlightLimitFor(e *entry, media bool) int {
	if media && p.maxInFlightMedia > 0 {
		return p.maxInFlightMedia
	}
	return p.inFlightLimit(e)
}

// inFlightFullFor 报告账号在指定用途下是否已占满在途名额。
// limit=0 不限 → 恒 false。调用方需已持 p.mu（本方法只读上限）。
func (p *Pool) inFlightFullFor(e *entry, media bool) bool {
	limit := p.inFlightLimitFor(e, media)
	if limit <= 0 {
		return false
	}
	return e.inFlight.Load() >= int64(limit)
}

// SetStore 注入池状态快照镜像（redisstore.Store）。nil 表示不镜像（纯本地恢复）。
// 必须在 SyncToDir 之前调用，使"择新恢复"发生在账号对齐之前。
func (p *Pool) SetStore(s StoreSnapshotter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.store = s
}

// RestoreFromSnapshot 择新恢复：比较本地 state.json 与 Redis 快照，采用较新者。
// 无快照、快照无 savedAt、或本地不存在/不可读时，都会被判定为"本地优先/跳过快照"，
// 同时打一条恢复来源日志。必须在 SyncToDir 之前调用（SyncToDir 只增删不入值）。
// Acquire 为 uid 占一个在途名额（chat 口径）；池上限内返回 true。
// 名额用 entry.inFlight 原子自增，满额返回 false。上限按账号 realm 分档
// （global 档 maxInFlightGlobal，P1-1；未设置回落 maxInFlight）。
func (p *Pool) Acquire(uid string) bool { return p.acquire(uid, false) }

// AcquireMedia 同 Acquire，但按**媒体专用档**判定上限（maxInFlightMedia>0 时生效，
// 否则回落 inFlightLimit）。生图/生视频必须用这个：设 maxInFlightMedia=1 即实现
// 「一条任务一个账号」——同号在途达到 1 后不再被媒体选号挑中，任务自动分散到其他账号。
// 释放用同一个 Release（媒体与 chat 共用 entry.inFlight 计数）。
func (p *Pool) AcquireMedia(uid string) bool { return p.acquire(uid, true) }

// acquire 是 Acquire/AcquireMedia 的共同实现：media 决定用哪一档上限。
// 上限先持读锁快照（inFlightLimitFor 只读字段），随后无锁 CAS 抢名额。
func (p *Pool) acquire(uid string, media bool) bool {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	if !ok {
		p.mu.RUnlock()
		return false
	}
	limit := p.inFlightLimitFor(e, media)
	p.mu.RUnlock()
	if limit <= 0 {
		// 不限：计数仍累加（供状态观测），但永不拒绝。
		e.inFlight.Add(1)
		return true
	}
	for {
		cur := e.inFlight.Load()
		if cur >= int64(limit) {
			return false
		}
		if e.inFlight.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// Release 释放一个在途名额。幂等减到 0 为止（防重复释放扣成负数）。
func (p *Pool) Release(uid string) {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	p.mu.RUnlock()
	if !ok {
		return
	}
	for {
		cur := e.inFlight.Load()
		if cur <= 0 {
			return
		}
		if e.inFlight.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// SetRandomSource 仅供测试注入确定性随机源；生产代码不应调用。
// 注入源取 n∈[0,n) 后，pickWeighted 的抽签结果完全可预测。
func (p *Pool) SetRandomSource(fn func(n int64) int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.randInt64N = fn
}

// Add 加入账号；已存在则保留原状态、更新凭证（upsert 单账号，不影响其他账号）。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upsertLocked(a)
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
// 剔除结果持久化回 state.json，避免已删账号在下次启动时被 load() 复活。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.syncToDirLocked(auths)
}

// syncToDirLocked 是 SyncToDir 的锁内实现（ReplaceFromState 复用）。调用方必须已持有 p.mu。
func (p *Pool) syncToDirLocked(auths []*auth.Auth) {
	seen := make(map[string]bool, len(auths))
	for _, a := range auths {
		seen[a.UID] = true
		p.upsertLocked(a)
	}
	changed := false
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
			changed = true
		}
	}
	if changed {
		p.saveLocked()
	}
}

// Remove 从池中移除账号并立即落盘（管理面板用）。返回被移除账号的凭证
// （含 FilePath，供调用方删除 auth 文件）；uid 不存在返回 nil。
// 在途请求的 Release 对已删条目是 no-op，无需等待。
func (p *Pool) Remove(uid string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	delete(p.byUID, uid)
	p.dirty.Store(true)
	p.saveLocked()
	return e.a
}

// upsertLocked 更新或插入单个账号；已存在则只换凭证、保留 credits/cooling 状态。
// 调用方必须已持有 p.mu；Add 与 SyncToDir 共用此 upsert 逻辑。
func (p *Pool) upsertLocked(a *auth.Auth) {
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // 保留 credits/cooling 状态
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

// Pick 返回 healthy 中积分最高的账号；无可用返回 nil。
