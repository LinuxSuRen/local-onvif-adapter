package stream

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// quotedNameRe 匹配 dshow 设备列表中带引号的设备名行。
var quotedNameRe = regexp.MustCompile(`"(.+)"`)

// DShowVideoDevices 枚举本机 DirectShow 视频设备（仅 Windows）。
// 通过 `ffmpeg -list_devices true -f dshow -i dummy` 实现（列表输出在 stderr，
// 且 ffmpeg 以非零退出，属预期）。非 Windows 平台返回不支持错误。
func DShowVideoDevices(ffmpegBin string) ([]string, error) {
	if runtime.GOOS != "windows" {
		return nil, fmt.Errorf("device_enum_unsupported") // 由 API 层转换为友好提示
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	//nolint:gosec // 二进制路径来自受控配置
	cmd := exec.CommandContext(ctx, ffmpegBin, "-hide_banner", "-loglevel", "info",
		"-list_devices", "true", "-f", "dshow", "-i", "dummy")
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	_ = cmd.Run() // 列表输出在 stderr 且退出码非 0，忽略退出状态
	devices := parseDShowVideoDevices(errBuf.String())
	if len(devices) == 0 && ctx.Err() != nil {
		return nil, fmt.Errorf("enumerate dshow devices timeout")
	}
	return devices, nil
}

// parseDShowVideoDevices 从 ffmpeg -list_devices 的 stderr 输出中解析视频设备名。
// 输出形如：
//
//	[dshow @ ...] DirectShow video devices (some may be both video and audio devices)
//	[dshow @ ...]  "Integrated Camera"
//	[dshow @ ...]     Alternative name "@device_cm_{...}"
//	[dshow @ ...] DirectShow audio devices
//	[dshow @ ...]  "Microphone (...)"
func parseDShowVideoDevices(stderr string) []string {
	var devices []string
	section := ""
	for _, line := range strings.Split(stderr, "\n") {
		switch {
		case strings.Contains(line, "DirectShow video devices"):
			section = "video"
			continue
		case strings.Contains(line, "DirectShow audio devices"):
			section = "audio"
			continue
		}
		if section != "video" || strings.Contains(line, "Alternative name") {
			continue
		}
		if m := quotedNameRe.FindStringSubmatch(line); m != nil {
			devices = append(devices, m[1])
		}
	}
	return devices
}
