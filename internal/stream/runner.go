// Package stream 管理每个摄像头的 ffmpeg 取流进程：
// 拉取本地源（v4l2/avfoundation/dshow/rtsp/testsrc）转码 H264 后推送到内嵌 RTSP 服务器。
package stream

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
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
}

// Manager 管理全部取流进程。
type Manager struct {
	mu        sync.Mutex
	runners   map[string]*Runner
	pushBase  func() string // 每次取流实时求值（认证开关变化时注入/移除 userinfo）
	lastAddr  string        // 上次使用的推流地址；变化即全部重启
	bin       string
	logger    *slog.Logger
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
func Spec(c config.Camera) string {
	return fmt.Sprintf("%s|%s|%d|%d|%d|%d", c.Type, c.Source, c.Width, c.Height, c.FramerateOrDefault(), c.BitrateOrDefault())
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

// Stop 请求停止（幂等）。
func (r *Runner) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.stopped {
		r.stopped = true
		close(r.stopCh)
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
		args := BuildArgs(r.cam, r.pushAddr+r.camPath())
		cmd := exec.Command(r.bin, args...) //nolint:gosec // 二进制路径来自受控配置
		var stderr tailBuffer
		cmd.Stderr = &stderr

		// macOS 屏幕采集：启动 screencapture 循环子进程，其 stdout 接到 ffmpeg 的 stdin。
		// 用 os.Pipe() 手动建管道；ffmpeg 退出后（cmd.Run 返回）再关写端并 kill shell。
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

		r.logger.Debug("ffmpeg starting", "camera", r.cam.ID, "args", args)
		r.setRunning(true, "", &starts)
		started := time.Now()
		err := cmd.Run()
		if r.isStopped() {
			r.setRunning(false, "", &starts)
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

// InputArgs 按摄像头类型构造 ffmpeg 输入参数。
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
	case config.TypeAVFoundation:
		args = append(args, "-f", "avfoundation")
		if fps > 0 {
			args = append(args, "-framerate", fmt.Sprintf("%d", fps))
		}
		src := c.Source
		if src == "" {
			src = "0"
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
		args = append(args, "-i", src)
	case config.TypeScreen:
		args = append(args, screenInputArgs(c)...)
	case config.TypeRTSP:
		args = append(args, "-rtsp_transport", "tcp", "-i", c.Source)
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
//   - Linux：source 为 X display（如 ":0.0"，一屏一源）
//   - Windows：source 为 desktop（gdigrab 主屏）或 title=窗口名
func screenInputArgs(c config.Camera) []string {
	return screenInputArgsFor(runtime.GOOS, c)
}

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
		if c.Width > 0 && c.Height > 0 {
			args = append(args, "-video_size", fmt.Sprintf("%dx%d", c.Width, c.Height))
		}
		src := strings.TrimSpace(c.Source)
		if src == "" {
			src = "desktop"
		}
		args = append(args, "-i", src)
	default: // linux（x11grab）
		args = append(args, "-f", "x11grab")
		if fps > 0 {
			args = append(args, "-framerate", fmt.Sprintf("%d", fps))
		}
		if c.Width > 0 && c.Height > 0 {
			args = append(args, "-video_size", fmt.Sprintf("%dx%d", c.Width, c.Height))
		}
		src := strings.TrimSpace(c.Source)
		if src == "" {
			src = ":0.0"
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

// BuildArgs 构造完整的 ffmpeg 取流推流命令参数。
func BuildArgs(c config.Camera, pushURL string) []string {
	fps := c.FramerateOrDefault()
	gov := fps * 2
	if gov < 10 {
		gov = 10
	}
	args := []string{"-hide_banner", "-loglevel", "error"}
	// macOS 屏幕源需要从 stdin 管道读 screencapture 帧，不能加 -nostdin。
	if !(c.Type == config.TypeScreen && runtime.GOOS == "darwin") {
		args = append(args, "-nostdin")
	}
	args = append(args, InputArgs(c)...)
	// macOS 屏幕采集的 PNG 帧很大（Retina 3360×2100 ≈3MB/帧），管道吞吐受限，
	// 缩放到 1280 宽保证实时性（未配置分辨率时）。
	if c.Type == config.TypeScreen && runtime.GOOS == "darwin" && c.Width == 0 {
		args = append(args, "-vf", "scale=1280:-2")
	}
	args = append(args,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-tune", "zerolatency",
		"-pix_fmt", "yuv420p",
		"-g", fmt.Sprintf("%d", gov),
		"-b:v", fmt.Sprintf("%dk", c.BitrateOrDefault()),
		"-an",
		"-f", "rtsp",
		"-rtsp_transport", "tcp",
		pushURL,
	)
	return args
}
