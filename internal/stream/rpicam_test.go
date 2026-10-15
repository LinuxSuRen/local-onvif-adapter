package stream

import (
	"strings"
	"testing"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
)

// TestRpicamVidArgs rpicam-vid 命令构造：分辨率/帧率/码率携带配置值，
// --inline 保证 SPS/PPS 随关键帧重发，--intra 与 gov 约定一致。
func TestRpicamVidArgs(t *testing.T) {
	cam := config.Camera{Type: config.TypeRpicam, Width: 640, Height: 480,
		Framerate: 15, BitrateKBPS: 2048}
	got := strings.Join(rpicamVidArgs(cam), " ")
	for _, want := range []string{
		"--codec h264", "--inline", "--width 640", "--height 480",
		"--framerate 15", "--bitrate 2048000", "--intra 30", "-o -",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	// 未配置时取默认（640x480@15）。
	def := config.Camera{Type: config.TypeRpicam}
	got = strings.Join(rpicamVidArgs(def), " ")
	if !strings.Contains(got, "--width 640") || !strings.Contains(got, "--framerate 15") {
		t.Fatalf("defaults missing: %q", got)
	}
	// source 指定 libcamera 摄像头索引。
	multi := config.Camera{Type: config.TypeRpicam, Source: "1"}
	if got := strings.Join(rpicamVidArgs(multi), " "); !strings.Contains(got, "--camera 1") {
		t.Fatalf("camera index missing: %q", got)
	}
}

// TestBuildArgsRpicamCopy rpicam 源走 -c copy 直通：无编码器参数、
// 无帧率/分辨率兜底（copy 不能过滤）、-an。
func TestBuildArgsRpicamCopy(t *testing.T) {
	setEncoderForTest(t, EncoderX264)
	cam := config.Camera{Type: config.TypeRpicam, Width: 640, Height: 480, Framerate: 15}
	got := strings.Join(BuildArgs(cam, "rtsp://127.0.0.1:8554/cam/cam1"), " ")
	for _, want := range []string{"-f h264 -i pipe:", "-c copy -an", "-f rtsp"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	for _, ban := range []string{"libx264", "-preset", "-tune", " -r ", "scale="} {
		if strings.Contains(got, ban) {
			t.Fatalf("copy path must not carry %q: %q", ban, got)
		}
	}
	// rpicam 输入不加 -nostdin（需要 stdin 管道）。
	if strings.Contains(got, "-nostdin") {
		t.Fatalf("rpicam must read stdin pipe: %q", got)
	}
}
