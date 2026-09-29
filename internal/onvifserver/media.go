package onvifserver

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
)

// registerMediaOps 注册 Media Service (trt) 操作。
func registerMediaOps() {
	reg(nsMedia, "GetProfiles", opGetProfiles)
	reg(nsMedia, "GetProfile", withProfileToken(opGetProfile))
	reg(nsMedia, "GetVideoSources", opGetVideoSources)
	reg(nsMedia, "GetVideoSourceConfigurations", opGetVideoSourceConfigurations)
	reg(nsMedia, "GetVideoEncoderConfigurations", opGetVideoEncoderConfigurations)
	reg(nsMedia, "GetVideoEncoderConfiguration", withEncoderToken(opGetVideoEncoderConfiguration))
	reg(nsMedia, "SetVideoEncoderConfiguration", opSetVideoEncoderConfiguration)
	reg(nsMedia, "GetVideoEncoderConfigurationOptions", withEncoderTokenOptions(opGetEncoderOptions))
	reg(nsMedia, "GetStreamUri", withProfileToken(opGetStreamUri))
	reg(nsMedia, "GetSnapshotUri", withProfileToken(opGetSnapshotUri))
	reg(nsMedia, "GetAudioSources", func(s *Service, _ []byte) (string, error) {
		return `<trt:GetAudioSourcesResponse/>`, nil
	})
	reg(nsMedia, "GetAudioEncoderConfigurations", func(s *Service, _ []byte) (string, error) {
		return `<trt:GetAudioEncoderConfigurationsResponse/>`, nil
	})
	reg(nsMedia, "GetServiceCapabilities", mediaCaps)
	reg(nsMedia, "GetVideoSourceConfiguration", withVSCfgToken(func(s *Service, v CameraView) (string, error) {
		return `<trt:GetVideoSourceConfigurationResponse>` + videoSourceCfgXML(v, "Configuration") + `</trt:GetVideoSourceConfigurationResponse>`, nil
	}))
}

func mediaCaps(_ *Service, _ []byte) (string, error) {
	return `<trt:GetServiceCapabilitiesResponse>` +
		`<trt:Capabilities SnapshotUri="true" Rotation="false" VideoSourceMode="false" OSD="false" ProfileChange="false"/>` +
		`</trt:GetServiceCapabilitiesResponse>`, nil
}

// ---- 请求解析 ----

type profileTokenReq struct {
	ProfileToken string `xml:"ProfileToken"`
}

type encoderTokenReq struct {
	ConfigurationToken string `xml:"ConfigurationToken"`
}

type setEncoderReq struct {
	Configuration struct {
		Token      string `xml:"token,attr"`
		Encoding   string `xml:"Encoding"`
		Resolution struct {
			Width  int `xml:"Width"`
			Height int `xml:"Height"`
		} `xml:"Resolution"`
		Quality     float64 `xml:"Quality"`
		RateControl struct {
			FrameRateLimit   int `xml:"FrameRateLimit"`
			EncodingInterval int `xml:"EncodingInterval"`
			BitrateLimit     int `xml:"BitrateLimit"`
		} `xml:"RateControl"`
	} `xml:"Configuration"`
	ForcePersistence bool `xml:"ForcePersistence"`
}

func parseInner(inner []byte, v interface{}) error {
	if err := xml.Unmarshal(inner, v); err != nil {
		return fmt.Errorf("parse request: %w", err)
	}
	return nil
}

func withProfileToken(fn func(s *Service, v CameraView) (string, error)) opHandler {
	return func(s *Service, inner []byte) (string, error) {
		var req profileTokenReq
		if err := parseInner(inner, &req); err != nil {
			return "", err
		}
		v, ok := s.findByProfile(req.ProfileToken)
		if !ok {
			return "", fmt.Errorf("profile %q not found", req.ProfileToken)
		}
		return fn(s, v)
	}
}

func withEncoderToken(fn func(s *Service, v CameraView) (string, error)) opHandler {
	return func(s *Service, inner []byte) (string, error) {
		var req encoderTokenReq
		if err := parseInner(inner, &req); err != nil {
			return "", err
		}
		v, ok := s.findByEncoder(req.ConfigurationToken)
		if !ok {
			return "", fmt.Errorf("video encoder configuration %q not found", req.ConfigurationToken)
		}
		return fn(s, v)
	}
}

