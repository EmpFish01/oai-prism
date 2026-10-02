package facade

import (
	"strings"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/prism"
)

func testSandbox() *prism.Sandbox {
	return &prism.Sandbox{
		URL:   "https://prism.openai.com/s/sandboxes/proxy/",
		Token: "sandbox-token",
	}
}

// TestSandboxCache_ProjectScopedSync 验证同步状态是按项目隔离的。
//
// 为什么不能按账号缓存：资源令牌**绑定单个项目**
// （project_uuid 编码在 JWT 里）且只有 1 小时有效期。
// 一个沙箱能服务多个项目，但每个项目都要各自走一遍资源注入。
// 若按账号缓存，第二个项目会拿到"已同步"的假信号，
// 结果就是会话处理固定 122 秒后 504。
func TestSandboxCache_ProjectScopedSync(t *testing.T) {
	c := newSandboxCache(time.Minute)
	c.Put("acct", testSandbox())

	if c.Synced("acct", "p1") {
		t.Fatal("还没同步过就报告已同步")
	}

	until := time.Now().Add(time.Hour)
	c.MarkSynced("acct", "p1", until)

	if !c.Synced("acct", "p1") {
		t.Error("p1 应报告已同步")
	}
	if c.Synced("acct", "p2") {
		t.Error("p2 不该受 p1 的同步状态影响")
	}
}

// TestSandboxCache_ExpiredTokenMeansNotSynced 验证缓存失效点绑定令牌过期时间。
//
// 令牌一过期，沙箱就读不到项目文件了，此时必须重做同步 ——
// 而不是等请求失败再重试。
func TestSandboxCache_ExpiredTokenMeansNotSynced(t *testing.T) {
	c := newSandboxCache(time.Minute)
	c.Put("acct", testSandbox())
	c.MarkSynced("acct", "p1", time.Now().Add(-time.Second))

	if c.Synced("acct", "p1") {
		t.Fatal("令牌已过期，不应再报告已同步")
	}
}

func TestSandboxCache_EmptyProjectNeverSynced(t *testing.T) {
	c := newSandboxCache(time.Minute)
	c.Put("acct", testSandbox())
	c.MarkSynced("acct", "", time.Now().Add(time.Hour))
	if c.Synced("acct", "") {
		t.Error("空项目 ID 不该被当作已同步（否则所有无项目请求都会误判）")
	}
}

// TestSandboxCache_InvalidateProjectKeepsSandbox 验证只清项目、不丢沙箱。
//
// 同步失败重试时不该再花一次容器分配 —— 沙箱本身还是好的。
func TestSandboxCache_InvalidateProjectKeepsSandbox(t *testing.T) {
	c := newSandboxCache(time.Minute)
	sb := testSandbox()
	c.Put("acct", sb)
	c.MarkSynced("acct", "p1", time.Now().Add(time.Hour))
	c.MarkSynced("acct", "p2", time.Now().Add(time.Hour))

	c.InvalidateProject("acct", "p1")

	if c.Synced("acct", "p1") {
		t.Error("p1 应已失效")
	}
	if !c.Synced("acct", "p2") {
		t.Error("p2 不该被牵连")
	}
	if c.Get("acct") == nil {
		t.Fatal("沙箱本身不该被清掉")
	}
}

// TestSandboxCache_NewSandboxClearsProjects 验证换沙箱会清空同步记录。
//
// 关键正确性点：旧沙箱的同步成果对新沙箱**毫无意义** ——
// 新容器的文件系统与 Y-Sweet provider 都是空的。
// 若这里偷懒保留记录，换沙箱后第一个请求必然 504。
func TestSandboxCache_NewSandboxClearsProjects(t *testing.T) {
	c := newSandboxCache(time.Minute)
	c.Put("acct", testSandbox())
	c.MarkSynced("acct", "p1", time.Now().Add(time.Hour))

	// 换一个 token 不同的沙箱（模拟容器被回收后重新申请）
	c.Put("acct", &prism.Sandbox{
		URL:   "https://prism.openai.com/s/sandboxes/proxy/",
		Token: "another-token",
	})

	if c.Synced("acct", "p1") {
		t.Fatal("换了沙箱之后旧的同步记录必须失效")
	}
}

