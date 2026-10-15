// Package stream 中的视频编码器选择：
// 默认 libx264；h264_v4l2m2m（V4L2 硬件编码，树莓派等）可经
// --video-encoder / VIDEO_ENCODER 显式启用（实验性，见 videoEncodeArgs
// 处的实测说明）。
package stream

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
)

// 编码器模式。
const (
	EncoderAuto    = "auto"         // 探测硬件，不可用则 libx264
	EncoderX264    = "libx264"      // 强制软编码
	EncoderV4L2M2M = "h264_v4l2m2m" // 强制 V4L2 硬件编码
)

var (
	encMu   sync.Mutex
	encMode = EncoderAuto
	encOnce sync.Once
	encName string
	encNote string
)

// SetVideoEncoder 设置编码器选择模式（main 按 flag/env 注入，进程级生效）。
func SetVideoEncoder(mode string) error {
	switch mode {
	case EncoderAuto, EncoderX264, EncoderV4L2M2M:
	default:
		return fmt.Errorf("不支持的编码器 %q（可选 auto|libx264|h264_v4l2m2m）", mode)
	}
	encMu.Lock()
	defer encMu.Unlock()
	if encMode != mode {
		encMode = mode
		encOnce = sync.Once{} // 模式变化后重新解析
	}
	return nil
}

// resolveEncoder 返回生效的编码器名与选择说明，每进程只解析一次。
func resolveEncoder(bin string, logger *slog.Logger) string {
	encOnce.Do(func() {
		mode := func() string {
			encMu.Lock()
			defer encMu.Unlock()
			return encMode
		}()
		encName, encNote = EncoderX264, "safe default"
		switch mode {
		case EncoderV4L2M2M:
			encName, encNote = EncoderV4L2M2M, "forced (experimental)"
		case EncoderAuto, EncoderX264:
			// libx264：见下方实测说明。
		}
		if logger != nil {
			logger.Info("video encoder selected", "encoder", encName, "mode", encNote)
		}
	})
	return encName
}

// videoEncodeArgs 按编码器构造视频编码参数。
// v4l2m2m 不支持 x264 专属的 -preset/-tune，参数按编码器分派。
//
// 实测说明（为什么 auto 不启用硬件编码）：树莓派 3B（Raspbian trixie /
// ffmpeg 7.1 / bcm2835-codec）上 h264_v4l2m2m 本地编码正常，但经 RTSP
// 分发后中途加入的读者全部报 non-existing PPS——bcm2835 编码器不随 GOP
// 周期重复 SPS/PPS；dump_extra=freq=keyframe、去掉 -g 等变体均无效。
// 硬件编码仅保留显式 opt-in（实验性），待驱动/上游修复后再评估默认启用。

// videoEncodeArgs 按编码器构造视频编码参数。
// v4l2m2m 不支持 x264 专属的 -preset/-tune，参数按编码器分派。
func videoEncodeArgs(encoder string, gov int, bitrateKbps int) []string {
	if encoder == EncoderV4L2M2M {
		return []string{
			"-c:v", EncoderV4L2M2M,
			"-pix_fmt", "yuv420p",
			"-g", fmt.Sprintf("%d", gov),
			"-b:v", fmt.Sprintf("%dk", bitrateKbps),
		}
	}
	return []string{
		"-c:v", EncoderX264,
		"-preset", "veryfast",
		"-tune", "zerolatency",
		"-pix_fmt", "yuv420p",
		"-g", fmt.Sprintf("%d", gov),
		"-b:v", fmt.Sprintf("%dk", bitrateKbps),
	}
}

// fpsGuaranteeArgs 输出侧帧率兜底：设备采集源（v4l2/dshow/avfoundation）的
// -framerate 请求可能被驱动静默拒绝（实测配置 10fps 回退到 30fps，白烧三倍
// 编码量）。输出侧 -r 强制对齐生效帧率：高于配置抽帧、低于配置补帧，
// 「配置即生效」。合成源（testsrc 自带 rate）与 rtsp 源（按源节奏转码）不强制。
func fpsGuaranteeArgs(c config.Camera) []string {
	switch c.Type {
	case config.TypeV4L2, config.TypeDShow, config.TypeAVFoundation:
		return []string{"-r", fmt.Sprintf("%d", c.FramerateOrDefault())}
	}
	return nil
}
