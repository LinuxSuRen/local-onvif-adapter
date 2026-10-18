package onvifserver

import (
	"strconv"
	"strings"
	"testing"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
)

func resXML(w, h int) string {
	tag := `<tt:ResolutionsAvailable><tt:Width>` + strconv.Itoa(w) + `</tt:Width><tt:Height>` + strconv.Itoa(h) + `</tt:Height></tt:ResolutionsAvailable>`
	return tag
}

// ov5647Modes 是树莓派 Camera Module V1（ov5647）的传感器真实模式。
func ov5647Modes() []ResolutionOption {
	return []ResolutionOption{
		{Width: 640, Height: 480, MaxFPS: 30},
		{Width: 1296, Height: 972, MaxFPS: 30},
		{Width: 1920, Height: 1080, MaxFPS: 30},
		{Width: 2592, Height: 1944, MaxFPS: 30},
	}
}

// TestEncoderOptionsProbed：rpicam 源配置探测器后，ResolutionsAvailable
// 使用传感器真实模式——包含 1296x972 / 2592x1944，不再出现静态列表特有的
// 1280x720；当前生效分辨率置顶且去重；帧率上限取传感器能力。
func TestEncoderOptionsProbed(t *testing.T) {
	s := newTestService(t, config.Camera{
		ID: "cam3", Name: "CSI 摄像头", Type: config.TypeRpicam, Source: "0",
		Width: 1296, Height: 972, Framerate: 15, BitrateKBPS: 2048, Enabled: true,
	})
	s.EncoderResolutions = func(config.Camera) []ResolutionOption { return ov5647Modes() }

	body := callOp(t, s, "trt", "GetVideoEncoderConfigurationOptions",
		`<trt:ConfigurationToken>vec_cam3</trt:ConfigurationToken>`)

	if !strings.Contains(body, resXML(2592, 1944)) || !strings.Contains(body, resXML(1296, 972)) {
		t.Fatalf("sensor modes missing: %s", body)
	}
	if strings.Contains(body, resXML(1280, 720)) {
		t.Fatalf("static-only resolution 1280x720 should be absent: %s", body)
	}
	// 去重后 1296x972 恰好三处：顶层、JpegOptions、H264Options。
	if n := strings.Count(body, resXML(1296, 972)); n != 3 {
		t.Fatalf("1296x972 appears %d times, want 3 (dedupe): %s", n, body)
	}
	// 当前分辨率置顶（QualityRange 之后紧跟的第一个候选）。
	q := `<tt:QualityRange><tt:Min>1</tt:Min><tt:Max>10</tt:Max></tt:QualityRange>`
	if !strings.HasPrefix(strings.SplitN(body, q, 2)[1], resXML(1296, 972)) {
		t.Fatalf("current resolution should lead the top list: %s", body)
	}
	// 探测路径下帧率上限取传感器最大能力（30），两处一致。
	if n := strings.Count(body, `<tt:Max>30</tt:Max>`); n < 2 {
		t.Fatalf("FrameRateRange should report sensor max fps: %s", body)
	}
}

// TestEncoderOptionsFallback：未配置探测器（或探测器返回空）时保持历史行为——
// 当前分辨率置顶 + 静态 1080p/720p/480p 列表，JPEG 帧率上限跟随配置帧率。
func TestEncoderOptionsFallback(t *testing.T) {
	s := newTestService(t, config.Camera{
		ID: "cam1", Name: "前门", Type: config.TypeV4L2, Source: "/dev/video0",
		Width: 1280, Height: 720, Framerate: 15, BitrateKBPS: 2048, Enabled: true,
	})

	body := callOp(t, s, "trt", "GetVideoEncoderConfigurationOptions",
		`<trt:ConfigurationToken>vec_cam1</trt:ConfigurationToken>`)

	if !strings.Contains(body, resXML(1920, 1080)) || !strings.Contains(body, resXML(640, 480)) {
		t.Fatalf("static fallback list missing: %s", body)
	}
	if strings.Contains(body, resXML(2592, 1944)) {
		t.Fatalf("sensor-only resolution should be absent in fallback: %s", body)
	}
	if n := strings.Count(body, resXML(1280, 720)); n != 3 {
		t.Fatalf("1280x720 appears %d times, want 3: %s", n, body)
	}
	if !strings.Contains(body, `<tt:JpegOptions>`) || !strings.Contains(body, `<tt:FrameRateRange><tt:Min>1</tt:Min><tt:Max>15</tt:Max></tt:FrameRateRange></tt:JpegOptions>`) {
		t.Fatalf("jpeg frame rate should follow camera config (15): %s", body)
	}
}

// TestEncoderOptionsEmptyProbe：探测器返回空（如 rpicam-hello 不可用）时
// 不 panic，回退静态列表。
func TestEncoderOptionsEmptyProbe(t *testing.T) {
	s := newTestService(t, config.Camera{
		ID: "cam3", Name: "CSI 摄像头", Type: config.TypeRpicam, Source: "0",
		Width: 1296, Height: 972, Framerate: 15, BitrateKBPS: 2048, Enabled: true,
	})
	s.EncoderResolutions = func(config.Camera) []ResolutionOption { return nil }

	body := callOp(t, s, "trt", "GetVideoEncoderConfigurationOptions",
		`<trt:ConfigurationToken>vec_cam3</trt:ConfigurationToken>`)
	if !strings.Contains(body, resXML(1280, 720)) {
		t.Fatalf("empty probe should fall back to static list: %s", body)
	}
}
