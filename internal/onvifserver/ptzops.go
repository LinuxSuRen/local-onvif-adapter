package onvifserver

import (
	"fmt"
	"time"
)

// ONVIF 标准 PTZ 空间 URI。
const (
	absPanTiltSpace = "http://www.onvif.org/ver10/tptz/PanTiltSpaces/PositionGenericSpace"
	absZoomSpace    = "http://www.onvif.org/ver10/tptz/ZoomSpaces/PositionGenericSpace"
	velPanTiltSpace = "http://www.onvif.org/ver10/tptz/PanTiltSpaces/VelocityGenericSpace"
	velZoomSpace    = "http://www.onvif.org/ver10/tptz/ZoomSpaces/VelocityGenericSpace"
)

// registerPTZOps 注册 PTZ Service (tptz, ver20) 操作。
// 云台为纯 mock：只维护虚拟位置，所有操作均返回成功。
func registerPTZOps() {
	reg(nsPTZ, "GetNodes", opGetNodes)
	reg(nsPTZ, "GetNode", withPTZNodeToken(opGetNode))
	reg(nsPTZ, "GetConfigurations", opGetConfigurations)
	reg(nsPTZ, "GetConfiguration", withPTZCfgToken(opGetConfiguration))
	reg(nsPTZ, "GetConfigurationOptions", withPTZCfgToken(opGetConfigurationOptions))
	reg(nsPTZ, "GetStatus", withProfileTokenPTZ(opGetStatus))
	reg(nsPTZ, "ContinuousMove", withProfileTokenPTZ(opContinuousMove))
	reg(nsPTZ, "RelativeMove", withProfileTokenPTZ(opRelativeMove))
	reg(nsPTZ, "AbsoluteMove", withProfileTokenPTZ(opAbsoluteMove))
	reg(nsPTZ, "Stop", withProfileTokenPTZ(opStop))
	reg(nsPTZ, "GotoHomePosition", withProfileTokenPTZ(opGotoHome))
	reg(nsPTZ, "SetHomePosition", withProfileTokenPTZ(opSetHome))
	reg(nsPTZ, "GetPresets", withProfileTokenPTZ(opGetPresets))
	reg(nsPTZ, "SetPreset", withProfileTokenPTZ(opSetPreset))
	reg(nsPTZ, "GotoPreset", withProfileTokenPTZ(opGotoPreset))
	reg(nsPTZ, "RemovePreset", withProfileTokenPTZ(opRemovePreset))
	reg(nsPTZ, "GetServiceCapabilities", func(s *Service, _ []byte) (string, error) {
		return `<tptz:GetServiceCapabilitiesResponse>` +
			`<tptz:Capabilities EFlip="false" Reverse="false" ContinuousMove="true" RelativeMove="true" AbsoluteMove="true" Presets="true"/>` +
			`</tptz:GetServiceCapabilitiesResponse>`, nil
	})
}

// ---- 请求解析 ----

type vector struct {
	X float64 `xml:"x,attr"`
	Y float64 `xml:"y,attr"`
}

type ptzVector struct {
	PanTilt vector `xml:"PanTilt"`
	Zoom    struct {
		X float64 `xml:"x,attr"`
	} `xml:"Zoom"`
}

func (v ptzVector) pan() float64  { return v.PanTilt.X }
func (v ptzVector) tilt() float64 { return v.PanTilt.Y }
func (v ptzVector) zoom() float64 { return v.Zoom.X }

type ptzReq struct {
	ProfileToken string    `xml:"ProfileToken"`
	Position     ptzVector `xml:"Position"`
	Translation  ptzVector `xml:"Translation"`
	Velocity     ptzVector `xml:"Velocity"`
	Speed        ptzVector `xml:"Speed"`
	PanTiltStop  bool      `xml:"PanTilt"`
	ZoomStop     bool      `xml:"Zoom"`
	PresetToken  string    `xml:"PresetToken"`
	PresetName   string    `xml:"PresetName"`
	Timeout      string    `xml:"Timeout"`
}

