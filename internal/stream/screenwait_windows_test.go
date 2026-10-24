//go:build windows

package stream

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
)

// TestScreenSourceBlockedRealDesktop 在真实显示拓扑上验证预检：
// 主屏原点矩形放行；远离虚拟桌面的矩形拦截并给出人类可读原因。
func TestScreenSourceBlockedRealDesktop(t *testing.T) {
	if reason, blocked := screenSourceBlocked(config.Camera{Source: "desktop@0,0,10x10"}); blocked {
		t.Fatalf("(0,0) 10x10 should always be inside the virtual desktop, got blocked: %s", reason)
	}
	reason, blocked := screenSourceBlocked(config.Camera{Source: "desktop@-99999,-99999,10x10"})
	if !blocked {
		t.Fatal("far-away rect should be blocked")
	}
	for _, want := range []string{"超出当前虚拟桌面", "RDP"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason missing %q: %s", want, reason)
		}
	}
	// 整桌面/窗口标题源不预检。
	if _, blocked := screenSourceBlocked(config.Camera{Source: "desktop"}); blocked {
		t.Fatal("plain desktop source should not be blocked")
	}
	if _, blocked := screenSourceBlocked(config.Camera{Source: "title=some window"}); blocked {
		t.Fatal("window title source should not be blocked")
	}
}

// TestAwaitScreenSourceStopWhileSuspended 挂起等待期间 Stop 必须立刻返回 false
//（不因轮询周期卡住调用方）。
func TestAwaitScreenSourceStopWhileSuspended(t *testing.T) {
	old := screenWaitPoll
	screenWaitPoll = 10 * time.Millisecond
	t.Cleanup(func() { screenWaitPoll = old })

	r := newRunner(config.Camera{ID: "t", Type: config.TypeScreen, Source: "desktop@-99999,-99999,10x10"},
		"rtsp://127.0.0.1:8554/cam/t", nil, "ffmpeg", slog.Default())
	done := make(chan bool, 1)
	go func() { done <- r.awaitScreenSource() }()

	// 等待进入挂起循环（首轮 select 走 poll 分支），再发停止。
	time.Sleep(50 * time.Millisecond)
	r.Stop()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("awaitScreenSource should return false when stopped while suspended")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("awaitScreenSource did not observe stop")
	}
}
