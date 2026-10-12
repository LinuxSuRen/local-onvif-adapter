//go:build windows

package stream

import (
	"fmt"
	"syscall"
	"unsafe"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
	"golang.org/x/sys/windows"
)

// monitorInfo 与 Win32 MONITORENCUMPROC/GetMonitorInfoW 的结构对齐（显式布局，
// 不依赖 x/sys/windows 是否导出 MONITORINFO）。
type monitorInfo struct {
	cbSize    uint32
	rcMonitor winRect
	rcWork    winRect
	dwFlags   uint32
	szDevice  [32]uint16 // 显示器设备名，如 \\.\DISPLAY1
}

type winRect struct {
	left, top, right, bottom int32
}

const monitorInfoFPrimary = 1

// enumMonitorDevices 枚举实际连接的显示器，返回每屏在虚拟桌面中的矩形。
//
// 坐标体系说明：GetMonitorInfoW 返回的是当前进程 DPI 感知视角下的虚拟桌面
// 坐标。Go 程序与 ffmpeg 默认均非 DPI-aware，两者看到的坐标一致（混合缩放
// 的多屏布局下同为虚拟化坐标），因此偏移可直接传给 gdigrab；不要在本进程
// 单方面开启 DPI 感知，否则会与 ffmpeg 的视角错位。
func enumMonitorDevices() []Device {
	user32 := windows.NewLazySystemDLL("user32.dll")
	enumDisplayMonitors := user32.NewProc("EnumDisplayMonitors")
	getMonitorInfoW := user32.NewProc("GetMonitorInfoW")

	type rect struct {
		x, y, w, h int32
		primary    bool
	}
	var monitors []rect

	cb := syscall.NewCallback(func(hMonitor, _, _ uintptr) uintptr {
		var mi monitorInfo
		mi.cbSize = uint32(unsafe.Sizeof(mi))
		r1, _, _ := getMonitorInfoW.Call(hMonitor, uintptr(unsafe.Pointer(&mi)))
		if r1 == 0 {
			return 1 // 查询失败跳过该显示器，继续枚举
		}
		monitors = append(monitors, rect{
			x:       mi.rcMonitor.left,
			y:       mi.rcMonitor.top,
			w:       mi.rcMonitor.right - mi.rcMonitor.left,
			h:       mi.rcMonitor.bottom - mi.rcMonitor.top,
			primary: mi.dwFlags&monitorInfoFPrimary != 0,
		})
		return 1 // 继续枚举
	})
	_, _, _ = enumDisplayMonitors.Call(0, 0, cb, 0)

	var devices []Device
	for i, m := range monitors {
		if m.w <= 0 || m.h <= 0 {
			continue
		}
		name := fmt.Sprintf("显示器 %d（%d×%d）", i+1, m.w, m.h)
		if m.primary {
			name += "，主屏"
		}
		devices = append(devices, Device{
			Name:   name,
			Source: fmt.Sprintf("desktop@%d,%d,%dx%d", m.x, m.y, m.w, m.h),
			Type:   string(config.TypeScreen),
			Kind:   kindScreen,
		})
	}
	return devices
}
