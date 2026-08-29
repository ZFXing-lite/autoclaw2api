package scheduler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"autoclaw2api/internal/auth"
	"autoclaw2api/internal/pool"
	"autoclaw2api/internal/upstream"
)

// fakeUserAPI 模拟 userapi：refresh / points / sandbox list / sandbox apply 均可用。
func fakeUserAPI(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.Contains(p, "/userapi/v1/refresh"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"new-at","refresh_token":"new-rt"}}`))
		case strings.Contains(p, "/agent-assetmgr/api/v2/wallets"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"total_balance":1234}}`))
		case strings.Contains(p, "/agentdr/v2/assistant/sandbox/list"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"sandbox_list":[{"sandbox_id":"sb1","sandbox_endpoint":"https://sb.example.com/ac","sandbox_status":"running","end_timestamp":1893456000}]}}`))
		case strings.Contains(p, "/agentdr/v2/assistant/sandbox/apply"):
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestRunMaintenanceNowHappyPath(t *testing.T) {
	api := fakeUserAPI(t)
	defer api.Close()

	dir := t.TempDir()
	a := &auth.Auth{
		AccessToken:     "old-at",
		RefreshToken:    "old-rt",
		ExpiresAt:       time.Now().Add(-time.Hour).Unix(), // 已过期 → 触发 refresh
		UserID:          "u1",
		UserName:        "u1",
		DeviceID:        "d1",
		Region:          "cn",
		FilePath:        dir + "/autoclaw-u1.json",
		SandboxEndpoint: "",
	}

	p := pool.New("")
	p.Add(a)
	up := upstream.New()
	up.UserAPIBaseOverride = api.URL
	up.RelayBaseOverride = api.URL // 无沙箱网络操作，仅占位

	s := New(Config{Pool: p, Upstream: up})
	s.RunMaintenanceNow()

	if a.AccessToken != "new-at" || a.RefreshToken != "new-rt" {
		t.Errorf("refresh not applied: at=%q rt=%q", a.AccessToken, a.RefreshToken)
	}
	if st, _ := p.Status("u1"); st.Points != 1234 {
		t.Errorf("points=%d want 1234", st.Points)
	}
	if a.SandboxID != "sb1" {
		t.Errorf("sandbox not ensured: %+v", a.SandboxID)
	}
}

func TestRunMaintenanceSkipsDisabled(t *testing.T) {
	// disabled 账号不刷新、不拉积分
	hit := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		http.Error(w, "should not be called", http.StatusTeapot)
	}))
	defer api.Close()

	a := &auth.Auth{UserID: "u1", AccessToken: "at", Region: "cn"}
	p := pool.New("")
	p.Add(a)
	p.Disable("u1", "dead")
	up := upstream.New()
	up.UserAPIBaseOverride = api.URL
	up.RelayBaseOverride = api.URL

	s := New(Config{Pool: p, Upstream: up})
	s.RunMaintenanceNow()
	if hit {
		t.Error("disabled account should be skipped")
	}
}
