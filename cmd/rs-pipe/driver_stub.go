//go:build !(realsense && cgo)

package main

import (
	"errors"

	"github.com/linuxsuren/local-onvif-adapter/internal/realsense"
)

// openDevice 默认构建（未打 realsense 标签）不支持真实设备。
// 提示信息包含两种出路：--fake 离线验证 / 带 SDK 重编。
func openDevice(cfg realsense.Config) (realsense.Source, error) {
	return nil, errors.New("this build has no librealsense support; use --fake for synthetic frames, " +
		"or rebuild with: CGO_ENABLED=1 go build -tags realsense ./cmd/rs-pipe (requires librealsense2 installed)")
}

// listDevices 默认构建无法枚举设备。
func listDevices() ([]deviceInfo, error) {
	return nil, errors.New("this build has no librealsense support; rebuild with -tags realsense to enumerate devices")
}

type deviceInfo struct {
	serial string
	name   string
}
