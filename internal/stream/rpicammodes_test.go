package stream

import (
	"testing"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
)

// piOv5647Output 是树莓派 2B + Camera Module V1（ov5647）上
// `rpicam-hello --list-cameras` 的真实输出。
const piOv5647Output = `Available cameras
-----------------
0 : ov5647 [2592x1944 10-bit GBRG] (/base/soc/i2c0mux/i2c@1/ov5647@36)
    Modes: 'SGBRG10_CSI2P' : 640x480 [30.00 fps - (65535, 65535)/65535x65535 crop]
                             1296x972 [30.00 fps - (65535, 65535)/65535x65535 crop]
                             1920x1080 [30.00 fps - (65535, 65535)/65535x65535 crop]
                             2592x1944 [30.00 fps - (65535, 65535)/65535x65535 crop]
`

func TestParseRpicamListCameras(t *testing.T) {
	modes := parseRpicamListCameras(piOv5647Output)
	got, ok := modes[0]
	if !ok {
		t.Fatalf("camera 0 not found: %v", modes)
	}
	want := []struct {
		w, h, fps int
	}{
		{640, 480, 30},
		{1296, 972, 30},
		{1920, 1080, 30},
		{2592, 1944, 30},
	}
	if len(got) != len(want) {
		t.Fatalf("mode count = %d, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Width != w.w || got[i].Height != w.h || got[i].MaxFPS != w.fps {
			t.Fatalf("mode[%d] = %+v, want %dx%d@%d", i, got[i], w.w, w.h, w.fps)
		}
	}
}

func TestParseRpicamListCamerasMultiCamera(t *testing.T) {
	// 双摄像头 + 同分辨率多像素格式：按索引路由，重复分辨率去重取最大帧率。
	out := `Available cameras
-----------------
0 : ov5647 [2592x1944 10-bit GBRG] (/base/soc/i2c0mux/i2c@1/ov5647@36)
    Modes: 'SGBRG10_CSI2P' : 640x480 [30.00 fps - (…) crop]
                             1296x972 [30.00 fps - (…) crop]
1 : imx708 [4608x2592 10-bit SRGGB] (/base/soc/i2c0mux/i2c@0/imx708@1a)
    Modes: 'PizelFormat_SRGGB10_CSI2P' : 1152x864 [120.00 fps - (…) crop]
                             1536x864 [120.00 fps - (…) crop]
`
	modes := parseRpicamListCameras(out)
	if len(modes) != 2 {
		t.Fatalf("camera count = %d, want 2: %v", len(modes), modes)
	}
	if m := modes[0]; len(m) != 2 || m[1].Width != 1296 || m[1].Height != 972 {
		t.Fatalf("camera 0 modes = %v", m)
	}
	if m := modes[1]; len(m) != 2 || m[0].MaxFPS != 120 {
		t.Fatalf("camera 1 modes = %v", m)
	}
}

func TestParseRpicamListCamerasDedup(t *testing.T) {
	// 同一分辨率出现两种像素格式（帧率不同），去重后保留最大帧率。
	out := `0 : ov5647 [2592x1944 10-bit GBRG] (/base/soc/…)
    Modes: 'SGBRG10_CSI2P' : 640x480 [30.00 fps - (…) crop]
                             640x480 [60.00 fps - (…) crop]
`
	modes := parseRpicamListCameras(out)
	got := modes[0]
	if len(got) != 1 || got[0].MaxFPS != 60 {
		t.Fatalf("dedupe/max-fps failed: %v", got)
	}
}

func TestParseRpicamListCamerasGarbage(t *testing.T) {
	for _, in := range []string{"", "rpicam-hello: command not found\n", "Available cameras\n---\n"} {
		if modes := parseRpicamListCameras(in); len(modes) != 0 {
			t.Fatalf("parse(%q) = %v, want empty", in, modes)
		}
	}
}

func TestRpiCamResolutionsNonRPiCam(t *testing.T) {
	// 非 rpicam 类型直接返回 nil（不触发探测），其他类型走静态回退。
	for _, c := range []config.Camera{
		{Type: config.TypeV4L2, Source: "/dev/video0"},
		{Type: config.TypeRTSP, Source: "rtsp://example/cam"},
		{},
	} {
		if got := RpiCamResolutions(c); got != nil {
			t.Fatalf("RpiCamResolutions(%+v) = %v, want nil", c, got)
		}
	}
}

func TestRpicamCameraIndex(t *testing.T) {
	cases := map[string]int{
		"0": 0, "1": 1, " 2 ": 2,
		"": 0, "abc": 0, "-1": 0,
	}
	for src, want := range cases {
		if got := rpicamCameraIndex(config.Camera{Source: src}); got != want {
			t.Fatalf("rpicamCameraIndex(%q) = %d, want %d", src, got, want)
		}
	}
}
