package stream

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
)

func TestValidatePushTarget(t *testing.T) {
	for _, ok := range []string{
		"rtsp://cloud:8554/robot/r1/cam/c0",
		"rtsp://user:pass@cloud:8554/live/x",
		"rtmp://ingest.example.com/live/key",
		"rtmps://ingest.example.com/live/key",
		"srt://cloud:9000?streamid=publish/x",
	} {
		if err := validatePushTarget(ok); err != nil {
			t.Errorf("target %q should be valid, got %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://cloud:8080/x",       // 不支持的 scheme
		"rtmp://cloud/live/a|b",     // tee 保留字符
		"rtsp://cloud/live/[extra]", // tee 保留字符
		"rtsp://",                   // 缺 host
		"ftp://cloud/x",             // 不支持的 scheme
	} {
		if err := validatePushTarget(bad); err == nil {
			t.Errorf("target %q should be rejected", bad)
		}
	}
}

func TestOutputArgs(t *testing.T) {
	// 无推流目标：保持历史单输出形态（兼容旧版本行为）。
	got := strings.Join(outputArgs("rtsp://127.0.0.1:8554/cam/c0", nil), " ")
	if got != "-f rtsp -rtsp_transport tcp rtsp://127.0.0.1:8554/cam/c0" {
		t.Fatalf("legacy single output: %q", got)
	}
	// 有推流目标：tee 多路，本地 RTSP 在前，目标按 scheme 选择封装。
	got = strings.Join(outputArgs("rtsp://127.0.0.1:8554/cam/c0", []string{
		"rtsp://u:p@cloud:8554/robot/r1/c0",
		"rtmp://ingest/live/key",
	}), " ")
	want := "-f tee " +
		"[f=rtsp:rtsp_transport=tcp]rtsp://127.0.0.1:8554/cam/c0" +
		"|[f=rtsp:rtsp_transport=tcp]rtsp://u:p@cloud:8554/robot/r1/c0" +
		"|[f=flv]rtmp://ingest/live/key"
	if got != want {
		t.Fatalf("tee output:\n got %q\nwant %q", got, want)
	}
}

func TestBuildArgsWithPushTargets(t *testing.T) {
	cam := config.Camera{Type: config.TypeTestSrc, Width: 1280, Height: 720, Framerate: 15}
	args := strings.Join(buildArgs(cam, "rtsp://127.0.0.1:8554/cam/c0",
		[]string{"rtsp://cloud:8554/r/c0"}, "ffmpeg", testLogger()), " ")
	if !strings.Contains(args, "-f tee ") || !strings.Contains(args, "rtsp://cloud:8554/r/c0") {
		t.Fatalf("encoded path should use tee with remote target: %s", args)
	}
	// rpicam 直通路径同样切换 tee（-c copy 不受影响）。
	rpi := config.Camera{Type: config.TypeRpicam, Width: 1280, Height: 720, Framerate: 15}
	args = strings.Join(buildArgs(rpi, "rtsp://127.0.0.1:8554/cam/c0",
		[]string{"rtsp://cloud:8554/r/c0"}, "ffmpeg", testLogger()), " ")
	if !strings.Contains(args, "-c copy -an") || !strings.Contains(args, "-f tee ") {
		t.Fatalf("rpicam path should keep copy and use tee: %s", args)
	}
}

func TestSpecWithPush(t *testing.T) {
	base := config.Camera{ID: "c0", Type: config.TypeTestSrc}
	if specWithPush(base, nil) != Spec(base) {
		t.Fatal("no targets should equal base spec")
	}
	one := specWithPush(base, []string{"rtsp://a"})
	two := specWithPush(base, []string{"rtsp://a", "rtsp://b"})
	if one == two || one == Spec(base) {
		t.Fatal("target list must be part of the spec fingerprint")
	}
	// 顺序不敏感（targetsForLocked 已排序）。
	if specWithPush(base, []string{"rtsp://a", "rtsp://b"}) !=
		specWithPush(base, []string{"rtsp://b", "rtsp://a"}) {
		t.Fatal("spec fingerprint should be order-insensitive")
	}
}

// newPushTestManager 构造带单个 rpicam 摄像头的会话管理器；
// bin 指向不存在的路径避免测试中真的拉起 ffmpeg（启动失败按退避重试，无害）。
// 日志走 io.Discard：janitor 与 runner 两个后台 goroutine 会并发写日志，
// testLogger 的 strings.Builder 非并发安全（-race 实测告警）。
// 摄像头选 rpicam 类型：其 buildArgs 直通路径不触碰 encoder 包级状态
// （resolveEncoder/encOnce），后台 runner 与其他测试并发改编码器全局不会竞态。
func newPushTestManager(t *testing.T) *Manager {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewManager(func() string { return "rtsp://127.0.0.1:8554" }, "/nonexistent-ffmpeg", logger)
	cam := config.Camera{ID: "c0", Type: config.TypeRpicam, Enabled: true}
	m.Sync([]config.Camera{cam})
	return m
}

func TestPushSessionLifecycle(t *testing.T) {
	m := newPushTestManager(t)
	m.pushTTL = 200 * time.Millisecond

	// 创建 + 幂等去重：同 (camera,target) 返回同一 token 并续租。
	token, existed, err := m.StartPush("c0", "profile_c0", "rtsp://u:p@cloud:8554/r/c0")
	if err != nil || existed || token == "" {
		t.Fatalf("StartPush: token=%q existed=%t err=%v", token, existed, err)
	}
	again, existed, err := m.StartPush("c0", "profile_c0", "rtsp://u:p@cloud:8554/r/c0")
	if err != nil || !existed || again != token {
		t.Fatalf("dedupe: token=%q existed=%t err=%v", again, existed, err)
	}
	// 快照：目标脱敏、状态 starting。
	ss := m.PushSessions()
	if len(ss) != 1 || ss[0].Target != "rtsp://u:***@cloud:8554/r/c0" || ss[0].Status != pushStatusStarting {
		t.Fatalf("snapshot: %+v", ss)
	}
	// 进程启动回调 → running。
	m.noteRunStart("c0")
	if s := m.PushSessions()[0]; s.Status != pushStatusRunning {
		t.Fatalf("status after run start: %s", s.Status)
	}
	// 续租/未知 token。
	if err := m.RenewPush(token); err != nil {
		t.Fatalf("RenewPush: %v", err)
	}
	if err := m.RenewPush("push_missing"); err == nil {
		t.Fatal("RenewPush on missing token should fail")
	}
	// 显式停止。
	if err := m.StopPush(token); err != nil {
		t.Fatalf("StopPush: %v", err)
	}
	if len(m.PushSessions()) != 0 {
		t.Fatal("session should be removed after StopPush")
	}
	if err := m.StopPush(token); err == nil {
		t.Fatal("StopPush twice should fail")
	}
}

func TestPushSessionLeaseExpiry(t *testing.T) {
	m := newPushTestManager(t)
	m.pushTTL = 30 * time.Millisecond
	token, _, err := m.StartPush("c0", "profile_c0", "rtsp://cloud:8554/r/c0")
	if err != nil {
		t.Fatalf("StartPush: %v", err)
	}
	// 未到期回收无效。
	m.expirePushes()
	if len(m.PushSessions()) != 1 {
		t.Fatal("session should survive before expiry")
	}
	time.Sleep(50 * time.Millisecond)
	m.expirePushes()
	if len(m.PushSessions()) != 0 {
		t.Fatal("expired session should be reaped")
	}
	_ = token
}

func TestPushFailureBudget(t *testing.T) {
	m := newPushTestManager(t)
	if _, _, err := m.StartPush("c0", "profile_c0", "rtsp://cloud:8554/r/c0"); err != nil {
		t.Fatalf("StartPush: %v", err)
	}
	// 长命运行重置计数，不应消耗预算。
	for i := 0; i < pushFailBudget; i++ {
		m.noteRunExit("c0", time.Minute, "long run")
	}
	if len(m.PushSessions()) != 1 {
		t.Fatal("long runs must not exhaust the budget")
	}
	// 短命连续失败达预算：会话摘除（failed 终态）。
	for i := 0; i < pushFailBudget; i++ {
		m.noteRunExit("c0", 2*time.Second, "exit status 1; ffmpeg: connection refused")
	}
	if len(m.PushSessions()) != 0 {
		t.Fatalf("budget exhausted should drop sessions, got %+v", m.PushSessions())
	}
	// 预算未满时不摘除。
	if _, _, err := m.StartPush("c0", "profile_c0", "rtsp://cloud:8554/r/c0"); err != nil {
		t.Fatalf("StartPush again: %v", err)
	}
	for i := 0; i < pushFailBudget-1; i++ {
		m.noteRunExit("c0", 2*time.Second, "boom")
	}
	if len(m.PushSessions()) != 1 {
		t.Fatal("below budget sessions should survive")
	}
}

func TestTargetsForLockedDedupSorted(t *testing.T) {
	m := newPushTestManager(t)
	for _, tgt := range []string{"rtsp://b", "rtsp://a", "rtsp://b"} {
		if _, _, err := m.StartPush("c0", "profile_c0", tgt); err != nil {
			t.Fatalf("StartPush(%s): %v", tgt, err)
		}
	}
	if _, _, err := m.StartPush("c9", "profile_c9", "rtsp://x"); err != nil {
		t.Fatalf("StartPush other cam: %v", err)
	}
	m.mu.Lock()
	got := m.targetsForLocked("c0")
	m.mu.Unlock()
	if len(got) != 2 || got[0] != "rtsp://a" || got[1] != "rtsp://b" {
		t.Fatalf("targets should be deduped and sorted: %v", got)
	}
}
