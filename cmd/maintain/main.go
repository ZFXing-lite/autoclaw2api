// main.go autoclaw2api 维护工具：对全部账号执行 积分刷新 + token 预刷新 + 沙箱确保/唤醒。
// 等价于 scheduler 的一轮 RunMaintenanceNow，便于手动触发。
// 用法：autoclaw-maintain -auths ./auths -region cn
package main

import (
	"flag"
	"log"

	"autoclaw2api/internal/auth"
	"autoclaw2api/internal/pool"
	"autoclaw2api/internal/scheduler"
	"autoclaw2api/internal/upstream"
)

func main() {
	authDir := flag.String("auths", "./auths", "凭证目录")
	region := flag.String("region", "cn", "地区：cn / global")
	stateFile := flag.String("state", "./data/state.json", "池状态文件（保留冷却状态）")
	flag.Parse()

	auths, err := auth.LoadDir(*authDir, *region)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	if len(auths) == 0 {
		log.Fatalf("no autoclaw-*.json in %s (region=%s)", *authDir, *region)
	}
	p := pool.New(*stateFile)
	defer p.Flush()
	p.SyncToDir(auths)

	up := upstream.New()
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	sch.RunMaintenanceNow()

	for _, st := range p.List() {
		mark := "healthy"
		if st.Disabled {
			mark = "disabled"
		} else if st.Cooling {
			mark = "cooling(" + st.CoolKind + ")"
		}
		log.Printf("%s points=%d %s", st.UID, st.Points, mark)
	}
	log.Printf("done: %d accounts", len(auths))
}