// withEncoderTokenOptions 兼容 GetVideoEncoderConfigurationOptions：
// 请求里的 ConfigurationToken 可为空（表示使用 profile 默认），空则取第一个编码器。
func withEncoderTokenOptions(fn func(s *Service, v CameraView) (string, error)) opHandler {
	return func(s *Service, inner []byte) (string, error) {
		var req struct {
			ConfigurationToken string `xml:"ConfigurationToken"`
			ProfileToken       string `xml:"ProfileToken"`
		}
		if err := parseInner(inner, &req); err != nil {
			return "", err
		}
		if req.ConfigurationToken != "" {
			if v, ok := s.findByEncoder(req.ConfigurationToken); ok {
				return fn(s, v)
			}
			return "", fmt.Errorf("video encoder configuration %q not found", req.ConfigurationToken)
		}
		if req.ProfileToken != "" {
			if v, ok := s.findByProfile(req.ProfileToken); ok {
				return fn(s, v)
			}
		}
		views := s.EnabledViews()
		if len(views) == 0 {
			return "", errors.New("no enabled camera")
		}
		return fn(s, views[0])
	}
}

func withVSCfgToken(fn func(s *Service, v CameraView) (string, error)) opHandler {
	return func(s *Service, inner []byte) (string, error) {
		var req struct {
			ConfigurationToken string `xml:"ConfigurationToken"`
		}
		if err := parseInner(inner, &req); err != nil {
			return "", err
		}
		for _, v := range s.EnabledViews() {
			if v.VideoSourceCfgTok == req.ConfigurationToken {
				return fn(s, v)
			}
		}
		return "", fmt.Errorf("video source configuration %q not found", req.ConfigurationToken)
	}
}

// ---- 响应构造 ----

func opGetProfiles(s *Service, _ []byte) (string, error) {
	body := `<trt:GetProfilesResponse>`
	for _, v := range s.EnabledViews() {
		body += profileXML(v)
	}
	return body + `</trt:GetProfilesResponse>`, nil
}

func opGetProfile(s *Service, v CameraView) (string, error) {
	return `<trt:GetProfileResponse>` + profileXML(v) + `</trt:GetProfileResponse>`, nil
}

func profileXML(v CameraView) string {
	return `<trt:Profiles token="` + v.ProfileToken + `" fixed="true">` +
		`<tt:Name>` + xmlEsc(v.ProfileName()) + `</tt:Name>` +
		videoSourceCfgXML(v, "VideoSourceConfiguration") +
		videoEncoderCfgXML(v, "VideoEncoderConfiguration") +
		ptzCfgXML(v) +
		`</trt:Profiles>`
}

// videoSourceCfgXML 生成视频源配置；元素名按场景区分：
// profile 内为 VideoSourceConfiguration，单查为 Configuration，列表为 Configurations。
func videoSourceCfgXML(v CameraView, elem string) string {
	w, h := resolutionOf(v.Camera)
	return `<trt:` + elem + ` token="` + v.VideoSourceCfgTok + `">` +
		`<tt:Name>` + xmlEsc(v.Camera.Name) + `</tt:Name><tt:UseCount>1</tt:UseCount>` +
		`<tt:SourceToken>` + v.VideoSourceToken + `</tt:SourceToken>` +
		fmt.Sprintf(`<tt:Bounds x="0" y="0" width="%d" height="%d"/>`, w, h) +
		`</trt:` + elem + `>`
}

