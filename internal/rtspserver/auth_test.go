package rtspserver

import (
	"strconv"
	"testing"
	"time"
)

// 认证场景:服务端开启凭证后,读者/发布者未带凭证被 401 拒绝,
// URL userinfo 凭证(gortsplib 自动 401 重试 Basic/Digest)全流程通过。
const (
	authUser = "admin"
	authPass = "p@ss:word"
)

func startAuthTestServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	port := freeTCPPort(t)
	srv := New("127.0.0.1:"+itoa(port), "", "", quietLogger())
	srv.PublisherWait = 3 * time.Second
	srv.Creds = func() (string, string, bool) { return authUser, authPass, true }
	if err := srv.Start(); err != nil {
		t.Fatalf("start rtsp server: %v", err)
	}
	t.Cleanup(srv.Close)
	base := "rtsp://127.0.0.1:" + itoa(port)
	credURL := "rtsp://" + authUser + ":p%40ss%3Aword@127.0.0.1:" + itoa(port)
	return srv, base, credURL
}

func itoa(v int) string {
	return strconv.Itoa(v)
}

func TestAuthDescribeRejectedWithoutCreds(t *testing.T) {
	_, base, _ := startAuthTestServer(t)
	_, waitErr := subscribe(t, base, "cam/cam1")
	if waitErr == nil || waitErr() == nil {
		t.Fatal("describe without credentials should fail")
	}
}

func TestAuthPublishAndReadWithCreds(t *testing.T) {
	_, _, credURL := startAuthTestServer(t)

	// 发布者携带 userinfo(特殊字符已转义),ANNOUNCE 走认证
	pub, medi := publish(t, credURL, "cam/cam1")
	defer pub.Close()

	// 读者携带 userinfo,DESCRIBE/SETUP/PLAY 全链路认证;
	// 订阅完成后再发包(直播分发不缓存历史帧)
	pkts, waitErr := subscribe(t, credURL, "cam/cam1")
	if waitErr != nil {
		t.Fatalf("read with credentials: %v", waitErr())
	}
	writePackets(t, pub, medi, 3)
	for i := 0; i < 3; i++ {
		select {
		case <-pkts:
		case <-time.After(3 * time.Second):
			t.Fatalf("packet %d not received", i)
		}
	}
}

func TestAuthCredsDisabledKeepsOpen(t *testing.T) {
	// 认证开关关闭(ok=false):无凭证照常读写
	port := freeTCPPort(t)
	srv := New("127.0.0.1:"+itoa(port), "", "", quietLogger())
	srv.PublisherWait = 3 * time.Second
	srv.Creds = func() (string, string, bool) { return "", "", false }
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(srv.Close)
	base := "rtsp://127.0.0.1:" + itoa(port)
	pub, medi := publish(t, base, "cam/cam1")
	defer pub.Close()
	pkts, waitErr := subscribe(t, base, "cam/cam1")
	if waitErr != nil {
		t.Fatalf("read without auth should work when disabled: %v", waitErr())
	}
	writePackets(t, pub, medi, 2)
	for i := 0; i < 2; i++ {
		select {
		case <-pkts:
		case <-time.After(3 * time.Second):
			t.Fatalf("packet %d not received", i)
		}
	}
}
