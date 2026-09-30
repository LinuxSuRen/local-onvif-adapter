package stream

import (
	"strings"
	"testing"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
)

func TestParseDShowVideoDevices(t *testing.T) {
	sample := `[dshow @ 0000026c4e05a940]  DirectShow video devices (some may be both video and audio devices)
[dshow @ 0000026c4e05a940]  "Integrated Camera"
[dshow @ 0000026c4e05a940]     Alternative name "@device_cm_{33D9A762-90C8-11D0-BD43-00A0C911CE86}\Integrated Camera"
[dshow @ 0000026c4e05a940]  "USB2.0 HD UVC WebCam"
[dshow @ 0000026c4e05a940]     Alternative name "@device_cm_{33D9A762-90C8-11D0-BD43-00A0C911CE86}\USB2.0 HD UVC WebCam"
[dshow @ 0000026c4e05a940]  DirectShow audio devices
[dshow @ 0000026c4e05a940]  "Microphone (Realtek Audio)"
[dshow @ 0000026c4e05a940]     Alternative name "@device_cm_{33D9A762-90C8-11D0-BD43-00A0C911CE86}\Microphone"
`
	devices := parseDShowVideoDevices(sample)
	if len(devices) != 2 {
		t.Fatalf("devices = %v, want 2 video devices", devices)
	}
	if devices[0] != "Integrated Camera" || devices[1] != "USB2.0 HD UVC WebCam" {
		t.Fatalf("unexpected devices: %v", devices)
	}
}

func TestParseDShowVideoDevicesEmpty(t *testing.T) {
	if got := parseDShowVideoDevices("dummy output without sections"); got != nil {
		t.Fatalf("want nil, got %v", got)
	}
}

func TestInputArgsDShow(t *testing.T) {
	cam := config.Camera{Type: config.TypeDShow, Source: "Integrated Camera", Width: 1280, Height: 720, Framerate: 30}
	got := strings.Join(InputArgs(cam), " ")
	for _, want := range []string{"-f dshow", "-framerate 30", "-video_size 1280x720", `-i video=Integrated Camera`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	// 已带 video= 前缀时不重复包装。
	cam.Source = `video=Integrated Camera`
	got = strings.Join(InputArgs(cam), " ")
	if strings.Count(got, "video=") != 1 {
		t.Fatalf("double video= prefix: %q", got)
	}
	// 别名形式原样使用。
	cam.Source = `video=@device_cm_{GUID}\Cam`
	got = strings.Join(InputArgs(cam), " ")
	if !strings.Contains(got, `-i video=@device_cm_{GUID}\Cam`) {
		t.Fatalf("alias passthrough: %q", got)
	}
}

func TestDShowDevicesNonWindows(t *testing.T) {
	// 在非 Windows 平台应返回不支持错误（本测试在 darwin/linux 上运行）。
	if _, err := DShowVideoDevices("ffmpeg"); err == nil {
		t.Fatal("want unsupported error on non-windows")
	}
}
