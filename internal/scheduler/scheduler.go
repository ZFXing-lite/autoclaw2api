// Package scheduler 定时任务：token 预刷新 + 积分刷新（解冻冷却账号）+ 沙箱预热。
//
// AutoClaw 无「每日签到/打卡」端点（bundle 内仅有 wallet 查询 + 一次性 newbie/promotion 任务），
// 其每日免费额度（wallet_scope=daily 的 Gift Credits）由系统自动发放、无需主动领取。
// 因此这里不做签到任务，余额维护仅靠周期拉取积分（恢复的账号自动解冻）。
package scheduler

import (
	"context"
	"errors"
	"log"
	"time"

	"autoclaw2api/internal/pool"
	"autoclaw2api/internal/upstream"
)

// Config 调度器依赖。
type Config struct {
	Pool     *pool.Pool
	Upstream *upstream.Client
	// Interval 周期（默认 30m）。
	Interval time.Duration
	// PreRefresh 提前刷新窗口（默认 10m）。
	PreRefresh time.Duration
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config
}

// New 构建。
func New(cfg Config) *Scheduler {
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Minute
	}
	if cfg.PreRefresh <= 0 {
		cfg.PreRefresh = 10 * time.Minute
	}
	return &Scheduler{cfg: cfg}
}

// Run 主循环；启动后立即跑一轮，之后按周期执行，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	s.RunMaintenanceNow()
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.RunMaintenanceNow()
		}
	}
}

// RunMaintenanceNow 对全账号执行：token 预刷新 → 积分刷新 → 沙箱预热。
// 冷却/禁用账号跳过 token 刷新；积分刷新会解冻已恢复的账号。
func (s *Scheduler) RunMaintenanceNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		if a.RefreshToken != "" && a.NeedsRefresh(s.cfg.PreRefresh) {
			if err := s.cfg.Upstream.RefreshToken(a); err != nil {
				log.Printf("maintain %s refresh: %v", st.UID, err)
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					s.cfg.Pool.Disable(st.UID, "refresh session dead")
					continue
				}
			} else if err := a.SaveAtomic(); err != nil {
				log.Printf("maintain %s save: %v", st.UID, err)
			}
		}
		points, err := s.cfg.Upstream.CheckPoints(a)
		if err != nil {
			log.Printf("maintain %s points: %v", st.UID, err)
		} else {
			s.cfg.Pool.SetPoints(st.UID, points)
			s.cfg.Pool.ReenableIfPoints(st.UID, points)
		}
		if st.Cooling && points <= 0 {
			continue // 积分仍为 0，沙箱预热无意义
		}
		if _, _, err := s.cfg.Upstream.EnsureSandbox(a); err != nil {
			log.Printf("maintain %s sandbox: %v", st.UID, err)
		} else if err := a.SaveAtomic(); err != nil {
			log.Printf("maintain %s save sandbox: %v", st.UID, err)
		}
	}
}
