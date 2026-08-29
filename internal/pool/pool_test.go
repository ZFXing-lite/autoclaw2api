package pool

import (
	"testing"
	"time"

	"autoclaw2api/internal/auth"
)

func mkAuth(uid string) *auth.Auth {
	return &auth.Auth{UserID: uid, UserName: uid, Region: "cn"}
}

func TestPickWeightsTowardHighPoints(t *testing.T) {
	p := New("")
	for i := 0; i < 10; i++ {
		p.Add(mkAuth(string(rune('a' + i))))
	}
	// 设 points：0~4 为 1, 5~9 为 100
	for i := 0; i < 5; i++ {
		p.SetPoints(string(rune('a'+i)), 1)
	}
	for i := 5; i < 10; i++ {
		p.SetPoints(string(rune('a'+i)), 100)
	}
	p.SetRandomSource(func(n int64) int64 { return n - 1 }) // 永远选中最后一个权重桶（highest points）

	var wins int
	for i := 0; i < 1000; i++ {
		got := p.Pick()
		if got.UserID >= "f" { // f..j = points 100
			wins++
		}
	}
	if wins < 900 {
		t.Errorf("high-point pick too low: %d/1000", wins)
	}
}

func TestCooldownAndDisable(t *testing.T) {
	p := New("")
	uid := "u1"
	p.Add(mkAuth(uid))
	p.Cooldown(uid, CoolSoft, time.Minute, "429")
	if st, _ := p.Status(uid); st.Cooling == false {
		t.Error("should be cooling")
	}
	p.ReenableIfPoints(uid, 10)
	if st, _ := p.Status(uid); st.Cooling {
		t.Error("should be reenabled after points")
	}
	p.Disable(uid, "dead")
	if st, _ := p.Status(uid); !st.Disabled {
		t.Error("should be disabled")
	}
	// disabled 不因积分刷新解冻
	p.ReenableIfPoints(uid, 99)
	if st, _ := p.Status(uid); !st.Disabled {
		t.Error("disabled stays disabled")
	}
	p.ReenableAfterManual(uid)
	if st, _ := p.Status(uid); st.Disabled {
		t.Error("manual reenable failed")
	}
}

func TestPickExcludingRotates(t *testing.T) {
	p := New("")
	p.Add(mkAuth("u1"))
	p.Add(mkAuth("u2"))
	p.SetPoints("u1", 100)
	p.SetPoints("u2", 100)

	seen := map[string]bool{}
	var got []string
	for {
		a := p.PickExcluding(seen)
		if a == nil {
			break // 耗尽
		}
		seen[a.UserID] = true
		got = append(got, a.UserID)
		if len(got) > 10 {
			t.Fatal("rotation did not terminate")
		}
	}
	if len(seen) != 2 {
		t.Errorf("expected both accounts exhausted, got %v", got)
	}
}

func TestStatePersistence(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	p.Add(mkAuth("u1"))
	p.Cooldown("u1", CoolHard, 30*time.Minute, "credit")
	p.SetPoints("u1", 42)
	p.Flush() // 主动落盘

	p2 := New(fp)
	if st, ok := p2.Status("u1"); !ok || st.Points != 42 || !st.Cooling {
		t.Errorf("state not restored: %+v ok=%v", st, ok)
	}
}
