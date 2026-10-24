//go:build !windows

package stream

import "github.com/linuxsuren/local-onvif-adapter/internal/config"

// screenSourceBlocked 非 Windows 平台不预检（macOS 走 screencapture 管道、
// Linux x11grab 按显示偏移采集，拓扑变化场景以实际报错走常规退避重启）。
func screenSourceBlocked(cam config.Camera) (reason string, blocked bool) {
	return "", false
}
