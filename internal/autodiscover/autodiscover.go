// Package autodiscover 启动时自动发现本机摄像头与显示器，把未配置的设备
// 自动创建为已启用的摄像头条目（issue #32）。
//
// 行为约定：
//   - 按 Type+Source（Windows 另比 AltSource/PnP 别名）匹配，已有条目不动——
//     用户的参数定制与手动禁用状态都被保留，不会重复添加；
//   - 摄像头类与屏幕类设备分别由 autoCameras / autoScreens 两个开关控制；
//   - 屏幕类跳过"整桌面聚合"项（Windows 的 desktop / Linux 的裸 :0.0），
//     只逐显示器添加，避免同一画面出现两路；
//   - 枚举失败（如无 ffmpeg）只告警不阻断启动；
//   - 数量上限与手动添加一致（16），到达上限停止补录。
package autodiscover

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
	"github.com/linuxsuren/local-onvif-adapter/internal/stream"
)

const maxCameras = 16

// rsPipeManagedRe 匹配由 rs-pipe 托管的深度相机家族设备名。
// 这类设备的整机会被 librealsense 独占：作为普通 dshow/v4l2 摄像头
// 自动添加必然与 rs-pipe 抢 USB（互斥失败），且 Depth 节点输出 raw16
// 无法直接采；统一走管理台"深度相机"一键添加流程。
var rsPipeManagedRe = regexp.MustCompile(`(?i)realsense`)

// listFn 设备枚举函数（测试注入用）。
type listFn func(ffmpegBin string) ([]stream.Device, error)

// Discover 枚举本机设备并补全配置；返回新增的摄像头条目。
func Discover(store *config.Store, ffmpegBin string, autoCameras, autoScreens bool, logger *slog.Logger) []config.Camera {
	return discover(store, ffmpegBin, autoCameras, autoScreens, logger, stream.ListLocalVideoDevices)
}

func discover(store *config.Store, ffmpegBin string, autoCameras, autoScreens bool, logger *slog.Logger, list listFn) []config.Camera {
	if logger == nil {
		logger = slog.Default()
	}
	if !autoCameras && !autoScreens {
		return nil
	}
	devices, err := list(ffmpegBin)
	if err != nil {
		// 枚举机制失败（如 ffmpeg 缺失）不阻断启动：已有配置照常工作。
		logger.Warn("auto discover: enumerate devices failed", "err", err.Error())
		return nil
	}

	var added []config.Camera
	err = store.Update(func(root *config.Root) error {
		for _, d := range devices {
			isScreen := d.Kind == "screen"
			if isScreen && !autoScreens || !isScreen && !autoCameras {
				continue
			}
			// 屏幕聚合项跳过：只逐显示器添加，否则整桌面与分屏内容重复。
			if isScreen && isAggregateScreenSource(d.Source) {
				continue
			}
			// RealSense 深度相机跳过：整机归 rs-pipe 独占托管，
			// 由管理台"深度相机"流程统一接入（真机 D435 实测枚举名含 RealSense）。
			if rsPipeManagedRe.MatchString(d.Name) {
				continue
			}
			if cameraExists(root.Cameras, d) {
				continue
			}
			if len(root.Cameras) >= maxCameras {
				logger.Warn("auto discover: camera limit reached, skip remaining devices", "limit", maxCameras)
				return nil
			}
			// Windows 同型号多台摄像头同名：优先用 PnP 别名精确定位物理设备。
			src := d.Source
			if d.AltSource != "" {
				src = d.AltSource
			}
			id := fmt.Sprintf("cam%d", root.NextID)
			root.NextID++
			cam := config.Camera{
				ID:      id,
				Name:    strings.TrimSpace(d.Name),
				Type:    config.CameraType(d.Type),
				Source:  strings.TrimSpace(src),
				Enabled: true,
			}
			root.Cameras = append(root.Cameras, cam)
			added = append(added, cam)
		}
		return nil
	})
	if err != nil {
		logger.Warn("auto discover: save config failed", "err", err.Error())
		return nil
	}
	for _, c := range added {
		logger.Info("auto discover: camera added", "id", c.ID, "name", c.Name, "type", c.Type, "source", c.Source)
	}
	if len(added) == 0 {
		logger.Info("auto discover: no new devices", "devices", len(devices))
	}
	return added
}

// cameraExists 判断设备是否已有对应摄像头条目（同类型，源或 PnP 别名一致）。
func cameraExists(cameras []config.Camera, d stream.Device) bool {
	for _, c := range cameras {
		if c.Type != config.CameraType(d.Type) {
			continue
		}
		if c.Source == d.Source || (d.AltSource != "" && c.Source == d.AltSource) {
			return true
		}
	}
	return false
}

// isAggregateScreenSource 屏幕聚合源：Windows 整虚拟桌面（desktop）、
// Linux 整 X display（:0.0，无偏移/尺寸后缀）。
func isAggregateScreenSource(src string) bool {
	return src == "desktop" || src == ":0.0"
}
