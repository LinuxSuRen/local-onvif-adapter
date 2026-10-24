package stream

import (
	"log/slog"
	"testing"
	"time"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
)

// TestRectWithin 屏幕源预检的纯几何判断：包含负坐标（副屏位于主屏左侧/上方）。
func TestRectWithin(t *testing.T) {
	cases := []struct {
		name                 string
		x, y, w, h           int32
		vx, vy, vw, vh       int32
		want                 bool
	}{
		// 实际事故坐标：cam4 采集 (-1945,-1480) 2560x1440，
		// 拓扑塌缩后虚拟桌面只剩主屏 (0,0)-(1920,1080)。
		{"incident cam4", -1945, -1480, 2560, 1440, 0, 0, 1920, 1080, false},
		{"incident cam3", -3440, -40, 2752, 1152, 0, 0, 1920, 1080, false},
		// 三屏拓扑完整时的虚拟桌面 (从最小左上到最大右下)，两块副屏均在内。
		{"full topology cam4", -1945, -1480, 2560, 1440, -3440, -1480, 6176, 3032, true},
		{"inside", 100, 100, 800, 600, 0, 0, 1920, 1080, true},
		// 边界贴合视为在内（矩形恰与虚拟桌面重合 / 贴边）。
		{"exact", 0, 0, 1920, 1080, 0, 0, 1920, 1080, true},
		{"touch edge", 920, 980, 1000, 100, 0, 0, 1920, 1080, true},
		// 越出右/下边界同样拦截。
		{"over right", 1200, 0, 800, 600, 0, 0, 1920, 1080, false},
		{"zero size", 0, 0, 0, 0, 0, 0, 1920, 1080, true},
	}
	for _, c := range cases {
		if got := rectWithin(c.x, c.y, c.w, c.h, c.vx, c.vy, c.vw, c.vh); got != c.want {
			t.Fatalf("%s: rectWithin(%d,%d,%d,%d in %d,%d,%d,%d) = %v, want %v",
				c.name, c.x, c.y, c.w, c.h, c.vx, c.vy, c.vw, c.vh, got, c.want)
		}
	}
}

// TestAwaitScreenSourceNotBlocked 可采（或非 Windows 无预检）时立即放行，
// 不进入轮询等待。
func TestAwaitScreenSourceNotBlocked(t *testing.T) {
	// (0,0) 1x1 必在任何虚拟桌面内（主屏原点）；非 Windows 的
	// screenSourceBlocked 恒不拦截，同样立即返回。
	r := newRunner(config.Camera{ID: "t", Type: config.TypeScreen, Source: "desktop@0,0,1x1"},
		"rtsp://127.0.0.1:8554/cam/t", nil, "ffmpeg", slog.Default())
	done := make(chan bool, 1)
	go func() { done <- r.awaitScreenSource() }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("awaitScreenSource should pass immediately for in-bounds source")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("awaitScreenSource did not return immediately for in-bounds source")
	}
}
