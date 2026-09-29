// Package onvifserver 实现一个最小而兼容的 ONVIF 设备端 SOAP 服务：
// Device / Media / PTZ / Imaging 四个服务共用单一 HTTP 端点，
// 将本地摄像头以"单设备多 profile"的方式暴露给 ONVIF 客户端。
package onvifserver

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
	"github.com/linuxsuren/local-onvif-adapter/internal/ptzmock"
)

// URIs 计算对外宣告的各类地址。
type URIs struct {
	AdvertiseIP string
	HTTPPort    int
	RTSPPort    int
}

// XAddr 返回 ONVIF Device Service 地址。
func (u URIs) XAddr() string {
	return fmt.Sprintf("http://%s:%d/onvif/device_service", u.AdvertiseIP, u.HTTPPort)
}

// StreamURI 返回摄像头的 RTSP 拉流地址（mediamtx 提供）。
func (u URIs) StreamURI(camID string) string {
	return fmt.Sprintf("rtsp://%s:%d/cam/%s", u.AdvertiseIP, u.RTSPPort, camID)
}

// SnapshotURI 返回摄像头快照 JPEG 地址。
func (u URIs) SnapshotURI(profileToken string) string {
	return fmt.Sprintf("http://%s:%d/onvif/snapshot/%s", u.AdvertiseIP, u.HTTPPort, profileToken)
}

// CameraView 是某个摄像头在 ONVIF 世界里的完整投影。
type CameraView struct {
	Camera            config.Camera
	ProfileToken      string
	VideoSourceToken  string
	VideoEncoderToken string
	VideoSourceCfgTok string
	PTZNodeToken      string
	PTZConfigToken    string
	PTZ               *ptzmock.Node
}

// ProfileName 返回 profile 展示名；红外通道追加 infrared 关键词，
// 便于 device-camera-onvif 等客户端按名称自动识别红外通道。
func (v CameraView) ProfileName() string {
	if v.Camera.Infrared {
		return v.Camera.Name + " infrared"
	}
	return v.Camera.Name
}

// NewCameraView 为摄像头构造 ONVIF 投影。
func NewCameraView(cam config.Camera, ptzReg *ptzmock.Registry) CameraView {
	id := cam.ID
	return CameraView{
		Camera:            cam,
		ProfileToken:      "profile_" + id,
		VideoSourceToken:  "vs_" + id,
		VideoEncoderToken: "vec_" + id,
		VideoSourceCfgTok: "vsc_" + id,
		PTZNodeToken:      "ptznode_" + id,
		PTZConfigToken:    "ptzcfg_" + id,
		PTZ:               ptzReg.Get(id),
	}
}

// imagingSettings 虚拟成像参数（mock）。
type imagingSettings struct {
	Brightness      float64
	ColorSaturation float64
	Contrast        float64
	Sharpness       float64
}

// Service 是 ONVIF SOAP 服务的核心依赖集合。
type Service struct {
	store   *config.Store
	ptzReg  *ptzmock.Registry
	uris    URIs
	logger  *slog.Logger
	imgMu   sync.Mutex
	imaging map[string]imagingSettings
	// OnEncoderChange 在编码参数被 ONVIF 客户端修改后回调（用于重启取流）。
	OnEncoderChange func(cam config.Camera)
}

// NewService 创建 ONVIF 服务。
func NewService(store *config.Store, ptzReg *ptzmock.Registry, uris URIs, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		store:   store,
		ptzReg:  ptzReg,
		uris:    uris,
		logger:  logger,
		imaging: map[string]imagingSettings{},
	}
}

// URIs 返回地址信息快照。
func (s *Service) URIs() URIs { return s.uris }

// EnabledViews 返回按配置顺序排列的已启用摄像头投影（即 profile 列表，
// 顺序与 device-camera-onvif 的 channel → profile index 映射一致）。
func (s *Service) EnabledViews() []CameraView {
	root := s.store.Root()
	views := make([]CameraView, 0, len(root.Cameras))
	for _, cam := range root.Cameras {
		if cam.Enabled {
			views = append(views, NewCameraView(cam, s.ptzReg))
		}
	}
	return views
}

func (s *Service) findByProfile(token string) (CameraView, bool) {
	for _, v := range s.EnabledViews() {
		if v.ProfileToken == token {
			return v, true
		}
	}
	return CameraView{}, false
}

func (s *Service) findByVideoSource(token string) (CameraView, bool) {
	for _, v := range s.EnabledViews() {
		if v.VideoSourceToken == token {
			return v, true
		}
	}
	return CameraView{}, false
}

func (s *Service) findByEncoder(token string) (CameraView, bool) {
	for _, v := range s.EnabledViews() {
		if v.VideoEncoderToken == token {
			return v, true
		}
	}
	return CameraView{}, false
}

func (s *Service) findByPTZConfig(token string) (CameraView, bool) {
	for _, v := range s.EnabledViews() {
		if v.PTZConfigToken == token {
			return v, true
		}
	}
	return CameraView{}, false
}

func (s *Service) findByPTZNode(token string) (CameraView, bool) {
	for _, v := range s.EnabledViews() {
		if v.PTZNodeToken == token {
			return v, true
		}
	}
	return CameraView{}, false
}

func (s *Service) imagingFor(videoSourceToken string) imagingSettings {
	s.imgMu.Lock()
	defer s.imgMu.Unlock()
	if v, ok := s.imaging[videoSourceToken]; ok {
		return v
	}
	// 与多数相机一致的默认值。
	return imagingSettings{Brightness: 128, ColorSaturation: 64, Contrast: 32, Sharpness: 32}
}

func (s *Service) setImaging(videoSourceToken string, in imagingSettings) {
	s.imgMu.Lock()
	defer s.imgMu.Unlock()
	s.imaging[videoSourceToken] = in
}
