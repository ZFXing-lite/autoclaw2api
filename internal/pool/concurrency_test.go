package pool

import (
	"sync"
	"testing"
)

// 并发挑号 + 冷却 + 状态查询不应 data race / panic。
// Pick 在异常高并发下可能因全部账号刚被冷却而返回 nil，属预期降级信号（调用方转 503），
// 此处只验证：不 panic、不 data race、账号计数稳定。
func TestConcurrentPickCooldownStatus(t *testing.T) {
	p := New("")
	for i := 0; i < 20; i++ {
		p.Add(mkAuth(string(rune('a' + i))))
		p.SetPoints(string(rune('a'+i)), 10)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for k := 0; k < 500; k++ {
				a := p.Pick()
				if a == nil {
					continue // 预期：并发冷却瞬间可能无可用账号
				}
				p.SetPoints(a.UserID, 10)
				p.Cooldown(a.UserID, CoolSoft, 50_000_000, "t") // 50ms 快速解冻
				p.Status(a.UserID)
				p.NoteSuccess(a.UserID)
				p.NoteError(a.UserID, 3, 1e9)
			}
		}(g)
	}
	wg.Wait()
	if total, _ := p.Counts(); total != 20 {
		t.Errorf("total=%d want 20", total)
	}
}
