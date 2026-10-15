// Package stream 管理每个摄像头的 ffmpeg 取流进程：
// 拉取本地源（v4l2/avfoundation/dshow/rtsp/testsrc）转码 H264 后推送到内嵌 RTSP 服务器。
package stream

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
	"github.com/linuxsuren/local-onvif-adapter/internal/onvifserver"
)

// Status 单个摄像头取流进程的运行状态。
type Status struct {
	Running   bool      `json:"running"`
	LastError string    `json:"last_error"`
	StartedAt time.Time `json:"started_at"`
	Restarts  int       `json:"restarts"`
}

// Runner 单个摄像头的 ffmpeg 进程监督器。
type Runner struct {
	cam      config.Camera
	pushAddr string
	bin      string
	logger   *slog.Logger

	mu      sync.Mutex
	status  Status
	spec    string
	stopCh  chan struct{}
	stopped bool
	tailErr string
	cmd     *exec.Cmd // 当前 ffmpeg 进程；Stop 时强制 Kill 防设备泄漏
}

// Manager 管理全部取流进程。
type Manager struct {
	mu       sync.Mutex
	runners  map[string]*Runner
	pushBase func() string // 每次取流实时求值（认证开关变化时注入/移除 userinfo）
	lastAddr string        // 上次使用的推流地址；变化即全部重启
	bin      string
	logger   *slog.Logger
}

// NewManager 创建取流管理器。pushBase 返回推流目标（形如 rtsp://127.0.0.1:8554，
// 认证启用时应携带 userinfo，见 main.go）。
func NewManager(pushBase func() string, bin string, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		runners:  map[string]*Runner{},
		pushBase: pushBase,
		bin:      bin,
		logger:   logger,
	}
}

// Spec 描述摄像头取流的关键参数，任一变化即需要重启进程。
// 音频开关/音频源也必须纳入指纹：否则改音频配置后旧进程继续跑（实测踩坑：
// 开启音频后旧的纯视频 ffmpeg 不重启，流里永远没有音轨）。
func Spec(c config.Camera) string {
	return fmt.Sprintf("%s|%s|%d|%d|%d|%d|%t|%s",
		c.Type, c.Source, c.Width, c.Height, c.FramerateOrDefault(), c.BitrateOrDefault(),
		c.AudioEnabled, c.AudioSource)
}

// Sync 将运行态对齐到期望的摄像头列表：
// 新增的启动、删除/禁用的停止、参数变化的重启。
func (m *Manager) Sync(cameras []config.Camera) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 推流地址变化（认证开关切换注入/移除 userinfo）：停掉全部进程，
	// 按新地址重建，避免旧进程以失效凭证反复 401。
	addr := m.pushBase()
	if addr != m.lastAddr {
		for id, r := range m.runners {
			r.Stop()
			delete(m.runners, id)
		}
		m.lastAddr = addr
	}

	want := map[string]config.Camera{}
	for _, c := range cameras {
		if c.Enabled {
			want[c.ID] = c
		}
	}
	// 停掉不再需要的。
	for id, r := range m.runners {
		if _, ok := want[id]; !ok {
			r.Stop()
			delete(m.runners, id)
			continue
		}
		if r.Spec() != Spec(want[id]) {
			r.Stop()
			delete(m.runners, id)
		}
	}
	// 启动/保留需要的。
	for id, c := range want {
		if _, ok := m.runners[id]; ok {
			continue
		}
		r := newRunner(c, m.pushBase(), m.bin, m.logger)
		m.runners[id] = r
		go r.Run()
	}
}

// StopAll 停止全部进程。
func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, r := range m.runners {
		r.Stop()
		delete(m.runners, id)
	}
}