func withProfileTokenPTZ(fn func(s *Service, v CameraView, req ptzReq) (string, error)) opHandler {
	return func(s *Service, inner []byte) (string, error) {
		var req ptzReq
		if err := parseInner(inner, &req); err != nil {
			return "", err
		}
		v, ok := s.findByProfile(req.ProfileToken)
		if !ok {
			// PTZ 的约定是"不因云台而报错"，token 无效时静默成功。
			s.logger.Debug("ptz op on unknown profile, ignored", "profile", req.ProfileToken)
			return `<tptz:Response/>`, nil
		}
		return fn(s, v, req)
	}
}

func withPTZNodeToken(fn func(s *Service, v CameraView) (string, error)) opHandler {
	return func(s *Service, inner []byte) (string, error) {
		var req struct {
			NodeToken string `xml:"NodeToken"`
		}
		if err := parseInner(inner, &req); err != nil {
			return "", err
		}
		v, ok := s.findByPTZNode(req.NodeToken)
		if !ok {
			return "", fmt.Errorf("ptz node %q not found", req.NodeToken)
		}
		return fn(s, v)
	}
}

func withPTZCfgToken(fn func(s *Service, v CameraView) (string, error)) opHandler {
	return func(s *Service, inner []byte) (string, error) {
		var req struct {
			PTZConfigurationToken string `xml:"PTZConfigurationToken"`
		}
		if err := parseInner(inner, &req); err != nil {
			return "", err
		}
		v, ok := s.findByPTZConfig(req.PTZConfigurationToken)
		if !ok {
			return "", fmt.Errorf("ptz configuration %q not found", req.PTZConfigurationToken)
		}
		return fn(s, v)
	}
}

// ---- 响应构造 ----

func opGetNodes(s *Service, _ []byte) (string, error) {
	body := `<tptz:GetNodesResponse>`
	for _, v := range s.EnabledViews() {
		body += ptzNodeXML(v)
	}
	return body + `</tptz:GetNodesResponse>`, nil
}

func opGetNode(_ *Service, v CameraView) (string, error) {
	return `<tptz:GetNodeResponse>` + ptzNodeXML(v) + `</tptz:GetNodeResponse>`, nil
}

func ptzNodeXML(v CameraView) string {
	return `<tptz:PTZNode token="` + v.PTZNodeToken + `" FixedHomePosition="false" HomeSupported="true">` +
		`<tt:Name>` + xmlEsc(v.Camera.Name) + ` PTZ</tt:Name>` +
		`<tt:SupportedPTZSpaces>` +
		`<tt:AbsolutePanTiltPositionSpace><tt:XRange><tt:Min>-180</tt:Min><tt:Max>180</tt:Max></tt:XRange>` +
		`<tt:YRange><tt:Min>-90</tt:Min><tt:Max>90</tt:Max></tt:YRange>` +
		`<tt:URI>` + absPanTiltSpace + `</tt:URI></tt:AbsolutePanTiltPositionSpace>` +
		`<tt:AbsoluteZoomPositionSpace><tt:XRange><tt:Min>0</tt:Min><tt:Max>1</tt:Max></tt:XRange>` +
		`<tt:URI>` + absZoomSpace + `</tt:URI></tt:AbsoluteZoomPositionSpace>` +
		`<tt:ContinuousPanTiltVelocitySpace><tt:XRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:XRange>` +
		`<tt:YRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:YRange>` +
		`<tt:URI>` + velPanTiltSpace + `</tt:URI></tt:ContinuousPanTiltVelocitySpace>` +
		`<tt:ContinuousZoomVelocitySpace><tt:XRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:XRange>` +
		`<tt:URI>` + velZoomSpace + `</tt:URI></tt:ContinuousZoomVelocitySpace>` +
		`</tt:SupportedPTZSpaces>` +
		`<tt:MaximumNumberOfPresets>16</tt:MaximumNumberOfPresets>` +
		`<tt:HomeSupported>true</tt:HomeSupported>` +
		`<tt:AuxiliaryCommands></tt:AuxiliaryCommands>` +
		`</tptz:PTZNode>`
}

