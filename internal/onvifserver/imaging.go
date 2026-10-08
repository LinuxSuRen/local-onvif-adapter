package onvifserver

import (
	"fmt"
	"sync"
)

// 焦距调整的虚拟状态（每视频源一份，mock）。
type focusState struct {
	mu       sync.Mutex
	position float64 // 绝对焦距位置 0.0-1.0
	velocity float64 // 连续移动速度 -1.0-1.0（正=远焦 Far，负=近焦 Near）
	autoMode string  // AUTO | MANUAL
}

// focusRegistry 全局焦距状态（key: videoSourceToken）。
var focusRegistry = struct {
	mu sync.RWMutex
	m  map[string]*focusState
}{m: map[string]*focusState{}}

func getFocus(token string) *focusState {
	focusRegistry.mu.Lock()
	defer focusRegistry.mu.Unlock()
	if f, ok := focusRegistry.m[token]; ok {
		return f
	}
	f := &focusState{autoMode: "AUTO"}
	focusRegistry.m[token] = f
	return f
}

// registerImagingOps 注册 Imaging Service (timg) 操作。
// 焦距（Focus）支持：GetOptions 声明能力、GetMoveOptions 返回移动范围、
// Move 执行绝对/相对/连续焦距调整、Stop 停止。
func registerImagingOps() {
	reg(nsImaging, "GetImagingSettings", withVideoSourceToken(opGetImagingSettings))
	reg(nsImaging, "SetImagingSettings", withVideoSourceToken(opSetImagingSettings))
	reg(nsImaging, "GetOptions", withVideoSourceToken(opGetImagingOptions))
	reg(nsImaging, "GetMoveOptions", withVideoSourceToken(opGetMoveOptions))
	reg(nsImaging, "Move", withVideoSourceToken(opImagingMove))
	reg(nsImaging, "Stop", withVideoSourceToken(opImagingStop))
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
		Focus           *struct {
			AutoFocusMode string `xml:"AutoFocusMode"`
		} `xml:"Focus"`
	} `xml:"ImagingSettings"`
}

func opGetImagingSettings(s *Service, v CameraView, _ []byte) (string, error) {
	in := s.imagingFor(v.VideoSourceToken)
	f := getFocus(v.VideoSourceToken)
	f.mu.Lock()
	autoMode := f.autoMode
	pos := f.position
	f.mu.Unlock()
	return `<timg:GetImagingSettingsResponse><tt:ImagingSettings>` +
		fmt.Sprintf(`<tt:Brightness>%s</tt:Brightness><tt:ColorSaturation>%s</tt:ColorSaturation>`+
			`<tt:Contrast>%s</tt:Contrast><tt:Sharpness>%s</tt:Sharpness>`,
			fmtF(in.Brightness), fmtF(in.ColorSaturation), fmtF(in.Contrast), fmtF(in.Sharpness)) +
		`<tt:Exposure><tt:Mode>AUTO</tt:Mode></tt:Exposure>` +
		fmt.Sprintf(`<tt:Focus><tt:AutoFocusMode>%s</tt:AutoFocusMode></tt:Focus>`, autoMode) +
		`<tt:IrCutFilter>ON</tt:IrCutFilter>` +
		`</tt:ImagingSettings></timg:GetImagingSettingsResponse>` + fmt.Sprintf("<!-- focusPos=%s -->", fmtF(pos)), nil
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
	if req.ImagingSettings.Focus != nil && req.ImagingSettings.Focus.AutoFocusMode != "" {
		f := getFocus(v.VideoSourceToken)
		f.mu.Lock()
		f.autoMode = req.ImagingSettings.Focus.AutoFocusMode
		f.mu.Unlock()
	}
	s.setImaging(v.VideoSourceToken, in)
	s.logger.Info("imaging settings updated via onvif", "camera", v.Camera.ID)
	return `<timg:SetImagingSettingsResponse/>`, nil
}

