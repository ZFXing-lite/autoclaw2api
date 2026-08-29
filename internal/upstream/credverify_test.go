// 真实账号验证：实时积分查询 + wallet 分类（验证「无每日签到」结论）。
package upstream

import (
	"os"
	"testing"

	"autoclaw2api/internal/auth"
)

func TestRealCredentialCheck(t *testing.T) {
	raw, err := os.ReadFile("../../auths/autoclaw-e86564cf536dfba879a5883e18eb557e.json")
	if err != nil {
		t.Skip("no real credential")
	}
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	up := New()

	// Q3: 实时总余额
	total, err := up.CheckPoints(a)
	t.Logf("[Q3 实时积分] total=%d err=%v", total, err)
	if err != nil {
		t.Logf("CheckTotalCred 失败: %v（可能需备用钱包接口）", err)
	}

	// Q2: wallet 分类（daily=每日赠送，看是否已有/自动发放）
	scopes, err := up.WalletScopes(a)
	t.Logf("[Q2 签到] walletScopes err=%v", err)
	if err == nil {
		for _, sc := range scopes {
			t.Logf("   scope=%s balance=%d", sc.Scope, sc.Balance)
		}
	}
}
