package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseRoundtrip(t *testing.T) {
	raw := []byte(`{
		"accessToken": "at1",
		"refreshToken": "rt1",
		"expiresAt": 1788000000,
		"userId": "u1",
		"userName": "测试",
		"phone": "phone-placeholder",
		"deviceId": "d1",
		"region": "cn",
		"sandboxId": "cloud-sb-1",
		"sandboxEndpoint": "https://sb1.example.com/autoclaw-cloud",
		"endTimestamp": 1789000000
	}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if a.AccessToken != "at1" || a.UserID != "u1" || a.Region != "cn" || a.SandboxID != "cloud-sb-1" {
		t.Errorf("parse mismatch: %+v", a)
	}
	a.ExpiresAt = time.Now().Add(-time.Minute).Unix() // 定为已过期
	if !a.NeedsRefresh(0) {
		t.Error("needs refresh should be true (expired vs now)")
	}

	dir := t.TempDir()
	a.FilePath = filepath.Join(dir, "autoclaw-u1.json")
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	raw2, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse(raw2)
	if err != nil {
		t.Fatal(err)
	}
	if b.AccessToken != "at1" || b.SandboxEndpoint != "https://sb1.example.com/autoclaw-cloud" {
		t.Errorf("roundtrip mismatch: %+v", b)
	}
}

func TestParseMissingToken(t *testing.T) {
	if _, err := Parse([]byte(`{"refreshToken":"x"}`)); err == nil {
		t.Error("want error for missing accessToken")
	}
	if _, err := Parse([]byte("")); err == nil {
		t.Error("want error for empty")
	}
}

func TestLoadDirRegionFilter(t *testing.T) {
	dir := t.TempDir()
	cn := `{"accessToken":"at","userId":"u1","region":"cn"}`
	global := `{"accessToken":"at","userId":"u2","region":"global"}`
	bad := `{"accessToken":"","userId":"u3"}`
	_ = os.WriteFile(filepath.Join(dir, "autoclaw-u1.json"), []byte(cn), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "autoclaw-u2.json"), []byte(global), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "autoclaw-u3.json"), []byte(bad), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "other-x.json"), []byte(cn), 0o600)

	list, err := LoadDir(dir, "cn")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].UserID != "u1" {
		t.Errorf("cn list = %+v", list)
	}
	list, _ = LoadDir(dir, "global")
	if len(list) != 1 || list[0].UserID != "u2" {
		t.Errorf("global list = %+v", list)
	}
}

func TestSaveAtomicRefusesEmptyToken(t *testing.T) {
	a := &Auth{FilePath: filepath.Join(t.TempDir(), "x.json")}
	if err := a.SaveAtomic(); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("want refusal error, got %v", err)
	}
}