// opGetImagingOptions 声明焦距能力：支持 AUTO/MANUAL 自动对焦、
// 焦距移动范围 0-1、连续移动 ±1、默认速度 0.5。
func opGetImagingOptions(_ *Service, _ CameraView, _ []byte) (string, error) {
	return `<timg:GetOptionsResponse><tt:ImagingOptions>` +
		`<tt:Brightness><tt:Min>0</tt:Min><tt:Max>255</tt:Max></tt:Brightness>` +
		`<tt:ColorSaturation><tt:Min>0</tt:Min><tt:Max>100</tt:Max></tt:ColorSaturation>` +
		`<tt:Contrast><tt:Min>0</tt:Min><tt:Max>100</tt:Max></tt:Contrast>` +
		`<tt:Sharpness><tt:Min>0</tt:Min><tt:Max>100</tt:Max></tt:Sharpness>` +
		`<tt:Exposure><tt:Mode>AUTO</tt:Mode><tt:Mode>MANUAL</tt:Mode></tt:Exposure>` +
		`<tt:Focus>` +
		`<tt:AutoFocusMode>AUTO</tt:AutoFocusMode>` +
		`<tt:AutoFocusMode>MANUAL</tt:AutoFocusMode>` +
		`<tt:DefaultSpeed>0.5</tt:DefaultSpeed>` +
		`<tt:NearLimit>0</tt:NearLimit>` +
		`<tt:FarLimit>1</tt:FarLimit>` +
		`</tt:Focus>` +
		`</tt:ImagingOptions></timg:GetOptionsResponse>`, nil
}

// opGetMoveOptions 返回焦距移动的详细能力（绝对/相对/连续的范围与默认速度）。
func opGetMoveOptions(_ *Service, _ CameraView, _ []byte) (string, error) {
	return `<timg:GetMoveOptionsResponse><tt:MoveOptions>` +
		`<tt:Focus>` +
		`<tt:Absolute><tt:Min>0</tt:Min><tt:Max>1</tt:Max><tt:URI>http://www.onvif.org/ver10/schema</tt:URI></tt:Absolute>` +
		`<tt:Relative><tt:Min>-1</tt:Min><tt:Max>1</tt:Max><tt:URI>http://www.onvif.org/ver10/schema</tt:URI></tt:Relative>` +
		`<tt:Continuous><tt:Min>-1</tt:Min><tt:Max>1</tt:Max><tt:URI>http://www.onvif.org/ver10/schema</tt:URI></tt:Continuous>` +
		`<tt:DefaultSpeed>0.5</tt:DefaultSpeed>` +
		`<tt:NearLimit>0</tt:NearLimit>` +
		`<tt:FarLimit>1</tt:FarLimit>` +
		`</tt:Focus>` +
		`</tt:MoveOptions></timg:GetMoveOptionsResponse>`, nil
}

// imagingMoveReq 焦距移动请求。
type imagingMoveReq struct {
	Focus struct {
		Absolute *struct {
			X float64 `xml:"x,attr"`
		} `xml:"Absolute"`
		Relative *struct {
			X float64 `xml:"x,attr"`
		} `xml:"Relative"`
		Continuous *struct {
			X float64 `xml:"x,attr"`
		} `xml:"Continuous"`
	} `xml:"Focus"`
}

// opImagingMove 执行焦距调整（绝对/相对/连续）。
func opImagingMove(s *Service, v CameraView, inner []byte) (string, error) {
	var req imagingMoveReq
	if err := parseInner(inner, &req); err != nil {
		return "", err
	}
	f := getFocus(v.VideoSourceToken)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case req.Focus.Absolute != nil:
		f.position = clampF(req.Focus.Absolute.X, 0, 1)
		f.velocity = 0
		s.logger.Info("focus absolute move", "camera", v.Camera.ID, "position", fmtF(f.position))
	case req.Focus.Relative != nil:
		f.position = clampF(f.position+req.Focus.Relative.X, 0, 1)
		f.velocity = 0
		s.logger.Info("focus relative move", "camera", v.Camera.ID, "position", fmtF(f.position))
	case req.Focus.Continuous != nil:
		f.velocity = clampF(req.Focus.Continuous.X, -1, 1)
		s.logger.Info("focus continuous move", "camera", v.Camera.ID, "velocity", fmtF(f.velocity))
	}
	return `<timg:MoveResponse/>`, nil
}

// opImagingStop 停止焦距连续移动。
func opImagingStop(s *Service, v CameraView, _ []byte) (string, error) {
	f := getFocus(v.VideoSourceToken)
	f.mu.Lock()
	f.velocity = 0
	f.mu.Unlock()
	s.logger.Info("focus stop", "camera", v.Camera.ID)
	return `<timg:StopResponse/>`, nil
}

func clampF(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
