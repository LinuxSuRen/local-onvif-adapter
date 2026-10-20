package stream

// RealSense 采集分发器（rs-pipe）托管。
//
// RealSense 设备同一时刻只允许一个进程访问（USB 独占），因此三路流
// （RGB/伪彩色深度/IR）共用一个 rs-pipe 守护进程，由 Manager 在检测到
// 任一启用的 realsense 摄像头时拉起、全部停用后回收。ffmpeg 进程各自
// 作为 TCP 客户端连到 base+0/1/2 端口读 rawvideo 裸流。

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
)

// RealSenseBasePort rs-pipe 监听起始端口：base+0=color、base+1=depth、base+2=ir。
// 摄像头 source 直接引用这些端口（tcp://127.0.0.1:18020 等）。
const RealSenseBasePort = 18020

// rsPipeSpec rs-pipe 的启动参数指纹（变化即需要重启进程）。
type rsPipeSpec struct {
	W, H, FPS int
}

func (s rsPipeSpec) fingerprint() string {
	return fmt.Sprintf("serve|%d|%d|%d", s.W, s.H, s.FPS)
}

// rsPipeSpecOf 从摄像头列表推导 rs-pipe 启动参数。
// 几何取第一路启用的 realsense 摄像头——三路流共用同一组几何参数
// （rs-pipe 统一启用三路流），各路摄像头的分辨率/帧率应保持一致。
func rsPipeSpecOf(cameras []config.Camera) (rsPipeSpec, bool) {
	for _, c := range cameras {
		if !c.Enabled || c.Type != config.TypeRealSense {
			continue
		}
		w, h := 640, 480
		if c.Width > 0 && c.Height > 0 {
			w, h = c.Width, c.Height
		}
		return rsPipeSpec{W: w, H: h, FPS: c.FramerateOrDefault()}, true
	}
	return rsPipeSpec{}, false
}

// rsPipeArgs 构造 rs-pipe 命令行参数。
func rsPipeArgs(spec rsPipeSpec, fake bool) []string {
	args := []string{"serve",
		"--port", fmt.Sprintf("%d", RealSenseBasePort),
		"--width", fmt.Sprintf("%d", spec.W),
		"--height", fmt.Sprintf("%d", spec.H),
		"--framerate", fmt.Sprintf("%d", spec.FPS),
	}
	if fake {
		args = append(args, "--fake")
	}
	return args
}

// rsPipe rs-pipe 守护进程监督器（异常退出指数退避重启）。
type rsPipe struct {
	bin    string
	fake   bool
	spec   rsPipeSpec
	logger *slog.Logger

	mu      sync.Mutex
	cmd     *exec.Cmd
	stopped bool
	stopCh  chan struct{}
}

func newRsPipe(spec rsPipeSpec, bin string, fake bool, logger *slog.Logger) *rsPipe {
	return &rsPipe{bin: bin, fake: fake, spec: spec, logger: logger, stopCh: make(chan struct{})}
}

// Stop 请求停止（幂等）：置位标志并强制结束当前进程。
func (p *rsPipe) Stop() {
	p.mu.Lock()
	cmd := p.cmd
	p.cmd = nil
	if !p.stopped {
		p.stopped = true
		close(p.stopCh)
	}
	p.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func (p *rsPipe) isStopped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopped
}

// Run 阻塞监督 rs-pipe 进程（与 Runner.Run 同款退避策略）。
func (p *rsPipe) Run() {
	backoff := time.Second
	const maxBackoff = 15 * time.Second
	for !p.isStopped() {
		args := rsPipeArgs(p.spec, p.fake)
		cmd := exec.Command(p.bin, args...) //nolint:gosec // 二进制路径来自受控配置
		p.logger.Info("rs-pipe starting", "bin", p.bin, "args", args)
		cmd.Stderr = os.Stderr              // rs-pipe 日志直接透传（量小，便于排障）
		if err := cmd.Start(); err != nil {
			p.logger.Warn("rs-pipe start failed", "err", err.Error(),
				"hint", "请安装/编译 rs-pipe（go build ./cmd/rs-pipe）并确保 realsense_bin 可寻址")
			select {
			case <-p.stopCh:
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		p.mu.Lock()
		p.cmd = cmd
		p.mu.Unlock()
		_ = cmd.Wait()
		p.mu.Lock()
		if p.cmd == cmd {
			p.cmd = nil
		}
		p.mu.Unlock()
		if p.isStopped() {
			return
		}
		p.logger.Warn("rs-pipe exited, restarting")
		select {
		case <-p.stopCh:
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}
