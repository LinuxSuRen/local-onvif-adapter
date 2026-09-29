package onvifserver

import (
	"fmt"
)

// registerImagingOps 注册 Imaging Service (timg) 操作（参数仅内存态 mock）。
func registerImagingOps() {
	reg(nsImaging, "GetImagingSettings", withVideoSourceToken(opGetImagingSettings))
	reg(nsImaging, "SetImagingSettings", withVideoSourceToken(opSetImagingSettings))
	reg(nsImaging, "GetOptions", withVideoSourceToken(opGetImagingOptions))
	reg(nsImaging, "GetMoveOptions", func(s *Service, _ []byte) (string, error) {
		return `<timg:GetMoveOptionsResponse/>`, nil
	})
	reg(nsImaging, "Stop", func(s *Service, _ []byte) (string, error) {
		return `<timg:StopResponse/>`, nil
	})
}

func withVideoSourceToken(fn func(s *Service, v CameraView, inner []byte) (string, error)) opHandler {
	return func(s *Service, inner []byte) (string, error) {
		var req struct {
			VideoSourceToken string `xml:"VideoSourceToken"`
		}
		if err := parseInner(inner, &req); err != nil {
			return "", err
		}
		v, ok := s.findByVideoSource(req.VideoSourceToken)
		if !ok {
			return "", fmt.Errorf("video source %q not found", req.VideoSourceToken)
		}
		return fn(s, v, inner)
	}
}

type imagingReq struct {
	ImagingSettings struct {
		Brightness      *float64 `xml:"Brightness"`
		ColorSaturation *float64 `xml:"ColorSaturation"`
		Contrast        *float64 `xml:"Contrast"`
		Sharpness       *float64 `xml:"Sharpness"`
	} `xml:"ImagingSettings"`
}

func opGetImagingSettings(s *Service, v CameraView, _ []byte) (string, error) {
	in := s.imagingFor(v.VideoSourceToken)
	return `<timg:GetImagingSettingsResponse><tt:ImagingSettings>` +
		fmt.Sprintf(`<tt:Brightness>%s</tt:Brightness><tt:ColorSaturation>%s</tt:ColorSaturation>`+
			`<tt:Contrast>%s</tt:Contrast><tt:Sharpness>%s</tt:Sharpness>`,
			fmtF(in.Brightness), fmtF(in.ColorSaturation), fmtF(in.Contrast), fmtF(in.Sharpness)) +
		`<tt:Exposure><tt:Mode>AUTO</tt:Mode></tt:Exposure>` +
		`<tt:Focus><tt:AutoFocusMode>AUTO</tt:AutoFocusMode></tt:Focus>` +
		`<tt:IrCutFilter>ON</tt:IrCutFilter>` +
		`</tt:ImagingSettings></timg:GetImagingSettingsResponse>`, nil
}

func opSetImagingSettings(s *Service, v CameraView, inner []byte) (string, error) {
	var req imagingReq
	if err := parseInner(inner, &req); err != nil {
		return "", err
	}
	in := s.imagingFor(v.VideoSourceToken)
	if req.ImagingSettings.Brightness != nil {
		in.Brightness = *req.ImagingSettings.Brightness
	}
	if req.ImagingSettings.ColorSaturation != nil {
		in.ColorSaturation = *req.ImagingSettings.ColorSaturation
	}
	if req.ImagingSettings.Contrast != nil {
		in.Contrast = *req.ImagingSettings.Contrast
	}
	if req.ImagingSettings.Sharpness != nil {
		in.Sharpness = *req.ImagingSettings.Sharpness
	}
	s.setImaging(v.VideoSourceToken, in)
	s.logger.Info("imaging settings updated via onvif", "camera", v.Camera.ID)
	return `<timg:SetImagingSettingsResponse/>`, nil
}

func opGetImagingOptions(_ *Service, _ CameraView, _ []byte) (string, error) {
	return `<timg:GetOptionsResponse><tt:ImagingOptions>` +
		`<tt:Brightness><tt:Min>0</tt:Min><tt:Max>255</tt:Max></tt:Brightness>` +
		`<tt:ColorSaturation><tt:Min>0</tt:Min><tt:Max>100</tt:Max></tt:ColorSaturation>` +
		`<tt:Contrast><tt:Min>0</tt:Min><tt:Max>100</tt:Max></tt:Contrast>` +
		`<tt:Sharpness><tt:Min>0</tt:Min><tt:Max>100</tt:Max></tt:Sharpness>` +
		`<tt:Exposure><tt:Mode>AUTO</tt:Mode><tt:Mode>MANUAL</tt:Mode></tt:Exposure>` +
		`<tt:Focus><tt:AutoFocusMode>AUTO</tt:AutoFocusMode><tt:AutoFocusMode>MANUAL</tt:AutoFocusMode></tt:Focus>` +
		`</tt:ImagingOptions></timg:GetOptionsResponse>`, nil
}
