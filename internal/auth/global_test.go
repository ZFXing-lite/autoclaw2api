package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// 覆盖「手工填充海外 token 文件」的接入路径：region=global、无沙箱缓存，
// 池首次聊天时自动 ensure sandbox。
func TestManualGlobalTokenIngestion(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "autoclaw-global-user.json"), []byte(`{
		"accessToken": "global-at",
		"refreshToken": "global-rt",
		"expiresAt": 1893456000,
		"userId": "gu1",
		"userName": "g-user",
		"deviceId": "dev-global",
		"region": "global"
	}`), 0o600)

	list, err := LoadDir(dir, "global")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 global account, got %d", len(list))
	}
	a := list[0]
	if a.Region != "global" || a.UserID != "gu1" {
		t.Fatalf("uid=%s region=%s", a.UserID, a.Region)
	}
	if a.SandboxID != "" || a.SandboxEndpoint != "" {
		t.Fatalf("sandbox cache should be empty for manual token, got %+v", a.SandboxEndpoint)
	}
	if a.AccessToken != "global-at" {
		t.Fatalf("accessToken=%q", a.AccessToken)
	}
}