func ptzCfgXML(v CameraView) string {
	return `<tptz:PTZConfiguration token="` + v.PTZConfigToken + `">` +
		`<tt:Name>PTZ</tt:Name><tt:UseCount>1</tt:UseCount>` +
		`<tt:NodeToken>` + v.PTZNodeToken + `</tt:NodeToken>` +
		`<tt:DefaultAbsolutePantTiltVelocitySpace>` + absPanTiltSpace + `</tt:DefaultAbsolutePantTiltVelocitySpace>` +
		`<tt:DefaultAbsoluteZoomVelocitySpace>` + absZoomSpace + `</tt:DefaultAbsoluteZoomVelocitySpace>` +
		`<tt:DefaultContinuousPanTiltVelocitySpace>` + velPanTiltSpace + `</tt:DefaultContinuousPanTiltVelocitySpace>` +
		`<tt:DefaultContinuousZoomVelocitySpace>` + velZoomSpace + `</tt:DefaultContinuousZoomVelocitySpace>` +
		`<tt:DefaultPTZSpeed><tt:PanTilt x="0.5" y="0.5" space="` + velPanTiltSpace + `"/>` +
		`<tt:Zoom x="0.5" space="` + velZoomSpace + `"/></tt:DefaultPTZSpeed>` +
		`<tt:DefaultPTZTimeout>PT5S</tt:DefaultPTZTimeout>` +
		`</tptz:PTZConfiguration>`
}

func opGetConfigurations(s *Service, _ []byte) (string, error) {
	body := `<tptz:GetConfigurationsResponse>`
	for _, v := range s.EnabledViews() {
		body += ptzCfgXML(v)
	}
	return body + `</tptz:GetConfigurationsResponse>`, nil
}

func opGetConfiguration(_ *Service, v CameraView) (string, error) {
	return `<tptz:GetConfigurationResponse>` + ptzCfgXML(v) + `</tptz:GetConfigurationResponse>`, nil
}

func opGetConfigurationOptions(_ *Service, v CameraView) (string, error) {
	return `<tptz:GetConfigurationOptionsResponse><tptz:PTZOptions>` +
		`<tt:Spaces>` +
		`<tt:AbsolutePanTiltPositionSpace><tt:XRange><tt:Min>-180</tt:Min><tt:Max>180</tt:Max></tt:XRange>` +
		`<tt:YRange><tt:Min>-90</tt:Min><tt:Max>90</tt:Max></tt:YRange><tt:URI>` + absPanTiltSpace + `</tt:URI></tt:AbsolutePanTiltPositionSpace>` +
		`<tt:AbsoluteZoomPositionSpace><tt:XRange><tt:Min>0</tt:Min><tt:Max>1</tt:Max></tt:XRange>` +
		`<tt:URI>` + absZoomSpace + `</tt:URI></tt:AbsoluteZoomPositionSpace>` +
		`<tt:ContinuousPanTiltVelocitySpace><tt:XRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:XRange>` +
		`<tt:YRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:YRange><tt:URI>` + velPanTiltSpace + `</tt:URI></tt:ContinuousPanTiltVelocitySpace>` +
		`<tt:ContinuousZoomVelocitySpace><tt:XRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:XRange>` +
		`<tt:URI>` + velZoomSpace + `</tt:URI></tt:ContinuousZoomVelocitySpace>` +
		`</tt:Spaces>` +
		`<tt:PTZTimeout><tt:Min>PT1S</tt:Min><tt:Max>PT60S</tt:Max></tt:PTZTimeout>` +
		`</tptz:PTZOptions></tptz:GetConfigurationOptionsResponse>`, nil
}

func statusXML(v CameraView) string {
	pan, tilt, zoom, moving := v.PTZ.Status()
	moveStatus := "IDLE"
	if moving {
		moveStatus = "MOVING"
	}
	now := time.Now().UTC()
	return `<tptz:PTZStatus>` +
		fmt.Sprintf(`<tt:Position><tt:PanTilt x="%s" y="%s" space="%s"/><tt:Zoom x="%s" space="%s"/></tt:Position>`,
			fmtF(pan), fmtF(tilt), absPanTiltSpace, fmtF(zoom), absZoomSpace) +
		`<tt:MoveStatus><tt:PanTilt>` + moveStatus + `</tt:PanTilt><tt:Zoom>` + moveStatus + `</tt:Zoom></tt:MoveStatus>` +
		`<tt:Error>No error</tt:Error>` +
		dateTimeXML(now) +
		`</tptz:PTZStatus>`
}

