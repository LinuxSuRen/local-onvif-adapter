package stream

import (
	"strings"
	"sync"
	"testing"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
)

// setEncoderForTest 设置编码器模式，并在测试结束后恢复默认。
// 测试包内串行执行，无并发竞争。
func setEncoderForTest(t *testing.T, mode string) {
	t.Helper()
	if err := SetVideoEncoder(mode); err != nil {
		t.Fatalf("SetVideoEncoder: %v", err)
	}
	encOnce = sync.Once{}
	t.Cleanup(func() {
		_ = SetVideoEncoder(EncoderAuto)
		encOnce = sync.Once{}
	})
}

func TestSetVideoEncoderValidation(t *testing.T) {
	for _, ok := range []string{EncoderAuto, EncoderX264, EncoderV4L2M2M} {
		if err := SetVideoEncoder(ok); err != nil {
			t.Fatalf("valid mode %q rejected: %v", ok, err)
		}
	}
	if err := SetVideoEncoder("hwaccel"); err == nil {
		t.Fatal("invalid mode should be rejected")
	}
	_ = SetVideoEncoder(EncoderAuto)
}

// TestResolveEncoderModes auto 恒为 libx264（硬件编码实测不可用，见
// encoder.go 实测说明）；显式指定直接生效。
func TestResolveEncoderModes(t *testing.T) {
	setEncoderForTest(t, EncoderAuto)
	if got := resolveEncoder("ffmpeg", nil); got != EncoderX264 {
		t.Fatalf("auto = %s, want %s（硬件编码仅显式 opt-in）", got, EncoderX264)
	}
	setEncoderForTest(t, EncoderV4L2M2M)
	if got := resolveEncoder("ffmpeg", nil); got != EncoderV4L2M2M {
		t.Fatalf("forced = %s, want %s", got, EncoderV4L2M2M)
	}
	setEncoderForTest(t, EncoderX264)
	if got := resolveEncoder("ffmpeg", nil); got != EncoderX264 {
		t.Fatalf("forced x264 = %s, want %s", got, EncoderX264)
	}
}

// TestVideoEncodeArgsDispatch 硬件编码不带 x264 专属参数。
func TestVideoEncodeArgsDispatch(t *testing.T) {
	hw := strings.Join(videoEncodeArgs(EncoderV4L2M2M, 20, 2048), " ")
	for _, want := range []string{"-c:v h264_v4l2m2m", "-b:v 2048k", "-g 20"} {
		if !strings.Contains(hw, want) {
			t.Fatalf("hw args missing %q: %q", want, hw)
		}
	}
	if strings.Contains(hw, "-preset") || strings.Contains(hw, "-tune") {
		t.Fatalf("v4l2m2m does not support x264 preset/tune: %q", hw)
	}

	sw := strings.Join(videoEncodeArgs(EncoderX264, 20, 2048), " ")
	for _, want := range []string{"-c:v libx264", "-preset veryfast", "-tune zerolatency", "-b:v 2048k"} {
		if !strings.Contains(sw, want) {
			t.Fatalf("sw args missing %q: %q", want, sw)
		}
	}
}

// TestFPSGuarantee 设备采集源输出侧 -r 兜底；rtsp/testsrc 不强制。
func TestFPSGuarantee(t *testing.T) {
	setEncoderForTest(t, EncoderX264)

	for _, typ := range []config.CameraType{config.TypeV4L2, config.TypeDShow, config.TypeAVFoundation} {
		cam := config.Camera{Type: typ, Source: "x", Framerate: 10}
		got := strings.Join(BuildArgs(cam, "rtsp://127.0.0.1:8554/cam/cam1"), " ")
		if !strings.Contains(got, "-r 10 ") {
			t.Fatalf("%s: framerate guarantee -r 10 missing: %q", typ, got)
		}
	}
	// 默认帧率（0 → 15）同样兜底。
	cam := config.Camera{Type: config.TypeV4L2, Source: "/dev/video0"}
	if got := strings.Join(BuildArgs(cam, "u"), " "); !strings.Contains(got, "-r 15 ") {
		t.Fatalf("default fps guarantee missing: %q", got)
	}
	// rtsp / testsrc 按源节奏，不强制。
	rtsp := config.Camera{Type: config.TypeRTSP, Source: "rtsp://x/y", Framerate: 10}
	if got := strings.Join(BuildArgs(rtsp, "u"), " "); strings.Contains(got, " -r ") {
		t.Fatalf("rtsp source should not force fps: %q", got)
	}
	src := config.Camera{Type: config.TypeTestSrc, Framerate: 10}
	if got := strings.Join(BuildArgs(src, "u"), " "); strings.Contains(got, " -r ") {
		t.Fatalf("testsrc should not force fps: %q", got)
	}
}

// TestResolutionGuarantee 设备源配置了分辨率时输出侧 scale 兜底
//（-video_size 会被不支持的驱动静默忽略）；未配置或非设备源不追加。
func TestResolutionGuarantee(t *testing.T) {
	setEncoderForTest(t, EncoderX264)
	cam := config.Camera{Type: config.TypeV4L2, Source: "/dev/video0", Width: 320, Height: 240}
	got := strings.Join(BuildArgs(cam, "u"), " ")
	if !strings.Contains(got, "-vf scale=320:240") {
		t.Fatalf("resolution guarantee missing: %q", got)
	}
	// 未配置分辨率：不追加 scale。
	cam.Width, cam.Height = 0, 0
	if got := strings.Join(BuildArgs(cam, "u"), " "); strings.Contains(got, "scale=") {
		t.Fatalf("no scale without configured resolution: %q", got)
	}
	// rtsp 源：转码自然缩放，无需强制。
	rcam := config.Camera{Type: config.TypeRTSP, Source: "rtsp://x", Width: 320, Height: 240}
	if got := strings.Join(BuildArgs(rcam, "u"), " "); strings.Contains(got, "scale=") {
		t.Fatalf("rtsp should not force scale: %q", got)
	}
}

// TestBuildArgsHardwareEncoder 硬件编码下的完整命令行：编码器切换、
// 音频参数不受影响、帧率兜底仍在。
func TestBuildArgsHardwareEncoder(t *testing.T) {
	setEncoderForTest(t, EncoderV4L2M2M)
	cam := config.Camera{Type: config.TypeV4L2, Source: "/dev/video0",
		Framerate: 10, BitrateKBPS: 1024, AudioEnabled: true, AudioSource: "default"}
	got := strings.Join(BuildArgs(cam, "rtsp://127.0.0.1:8554/cam/cam1"), " ")
	for _, want := range []string{"-c:v h264_v4l2m2m", "-b:v 1024k", "-r 10 ", "-c:a aac", "-f pulse -i default"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "-preset") {
		t.Fatalf("hw encoder should not carry preset: %q", got)
	}
}