// videoEncoderCfgXML 生成视频编码配置；元素名按场景区分（同上）。
func videoEncoderCfgXML(v CameraView, elem string) string {
	w, h := resolutionOf(v.Camera)
	fps := v.Camera.FramerateOrDefault()
	br := v.Camera.BitrateOrDefault()
	gov := fps * 2
	if gov < 10 {
		gov = 10
	}
	return `<trt:` + elem + ` token="` + v.VideoEncoderToken + `">` +
		`<tt:Name>H264</tt:Name><tt:UseCount>1</tt:UseCount>` +
		`<tt:Encoding>H264</tt:Encoding>` +
		fmt.Sprintf(`<tt:Resolution><tt:Width>%d</tt:Width><tt:Height>%d</tt:Height></tt:Resolution>`, w, h) +
		`<tt:Quality>5</tt:Quality>` +
		fmt.Sprintf(`<tt:RateControl><tt:FrameRateLimit>%d</tt:FrameRateLimit><tt:EncodingInterval>1</tt:EncodingInterval><tt:BitrateLimit>%d</tt:BitrateLimit></tt:RateControl>`, fps, br) +
		fmt.Sprintf(`<tt:H264><tt:GovLength>%d</tt:GovLength><tt:H264Profile>Main</tt:H264Profile></tt:H264>`, gov) +
		`<tt:Multicast><tt:Address><tt:Type>IPv4</tt:Type></tt:Address></tt:Multicast>` +
		`<tt:SessionTimeout>PT60S</tt:SessionTimeout>` +
		`</trt:` + elem + `>`
}

// resolutionOf 计算生效分辨率；未配置时按类型给出默认值。
func resolutionOf(c config.Camera) (int, int) {
	if c.Width > 0 && c.Height > 0 {
		return c.Width, c.Height
	}
	if c.Type == config.TypeTestSrc {
		return 1280, 720
	}
	return 1280, 720
}

func opGetVideoSources(s *Service, _ []byte) (string, error) {
	body := `<trt:GetVideoSourcesResponse>`
	for _, v := range s.EnabledViews() {
		w, h := resolutionOf(v.Camera)
		body += `<trt:VideoSources token="` + v.VideoSourceToken + `">` +
			fmt.Sprintf(`<tt:Framerate>%d</tt:Framerate>`, v.Camera.FramerateOrDefault()) +
			fmt.Sprintf(`<tt:Resolution><tt:Width>%d</tt:Width><tt:Height>%d</tt:Height></tt:Resolution>`, w, h) +
			`</trt:VideoSources>`
	}
	return body + `</trt:GetVideoSourcesResponse>`, nil
}

func opGetVideoSourceConfigurations(s *Service, _ []byte) (string, error) {
	body := `<trt:GetVideoSourceConfigurationsResponse>`
	for _, v := range s.EnabledViews() {
		body += videoSourceCfgXML(v, "Configurations")
	}
	return body + `</trt:GetVideoSourceConfigurationsResponse>`, nil
}

func opGetVideoEncoderConfigurations(s *Service, _ []byte) (string, error) {
	body := `<trt:GetVideoEncoderConfigurationsResponse>`
	for _, v := range s.EnabledViews() {
		body += videoEncoderCfgXML(v, "Configurations")
	}
	return body + `</trt:GetVideoEncoderConfigurationsResponse>`, nil
}

func opGetVideoEncoderConfiguration(_ *Service, v CameraView) (string, error) {
	return `<trt:GetVideoEncoderConfigurationResponse>` +
		videoEncoderCfgXML(v, "Configuration") +
		`</trt:GetVideoEncoderConfigurationResponse>`, nil
}

// opSetVideoEncoderConfiguration 接受客户端下发的编码参数并应用到摄像头配置，
// 随后触发取流进程重启（分辨率/帧率/码率生效）。
func opSetVideoEncoderConfiguration(s *Service, inner []byte) (string, error) {
	var req setEncoderReq
	if err := parseInner(inner, &req); err != nil {
		return "", err
	}
	token := req.Configuration.Token
	if token == "" {
		return "", errors.New("configuration token is required")
	}
	view, ok := s.findByEncoder(token)
	if !ok {
		return "", fmt.Errorf("video encoder configuration %q not found", token)
	}
	var updated config.Camera
	err := s.store.Update(func(r *config.Root) error {
		for i := range r.Cameras {
			if r.Cameras[i].ID != view.Camera.ID {
				continue
			}
			if req.Configuration.Resolution.Width > 0 {
				r.Cameras[i].Width = req.Configuration.Resolution.Width
			}
			if req.Configuration.Resolution.Height > 0 {
				r.Cameras[i].Height = req.Configuration.Resolution.Height
			}
			if req.Configuration.RateControl.FrameRateLimit > 0 {
				r.Cameras[i].Framerate = req.Configuration.RateControl.FrameRateLimit
			}
			if req.Configuration.RateControl.BitrateLimit > 0 {
				r.Cameras[i].BitrateKBPS = req.Configuration.RateControl.BitrateLimit
			}
			updated = r.Cameras[i]
			return nil
		}
		return fmt.Errorf("camera %q not found", view.Camera.ID)
	})
	if err != nil {
		return "", err
	}
	s.logger.Info("encoder config updated via onvif",
		"camera", updated.ID, "width", updated.Width, "height", updated.Height,
		"framerate", updated.Framerate, "bitrate_kbps", updated.BitrateKBPS)
	if s.OnEncoderChange != nil {
		s.OnEncoderChange(updated)
	}
	return `<trt:SetVideoEncoderConfigurationResponse/>`, nil
}

