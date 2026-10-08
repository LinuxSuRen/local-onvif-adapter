// Package config 提供服务配置与摄像头列表的持久化（JSON 文件）。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// CameraType 表示摄像头取流来源类型。
type CameraType string

// 支持的来源类型。
const (
	TypeV4L2         CameraType = "v4l2"         // Linux 本地设备，如 /dev/video0
	TypeAVFoundation CameraType = "avfoundation" // macOS 本地摄像头，source 为设备索引
	TypeDShow        CameraType = "dshow"        // Windows 本地摄像头，source 为 DirectShow 设备名
	TypeScreen       CameraType = "screen"       // 屏幕采集：macOS 屏幕索引 / Linux X display(:0.0) / Windows desktop
	TypeRTSP         CameraType = "rtsp"         // 拉取已有网络摄像头的 RTSP 流
	TypeTestSrc      CameraType = "testsrc"      // ffmpeg 测试彩条，无需真实摄像头
)

// Valid 校验来源类型是否合法。
func (t CameraType) Valid() bool {
	switch t {
	case TypeV4L2, TypeAVFoundation, TypeDShow, TypeScreen, TypeRTSP, TypeTestSrc:
		return true
	default:
		return false
	}
}

// Camera 描述一个本地摄像头（对应一个 ONVIF media profile）。
type Camera struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Type        CameraType `json:"type"`
	Source      string     `json:"source"`
	Width       int        `json:"width"`     // 0 表示自动
	Height      int        `json:"height"`    // 0 表示自动
	Framerate   int        `json:"framerate"` // 0 表示默认 15
	BitrateKBPS int        `json:"bitrate_kbps"`
	Infrared    bool       `json:"infrared"`      // 标记为红外通道，profile 名称会带 infrared
	PTZ         bool       `json:"ptz_supported"` // 是否支持云台（PTZ）；不支持的摄像头 PTZ 操作返回不支持
	Enabled     bool       `json:"enabled"`
}

// FramerateOrDefault 返回生效帧率。
func (c Camera) FramerateOrDefault() int {
	if c.Framerate <= 0 {
		return 15
	}
	return c.Framerate
}

// BitrateOrDefault 返回生效码率（kbps）。
func (c Camera) BitrateOrDefault() int {
	if c.BitrateKBPS <= 0 {
		return 2048
	}
	return c.BitrateKBPS
}

// Server 服务级配置。
type Server struct {
	HTTPAddr     string `json:"http_addr"`      // HTTP 监听地址，默认 :8080
	AdvertiseIP  string `json:"advertise_ip"`   // 对外宣告 IP，空则自动探测
	RTSPPushAddr string `json:"rtsp_push_addr"` // ffmpeg 推流目标（仅外部 mediamtx 模式使用）
	RTSPPort     int    `json:"rtsp_port"`      // RTSP 端口：内嵌服务器监听端口 + 对外宣告端口
	RTSPEmbedded bool   `json:"rtsp_embedded"`  // 是否启用内嵌 RTSP 服务器（默认 true）
	FFmpegBin    string `json:"ffmpeg_bin"`
	Discovery    bool   `json:"discovery"` // 是否开启 WS-Discovery
}

// Root 配置文件根节点。
type Root struct {
	Server       Server   `json:"server"`
	Manufacturer string   `json:"manufacturer"`
	Model        string   `json:"model"`
	Firmware     string   `json:"firmware"`
	Serial       string   `json:"serial"` // 设备序列号，首次生成后持久化
	NextID       int      `json:"next_id"`
	Cameras      []Camera `json:"cameras"`
}

// Default 返回默认配置。
func Default() *Root {
	return &Root{
		Server: Server{
			HTTPAddr:     ":8080",
			AdvertiseIP:  "",
			RTSPPushAddr: "rtsp://127.0.0.1:8554",
			RTSPPort:     8554,
			FFmpegBin:    "ffmpeg",
			Discovery:    true,
		},
		Manufacturer: "linuxsuren",
		Model:        "local-onvif-adapter",
		Firmware:     "0.1.0",
		NextID:       1,
		Cameras:      []Camera{},
	}
}

// ErrNotFound 表示配置项不存在。
var ErrNotFound = errors.New("not found")

// Store 管理配置文件的读写，所有方法并发安全。
type Store struct {
	mu   sync.RWMutex
	path string
	root *Root
}

// LoadStore 从 dataDir 加载配置；文件不存在时创建默认配置。
func LoadStore(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	path := filepath.Join(dataDir, "config.json")
	s := &Store{path: path, root: Default()}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.root.normalize()
		if err := persist(s.root, path); err != nil {
			return nil, err
		}
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, s.root); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	s.root.normalize()
	return s, nil
}

// normalize 补齐历史/手工编辑配置中的缺省字段。
func (r *Root) normalize() {
	d := Default()
	if r.Server.HTTPAddr == "" {
		r.Server.HTTPAddr = d.Server.HTTPAddr
	}
	if r.Server.RTSPPushAddr == "" {
		r.Server.RTSPPushAddr = d.Server.RTSPPushAddr
	}
	if r.Server.RTSPPort == 0 {
		r.Server.RTSPPort = d.Server.RTSPPort
	}
	if r.Server.FFmpegBin == "" {
		r.Server.FFmpegBin = d.Server.FFmpegBin
	}
	if r.Manufacturer == "" {
		r.Manufacturer = d.Manufacturer
	}
	if r.Model == "" {
		r.Model = d.Model
	}
	if r.Firmware == "" {
		r.Firmware = d.Firmware
	}
	if r.Serial == "" {
		r.Serial = randomSerial()
	}
	if r.NextID <= 0 {
		r.NextID = len(r.Cameras) + 1
	}
	if r.Cameras == nil {
		r.Cameras = []Camera{}
	}
}

// Root 返回当前配置的深拷贝快照。
func (s *Store) Root() Root {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.root.clone()
}

// Update 在写事务中修改配置并持久化；fn 返回错误则放弃全部修改（事务回滚）。
func (s *Store) Update(fn func(r *Root) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	draft := s.root.clone()
	if err := fn(&draft); err != nil {
		return err
	}
	draft.normalize()
	if err := persist(&draft, s.path); err != nil {
		return err
	}
	s.root = &draft
	return nil
}

// persist 序列化并写入磁盘。
func persist(r *Root, path string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	return nil
}

func (r *Root) clone() Root {
	out := *r
	out.Cameras = append([]Camera(nil), r.Cameras...)
	return out
}

// FindCamera 按 ID 查找摄像头。
func (r Root) FindCamera(id string) (Camera, bool) {
	for _, c := range r.Cameras {
		if c.ID == id {
			return c, true
		}
	}
	return Camera{}, false
}
