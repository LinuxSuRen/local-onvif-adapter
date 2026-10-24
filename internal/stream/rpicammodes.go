package stream

import (
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
	"github.com/linuxsuren/local-onvif-adapter/internal/onvifserver"
)

// rpicamModesRetryInterval 探测失败后的重试节流：CSI 传感器不会热插拔，
// 一次失败多为启动早期 libcamera 未就绪，短时间内不反复起进程。
const rpicamModesRetryInterval = time.Minute

var (
	rpicamModesMu    sync.Mutex
	rpicamModesCache map[int][]onvifserver.ResolutionOption
	rpicamModesFail  time.Time // 最近一次探测失败时刻（零值表示未失败过）
)

// RpiCamResolutions 返回 rpicam 摄像头传感器真实支持的分辨率模式
// （含各模式最大帧率），供 ONVIF GetVideoEncoderConfigurationOptions 下发。
// 非 rpicam 类型、rpicam-hello 不可用或解析失败时返回 nil，调用方回退静态列表。
func RpiCamResolutions(cam config.Camera) []onvifserver.ResolutionOption {
	if cam.Type != config.TypeRpicam {
		return nil
	}
	return rpicamModes(rpicamCameraIndex(cam))
}

// rpicamCameraIndex 取 libcamera 摄像头索引，语义与 rpicamVidArgs 的 --camera 一致。
func rpicamCameraIndex(c config.Camera) int {
	idx, err := strconv.Atoi(strings.TrimSpace(c.Source))
	if err != nil || idx < 0 {
		return 0
	}
	return idx
}

// rpicamModes 探测并缓存全部 CSI 摄像头的传感器模式（进程内一次）。
func rpicamModes(index int) []onvifserver.ResolutionOption {
	rpicamModesMu.Lock()
	defer rpicamModesMu.Unlock()
	if rpicamModesCache == nil {
		if !rpicamModesFail.IsZero() && time.Since(rpicamModesFail) < rpicamModesRetryInterval {
			return nil
		}
		out, err := exec.Command("rpicam-hello", "--list-cameras").CombinedOutput() //nolint:gosec // 固定工具名
		if err != nil {
			rpicamModesFail = time.Now()
			return nil
		}
		parsed := parseRpicamListCameras(string(out))
		if len(parsed) == 0 {
			rpicamModesFail = time.Now()
			return nil
		}
		rpicamModesCache = parsed
	}
	return rpicamModesCache[index]
}

var (
	// 相机头行：`0 : ov5647 [2592x1944 10-bit GBRG] (/base/soc/...)`。
	rpicamCamLine = regexp.MustCompile(`^(\d+)\s*:\s*\S+\s+\[\d+x\d+`)
	// 模式项：`640x480 [30.00 fps - (…) crop]`，同一行可出现多个。
	rpicamModeItem = regexp.MustCompile(`(\d+)x(\d+)\s+\[(\d+)(?:\.\d+)?\s+fps`)
)

// parseRpicamListCameras 解析 `rpicam-hello --list-cameras` 输出：
//
//	0 : ov5647 [2592x1944 10-bit GBRG] (/base/soc/i2c0mux/i2c@1/ov5647@36)
//	    Modes: 'SGBRG10_CSI2P' : 640x480 [30.00 fps - (65535, 65535)/65535x65535 crop]
//	                         1296x972 [30.00 fps - (65535, 65535)/65535x65535 crop]
//
// 同一分辨率可能以多种像素格式出现，去重时保留最大帧率。
func parseRpicamListCameras(out string) map[int][]onvifserver.ResolutionOption {
	modes := map[int][]onvifserver.ResolutionOption{}
	current := -1
	for _, line := range strings.Split(out, "\n") {
		if m := rpicamCamLine.FindStringSubmatch(line); m != nil {
			current, _ = strconv.Atoi(m[1])
			continue
		}
		if current < 0 {
			continue
		}
		for _, m := range rpicamModeItem.FindAllStringSubmatch(line, -1) {
			w, _ := strconv.Atoi(m[1])
			h, _ := strconv.Atoi(m[2])
			fps, _ := strconv.Atoi(m[3])
			if w <= 0 || h <= 0 || fps <= 0 {
				continue
			}
			modes[current] = upsertResolution(modes[current], w, h, fps)
		}
	}
	return modes
}

// upsertResolution 按 WxH 去重合并，保留最大帧率。
func upsertResolution(list []onvifserver.ResolutionOption, w, h, fps int) []onvifserver.ResolutionOption {
	for i, m := range list {
		if m.Width == w && m.Height == h {
			if fps > m.MaxFPS {
				list[i].MaxFPS = fps
			}
			return list
		}
	}
	return append(list, onvifserver.ResolutionOption{Width: w, Height: h, MaxFPS: fps})
}
