//go:build !windows

package stream

// enumMonitorDevices 非 Windows 平台无此枚举（macOS 走 avfoundation 原生
// 屏幕设备，Linux 由 xrandr 路径处理），返回空。
func enumMonitorDevices() []Device { return nil }
