package stream

import (
	"reflect"
	"strings"
	"testing"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
)

func TestInputArgsRealSense(t *testing.T) {
	c := config.Camera{
		Type:   config.TypeRealSense,
		Source: "tcp://127.0.0.1:18021",
	}
	got := InputArgs(c)
	want := []string{
		"-f", "rawvideo", "-pixel_format", "rgb24",
		"-video_size", "640x480", "-framerate", "15",
		"-i", "tcp://127.0.0.1:18021",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("default geometry:\n got %v\nwant %v", got, want)
	}

	c.Width, c.Height, c.Framerate = 848, 480, 30
	got = InputArgs(c)
	want = []string{
		"-f", "rawvideo", "-pixel_format", "rgb24",
		"-video_size", "848x480", "-framerate", "30",
		"-i", "tcp://127.0.0.1:18021",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("custom geometry:\n got %v\nwant %v", got, want)
	}
}

func TestBuildArgsRealSenseHasNostdin(t *testing.T) {
	// realsense 输入是 TCP，不是 stdin：应保留 -nostdin（与 rpicam/screen 相反）。
	args := BuildArgs(config.Camera{Type: config.TypeRealSense, Source: "tcp://127.0.0.1:18020"}, "rtsp://x/cam/1")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-nostdin") {
		t.Errorf("realsense should keep -nostdin, args: %v", args)
	}
	if !strings.Contains(joined, "tcp://127.0.0.1:18020") {
		t.Errorf("source url missing: %v", args)
	}
}

func TestRsPipeSpecOf(t *testing.T) {
	// 无 realsense 摄像头：不启动。
	if _, wants := rsPipeSpecOf([]config.Camera{{ID: "1", Type: config.TypeTestSrc, Enabled: true}}); wants {
		t.Error("non-realsense cameras should not want rs-pipe")
	}
	// 禁用的 realsense 摄像头：不启动。
	if _, wants := rsPipeSpecOf([]config.Camera{{ID: "1", Type: config.TypeRealSense, Enabled: false}}); wants {
		t.Error("disabled realsense camera should not want rs-pipe")
	}
	// 默认几何。
	spec, wants := rsPipeSpecOf([]config.Camera{{ID: "1", Type: config.TypeRealSense, Enabled: true}})
	if !wants || spec.W != 640 || spec.H != 480 || spec.FPS != 15 {
		t.Errorf("unexpected spec %+v wants=%t", spec, wants)
	}
	// 自定义几何取第一路启用的摄像头。
	spec, _ = rsPipeSpecOf([]config.Camera{
		{ID: "1", Type: config.TypeRealSense, Enabled: false, Width: 1280, Height: 720},
		{ID: "2", Type: config.TypeRealSense, Enabled: true, Width: 848, Height: 480, Framerate: 30},
	})
	if spec.W != 848 || spec.H != 480 || spec.FPS != 30 {
		t.Errorf("unexpected spec %+v", spec)
	}
	// 参数指纹不同即触发重启。
	other := rsPipeSpec{W: 1280, H: 720, FPS: 15}
	if spec.fingerprint() == other.fingerprint() {
		t.Error("different geometry must produce different fingerprint")
	}
}

func TestRsPipeArgs(t *testing.T) {
	args := rsPipeArgs(rsPipeSpec{W: 640, H: 480, FPS: 15}, true)
	joined := strings.Join(args, " ")
	for _, want := range []string{"serve", "--port", "18020", "--width 640", "--height 480", "--framerate 15", "--fake"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %v", want, args)
		}
	}
	if args := rsPipeArgs(rsPipeSpec{W: 640, H: 480, FPS: 15}, false); strings.Contains(strings.Join(args, " "), "--fake") {
		t.Errorf("fake mode off should not pass --fake: %v", args)
	}
}
