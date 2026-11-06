// 宇树 A2 前摄取帧泵：本机 spawn python3 子进程，经 Unitree SDK2（DDS）
// 轮询 videohub 服务的 GetImageSample，把 JPEG 帧连续写入 stdout（ffmpeg
// image2pipe 兼容格式），由本进程接到 ffmpeg 的 stdin 完成转码推流。
//
// 背景：A2 的摄像头不出 RTSP——机载 mediamtx 平台栈默认无人推流，原生
// camera.service 亦可能因固件目录缺失处于崩溃循环；而 SDK2 videohub 服务
// 常驻主控 DDS 域（机载 eth0），GetImageSample 单次 ~10ms 出 1920×1080
// JPEG。adapter 与机器人同机部署（本类型的预期形态），直接子进程取帧，
// 零网络开销、零部署。
package stream

import (
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// unitreePumpIface 机载 DDS 域网卡：A2 出厂固定 eth0（192.168.123.x）。
const unitreePumpIface = "eth0"

// unitreePumpScript 取帧脚本本体，经子进程 stdin 现场投递（零落盘），
// 以 `python3 -u - <fps> <iface>` 运行。设计要点：
//   - unitree_sdk2py 按 OTA 版本号 glob 探测，固件升级后路径变化仍可用；
//   - 节拍按 fps 目标间隔循环，落后过多时重置基准（不追帧堆积）；
//   - 连续失败 10 次退出非零码——由 Runner 的退避重启机制接管，
//     避免 SDK 异常时无限空转刷日志。
const unitreePumpScript = `import glob, sys, time

fps = float(sys.argv[1]) if len(sys.argv) > 1 else 10.0
iface = sys.argv[2] if len(sys.argv) > 2 else "eth0"

candidates = sorted(glob.glob(
    "/unitree/ota/backup/module/slam_nav/*/module/slam_nav/file/home/unitree/graph_pid_ws/bin/tools/py-planner"))
if not candidates:
    sys.stderr.write("unitree_sdk2py not found on robot\n")
    sys.exit(1)
sys.path.insert(0, candidates[-1])

from unitree_sdk2py.core.channel import ChannelFactoryInitialize
from unitree_sdk2py.go2.video.video_client import VideoClient

ChannelFactoryInitialize(0, iface)
client = VideoClient()
client.SetTimeout(2.0)
client.Init()

interval = 1.0 / max(fps, 0.1)
failures = 0
next_at = time.monotonic()
while True:
    code, data = client.GetImageSample()
    if code == 0 and data:
        sys.stdout.buffer.write(bytes(data))
        sys.stdout.buffer.flush()
        failures = 0
    else:
        failures += 1
        sys.stderr.write("GetImageSample code=%s failures=%d\n" % (code, failures))
        if failures >= 10:
            sys.exit(2)
        time.sleep(min(failures * 0.2, 1.0))
        continue
    next_at += interval
    delay = next_at - time.monotonic()
    if delay > 0:
        time.sleep(delay)
    else:
        next_at = time.monotonic()
`

// IsUnitreeLocal 源是否合法：本类型只在本机取帧，源留空或 "local"。
// 其余取值（历史 SSH 远程地址等）不再支持，创建时由 API 层拒绝。
func IsUnitreeLocal(source string) bool {
	s := strings.TrimSpace(source)
	if s == "" {
		return true
	}
	return strings.EqualFold(s, "local")
}

// UnitreePump 取帧泵句柄：python3 子进程，stdout 持续产出 JPEG 帧流。
// 泵结束后调用方应关闭管道写端让下游 ffmpeg 读到 EOF 并退出，
// 交由上层监督器重启整条链路。
type UnitreePump struct {
	cmd *exec.Cmd
}

// StartUnitreePump 启动本机取帧泵：spawn python3（脚本经 stdin 投递），
// frames 接收 JPEG 帧流（建议为管道写端）；diag 接收脚本诊断输出（可为 nil）。
func StartUnitreePump(camfps int, frames io.Writer, diag io.Writer) (*UnitreePump, error) {
	cmd := exec.Command("python3", "-u", "-", strconv.Itoa(camfps), unitreePumpIface) //nolint:gosec // 固定 python3 + 受控参数
	cmd.Stdout = frames
	if diag != nil {
		cmd.Stderr = diag
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin 管道: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 python3 取帧泵（确认机器人自带 python3）: %w", err)
	}
	if _, err := io.WriteString(stdin, unitreePumpScript); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("投递取帧脚本: %w", err)
	}
	_ = stdin.Close() // 脚本不读后续 stdin
	return &UnitreePump{cmd: cmd}, nil
}

// Wait 阻塞等待泵退出，返回其退出错误。进程退出后由调用方关闭管道写端。
func (p *UnitreePump) Wait() error { return p.cmd.Wait() }

// Close 终止泵（幂等）：Kill 子进程，尸体由 Wait 回收。
func (p *UnitreePump) Close() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}
