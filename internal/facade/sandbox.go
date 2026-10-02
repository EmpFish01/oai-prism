package facade

import (
	"context"
	"sync"
	"time"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// sandboxCache 缓存沙箱，以及"这个沙箱已经为哪些项目完成过工作区同步"。
//
// 两层缓存的粒度**刻意不同**：
//
//	沙箱本身按 (账号) 缓存 —— 一个容器能服务该账号下的多个项目；
//	工作区同步按 (账号, 项目) 缓存 —— 资源令牌绑定单个项目
//	（project_uuid 编码在 JWT 里）且只有 1 小时有效期。
//
// 把同步状态也缓存起来是性能关键：整套同步要 4 次往返，
// 而沙箱与文档都还是热的，重复做纯属浪费。
type sandboxCache struct {
	ttl   time.Duration
	mu    sync.RWMutex
	items map[string]*sandboxEntry
	locks *keyedLocker
}

type sandboxEntry struct {
	sb      *prism.Sandbox
	expires time.Time

	// bound 是沙箱当前实际绑定的项目。
	//
	// 沙箱一次只服务一个项目：它的状态里资源项目、Y-Sweet 凭证都是单数
	// （hasResourceProjectId / hasCurrentYSweetToken），资源令牌也只带一个
	// projectId。给它同步项目 B 就会顶掉项目 A —— 此时 A 的同步记录虽未过期，
	// 却已不再成立。只看 projects 会误判 A 仍可用，A 的下一轮跳过同步直接 start，
	// 落到一个指向别的项目的沙箱上。
	bound string

	// projects: projectID -> 该次同步所用资源令牌的过期时刻。
	//
	// 拿令牌的过期时间当缓存失效点，而不是自己拍一个 TTL：
	// 令牌过期后沙箱就读不到项目资源了，再发请求必然失败，
	// 与其等失败再重试，不如到点就重新同步一遍。
	projects map[string]time.Time
}

func newSandboxCache(ttl time.Duration) *sandboxCache {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &sandboxCache{
		ttl:   ttl,
		items: make(map[string]*sandboxEntry, 8),
		locks: newKeyedLocker(),
	}
}

// Get 取未过期的沙箱。
func (c *sandboxCache) Get(accountID string) *prism.Sandbox {
	c.mu.RLock()
	e, ok := c.items[accountID]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.expires) || !e.sb.Usable() {
		return nil
	}
	return e.sb
}

// Put 写入（或更新）沙箱。
//
// 若该账号已有条目且**是同一个沙箱令牌**，保留其 projects 同步记录 ——
// 并发申请撞车时不该把已经做好的同步成果丢掉。
// 换成新沙箱要清记录的话走 Invalidate。
func (c *sandboxCache) Put(accountID string, sb *prism.Sandbox) {
	if !sb.Usable() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	projects := map[string]time.Time{}
	bound := ""
	if old, ok := c.items[accountID]; ok && old.sb != nil && old.sb.Token == sb.Token {
		projects, bound = old.projects, old.bound
	}
	c.items[accountID] = &sandboxEntry{
		sb:       sb,
		expires:  time.Now().Add(c.ttl),
		bound:    bound,
		projects: projects,
	}
}

// Synced 报告沙箱当前是否绑定在该项目上，且同步所用的令牌仍有效。
func (c *sandboxCache) Synced(accountID, projectID string) bool {
	if projectID == "" {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.items[accountID]
	if !ok || e.bound != projectID {
		return false
	}
	until, ok := e.projects[projectID]
	return ok && time.Now().Before(until)
}

// MarkSynced 记录一次成功的工作区同步。
func (c *sandboxCache) MarkSynced(accountID, projectID string, until time.Time) {
	if projectID == "" || until.IsZero() {
		return
	}
	c.mu.Lock()
	if e, ok := c.items[accountID]; ok {
		if e.projects == nil {
			e.projects = make(map[string]time.Time, 2)
		}
		e.projects[projectID] = until
		e.bound = projectID
	}
	c.mu.Unlock()
}

// Invalidate 丢弃该账号的整个缓存（沙箱容器被回收时调用）。
func (c *sandboxCache) Invalidate(accountID string) {
	c.mu.Lock()
	delete(c.items, accountID)
	c.mu.Unlock()
}

// InvalidateProject 只让某个项目的同步记录失效，保留沙箱本身。
//
// 用在"同步失败但沙箱还在"的场景：重试时只要重做同步，
// 不必浪费一次容器分配。
func (c *sandboxCache) InvalidateProject(accountID, projectID string) {
	c.mu.Lock()
	if e, ok := c.items[accountID]; ok {
		delete(e.projects, projectID)
		if e.bound == projectID {
			e.bound = ""
		}
	}
	c.mu.Unlock()
}

// Lock 返回按账号粒度的互斥锁，保证同一账号只有一个在飞的沙箱申请。
func (c *sandboxCache) Lock(accountID string) func() {
	return c.locks.Lock(accountID)
}

// LockSync 返回按账号（也就是按沙箱）粒度的同步互斥锁。
//
// 同一个项目的并发请求应当**等**前一个同步完成，而不是各自去签一份
// 资源令牌（那会同时开多个沙箱会话，白耗额度）。不同项目也必须排队：
// 它们同步的是同一个沙箱，交错发送令牌会互相覆盖。实测（2026-10-01）
// Codex 桌面版的标题请求与正文请求同时同步时，出现 Y-Sweet 凭证交付失败、
// 沙箱被重置、正文请求失败重试。
func (c *sandboxCache) LockSync(accountID string) func() {
	return c.locks.Lock(accountID + "\x00sync")
}

// Size 当前缓存的沙箱数（供运维端点展示）。
func (c *sandboxCache) Size() int {
	c.mu.RLock()
	n := len(c.items)
	c.mu.RUnlock()
	return n
}

// gc 周期清理过期条目。
func (c *sandboxCache) gc(ctx context.Context) {
	t := time.NewTicker(c.ttl / 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now()
			c.mu.Lock()
			for k, e := range c.items {
				if now.After(e.expires) {
					delete(c.items, k)
					continue
				}
				// 顺带清掉过期的项目同步记录，避免 projects 无限增长。
				for pid, until := range e.projects {
					if now.After(until) {
						delete(e.projects, pid)
					}
				}
			}
			c.mu.Unlock()
		}
	}
}