func opGetEncoderOptions(s *Service, v CameraView) (string, error) {
	w, h := resolutionOf(v.Camera)
	res := func(w, h int) string {
		return fmt.Sprintf(`<tt:ResolutionsAvailable><tt:Width>%d</tt:Width><tt:Height>%d</tt:Height></tt:ResolutionsAvailable>`, w, h)
	}
	body := `<trt:GetVideoEncoderConfigurationOptionsResponse><trt:Options>` +
		`<tt:QualityRange><tt:Min>1</tt:Min><tt:Max>10</tt:Max></tt:QualityRange>` +
		res(w, h) + res(1920, 1080) + res(1280, 720) + res(640, 480) +
		`<tt:JpegOptions>` + res(1920, 1080) + res(1280, 720) + res(640, 480) +
		fmt.Sprintf(`<tt:FrameRateRange><tt:Min>1</tt:Min><tt:Max>%d</tt:Max></tt:FrameRateRange>`, v.Camera.FramerateOrDefault()) +
		`</tt:JpegOptions>` +
		`<tt:Mpeg4Options/>` +
		`<tt:H264Options>` + res(1920, 1080) + res(1280, 720) + res(640, 480) +
		`<tt:GovLengthRange><tt:Min>1</tt:Min><tt:Max>120</tt:Max></tt:GovLengthRange>` +
		`<tt:FrameRateRange><tt:Min>1</tt:Min><tt:Max>30</tt:Max></tt:FrameRateRange>` +
		`<tt:EncodingProfiles>Baseline</tt:EncodingProfiles>` +
		`<tt:EncodingProfiles>Main</tt:EncodingProfiles>` +
		`<tt:EncodingProfiles>High</tt:EncodingProfiles>` +
		`</tt:H264Options>` +
		`</trt:Options></trt:GetVideoEncoderConfigurationOptionsResponse>`
	return body, nil
}

func opGetStreamUri(s *Service, v CameraView) (string, error) {
	uri := xmlEsc(s.uris.StreamURI(v.Camera.ID))
	return `<trt:GetStreamUriResponse><trt:MediaUri>` +
		`<tt:Uri>` + uri + `</tt:Uri>` +
		`<tt:InvalidAfterConnect>false</tt:InvalidAfterConnect>` +
		`<tt:InvalidAfterReboot>false</tt:InvalidAfterReboot>` +
		`<tt:Timeout>PT60S</tt:Timeout>` +
		`</trt:MediaUri></trt:GetStreamUriResponse>`, nil
}

func opGetSnapshotUri(s *Service, v CameraView) (string, error) {
	uri := xmlEsc(s.uris.SnapshotURI(v.ProfileToken))
	return `<trt:GetSnapshotUriResponse><trt:MediaUri>` +
		`<tt:Uri>` + uri + `</tt:Uri>` +
		`<tt:InvalidAfterConnect>false</tt:InvalidAfterConnect>` +
		`<tt:InvalidAfterReboot>false</tt:InvalidAfterReboot>` +
		`<tt:Timeout>PT10S</tt:Timeout>` +
		`</trt:MediaUri></trt:GetSnapshotUriResponse>`, nil
}

// fmtF 以最短形式格式化浮点数（供各处 XML 使用）。
func fmtF(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