func opGetStatus(_ *Service, v CameraView, _ ptzReq) (string, error) {
	return `<tptz:GetStatusResponse>` + statusXML(v) + `</tptz:GetStatusResponse>`, nil
}

func opContinuousMove(s *Service, v CameraView, req ptzReq) (string, error) {
	v.PTZ.ContinuousMove(req.Velocity.pan(), req.Velocity.tilt(), req.Velocity.zoom())
	s.logger.Debug("ptz continuous move", "camera", v.Camera.ID,
		"pan", req.Velocity.pan(), "tilt", req.Velocity.tilt(), "zoom", req.Velocity.zoom())
	return `<tptz:ContinuousMoveResponse/>`, nil
}

func opRelativeMove(s *Service, v CameraView, req ptzReq) (string, error) {
	v.PTZ.RelativeMove(req.Translation.pan(), req.Translation.tilt(), req.Translation.zoom())
	return `<tptz:RelativeMoveResponse/>`, nil
}

func opAbsoluteMove(s *Service, v CameraView, req ptzReq) (string, error) {
	v.PTZ.AbsoluteMove(req.Position.pan(), req.Position.tilt(), req.Position.zoom())
	s.logger.Debug("ptz absolute move", "camera", v.Camera.ID,
		"pan", req.Position.pan(), "tilt", req.Position.tilt(), "zoom", req.Position.zoom())
	return `<tptz:AbsoluteMoveResponse/>`, nil
}

func opStop(s *Service, v CameraView, req ptzReq) (string, error) {
	// 按规范：未显式指定 PanTilt/Zoom 时默认全部停止；指定了则只停指定的轴。
	panTilt, zoom := true, true
	if req.PanTiltStop || req.ZoomStop {
		panTilt, zoom = req.PanTiltStop, req.ZoomStop
	}
	v.PTZ.Stop(panTilt, zoom)
	return `<tptz:StopResponse/>`, nil
}

func opGotoHome(_ *Service, v CameraView, _ ptzReq) (string, error) {
	v.PTZ.GotoHomePosition()
	return `<tptz:GotoHomePositionResponse/>`, nil
}

func opSetHome(_ *Service, v CameraView, _ ptzReq) (string, error) {
	v.PTZ.SetHomePosition()
	return `<tptz:SetHomePositionResponse/>`, nil
}

func opGetPresets(_ *Service, v CameraView, _ ptzReq) (string, error) {
	body := `<tptz:GetPresetsResponse>`
	for _, p := range v.PTZ.Presets() {
		body += `<tptz:Preset token="` + xmlEsc(p.Token) + `">` +
			`<tt:Name>` + xmlEsc(p.Name) + `</tt:Name>` +
			fmt.Sprintf(`<tt:PTZPosition><tt:PanTilt x="%s" y="%s" space="%s"/><tt:Zoom x="%s" space="%s"/></tt:PTZPosition>`,
				fmtF(p.Pan), fmtF(p.Tilt), absPanTiltSpace, fmtF(p.Zoom), absZoomSpace) +
			`</tptz:Preset>`
	}
	return body + `</tptz:GetPresetsResponse>`, nil
}

func opSetPreset(s *Service, v CameraView, req ptzReq) (string, error) {
	name := req.PresetName
	if name == "" {
		name = req.PresetToken
	}
	token := v.PTZ.SetPreset(name)
	s.logger.Info("ptz preset saved", "camera", v.Camera.ID, "name", name, "token", token)
	return `<tptz:SetPresetResponse><tptz:PresetToken>` + xmlEsc(token) + `</tptz:PresetToken></tptz:SetPresetResponse>`, nil
}

func opGotoPreset(_ *Service, v CameraView, req ptzReq) (string, error) {
	v.PTZ.GotoPreset(req.PresetToken)
	return `<tptz:GotoPresetResponse/>`, nil
}

func opRemovePreset(_ *Service, v CameraView, req ptzReq) (string, error) {
	v.PTZ.RemovePreset(req.PresetToken)
	return `<tptz:RemovePresetResponse/>`, nil
}