// Status 返回指定摄像头的取流状态。
func (m *Manager) Status(camID string) (Status, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runners[camID]
	if !ok {
		return Status{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status, true
}

func newRunner(cam config.Camera, pushAddr, bin string, logger *slog.Logger) *Runner {
	return &Runner{
		cam:      cam,
		pushAddr: pushAddr,
		bin:      bin,
		logger:   logger,
		stopCh:   make(chan struct{}),
		spec:     Spec(cam),
	}
}

// Spec 返回创建时的参数指纹。
func (r *Runner) Spec() string { return r.spec }

// Stop 请求停止（幂等）：置位停止标志并**强制结束当前 ffmpeg 进程**。
// ffmpeg 推流期间不会自然退出，Run 阻塞在 cmd.Run() 上——不主动 Kill 的话
// 旧进程会一直占着采集设备（dshow/v4l2 独占），spec 变化/禁用/删除后的
// 新进程将永远打不开设备（实测踩坑：改音频配置后旧进程泄漏，新进程循环
// "device already in use"）。
func (r *Runner) Stop() {
	r.mu.Lock()
	cmd := r.cmd
	r.cmd = nil
	if !r.stopped {
		r.stopped = true
		close(r.stopCh)
	}
	r.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func (r *Runner) isStopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

// Run 阻塞监督 ffmpeg 进程，异常退出后指数退避重启。
func (r *Runner) Run() {
	backoff := time.Second
	const maxBackoff = 15 * time.Second
	var starts int
	for !r.isStopped() {
		args := buildArgs(r.cam, r.pushAddr+r.camPath(), r.bin, r.logger)
		cmd := exec.Command(r.bin, args...) //nolint:gosec // 二进制路径来自受控配置
		var stderr tailBuffer
		cmd.Stderr = &stderr

		// macOS 屏幕采集：启动 screencapture 循环子进程，其 stdout 接到 ffmpeg 的 stdin。
		// 用 os.Pipe() 手动建管道；ffmpeg 退出后（cmd.Wait 返回）再关写端并 kill shell。
		if r.cam.Type == config.TypeScreen && runtime.GOOS == "darwin" {
			pr, pw, pipeErr := os.Pipe()
			if pipeErr == nil {
				shell := exec.Command("/bin/sh", "-c", darwinScreenCmd(r.cam))
				shell.Stdout = pw
				if startErr := shell.Start(); startErr == nil {
					cmd.Stdin = pr
					defer func() {
						_ = pw.Close()
						_ = pr.Close()
						if shell.Process != nil {
							_ = shell.Process.Kill()
						}
					}()
				} else {
					_ = pr.Close()
					_ = pw.Close()
				}
			}
		}

		// 树莓派 CSI 摄像头：rpicam-vid（libcamera + 硬件 H264）写 stdout，
		// 接到 ffmpeg 的 stdin；ffmpeg 仅 -c copy 直通封装（零转码）。
		if r.cam.Type == config.TypeRpicam {
			pr, pw, pipeErr := os.Pipe()
			if pipeErr == nil {
				rpicam := exec.Command("rpicam-vid", rpicamVidArgs(r.cam)...) //nolint:gosec // 固定工具名
				rpicam.Stdout = pw
				var rpErr tailBuffer
				rpicam.Stderr = &rpErr
				if startErr := rpicam.Start(); startErr == nil {
					cmd.Stdin = pr
					defer func() {
						_ = pw.Close()
						_ = pr.Close()
						if rpicam.Process != nil {
							_ = rpicam.Process.Kill()
						}
					}()
				} else {
					_ = pr.Close()
					_ = pw.Close()
					r.logger.Warn("rpicam-vid start failed（仅树莓派可用）",
						"camera", r.cam.ID, "err", startErr.Error())
				}
			}
		}

		r.logger.Debug("ffmpeg starting", "camera", r.cam.ID, "args", args)
		// 先 Start 再发布 r.cmd：cmd.Process 字段由 Start 写入，若在 Run() 之前
		// 发布，Stop() 在锁外读 cmd.Process 会与 Start 的写构成数据竞争
		//（go test -race 实测告警）。Start/Wait 分离后，Stop 只可能看到
		// 「已启动的进程」或 nil；Kill 与 Wait 并发是 exec.CommandContext
		// 的同款安全用法。
		if startErr := cmd.Start(); startErr != nil {
			r.setRunning(false, "ffmpeg start: "+startErr.Error(), &starts)
			r.logger.Warn("ffmpeg start failed", "camera", r.cam.ID, "err", startErr.Error())
			select {
			case <-r.stopCh:
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		r.mu.Lock()
		r.cmd = cmd
		r.mu.Unlock()
		r.setRunning(true, "", &starts)
		// 事件：进程启动 = 信号恢复（重启后的第一拍）；Source/Lifecycle 记录启动序号
		onvifserver.PublishEvent("tns1:VideoSource/SignalLoss",
			[]onvifserver.EventItem{{Name: "Source", Value: "Camera_" + r.cam.ID}},
			[]onvifserver.EventItem{{Name: "State", Value: "false"}})
		onvifserver.PublishEvent("lns:Source/Lifecycle",
			[]onvifserver.EventItem{{Name: "Camera", Value: r.cam.ID}},
			[]onvifserver.EventItem{{Name: "Phase", Value: "Started"}, {Name: "Restarts", Value: fmt.Sprintf("%d", starts-1)}})
		started := time.Now()
		err := cmd.Wait()
		// 进程已结束：摘除句柄，Stop 不再 Kill 已回收的进程
		//（Kill 已结束进程会返回 ErrProcessDone，无害但没必要）。
		r.mu.Lock()
		if r.cmd == cmd {
			r.cmd = nil
		}
		r.mu.Unlock()
		if r.isStopped() {
			r.setRunning(false, "", &starts)
			onvifserver.PublishEvent("lns:Source/Lifecycle",
				[]onvifserver.EventItem{{Name: "Camera", Value: r.cam.ID}},
				[]onvifserver.EventItem{{Name: "Phase", Value: "Stopped"}})
			return
		}
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if tail := stderr.Tail(); tail != "" {
			msg = msg + "; ffmpeg: " + tail
		}
		r.setRunning(false, msg, &starts)
		r.logger.Warn("ffmpeg exited, restarting", "camera", r.cam.ID, "err", msg,
			"uptime", time.Since(started).Round(time.Second).String())
		// 事件：非预期退出 = 信号丢失（NVR 区分「相机挂了」与「网络挂了」）+ 重启中
		onvifserver.PublishEvent("tns1:VideoSource/SignalLoss",
			[]onvifserver.EventItem{{Name: "Source", Value: "Camera_" + r.cam.ID}},
			[]onvifserver.EventItem{{Name: "State", Value: "true"}})
		onvifserver.PublishEvent("lns:Source/Lifecycle",
			[]onvifserver.EventItem{{Name: "Camera", Value: r.cam.ID}},
			[]onvifserver.EventItem{{Name: "Phase", Value: "Restarting"},
				{Name: "Restarts", Value: fmt.Sprintf("%d", starts-1)},
				{Name: "Error", Value: trimEvent(msg, 120)}})
		select {
		case <-r.stopCh:
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (r *Runner) setRunning(v bool, errMsg string, starts *int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.Running = v
	r.status.LastError = errMsg
	if v {
		*starts++
		r.status.StartedAt = time.Now()
		r.status.Restarts = *starts - 1
	}
}

// tailBuffer 有界地记录 ffmpeg stderr，只保留末尾片段用于排障。
type tailBuffer struct {
	buf [4096]byte
	n   int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	if len(p) >= len(t.buf) {
		copy(t.buf[:], p[len(p)-len(t.buf):])
		t.n = len(t.buf)
		return len(p), nil
	}
	room := len(t.buf) - t.n
	if room < len(p) {
		copy(t.buf[:], t.buf[len(p)-room:])
		t.n = len(t.buf) - len(p)
	}
	copy(t.buf[t.n:], p)
	t.n += len(p)
	return len(p), nil
}

func (t *tailBuffer) Tail() string {
	return string(t.buf[:t.n])
}

func (r *Runner) camPath() string { return "/cam/" + r.cam.ID }

// trimEvent 截断错误文本用于事件载荷（stderr 尾巴可能很长，事件只要可读线索）。
func trimEvent(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = s[len(s)-n:]
		if i := strings.Index(s, " "); i > 0 {
			s = s[i+1:]
		}
	}
	return s
}

// InputArgs 按摄像头类型构造 ffmpeg 输入参数。
// 启用音频时按平台把麦克风并入输入（dshow 联合输入 / avfoundation 双索引 /
// v4l2+pulse 双输入；rtsp 源直接读源流音轨，无需改动输入）。
func InputArgs(c config.Camera) []string {
	fps := c.FramerateOrDefault()
	var args []string
	switch c.Type {
	case config.TypeV4L2:
		args = append(args, "-f", "v4l2")
		if fps > 0 {
			args = append(args, "-framerate", fmt.Sprintf("%d", fps))
		}
		if c.Width > 0 && c.Height > 0 {
			args = append(args, "-video_size", fmt.Sprintf("%dx%d", c.Width, c.Height))
		}
		args = append(args, "-i", c.Source)
		if c.AudioEnabled {
			// Linux 视频与音频是两个输入：v4l2 设备 + pulse 源（默认 default）。
			src := strings.TrimSpace(c.AudioSource)
			if src == "" {
				src = "default"
			}
			args = append(args, "-f", "pulse", "-i", src)
		}
	case config.TypeAVFoundation:
		args = append(args, "-f", "avfoundation")
		if fps > 0 {
			args = append(args, "-framerate", fmt.Sprintf("%d", fps))
		}
		src := c.Source
		if src == "" {
			src = "0"
		}
		if c.AudioEnabled && strings.TrimSpace(c.AudioSource) != "" {
			// avfoundation 原生联合输入："视频索引:音频索引"。
			src = src + ":" + strings.TrimSpace(c.AudioSource)
		}
		args = append(args, "-i", src)
	case config.TypeDShow:
		args = append(args, "-f", "dshow")
		if fps > 0 {
			args = append(args, "-framerate", fmt.Sprintf("%d", fps))
		}
		if c.Width > 0 && c.Height > 0 {
			args = append(args, "-video_size", fmt.Sprintf("%dx%d", c.Width, c.Height))
		}
		src := strings.TrimSpace(c.Source)
		// 允许直接填设备名；已带 video= 前缀或别名（@device_...）时原样使用。
		if src != "" && !strings.Contains(src, "=") {
			src = "video=" + src
		}
		if c.AudioEnabled && strings.TrimSpace(c.AudioSource) != "" {
			// dshow 联合输入：video=..:audio=..（音频名含冒号/等号时允许 audio= 前缀原样）。
			audio := strings.TrimSpace(c.AudioSource)
			if !strings.HasPrefix(audio, "audio=") {
				audio = "audio=" + audio
			}
			src = src + ":" + audio
		}
		args = append(args, "-i", src)
	case config.TypeScreen:
		args = append(args, screenInputArgs(c)...)
	case config.TypeRTSP:
		args = append(args, "-rtsp_transport", "tcp", "-i", c.Source)
	case config.TypeRpicam:
		// rpicam-vid（libcamera）经 stdin 管道喂 H264 裸流，见 rpicamVidArgs。
		args = append(args, "-f", "h264", "-i", "pipe:")
	case config.TypeTestSrc:
		w, h := 1280, 720
		if c.Width > 0 && c.Height > 0 {
			w, h = c.Width, c.Height
		}
		// lavfi 合成源没有固有节奏，必须 -re 按帧率实时读取，
		// 否则 ffmpeg 会全速推流（实测 ~32 倍速），拖垮整条转发链路
		args = append(args, "-re", "-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=%dx%d:rate=%d", w, h, fps))
	default:
		args = append(args, "-i", c.Source)
	}
	return args
}

// screenInputArgs 按平台构造屏幕采集的 ffmpeg 输入参数：
//   - macOS：用包装脚本持续 screencapture → ffmpeg image2pipe（绕开 avfoundation 权限问题）
//   - Linux：source 为 X display（":0.0" 全屏或 ":0.0+X,Y@WxH" 指定显示器）
//   - Windows：source 为 desktop（全虚拟桌面）、desktop@X,Y,WxH（指定显示器）
//     或 title=窗口名
func screenInputArgs(c config.Camera) []string {
	return screenInputArgsFor(runtime.GOOS, c)
}

// monitorRectRe 匹配 Windows 显示器矩形 source："desktop@X,Y,WxH"
// （X/Y 为虚拟桌面坐标，可为负；W/H 为该显示器尺寸）。
var monitorRectRe = regexp.MustCompile(`^desktop@(-?\d+),(-?\d+),(\d+)x(\d+)$`)

// x11ScreenRectRe 匹配 Linux 显示器 source：":0.0+X,Y@WxH"
// （x11grab 原生支持 display 偏移 +X,Y；@WxH 为本程序附加的期望尺寸）。
var x11ScreenRectRe = regexp.MustCompile(`^(:[0-9.]+(?:\+\d+,\d+)?)@(\d+)x(\d+)$`)

// screenInputArgsFor 按指定 GOOS 构造参数（goos 参数仅为可测试性）。
func screenInputArgsFor(goos string, c config.Camera) []string {
	if goos == "darwin" {
		return darwinScreenArgs(c)
	}
	fps := c.FramerateOrDefault()
	var args []string
	switch goos {
	case "windows":
		args = append(args, "-f", "gdigrab")
		if fps > 0 {
			args = append(args, "-framerate", fmt.Sprintf("%d", fps))
		}
		src := strings.TrimSpace(c.Source)
		if src == "" {
			src = "desktop"
		}
		// 指定显示器：desktop@X,Y,WxH → 虚拟桌面坐标偏移 + 按显示器尺寸裁剪。
		// 用户显式配置了分辨率时以用户值为准（在显示器内裁剪）。
		if m := monitorRectRe.FindStringSubmatch(src); m != nil {
			args = append(args, "-offset_x", m[1], "-offset_y", m[2])
			if c.Width <= 0 || c.Height <= 0 {
				args = append(args, "-video_size", m[3]+"x"+m[4])
			} else {
				args = append(args, "-video_size", fmt.Sprintf("%dx%d", c.Width, c.Height))
			}
			src = "desktop"
		} else if c.Width > 0 && c.Height > 0 {
			args = append(args, "-video_size", fmt.Sprintf("%dx%d", c.Width, c.Height))
		}
		args = append(args, "-i", src)
	default: // linux（x11grab）
		args = append(args, "-f", "x11grab")
		if fps > 0 {
			args = append(args, "-framerate", fmt.Sprintf("%d", fps))
		}
		src := strings.TrimSpace(c.Source)
		if src == "" {
			src = ":0.0"
		}
		// 指定显示器：":0.0+X,Y@WxH" → display 偏移采集，尺寸默认取显示器矩形。
		if m := x11ScreenRectRe.FindStringSubmatch(src); m != nil {
			src = m[1]
			if c.Width <= 0 || c.Height <= 0 {
				args = append(args, "-video_size", m[2]+"x"+m[3])
			} else {
				args = append(args, "-video_size", fmt.Sprintf("%dx%d", c.Width, c.Height))
			}
		} else if c.Width > 0 && c.Height > 0 {
			args = append(args, "-video_size", fmt.Sprintf("%dx%d", c.Width, c.Height))
		}
		args = append(args, "-i", src)
	}
	return args
}

// darwinScreenArgs macOS 屏幕采集：通过 /bin/sh -c 包装的 screencapture 循环
// 截图到管道，ffmpeg 以 image2pipe 读取。
// 绕开未签名 ffmpeg 的 avfoundation 屏幕采集权限限制（macOS 对未签名
// 二进制不弹权限框，avfoundation 会挂起或返回灰色帧）。
// screencapture 是系统工具自带屏幕录制权限。
func darwinScreenArgs(c config.Camera) []string {
	fps := c.FramerateOrDefault()
	if fps <= 0 {
		fps = 5
	}
	return []string{"-f", "image2pipe", "-framerate", fmt.Sprintf("%d", fps), "-i", "pipe:"}
}

// darwinScreenCmd 返回 macOS 屏幕采集的 shell 包装命令。
// runner 在 darwin 平台 screen 类型时用此命令作为 ffmpeg 的 stdin 来源。
func darwinScreenCmd(c config.Camera) string {
	fps := c.FramerateOrDefault()
	if fps <= 0 {
		fps = 5
	}
	return fmt.Sprintf(
		"while true; do screencapture -x -t png /tmp/_onvif_sc_tmp.png 2>/dev/null; cat /tmp/_onvif_sc_tmp.png; rm -f /tmp/_onvif_sc_tmp.png; sleep %.3f; done",
		1.0/float64(fps),
	)
}

// BuildArgs 构造完整的 ffmpeg 取流推流命令参数（编码器按默认 auto 解析，
// 仅供测试与兼容旧调用；运行路径请用 buildArgs 传入实际 bin 与 logger）。
func BuildArgs(c config.Camera, pushURL string) []string {
	return buildArgs(c, pushURL, "ffmpeg", nil)
}

// buildArgs 构造完整的 ffmpeg 取流推流命令参数（含编码器选择与帧率兜底）。
func buildArgs(c config.Camera, pushURL, bin string, logger *slog.Logger) []string {
	fps := c.FramerateOrDefault()
	gov := fps * 2
	if gov < 10 {
		gov = 10
	}
	args := []string{"-hide_banner", "-loglevel", "error"}
	// macOS 屏幕源与 rpicam 源需要从 stdin 管道读帧，不能加 -nostdin。
	if !(c.Type == config.TypeScreen && runtime.GOOS == "darwin") && c.Type != config.TypeRpicam {
		args = append(args, "-nostdin")
	}
	args = append(args, InputArgs(c)...)
	if c.Type == config.TypeRpicam {
		// rpicam-vid 已在采集侧完成硬件 H264 编码（分辨率/帧率/码率由
		// libcamera 强制执行，不存在驱动静默回退），ffmpeg 仅直通封装：
		// 零转码，CPU 几乎无开销；帧率/分辨率兜底不适用（copy 不能过滤）。
		args = append(args, "-c", "copy", "-an",
			"-f", "rtsp",
			"-rtsp_transport", "tcp",
			pushURL,
		)
		return args
	}
	// 屏幕采集未配置分辨率时统一缩放到 1280 宽：
	//   - macOS：PNG 帧很大（Retina 3360×2100 ≈3MB/帧），管道吞吐受限；
	//   - Windows/Linux：多屏全分辨率（如 2752×1152）多路并发时浏览器
	//     GPU 硬解扛不住，实测 5 路（含 3 路巨分辨率）在 Edge/Chrome 中
	//     解码管线反复失效表现为画面闪烁；降到 1280 宽后硬解可稳定承载。
	if c.Type == config.TypeScreen && c.Width == 0 {
		args = append(args, "-vf", "scale=1280:-2")
	}
	args = append(args, videoEncodeArgs(resolveEncoder(bin, logger), gov, c.BitrateOrDefault())...)
	args = append(args, audioEncodeArgs(c)...)
	args = append(args, fpsGuaranteeArgs(c)...)
	args = append(args, resolutionGuaranteeArgs(c)...)
	args = append(args,
		"-f", "rtsp",
		"-rtsp_transport", "tcp",
		pushURL,
	)
	return args
}

// rpicamVidArgs 构造 rpicam-vid 命令参数（libcamera 管线采集 CSI 摄像头，
// 硬件 H264 编码后写 stdout，由 runner 接到 ffmpeg 的 stdin）。
//   - --inline：SPS/PPS 随每个关键帧内联重发——RTSP 读者中途加入也能解码
//    （这正是 ffmpeg 直调 v4l2m2m 翻车缺的特性，见 encoder.go 实测说明）；
//   - 分辨率/帧率/码率由 libcamera 强制执行，不存在 v4l2 的静默回退；
//   - --intra：关键帧周期与软编码 gov 约定一致（2×帧率），控制读者接入延迟。
func rpicamVidArgs(c config.Camera) []string {
	fps := c.FramerateOrDefault()
	if fps <= 0 {
		fps = 15
	}
	w, h := 640, 480
	if c.Width > 0 && c.Height > 0 {
		w, h = c.Width, c.Height
	}
	intra := fps * 2
	if intra < 10 {
		intra = 10
	}
	args := []string{
		"-t", "0",
		"--codec", "h264",
		"--inline",
		"--width", fmt.Sprintf("%d", w),
		"--height", fmt.Sprintf("%d", h),
		"--framerate", fmt.Sprintf("%d", fps),
		"--bitrate", fmt.Sprintf("%d", c.BitrateOrDefault()*1000),
		"--intra", fmt.Sprintf("%d", intra),
	}
	if src := strings.TrimSpace(c.Source); src != "" {
		args = append(args, "--camera", src) // 多摄像头时按 libcamera 索引选择
	}
	return append(args, "-o", "-")
}

// audioEncodeArgs 构造音频编码与流映射参数：
//   - 未启用音频（或该来源不支持）：-an 丢弃音轨，行为与历史版本一致；
//   - 启用音频：显式 -map 绑定视频与音频输入（双输入场景 v4l2+pulse 的音频在
//     输入 1；其余场景同输入 0），音轨可选（"?"）——麦克风缺失/源无音轨时
//     优雅退化为纯视频而不是整路失败；统一转码 AAC 并重采样到与 ONVIF
//     声明一致的 44.1kHz。
func audioEncodeArgs(c config.Camera) []string {
	audioInputIdx := "0"
	if c.Type == config.TypeV4L2 {
		audioInputIdx = "1"
	}
	// 历史行为兜底：dshow/avfoundation 配置了音频开关但没选设备时按未启用处理。
	enabled := c.AudioEnabled
	if enabled && (c.Type == config.TypeDShow || c.Type == config.TypeAVFoundation) && strings.TrimSpace(c.AudioSource) == "" {
		enabled = false
	}
	if !enabled {
		return []string{"-an"}
	}
	return []string{
		"-map", "0:v:0",
		"-map", audioInputIdx + ":a:0?",
		"-c:a", "aac", "-b:a", fmt.Sprintf("%d", c.AudioBitrate()), "-ar", fmt.Sprintf("%d", c.AudioSampleRate()),
	}
}
