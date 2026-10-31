package stream

import (
	"strings"
	"testing"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
)

// TestDarwinScreenCmdUniquePerCamera 多屏并发时每路独立临时文件：
// 共享一个文件会互相 cat/rm 踩踏导致 image2pipe 流中断（双屏真机复现）。
func TestDarwinScreenCmdUniquePerCamera(t *testing.T) {
	a := darwinScreenCmd(config.Camera{ID: "cam8", Type: config.TypeScreen, Source: "0", Framerate: 15})
	b := darwinScreenCmd(config.Camera{ID: "cam9", Type: config.TypeScreen, Source: "1", Framerate: 15})
	if strings.Contains(a, "_onvif_sc_cam9") || strings.Contains(b, "_onvif_sc_cam8") {
		t.Fatalf("temp file must be per-camera:\n%s\n%s", a, b)
	}
	if !strings.Contains(a, "_onvif_sc_cam8") || !strings.Contains(b, "_onvif_sc_cam9") {
		t.Fatalf("temp file should embed camera id:\n%s\n%s", a, b)
	}
}

// TestDarwinScreenCmdDisplayMapping avfoundation 屏幕索引（0 起）映射到
// screencapture 的 1 起显示器编号；非整数 source 不指定（主屏）。
func TestDarwinScreenCmdDisplayMapping(t *testing.T) {
	got := darwinScreenCmd(config.Camera{ID: "c1", Type: config.TypeScreen, Source: "1", Framerate: 5})
	if !strings.Contains(got, "-D 2") {
		t.Fatalf("source 1 should map to display -D 2: %q", got)
	}
	got = darwinScreenCmd(config.Camera{ID: "c1", Type: config.TypeScreen, Source: "0", Framerate: 5})
	if !strings.Contains(got, "-D 1") {
		t.Fatalf("source 0 should map to main display -D 1: %q", got)
	}
	got = darwinScreenCmd(config.Camera{ID: "c1", Type: config.TypeScreen, Source: "desktop", Framerate: 5})
	if strings.Contains(got, "-D ") {
		t.Fatalf("non-integer source should not carry -D: %q", got)
	}
}

// TestDarwinScreenFeedCmdExported 快照直采路径复用同一喂帧命令（保持行为一致）。
func TestDarwinScreenFeedCmdExported(t *testing.T) {
	cam := config.Camera{ID: "c9", Type: config.TypeScreen, Source: "1", Framerate: 10}
	if DarwinScreenFeedCmd(cam) != darwinScreenCmd(cam) {
		t.Fatal("DarwinScreenFeedCmd must delegate to darwinScreenCmd")
	}
}
