package stream

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
}

func TestInputArgsV4L2(t *testing.T) {
	cam := config.Camera{Type: config.TypeV4L2, Source: "/dev/video0", Width: 1280, Height: 720, Framerate: 30}
	got := strings.Join(InputArgs(cam), " ")
	for _, want := range []string{"-f v4l2", "-framerate 30", "-video_size 1280x720", "-i /dev/video0"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	// 启用音频：v4l2 + pulse 双输入（默认 default 源）。
	cam.AudioEnabled = true
	got = strings.Join(InputArgs(cam), " ")
	if !strings.Contains(got, "-f pulse -i default") {
		t.Fatalf("pulse input missing: %q", got)
	}
	cam.AudioSource = "alsa_input.pci-0000_00_1f.3.analog-stereo"
	got = strings.Join(InputArgs(cam), " ")
	if !strings.Contains(got, "-f pulse -i alsa_input.pci-0000_00_1f.3.analog-stereo") {
		t.Fatalf("custom pulse source missing: %q", got)
	}
}

// TestAudioInputArgs 各平台音频输入形态：dshow 联合输入、avfoundation 双索引。
func TestAudioInputArgs(t *testing.T) {
	cam := config.Camera{Type: config.TypeDShow, Source: "WL24A", AudioEnabled: true, AudioSource: "麦克风 (WL24A)"}
	got := strings.Join(InputArgs(cam), " ")
	want := `-i video=WL24A:audio=麦克风 (WL24A)`
	if !strings.Contains(got, want) {
		t.Fatalf("dshow combined input: %q", got)
	}
	// 已带 audio= 前缀时原样使用，不重复包装。
	cam.AudioSource = "audio=麦克风 (WL24A)"
	got = strings.Join(InputArgs(cam), " ")
	if strings.Count(got, "audio=") != 1 {
		t.Fatalf("double audio= prefix: %q", got)
	}
	// 别名视频源同样支持联合音频输入。
	cam.Source = `video=@device_pnp_xxx`
	cam.AudioSource = "Mic"
	got = strings.Join(InputArgs(cam), " ")
	if !strings.Contains(got, `:audio=Mic`) {
		t.Fatalf("alias with audio: %q", got)
	}

	avf := config.Camera{Type: config.TypeAVFoundation, Source: "0", AudioEnabled: true, AudioSource: "1"}
	got = strings.Join(InputArgs(avf), " ")
	if !strings.Contains(got, "-i 0:1") {
		t.Fatalf("avfoundation dual index: %q", got)
	}
	// 开了音频但没选设备：输入保持纯视频（编码侧同样按未启用处理）。
	avf.AudioSource = ""
	got = strings.Join(InputArgs(avf), " ")
	if !strings.Contains(got, "-i 0") || strings.Contains(got, "0:") {
		t.Fatalf("avfoundation without audio source: %q", got)
	}
}

// TestAudioEncodeArgs 音频编码与流映射：未启用 -an；启用后显式 map + AAC 44.1k。
func TestAudioEncodeArgs(t *testing.T) {
	cam := config.Camera{Type: config.TypeDShow, Source: "WL24A"}
	got := strings.Join(audioEncodeArgs(cam), " ")
	if got != "-an" {
		t.Fatalf("disabled audio should be -an, got %q", got)
	}

	cam.AudioEnabled = true
	cam.AudioSource = "Mic"
	got = strings.Join(audioEncodeArgs(cam), " ")
	for _, want := range []string{"-map 0:v:0", "-map 0:a:0?", "-c:a aac", "-ar 44100"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}

	// v4l2 双输入：音频在输入 1。
	v4l := config.Camera{Type: config.TypeV4L2, Source: "/dev/video0", AudioEnabled: true}
	got = strings.Join(audioEncodeArgs(v4l), " ")
	if !strings.Contains(got, "-map 1:a:0?") {
		t.Fatalf("v4l2 audio map: %q", got)
	}

	// 开关开了但设备为空（dshow/avfoundation）：按未启用处理，保持纯视频。
	noSrc := config.Camera{Type: config.TypeDShow, Source: "WL24A", AudioEnabled: true}
	if got := audioEncodeArgs(noSrc); strings.Join(got, " ") != "-an" {
		t.Fatalf("dshow without audio source should degrade to -an, got %q", got)
	}

	// rtsp 源：无需音频设备，开关即生效（音轨可选映射，源无音轨时优雅退化）。
	rtsp := config.Camera{Type: config.TypeRTSP, Source: "rtsp://x/y", AudioEnabled: true}
	got = strings.Join(audioEncodeArgs(rtsp), " ")
	if !strings.Contains(got, "-map 0:a:0?") || !strings.Contains(got, "-c:a aac") {
		t.Fatalf("rtsp audio passthrough args: %q", got)
	}
	// 完整 BuildArgs 冒烟：启用音频的命令行不再包含 -an。
	full := strings.Join(BuildArgs(rtsp, "rtsp://127.0.0.1:8554/cam/cam1"), " ")
	if strings.Contains(full, " -an") {
		t.Fatalf("BuildArgs should not drop audio when enabled: %q", full)
	}
}

func TestInputArgsAVFoundation(t *testing.T) {
	cam := config.Camera{Type: config.TypeAVFoundation, Source: "1", Framerate: 30}
	got := strings.Join(InputArgs(cam), " ")
	if !strings.Contains(got, "-f avfoundation") || !strings.Contains(got, "-i 1") {
		t.Fatalf("avfoundation args: %q", got)
	}
	// 空源默认 0 号设备。
	got = strings.Join(InputArgs(config.Camera{Type: config.TypeAVFoundation}), " ")
	if !strings.Contains(got, "-i 0") {
		t.Fatalf("default avfoundation index: %q", got)
	}
}

func TestInputArgsRTSP(t *testing.T) {
	cam := config.Camera{Type: config.TypeRTSP, Source: "rtsp://127.0.0.1:8554/cam/cam1"}
	got := strings.Join(InputArgs(cam), " ")
	if !strings.Contains(got, "-rtsp_transport tcp") || !strings.Contains(got, "-i rtsp://127.0.0.1:8554/cam/cam1") {
		t.Fatalf("rtsp args: %q", got)
	}
}

func TestInputArgsTestSrc(t *testing.T) {
	cam := config.Camera{Type: config.TypeTestSrc, Width: 640, Height: 480, Framerate: 10}
	got := strings.Join(InputArgs(cam), " ")
	if !strings.Contains(got, "testsrc=size=640x480:rate=10") {
		t.Fatalf("testsrc args: %q", got)
	}
}

func TestBuildArgsOutput(t *testing.T) {
	cam := config.Camera{Type: config.TypeTestSrc, Framerate: 15, BitrateKBPS: 2048}
	got := strings.Join(BuildArgs(cam, "rtsp://127.0.0.1:8554/cam/cam1"), " ")
	for _, want := range []string{"libx264", "-f rtsp", "-rtsp_transport tcp", "rtsp://127.0.0.1:8554/cam/cam1", "-b:v 2048k", "-an"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

func TestSpecSensitivity(t *testing.T) {
	base := config.Camera{Type: config.TypeV4L2, Source: "/dev/video0", Framerate: 15, BitrateKBPS: 2048}
	if Spec(base) != Spec(base) {
		t.Fatal("same camera should have same spec")
	}
	changed := base
	changed.Framerate = 25
	if Spec(base) == Spec(changed) {
		t.Fatal("framerate change should alter spec")
	}
	changed = base
	changed.Source = "/dev/video1"
	if Spec(base) == Spec(changed) {
		t.Fatal("source change should alter spec")
	}
}

func TestManagerSyncStartStop(t *testing.T) {
	m := NewManager(func() string { return "rtsp://127.0.0.1:8554" }, "false", testLogger())
	cam1 := config.Camera{ID: "cam1", Type: config.TypeTestSrc, Enabled: true}
	m.Sync([]config.Camera{cam1})
	if _, ok := m.Status("cam1"); !ok {
		t.Fatal("runner should exist")
	}
	// 禁用后应停止并移除。
	m.Sync([]config.Camera{{ID: "cam1", Type: config.TypeTestSrc}})
	if _, ok := m.Status("cam1"); ok {
		t.Fatal("runner should be removed")
	}
	// 参数变化（spec 不同）应能再次接受新 runner。
	m.Sync([]config.Camera{{ID: "cam1", Type: config.TypeTestSrc, Enabled: true, Framerate: 25}})
	if _, ok := m.Status("cam1"); !ok {
		t.Fatal("runner should be recreated")
	}
	m.StopAll()
}

func TestInputArgsTestSrcRealtimePacing(t *testing.T) {
	cam := config.Camera{Type: config.TypeTestSrc, Framerate: 15}
	got := strings.Join(InputArgs(cam), " ")
	if !strings.Contains(got, "-re") {
		t.Fatalf("testsrc 输入应包含 -re 实时读取，got: %s", got)
	}
	// 设备/网络源不应被 -re 干预读取节奏
	dev := config.Camera{Type: config.TypeV4L2, Source: "/dev/video0"}
	if dv := strings.Join(InputArgs(dev), " "); strings.Contains(dv, "-re") {
		t.Fatalf("v4l2 输入不应包含 -re，got: %s", dv)
	}
}
