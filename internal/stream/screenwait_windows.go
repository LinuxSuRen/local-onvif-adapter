//go:build windows

package stream

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
	"golang.org/x/sys/windows"
)

// SM_* 索引：GetSystemMetrics 查询虚拟桌面（全部显示器的并集）矩形。
const (
	smXVirtualScreen = 76
	smYVirtualScreen = 77
	smCxVirtualScreen = 78
	smCyVirtualScreen = 79
)

// virtualDesktopRect 返回当前虚拟桌面矩形（虚拟桌面坐标，副屏可产生负坐标）。
// 与 screens_windows.go 的坐标视角约定一致：本进程与 ffmpeg 均非 DPI-aware，
// 看到的是同一套虚拟化坐标。
func virtualDesktopRect() (x, y, w, h int32) {
	user32 := windows.NewLazySystemDLL("user32.dll")
	getSystemMetrics := user32.NewProc("GetSystemMetrics")
	val := func(index int) int32 {
		v, _, _ := getSystemMetrics.Call(uintptr(index))
		return int32(v)
	}
	return val(smXVirtualScreen), val(smYVirtualScreen), val(smCxVirtualScreen), val(smCyVirtualScreen)
}

// screenSourceBlocked 预检屏幕源在当前显示拓扑下是否可采。
// desktop@X,Y,WxH 的矩形必须完整落在当前虚拟桌面内，否则 gdigrab 打开输入
// 即失败（"Capture area ... extends outside window area"），监督循环陷入
// 秒退重启风暴。整桌面（desktop）/窗口标题（title=...）源随拓扑自适应，不预检。
// 拓扑变化的典型触发：RDP 接管（物理屏被剥离，只剩客户端单屏）、扩展坞/显示器
// 断开、开机自启早于多屏布局就绪。
func screenSourceBlocked(cam config.Camera) (reason string, blocked bool) {
	m := monitorRectRe.FindStringSubmatch(strings.TrimSpace(cam.Source))
	if m == nil {
		return "", false
	}
	x, _ := strconv.Atoi(m[1])
	y, _ := strconv.Atoi(m[2])
	w, _ := strconv.Atoi(m[3])
	h, _ := strconv.Atoi(m[4])
	vx, vy, vw, vh := virtualDesktopRect()
	if rectWithin(int32(x), int32(y), int32(w), int32(h), vx, vy, vw, vh) {
		return "", false
	}
	return fmt.Sprintf("采集区域 (%d,%d) %dx%d 超出当前虚拟桌面 (%d,%d)-(%d,%d)，显示器布局已变化（RDP 接管/扩展坞断开/分辨率切换），等待恢复",
		x, y, w, h, vx, vy, vx+vw, vy+vh), true
}
