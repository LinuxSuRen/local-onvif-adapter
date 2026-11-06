package stream

import (
	"strings"
	"testing"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
)

// TestUnitreeInputArgs 宇树 A2 输入形态：本机取帧泵喂 stdin，
// ffmpeg 以 image2pipe 读 pipe:，帧率声明随配置。
func TestUnitreeInputArgs(t *testing.T) {
	cam := config.Camera{Type: config.TypeUnitreeA2, Framerate: 10}
	got := strings.Join(InputArgs(cam), " ")
	for _, want := range []string{"-f image2pipe", "-framerate 10", "-i pipe:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

// TestUnitreeBuildArgs 完整命令约束：不加 -nostdin（stdin 是帧来源）、
// 输出侧 -r 兜底（RPC 轮询帧率可能低于目标）、纯视频 -an（帧流无音轨）。
func TestUnitreeBuildArgs(t *testing.T) {
	cam := config.Camera{
		Type: config.TypeUnitreeA2,
		Framerate: 10, BitrateKBPS: 2048, AudioEnabled: true,
	}
	got := strings.Join(BuildArgs(cam, "rtsp://127.0.0.1:8554/cam/cam1"), " ")
	if strings.Contains(got, "-nostdin") {
		t.Fatalf("unitree_a2 must not use -nostdin: %q", got)
	}
	for _, want := range []string{"-f image2pipe", "-r 10", "-an"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	// 配置分辨率时输出侧 scale 兜底（帧自带 1920×1080）。
	cam.Width, cam.Height = 1280, 720
	got = strings.Join(BuildArgs(cam, "rtsp://127.0.0.1:8554/cam/cam1"), " ")
	if !strings.Contains(got, "scale=1280:720") {
		t.Fatalf("resolution guarantee missing: %q", got)
	}
}

// TestIsUnitreeLocal 源校验：本类型只在本机取帧，源必须留空或 "local"。
func TestIsUnitreeLocal(t *testing.T) {
	for _, s := range []string{"", "   ", "local", "Local", "LOCAL"} {
		if !IsUnitreeLocal(s) {
			t.Fatalf("%q should be accepted", s)
		}
	}
	for _, s := range []string{"192.168.124.162", "unitree@host", "local-1", "host:22"} {
		if IsUnitreeLocal(s) {
			t.Fatalf("%q should be rejected", s)
		}
	}
}

// TestUnitreePumpScriptRequirements 内嵌取帧脚本的关键行为锚点：
// 失败上限退出（交监督器重启）、按 fps 节拍、SDK 路径按固件版本 glob。
func TestUnitreePumpScriptRequirements(t *testing.T) {
	for _, want := range []string{
		"GetImageSample",
		"glob.glob",
		"sys.exit(2)",
		"ChannelFactoryInitialize",
	} {
		if !strings.Contains(unitreePumpScript, want) {
			t.Fatalf("pump script missing %q", want)
		}
	}
}