// TestSandboxCache_SameSandboxKeepsProjects 验证同一个沙箱重复登记时保留记录。
//
// 并发申请可能撞车：两个请求各申请一次，后写的那次不该把
// 前一次已经做好的同步成果抹掉。
func TestSandboxCache_SameSandboxKeepsProjects(t *testing.T) {
	c := newSandboxCache(time.Minute)
	c.Put("acct", testSandbox())
	c.MarkSynced("acct", "p1", time.Now().Add(time.Hour))

	// 同一个 token = 同一个沙箱
	c.Put("acct", testSandbox())

	if !c.Synced("acct", "p1") {
		t.Fatal("同一个沙箱重复登记不该丢掉同步记录")
	}
}

func TestSandboxCache_InvalidateDropsEverything(t *testing.T) {
	c := newSandboxCache(time.Minute)
	c.Put("acct", testSandbox())
	c.MarkSynced("acct", "p1", time.Now().Add(time.Hour))

	c.Invalidate("acct")

	if c.Get("acct") != nil {
		t.Error("沙箱应已被清掉")
	}
	if c.Synced("acct", "p1") {
		t.Error("同步记录应已被清掉")
	}
}

func TestSandboxCache_UnusableNotStored(t *testing.T) {
	c := newSandboxCache(time.Minute)
	c.Put("acct", &prism.Sandbox{})         // 没有 url/token
	c.Put("acct", nil)                      // 显式 nil
	c.Put("acct", &prism.Sandbox{URL: "u"}) // 缺 token
	if c.Size() != 0 {
		t.Fatalf("不完整的沙箱不该入缓存，Size = %d", c.Size())
	}
}

func TestSandboxCache_ExpiredSandboxNotReturned(t *testing.T) {
	c := newSandboxCache(20 * time.Millisecond)
	c.Put("acct", testSandbox())
	if c.Get("acct") == nil {
		t.Fatal("刚写入时应能取到")
	}
	time.Sleep(40 * time.Millisecond)
	if c.Get("acct") != nil {
		t.Fatal("TTL 过期后不该再返回")
	}
}

// TestDescribeSyncStatus 保证诊断信息带上"卡在哪一项"。
//
// 上游是把"还缺什么"拆成若干布尔标志告知的，
// 不把这些记进日志就没法定位。
func TestDescribeSyncStatus(t *testing.T) {
	if got := describeSyncStatus(nil); got != "(无状态)" {
		t.Errorf("nil 状态 = %q", got)
	}
	st := &prism.SandboxSyncStatus{Status: "syncing"}
	st.Tokens.HasResourceProjectID = true
	st.Tokens.FileCredentialSource = "resources-token"
	got := describeSyncStatus(st)
	for _, want := range []string{"syncing", "projId=true", "resources-token"} {
		if !strings.Contains(got, want) {
			t.Errorf("诊断信息缺少 %q: %s", want, got)
		}
	}
}

// TestSandboxCache_SyncingAnotherProjectUnbinds 验证沙箱一次只绑定一个项目：
// 同步了 p2 之后，p1 的记录即使未过期也不再算"已同步"，下一轮必须重新同步。
func TestSandboxCache_SyncingAnotherProjectUnbinds(t *testing.T) {
	c := newSandboxCache(time.Hour)
	c.Put("acct", &prism.Sandbox{URL: "https://sb", Token: "tok"})
	until := time.Now().Add(time.Hour)

	c.MarkSynced("acct", "p1", until)
	c.MarkSynced("acct", "p2", until)
	if c.Synced("acct", "p1") {
		t.Error("沙箱已改绑 p2，p1 不应再算已同步")
	}
	if !c.Synced("acct", "p2") {
		t.Error("p2 应为已同步")
	}

	c.MarkSynced("acct", "p1", until)
	if !c.Synced("acct", "p1") || c.Synced("acct", "p2") {
		t.Error("重新同步 p1 后应只有 p1 处于绑定状态")
	}
}
