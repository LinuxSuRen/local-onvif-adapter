// Package realsense 提供 RealSense 深度相机的取帧与分发能力：
//
//   - Source 抽象三类流（RGB 彩色 / 深度伪彩色 / IR 红外），统一输出 rgb24 帧；
//   - fake 合成源：无硬件时生成可预测的测试画面，全链路可离线验证；
//   - Server 把三路帧以 rawvideo over TCP 分发，ffmpeg 端用
//     `-f rawvideo -pixel_format rgb24 -video_size WxH -i tcp://...` 读取；
//   - librealsense cgo 驱动位于 cmd/rs-pipe（构建标签 realsense 隔离，
//     未打标签时本包无任何 cgo 依赖，跨平台构建不受影响）。
package realsense

import "fmt"

// StreamKind 流类型：一台 RealSense 设备同时输出的三类画面。
// 显式语义（而非通道序号），避免「配成可见光却显示红外」一类的混淆。
type StreamKind string

const (
	KindColor StreamKind = "color" // RGB 彩色图（RGB8）
	KindDepth StreamKind = "depth" // 深度图（Z16 毫米 → Turbo 伪彩色 rgb24）
	KindIR    StreamKind = "ir"    // 红外图（Y8 灰度 → rgb24）
)

// Kinds 全部流类型，也即 Server 依序监听的端口顺序（base+0/1/2）。
var Kinds = []StreamKind{KindColor, KindDepth, KindIR}

// Valid 校验流类型合法。
func (k StreamKind) Valid() bool {
	switch k {
	case KindColor, KindDepth, KindIR:
		return true
	default:
		return false
	}
}

// Frame 一帧 rgb24 画面（已统一色彩空间：深度已伪彩色化、IR 已灰度展开）。
type Frame struct {
	Kind   StreamKind
	Width  int
	Height int
	Data   []byte // len == Width*Height*3
}

// FrameSet 一次轮询得到的一组帧；设备按自身节奏成组产出（软同步）。
type FrameSet map[StreamKind]*Frame

// Source 帧来源：实现方负责按输出帧率节奏阻塞产出。
// 返回错误视为暂时性故障，由调用方决定重试；ctx 取消后应返回 ctx.Err()。
type Source interface {
	// Streams 声明本源能产出的流类型。
	Streams() []StreamKind
	// Poll 阻塞取下一组帧。
	Poll() (FrameSet, error)
}

// Config 采集参数（fake 源与 cgo 驱动共用同一套几何约定，
// ffmpeg 侧的 rawvideo 声明必须与之完全一致）。
type Config struct {
	Width     int // 默认 640
	Height    int // 默认 480
	Framerate int // 默认 15
	// 深度伪彩色映射范围（毫米）。0 与超范围分别钳到两端；
	// 钳位值 0 表示无效点（太近/太远/遮挡），映射为近端色。
	DepthMinMM int // 默认 300
	DepthMaxMM int // 默认 8000
	// Serial 指定设备序列号；空取第一台（cgo 驱动用，fake 忽略）。
	Serial string
}

// WithDefaults 填充缺省值。
func (c Config) WithDefaults() Config {
	if c.Width <= 0 {
		c.Width = 640
	}
	if c.Height <= 0 {
		c.Height = 480
	}
	if c.Framerate <= 0 {
		c.Framerate = 15
	}
	if c.DepthMinMM <= 0 {
		c.DepthMinMM = 300
	}
	if c.DepthMaxMM <= c.DepthMinMM {
		c.DepthMaxMM = c.DepthMinMM + 7700
	}
	return c
}

// ErrNoDevice 未找到 RealSense 设备。
var ErrNoDevice = fmt.Errorf("no realsense device found")
