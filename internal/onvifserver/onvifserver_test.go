package onvifserver

import (
	"encoding/xml"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
	"github.com/linuxsuren/local-onvif-adapter/internal/ptzmock"
)

// ---- 测试脚手架 ----

func newTestService(t *testing.T, cams ...config.Camera) *Service {
	t.Helper()
	store, err := config.LoadStore(t.TempDir())
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	if err := store.Update(func(r *config.Root) error { r.Cameras = cams; return nil }); err != nil {
		t.Fatalf("seed cameras: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewService(store, ptzmock.NewRegistry(),
		URIs{AdvertiseIP: "192.0.2.10", HTTPPort: 8080, RTSPPort: 8554}, logger)
}

func twoCameras() []config.Camera {
	return []config.Camera{
		{ID: "cam1", Name: "前门", Type: config.TypeV4L2, Source: "/dev/video0",
			Width: 1280, Height: 720, Framerate: 15, BitrateKBPS: 2048, Enabled: true},
		{ID: "cam2", Name: "热成像", Type: config.TypeTestSrc, Source: "",
			Width: 640, Height: 512, Framerate: 9, BitrateKBPS: 1024, Infrared: true, Enabled: true},
	}
}

// call 以 SOAP 请求调用 op，返回响应 Body 的 innerxml。
func call(t *testing.T, s *Service, inner string) (int, string) {
	t.Helper()
	body := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://www.w3.org/2003/05/soap-envelope">` +
		`<SOAP-ENV:Body>` + inner + `</SOAP-ENV:Body></SOAP-ENV:Envelope>`
	req := httptest.NewRequest("POST", "/onvif/device_service", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	respBody := rec.Body.String()
	var env struct {
		Body struct {
			Inner []byte `xml:",innerxml"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal([]byte(respBody), &env); err != nil {
		t.Fatalf("response is not valid SOAP envelope: %v\n%s", err, respBody)
	}
	return rec.Code, string(env.Body.Inner)
}

// callOp 生成 `前缀:Op` 请求体并调用（前缀映射到对应服务命名空间）。
func callOp(t *testing.T, s *Service, prefix, op, innerAttrsAndChildren string) string {
	t.Helper()
	ns := map[string]string{
		"tds":  "http://www.onvif.org/ver10/device/wsdl",
		"trt":  "http://www.onvif.org/ver10/media/wsdl",
		"tptz": "http://www.onvif.org/ver20/ptz/wsdl",
		"timg": "http://www.onvif.org/ver10/imaging/wsdl",
	}[prefix]
	code, body := call(t, s, `<`+prefix+`:`+op+` xmlns:`+prefix+`="`+ns+`">`+
		innerAttrsAndChildren+`</`+prefix+`:`+op+`>`)
	if code != 200 {
		t.Fatalf("op %s status = %d", op, code)
	}
	return body
}

// ---- 解析结构体：与 device-camera-onvif 使用的 onvif-go 客户端解析方式一致（按 local name）----

type deviceInfoResp struct {
	XMLName         xml.Name `xml:"GetDeviceInformationResponse"`
	Manufacturer    string   `xml:"Manufacturer"`
	Model           string   `xml:"Model"`
	FirmwareVersion string   `xml:"FirmwareVersion"`
	SerialNumber    string   `xml:"SerialNumber"`
	HardwareID      string   `xml:"HardwareId"`
}

type capabilitiesResp struct {
	XMLName      xml.Name `xml:"GetCapabilitiesResponse"`
	Capabilities struct {
		Media struct {
			XAddr                 string `xml:"XAddr"`
			StreamingCapabilities struct {
				RTP_RTSP_TCP bool `xml:"RTP_RTSP_TCP"`
			} `xml:"StreamingCapabilities"`
		} `xml:"Media"`
		PTZ struct {
			XAddr string `xml:"XAddr"`
		} `xml:"PTZ"`
		Imaging struct {
			XAddr string `xml:"XAddr"`
		} `xml:"Imaging"`
	} `xml:"Capabilities"`
}

type servicesResp struct {
	XMLName xml.Name `xml:"GetServicesResponse"`
	Service []struct {
		Namespace string `xml:"Namespace"`
		XAddr     string `xml:"XAddr"`
	} `xml:"Service"`
}

type profilesResp struct {
	XMLName  xml.Name `xml:"GetProfilesResponse"`
	Profiles []struct {
		Token             string `xml:"token,attr"`
		Name              string `xml:"Name"`
		VideoSourceConfig struct {
			Token       string `xml:"token,attr"`
			SourceToken string `xml:"SourceToken"`
		} `xml:"VideoSourceConfiguration"`
		VideoEncoderConfig struct {
			Token      string `xml:"token,attr"`
			Encoding   string `xml:"Encoding"`
			Resolution struct {
				Width  int `xml:"Width"`
				Height int `xml:"Height"`
			} `xml:"Resolution"`
			RateControl struct {
				FrameRateLimit int `xml:"FrameRateLimit"`
				BitrateLimit   int `xml:"BitrateLimit"`
			} `xml:"RateControl"`
		} `xml:"VideoEncoderConfiguration"`
		PTZConfig struct {
			Token     string `xml:"token,attr"`
			NodeToken string `xml:"NodeToken"`
		} `xml:"PTZConfiguration"`
	} `xml:"Profiles"`
}

type mediaURIResp struct {
	XMLName  xml.Name `xml:"GetStreamUriResponse"`
	MediaURI struct {
		Uri string `xml:"Uri"`
	} `xml:"MediaUri"`
}

type snapshotURIResp struct {
	XMLName  xml.Name `xml:"GetSnapshotUriResponse"`
	MediaURI struct {
		Uri string `xml:"Uri"`
	} `xml:"MediaUri"`
}

type ptzStatusRespT struct {
	XMLName xml.Name `xml:"GetStatusResponse"`
	Status  struct {
		Position struct {
			PanTilt struct {
				X float64 `xml:"x,attr"`
				Y float64 `xml:"y,attr"`
			} `xml:"PanTilt"`
			Zoom struct {
				X float64 `xml:"x,attr"`
			} `xml:"Zoom"`
		} `xml:"Position"`
		MoveStatus struct {
			PanTilt string `xml:"PanTilt"`
			Zoom    string `xml:"Zoom"`
		} `xml:"MoveStatus"`
	} `xml:"PTZStatus"`
}

type presetsRespT struct {
	XMLName xml.Name `xml:"GetPresetsResponse"`
	Preset  []struct {
		Token string `xml:"token,attr"`
		Name  string `xml:"Name"`
	} `xml:"Preset"`
}

type setPresetRespT struct {
	XMLName     xml.Name `xml:"SetPresetResponse"`
	PresetToken string   `xml:"PresetToken"`
}

type videoSourcesRespT struct {
	XMLName      xml.Name `xml:"GetVideoSourcesResponse"`
	VideoSources []struct {
		Token     string `xml:"token,attr"`
		Framerate int    `xml:"Framerate"`
	} `xml:"VideoSources"`
}

type encoderCfgRespT struct {
	XMLName       xml.Name `xml:"GetVideoEncoderConfigurationResponse"`
	Configuration struct {
		Token      string `xml:"token,attr"`
		Resolution struct {
			Width  int `xml:"Width"`
			Height int `xml:"Height"`
		} `xml:"Resolution"`
		RateControl struct {
			FrameRateLimit int `xml:"FrameRateLimit"`
			BitrateLimit   int `xml:"BitrateLimit"`
		} `xml:"RateControl"`
	} `xml:"Configuration"`
}

// ---- 用例 ----

func TestGetDeviceInformation(t *testing.T) {
	s := newTestService(t)
	var resp deviceInfoResp
	if err := xml.Unmarshal([]byte(callOp(t, s, "tds", "GetDeviceInformation", "")), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Manufacturer == "" || resp.Model != "local-onvif-adapter" || resp.SerialNumber == "" {
		t.Fatalf("unexpected device info: %+v", resp)
	}
}

func TestGetCapabilitiesAndServices(t *testing.T) {
	s := newTestService(t)
	var caps capabilitiesResp
	if err := xml.Unmarshal([]byte(callOp(t, s, "tds", "GetCapabilities", "")), &caps); err != nil {
		t.Fatalf("parse capabilities: %v", err)
	}
	want := "http://192.0.2.10:8080/onvif/device_service"
	if caps.Capabilities.Media.XAddr != want || caps.Capabilities.PTZ.XAddr != want || caps.Capabilities.Imaging.XAddr != want {
		t.Fatalf("capability xaddrs: %+v", caps.Capabilities)
	}
	if !caps.Capabilities.Media.StreamingCapabilities.RTP_RTSP_TCP {
		t.Fatal("RTP_RTSP_TCP should be true")
	}

	var svc servicesResp
	if err := xml.Unmarshal([]byte(callOp(t, s, "tds", "GetServices", "")), &svc); err != nil {
		t.Fatalf("parse services: %v", err)
	}
	got := map[string]bool{}
	for _, e := range svc.Service {
		got[e.Namespace] = true
	}
	for _, ns := range []string{
		"http://www.onvif.org/ver10/device/wsdl",
		"http://www.onvif.org/ver10/media/wsdl",
		"http://www.onvif.org/ver20/ptz/wsdl",
	} {
		if !got[ns] {
			t.Fatalf("service %s missing: %+v", ns, got)
		}
	}
}

func TestGetProfilesMultiCamera(t *testing.T) {
	s := newTestService(t, twoCameras()...)
	var resp profilesResp
	if err := xml.Unmarshal([]byte(callOp(t, s, "trt", "GetProfiles", "")), &resp); err != nil {
		t.Fatalf("parse profiles: %v", err)
	}
	if len(resp.Profiles) != 2 {
		t.Fatalf("profiles = %d, want 2", len(resp.Profiles))
	}
	p0 := resp.Profiles[0]
	if p0.Token != "profile_cam1" || p0.Name != "前门" {
		t.Fatalf("profile0: %+v", p0)
	}
	if p0.VideoSourceConfig.SourceToken != "vs_cam1" {
		t.Fatalf("video source token: %+v", p0.VideoSourceConfig)
	}
	if p0.VideoEncoderConfig.Encoding != "H264" ||
		p0.VideoEncoderConfig.Resolution.Width != 1280 ||
		p0.VideoEncoderConfig.Resolution.Height != 720 ||
		p0.VideoEncoderConfig.RateControl.FrameRateLimit != 15 {
		t.Fatalf("encoder config: %+v", p0.VideoEncoderConfig)
	}
	if p0.PTZConfig.NodeToken != "ptznode_cam1" {
		t.Fatalf("ptz config missing: %+v", p0.PTZConfig)
	}
	// 红外摄像头名称必须带 infrared（客户端按名称识别红外通道）。
	if !strings.Contains(resp.Profiles[1].Name, "infrared") {
		t.Fatalf("infrared profile name: %q", resp.Profiles[1].Name)
	}
}

func TestGetStreamAndSnapshotURI(t *testing.T) {
	s := newTestService(t, twoCameras()...)
	var stream mediaURIResp
	body := callOp(t, s, "trt", "GetStreamUri", `<trt:ProfileToken>profile_cam1</trt:ProfileToken>`)
	if err := xml.Unmarshal([]byte(body), &stream); err != nil {
		t.Fatalf("parse stream uri: %v", err)
	}
	if stream.MediaURI.Uri != "rtsp://192.0.2.10:8554/cam/cam1" {
		t.Fatalf("stream uri: %q", stream.MediaURI.Uri)
	}
	var snap snapshotURIResp
	body = callOp(t, s, "trt", "GetSnapshotUri", `<trt:ProfileToken>profile_cam1</trt:ProfileToken>`)
	if err := xml.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("parse snapshot uri: %v", err)
	}
	if snap.MediaURI.Uri != "http://192.0.2.10:8080/onvif/snapshot/profile_cam1" {
		t.Fatalf("snapshot uri: %q", snap.MediaURI.Uri)
	}
}

func TestGetVideoSources(t *testing.T) {
	s := newTestService(t, twoCameras()...)
	var resp videoSourcesRespT
	if err := xml.Unmarshal([]byte(callOp(t, s, "trt", "GetVideoSources", "")), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.VideoSources) != 2 || resp.VideoSources[0].Token != "vs_cam1" || resp.VideoSources[0].Framerate != 15 {
		t.Fatalf("video sources: %+v", resp.VideoSources)
	}
}

func TestPTZMockFlow(t *testing.T) {
	s := newTestService(t, twoCameras()...)
	getStatus := func() ptzStatusRespT {
		var st ptzStatusRespT
		body := callOp(t, s, "tptz", "GetStatus", `<tptz:ProfileToken>profile_cam1</tptz:ProfileToken>`)
		if err := xml.Unmarshal([]byte(body), &st); err != nil {
			t.Fatalf("parse status: %v", err)
		}
		return st
	}

	// 初始位置 0，IDLE。
	st := getStatus()
	if st.Status.Position.PanTilt.X != 0 || st.Status.MoveStatus.PanTilt != "IDLE" {
		t.Fatalf("initial status: %+v", st.Status)
	}

	// 连续移动：pan 全速。
	callOp(t, s, "tptz", "ContinuousMove",
		`<tptz:ProfileToken>profile_cam1</tptz:ProfileToken>`+
			`<tptz:Velocity><tt:PanTilt x="1" y="0" xmlns:tt="http://www.onvif.org/ver10/schema"/><tt:Zoom x="0" xmlns:tt="http://www.onvif.org/ver10/schema"/></tptz:Velocity>`)
	time.Sleep(200 * time.Millisecond)
	st = getStatus()
	if st.Status.Position.PanTilt.X <= 0 {
		t.Fatalf("pan should increase: %+v", st.Status.Position)
	}
	if st.Status.MoveStatus.PanTilt != "MOVING" {
		t.Fatalf("should be moving: %+v", st.Status.MoveStatus)
	}

	// 停止。
	callOp(t, s, "tptz", "Stop", `<tptz:ProfileToken>profile_cam1</tptz:ProfileToken>`)
	st = getStatus()
	if st.Status.MoveStatus.PanTilt != "IDLE" {
		t.Fatalf("should be idle after stop: %+v", st.Status.MoveStatus)
	}

	// 绝对定位。
	callOp(t, s, "tptz", "AbsoluteMove",
		`<tptz:ProfileToken>profile_cam1</tptz:ProfileToken>`+
			`<tptz:Position><tt:PanTilt x="30" y="-10" xmlns:tt="http://www.onvif.org/ver10/schema"/><tt:Zoom x="0.5" xmlns:tt="http://www.onvif.org/ver10/schema"/></tptz:Position>`)
	st = getStatus()
	if st.Status.Position.PanTilt.X != 30 || st.Status.Position.PanTilt.Y != -10 || st.Status.Position.Zoom.X != 0.5 {
		t.Fatalf("absolute move: %+v", st.Status.Position)
	}

	// 预置位：保存 → 移开 → 调用回来 → 删除。
	var setResp setPresetRespT
	body := callOp(t, s, "tptz", "SetPreset",
		`<tptz:ProfileToken>profile_cam1</tptz:ProfileToken><tptz:PresetName>door</tptz:PresetName>`)
	if err := xml.Unmarshal([]byte(body), &setResp); err != nil || setResp.PresetToken == "" {
		t.Fatalf("set preset: %v %+v", err, setResp)
	}
	callOp(t, s, "tptz", "AbsoluteMove",
		`<tptz:ProfileToken>profile_cam1</tptz:ProfileToken>`+
			`<tptz:Position><tt:PanTilt x="-60" y="0" xmlns:tt="http://www.onvif.org/ver10/schema"/><tt:Zoom x="0" xmlns:tt="http://www.onvif.org/ver10/schema"/></tptz:Position>`)
	callOp(t, s, "tptz", "GotoPreset",
		`<tptz:ProfileToken>profile_cam1</tptz:ProfileToken><tptz:PresetToken>`+setResp.PresetToken+`</tptz:PresetToken>`)
	st = getStatus()
	if st.Status.Position.PanTilt.X != 30 {
		t.Fatalf("goto preset: %+v", st.Status.Position)
	}
	var pres presetsRespT
	body = callOp(t, s, "tptz", "GetPresets", `<tptz:ProfileToken>profile_cam1</tptz:ProfileToken>`)
	if err := xml.Unmarshal([]byte(body), &pres); err != nil {
		t.Fatalf("parse presets: %v", err)
	}
	if len(pres.Preset) != 1 || pres.Preset[0].Name != "door" {
		t.Fatalf("presets: %+v", pres.Preset)
	}
	callOp(t, s, "tptz", "RemovePreset",
		`<tptz:ProfileToken>profile_cam1</tptz:ProfileToken><tptz:PresetToken>`+setResp.PresetToken+`</tptz:PresetToken>`)
	var presAfter presetsRespT
	body = callOp(t, s, "tptz", "GetPresets", `<tptz:ProfileToken>profile_cam1</tptz:ProfileToken>`)
	if err := xml.Unmarshal([]byte(body), &presAfter); err != nil {
		t.Fatalf("parse presets: %v", err)
	}
	if len(presAfter.Preset) != 0 {
		t.Fatalf("presets after remove: %+v", presAfter.Preset)
	}

	// Home。
	callOp(t, s, "tptz", "SetHomePosition", `<tptz:ProfileToken>profile_cam1</tptz:ProfileToken>`)
	callOp(t, s, "tptz", "GotoHomePosition", `<tptz:ProfileToken>profile_cam1</tptz:ProfileToken>`)

	// 未知 profile 上的 PTZ 操作静默成功（不报错是硬性要求）。
	body = callOp(t, s, "tptz", "ContinuousMove", `<tptz:ProfileToken>profile_nothing</tptz:ProfileToken>`)
	if !strings.Contains(body, "Response") {
		t.Fatalf("unknown profile should still succeed: %q", body)
	}
}

func TestPTZNodesAndConfigs(t *testing.T) {
	s := newTestService(t, twoCameras()...)
	callOp(t, s, "tptz", "GetNodes", "")
	callOp(t, s, "tptz", "GetConfigurations", "")
	callOp(t, s, "tptz", "GetConfigurationOptions", `<tptz:PTZConfigurationToken>ptzcfg_cam1</tptz:PTZConfigurationToken>`)
	callOp(t, s, "tptz", "GetConfiguration", `<tptz:PTZConfigurationToken>ptzcfg_cam1</tptz:PTZConfigurationToken>`)
	callOp(t, s, "tptz", "GetNode", `<tptz:NodeToken>ptznode_cam1</tptz:NodeToken>`)
}

func TestSetVideoEncoderConfigurationApplies(t *testing.T) {
	s := newTestService(t, twoCameras()...)
	encoderChanged := false
	s.OnEncoderChange = func(config.Camera) { encoderChanged = true }

	callOp(t, s, "trt", "SetVideoEncoderConfiguration",
		`<trt:Configuration token="vec_cam1">`+
			`<tt:Encoding xmlns:tt="http://www.onvif.org/ver10/schema">H264</tt:Encoding>`+
			`<tt:Resolution xmlns:tt="http://www.onvif.org/ver10/schema"><tt:Width>1920</tt:Width><tt:Height>1080</tt:Height></tt:Resolution>`+
			`<tt:RateControl xmlns:tt="http://www.onvif.org/ver10/schema"><tt:FrameRateLimit>25</tt:FrameRateLimit><tt:EncodingInterval>1</tt:EncodingInterval><tt:BitrateLimit>4096</tt:BitrateLimit></tt:RateControl>`+
			`</trt:Configuration>`)

	if !encoderChanged {
		t.Fatal("OnEncoderChange should fire")
	}
	root := s.store.Root()
	cam, ok := root.FindCamera("cam1")
	if !ok || cam.Width != 1920 || cam.Height != 1080 || cam.Framerate != 25 || cam.BitrateKBPS != 4096 {
		t.Fatalf("camera after set: %+v", cam)
	}

	var resp encoderCfgRespT
	body := callOp(t, s, "trt", "GetVideoEncoderConfiguration", `<trt:ConfigurationToken>vec_cam1</trt:ConfigurationToken>`)
	if err := xml.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Configuration.Resolution.Width != 1920 || resp.Configuration.RateControl.FrameRateLimit != 25 {
		t.Fatalf("encoder cfg after set: %+v", resp.Configuration)
	}
}

func TestImagingMock(t *testing.T) {
	s := newTestService(t, twoCameras()...)
	body := callOp(t, s, "timg", "GetImagingSettings", `<timg:VideoSourceToken>vs_cam1</timg:VideoSourceToken>`)
	if !strings.Contains(body, "Brightness") {
		t.Fatalf("imaging settings: %s", body)
	}
	callOp(t, s, "timg", "SetImagingSettings",
		`<timg:VideoSourceToken>vs_cam1</timg:VideoSourceToken>`+
			`<timg:ImagingSettings xmlns:tt="http://www.onvif.org/ver10/schema"><tt:Brightness>200</tt:Brightness></timg:ImagingSettings>`)
	body = callOp(t, s, "timg", "GetImagingSettings", `<timg:VideoSourceToken>vs_cam1</timg:VideoSourceToken>`)
	if !strings.Contains(body, "<tt:Brightness>200</tt:Brightness>") {
		t.Fatalf("brightness not applied: %s", body)
	}
	callOp(t, s, "timg", "GetOptions", `<timg:VideoSourceToken>vs_cam1</timg:VideoSourceToken>`)
}

func TestUnknownOpReturnsFault(t *testing.T) {
	s := newTestService(t)
	code, body := call(t, s, `<tds:SomethingStrange xmlns:tds="http://www.onvif.org/ver10/device/wsdl"/>`)
	if code != 200 {
		t.Fatalf("fault should still be HTTP 200, got %d", code)
	}
	if !strings.Contains(body, "ActionNotSupported") {
		t.Fatalf("fault body: %s", body)
	}
}

func TestSOAPActionHeaderDispatch(t *testing.T) {
	// 部分客户端（如 gSOAP）不带前缀发送 body；验证无前缀也能解析。
	s := newTestService(t)
	code, body := call(t, s, `<GetDeviceInformation xmlns="http://www.onvif.org/ver10/device/wsdl"/>`)
	if code != 200 || !strings.Contains(body, "GetDeviceInformationResponse") {
		t.Fatalf("prefixless request: code=%d body=%s", code, body)
	}
}

func TestGetSystemDateAndTime(t *testing.T) {
	s := newTestService(t)
	body := callOp(t, s, "tds", "GetSystemDateAndTime", "")
	for _, want := range []string{"UTCDateTime", "Year", "Hour"} {
		if !strings.Contains(body, want) {
			t.Fatalf("date time missing %s: %s", want, body)
		}
	}
}
