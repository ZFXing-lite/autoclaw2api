// main.go autoclaw2api 积分/状态查询工具。
// 用法：
//
//	autoclaw-credit                 # 全部账号积分日报（美化输出）
//	autoclaw-credit -json           # 原始 JSON
//	autoclaw-credit -uid <uid>      # 指定账号
//	autoclaw-credit -auths <dir>    # 凭证目录（默认 ./auths）
//	autoclaw-credit -region cn      # 地区（默认 cn）
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"sort"
	"time"

	"autoclaw2api/internal/auth"
	"autoclaw2api/internal/upstream"
)

type row struct {
	UID    string `json:"uid"`
	Name   string `json:"name,omitempty"`
	Phone  string `json:"phone,omitempty"`
	Points int64  `json:"points"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

func main() {
	jsonOut := flag.Bool("json", false, "输出 JSON")
	uid := flag.String("uid", "", "指定账号 uid")
	authDir := flag.String("auths", "./auths", "凭证目录")
	region := flag.String("region", "cn", "地区：cn / global")
	flag.Parse()

	auths, err := auth.LoadDir(*authDir, *region)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	if len(auths) == 0 {
		log.Fatalf("no autoclaw-*.json in %s (region=%s)", *authDir, *region)
	}
	up := upstream.New()

	var rows []row
	for _, a := range auths {
		if *uid != "" && a.UserID != *uid {
			continue
		}
		r := row{UID: a.UserID, Name: a.UserName, Phone: a.Phone}
		pts, err := up.CheckPoints(a)
		if err != nil {
			r.Error = err.Error()
		} else {
			r.Points = pts
			r.OK = true
		}
		rows = append(rows, r)
	}
	if *uid != "" && len(rows) == 0 {
		log.Fatalf("uid %s not found", *uid)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Points > rows[j].Points })

	if *jsonOut {
		raw, _ := json.MarshalIndent(map[string]any{"accounts": rows, "time": time.Now().Format(time.RFC3339)}, "", "  ")
		fmt.Println(string(raw))
		return
	}

	var total int64
	var okCount int
	fmt.Printf("%-22s %-16s %-14s %12s\n", "UID", "NAME", "PHONE", "POINTS")
	fmt.Println("------------------------------------------------------------------------")
	for _, r := range rows {
		pts := "ERR"
		if r.OK {
			pts = fmt.Sprintf("%d", r.Points)
			total += r.Points
			okCount++
		}
		fmt.Printf("%-22s %-16s %-14s %12s\n", trunc(r.UID, 22), trunc(r.Name, 16), trunc(r.Phone, 14), pts)
		if r.Error != "" {
			fmt.Printf("  ! %s\n", r.Error)
		}
	}
	fmt.Println("------------------------------------------------------------------------")
	fmt.Printf("合计: %d 账号可用 / %d 总数, 总积分 %d\n", okCount, len(rows), total)
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
