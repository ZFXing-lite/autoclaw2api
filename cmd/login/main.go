// main.go autoclaw2api 登录工具：
//  1. 发短信验证码（手机号）
//  2. 输验证码换 access/refresh token
//  3. 拉用户信息 + 积分 + 确保沙箱（校验账号可用）
//  4. 落盘 auths/autoclaw-<user_id>.json
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"autoclaw2api/internal/auth"
	"autoclaw2api/internal/upstream"
)

func main() {
	phone := flag.String("phone", "", "手机号（缺省则交互输入）")
	code := flag.String("code", "", "验证码（缺省则交互输入；配合 -phone 非交互）")
	region := flag.String("region", "cn", "地区：cn / global")
	out := flag.String("out", "./auths", "凭证输出目录")
	flag.Parse()

	if *region != "cn" && *region != "global" {
		log.Fatalf("region must be cn or global, got %q", *region)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", *out, err)
	}

	up := upstream.New()

	ph := strings.TrimSpace(*phone)
	if ph == "" {
		fmt.Print("手机号: ")
		ph = strings.TrimSpace(readLine())
	}
	if ph == "" {
		log.Fatal("phone required")
	}
	deviceID := loadOrNewDevice(*out, ph)

	cd := strings.TrimSpace(*code)
	if cd == "" {
		fmt.Printf("发送验证码到 %s ...\n", maskPhone(ph))
		if err := up.SendCode(*region, deviceID, ph); err != nil {
			log.Fatalf("send code: %v", err)
		}
		fmt.Println("验证码已发送。")
		fmt.Print("验证码: ")
		cd = strings.TrimSpace(readLine())
		if cd == "" {
			log.Fatal("code required")
		}
	}

	lr, err := up.Login(*region, deviceID, ph, cd)
	if err != nil {
		log.Fatalf("login: %v", err)
	}
	fmt.Printf("登录成功 uid=%s name=%s\n", lr.UserID, lr.UserName)

	a := &auth.Auth{
		AccessToken:  lr.AccessToken,
		RefreshToken: lr.RefreshToken,
		ExpiresAt:    time.Now().Add(28 * 24 * time.Hour).Unix(), // 未返回 expires_in，保守 28 天，到期前自动刷新
		UserID:       lr.UserID,
		UserName:     lr.UserName,
		Phone:        ph,
		DeviceID:     deviceID,
		Region:       *region,
	}
	a.FilePath = filepath.Join(*out, fmt.Sprintf("autoclaw-%s.json", lr.UserID))

	// 登录后校验：用户信息 + 积分 + 沙箱（失败不阻断落盘，只告警）
	if info, err := up.UserProfile(a); err != nil {
		log.Printf("warn: user profile: %v", err)
	} else if n, _ := info["user_name"].(string); n != "" {
		a.UserName = n
	}
	if pts, err := up.CheckPoints(a); err != nil {
		log.Printf("warn: points: %v", err)
	} else {
		fmt.Printf("当前积分: %d\n", pts)
	}
	if _, _, err := up.EnsureSandbox(a); err != nil {
		log.Printf("warn: sandbox ensure: %v（首次申请可能稍慢，服务启动后会自动重试）", err)
	} else {
		if err := up.WakeSandbox(a, a.SandboxID); err != nil {
			log.Printf("warn: sandbox wake: %v", err)
		}
		fmt.Printf("沙箱就绪: %s\n", a.SandboxEndpoint)
	}

	if err := a.SaveAtomic(); err != nil {
		log.Fatalf("save auth: %v", err)
	}
	fmt.Printf("凭证已保存: %s\n", a.FilePath)
	fmt.Println("重启 autoclaw2api 容器即可加载新账号。")
}

func readLine() string {
	r := bufio.NewReader(os.Stdin)
	s, err := r.ReadString('\n')
	if err != nil && s == "" {
		return ""
	}
	return strings.TrimSpace(s)
}

func newDeviceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("web-%d-%d", time.Now().UnixNano(), time.Now().UnixNano()%1e6)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// deviceCacheFile device 缓存文件：{out}/.device-<phone>。
// 验证码绑定「发送时的 device」，跨命令行登录必须复用同一 device，
// 否则 agent-login 会判 device 不匹配。
func deviceCacheFile(out, phone string) string {
	return filepath.Join(out, ".device-"+phone)
}

// loadOrNewDevice 读取或新建一个 per-phone device 缓存。
func loadOrNewDevice(out, phone string) string {
	fp := deviceCacheFile(out, phone)
	if b, err := os.ReadFile(fp); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	d := newDeviceID()
	_ = os.WriteFile(fp, []byte(d), 0o600)
	return d
}

func maskPhone(p string) string {
	if len(p) < 7 {
		return p
	}
	return p[:3] + "****" + p[len(p)-4:]
}
