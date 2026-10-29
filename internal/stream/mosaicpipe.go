// mosaicpipe.go 实现 blackframe 断流策略的运行时：
//
//	画面格(cell) ──ffmpeg 中继──▶ rawvideo 定长帧 ──┐
//	画面格(cell) ──ffmpeg 中继──▶ rawvideo 定长帧 ──┼─▶ Go 拼画布 ──▶ 编码 ffmpeg ──▶ tee 输出
//	（断流格补黑帧）                                  ┘    (stdin 管道)
//
// 设计要点：
//   - 每格一个独立中继进程（拉本地回环流 → scale/fps/format 归一 → yuv420p 裸帧写 stdout），
//     进程退出只影响自己那一格：中继退避重启，期间合成循环用黑帧兜底，整流不断。
//   - 定长帧（w*h*3/2 字节）无任何容器头，io.ReadFull 即切帧，无需解析。
//   - 编码进程从 stdin 读画布帧（Go 按目标帧率节流写入，写阻塞即天然背压），
//     音频由编码进程直接拉音频摄像头的 RTSP 流（第二输入，可选映射）。
//   - 管道全部用 os.Pipe 手工建（StdoutPipe/StdinPipe 与 cmd.Wait 有竞态，实测踩坑）。
package stream

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
)

// mosaicRelayArgs 构造画面格中继命令参数：拉本地回环流缩放到格尺寸并归一
// 帧率/像素格式，输出 rawvideo 定长帧到 stdout。
func mosaicRelayArgs(camURL string, w, h, fps int) []string {
	return []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-rtsp_transport", "tcp", "-i", camURL,
		"-an",
		"-vf", fmt.Sprintf("scale=%d:%d,fps=%d,setsar=1,format=yuv420p", w, h, fps),
		"-f", "rawvideo", "-pix_fmt", "yuv420p", "pipe:1",
	}
}

// mosaicEncoderArgs 构造 blackframe 模式的编码进程参数：stdin 读画布
// rawvideo 定长帧；音频可选（直接拉音频摄像头的 RTSP 流为第二输入，
// 可选映射——源无音轨时优雅退化为纯视频）。
func mosaicEncoderArgs(m config.Mosaic, audioCamURL string, pushURL string,
	targets []string, bin string, logger *slog.Logger) []string {
	fps := m.FramerateOrDefault()
	args := []string{"-hide_banner", "-loglevel", "error",
		"-f", "rawvideo", "-pix_fmt", "yuv420p",
		"-video_size", fmt.Sprintf("%dx%d", m.Width, m.Height),
		"-framerate", fmt.Sprintf("%d", fps),
		"-i", "pipe:0"}
	if audioCamURL != "" {
		args = append(args, "-rtsp_transport", "tcp", "-i", audioCamURL)
	}
	args = append(args, "-map", "0:v")
	if audioCamURL != "" {
		args = append(args, "-map", "1:a?")
	} else {
		args = append(args, "-an")
	}
	gov := fps * 2
	if gov < 10 {
		gov = 10
	}
	args = append(args, videoEncodeArgs(resolveEncoder(bin, logger), gov, m.BitrateOrDefault())...)
	if audioCamURL != "" {
		args = append(args, "-c:a", "aac", "-b:a", "64k", "-ar", "44100")
	}
	args = append(args, outputArgs(pushURL, targets)...)
	return args
}

// blackYUV420P 生成 yuv420p 黑帧（有限范围：Y=16，U=V=128）。
func blackYUV420P(w, h int) []byte {
	buf := make([]byte, w*h*3/2)
	for i := 0; i < w*h; i++ {
		buf[i] = 16
	}
	for i := w * h; i < len(buf); i++ {
		buf[i] = 128
	}
	return buf
}

// blitYUV420P 把 w×h 的 yuv420p 帧拷贝到 cw×ch 画布的 (x,y) 位置。
// frame/canvas 均为紧凑排布：Y 平面后跟 U、V 平面（各为半宽半高）。
// 调用方保证 cw/ch/w/h/x/y 均为偶数且矩形完整落在画布内（配置层已校验）。
func blitYUV420P(canvas, frame []byte, cw, ch, x, y, w, h int) {
	for row := 0; row < h; row++ {
		dst := canvas[(y+row)*cw+x:]
		src := frame[row*w:]
		copy(dst[:w], src[:w])
	}
	hw, hh := w/2, h/2
	swh := cw / 2   // 画布半宽（U/V 平面行距）
	sH := ch / 2    // 画布半高
	hx, hy := x/2, y/2
	ySize := cw * ch
	fYSize := w * h
	for row := 0; row < hh; row++ {
		dstU := canvas[ySize+(hy+row)*swh+hx:]
		srcU := frame[fYSize+row*hw:]
		copy(dstU[:hw], srcU[:hw])
		dstV := canvas[ySize+sH*swh+(hy+row)*swh+hx:]
		srcV := frame[fYSize+hw*hh+row*hw:]
		copy(dstV[:hw], srcV[:hw])
	}
}

// cellRelay 单路画面格的中继监督器：拉流进程异常退出后指数退避重启，
// 最新完整帧与时间戳经 mutex 发布给合成循环。
type cellRelay struct {
	bin  string
	url  string
	w, h int
	fps  int
	logger *slog.Logger

	mu       sync.Mutex
	cmd      *exec.Cmd
	frame    []byte
	frameAt  time.Time
	running  bool
	lastErr  string
	stopped  bool
	stopCh   chan struct{}
}

func newCellRelay(bin, url string, w, h, fps int, logger *slog.Logger) *cellRelay {
	return &cellRelay{
		bin: bin, url: url, w: w, h: h, fps: fps, logger: logger,
		frame:  make([]byte, w*h*3/2),
		stopCh: make(chan struct{}),
	}
}

// Stop 请求停止（幂等）：置位标志并强制结束当前中继进程。
func (r *cellRelay) Stop() {
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

func (r *cellRelay) isStopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

// snapshot 把最新帧拷入 buf；返回该格是否新鲜（近期有帧）。
// 非新鲜时 buf 内容不保证有效，调用方应铺黑帧。
func (r *cellRelay) snapshot(buf []byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.frameAt) > mosaicFrameStale {
		return false
	}
	copy(buf, r.frame)
	return true
}

// status 返回中继健康度。
func (r *cellRelay) status() (running, fresh bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running, time.Since(r.frameAt) <= mosaicFrameStale
}

// Run 阻塞监督中继进程（退避策略与 Runner 一致）。
func (r *cellRelay) Run() {
	frameSize := r.w * r.h * 3 / 2
	backoff := time.Second
	const maxBackoff = 15 * time.Second
	for !r.isStopped() {
		args := mosaicRelayArgs(r.url, r.w, r.h, r.fps)
		pr, pw, err := os.Pipe()
		if err != nil {
			r.setRunning(false, "pipe: "+err.Error())
			if !r.sleepBackoff(&backoff, maxBackoff) {
				return
			}
			continue
		}
		cmd := exec.Command(r.bin, args...) //nolint:gosec // 二进制路径来自受控配置
		var stderr tailBuffer
		cmd.Stderr = &stderr
		cmd.Stdout = pw
		r.logger.Debug("mosaic relay starting", "url", redactArgs([]string{r.url}), "cell", fmt.Sprintf("%dx%d", r.w, r.h))
		if err := cmd.Start(); err != nil {
			_ = pr.Close()
			_ = pw.Close()
			r.setRunning(false, "start: "+err.Error())
			if !r.sleepBackoff(&backoff, maxBackoff) {
				return
			}
			continue
		}
		// 子进程持有写端副本，Go 侧立即关闭自己的写端（否则读端永远等不到 EOF）。
		_ = pw.Close()
		r.mu.Lock()
		r.cmd = cmd
		r.running = true
		r.lastErr = ""
		r.mu.Unlock()

		buf := make([]byte, frameSize)
	ReadLoop:
		for {
			if _, err := io.ReadFull(pr, buf); err != nil {
				if r.isStopped() {
					break ReadLoop
				}
				r.setRunning(false, readErrText(err, stderr.Tail()))
				break ReadLoop
			}
			r.mu.Lock()
			copy(r.frame, buf)
			r.frameAt = time.Now()
			r.mu.Unlock()
			backoff = time.Second // 持续有帧视为健康，退避归零
		}
		_ = pr.Close()
		_ = cmd.Wait()
		r.mu.Lock()
		if r.cmd == cmd {
			r.cmd = nil
		}
		r.mu.Unlock()
		if r.isStopped() {
			return
		}
		r.logger.Warn("mosaic relay exited, restarting", "url", redactArgs([]string{r.url}))
		if !r.sleepBackoff(&backoff, maxBackoff) {
			return
		}
	}
}

func (r *cellRelay) setRunning(v bool, errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running = v
	if errMsg != "" {
		r.lastErr = errMsg
	}
}

// sleepBackoff 等待退避时长；stopped 时返回 false。
func (r *cellRelay) sleepBackoff(backoff *time.Duration, maxBackoff time.Duration) bool {
	select {
	case <-r.stopCh:
		return false
	case <-time.After(*backoff):
	}
	*backoff = min(*backoff*2, maxBackoff)
	return true
}

func readErrText(err error, stderrTail string) string {
	msg := "read: " + err.Error()
	if stderrTail != "" {
		msg += "; ffmpeg: " + stderrTail
	}
	return msg
}

// runBlackframeOnce 执行一代 blackframe 模式合成：
// 启动全部画面中继 → 启动编码进程 → 按帧率合成画布写入 stdin，
// 阻塞到编码进程退出（任一 essential 失败即整代结束交由 Run 退避重启；
// 单格断流由黑帧兜底，不会走到这里）。
func (r *mosaicRunner) runBlackframeOnce() string {
	fps := r.m.FramerateOrDefault()
	avail := availableCameras(r.cameras)

	// 1. 画面中继：可用摄像头每格一个；缺失格无中继（合成循环恒铺黑帧）。
	type cellRun struct {
		cell  config.MosaicCell
		relay *cellRelay
		black []byte // 黑帧兜底（格尺寸）
		snap  []byte // 中继帧快照缓冲（格尺寸，预分配防每帧 realloc）
	}
	runs := make([]cellRun, 0, len(r.m.Cells))
	var relays []*cellRelay
	for _, cell := range r.m.Cells {
		cr := cellRun{
			cell:  cell,
			black: blackYUV420P(cell.W, cell.H),
			snap:  make([]byte, cell.W*cell.H*3/2),
		}
		if _, alive := avail[cell.CameraID]; alive {
			cr.relay = newCellRelay(r.bin, mosaicCamURL(r.addr, cell.CameraID), cell.W, cell.H, fps, r.logger)
			relays = append(relays, cr.relay)
		}
		runs = append(runs, cr)
	}
	for _, rl := range relays {
		go rl.Run()
	}
	defer func() {
		for _, rl := range relays {
			rl.Stop()
		}
	}()
	r.mu.Lock()
	r.relays = relays
	r.mu.Unlock()

	// 2. 编码进程：stdin 读画布帧；音频直接拉音频摄像头流。
	audioURL := ""
	if r.m.AudioCameraID != "" {
		if _, alive := avail[r.m.AudioCameraID]; alive {
			audioURL = mosaicCamURL(r.addr, r.m.AudioCameraID)
		}
	}
	pushURL := mosaicCamURL(r.addr, r.m.ID)
	args := mosaicEncoderArgs(r.m, audioURL, pushURL, r.targets, r.bin, r.logger)
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return "pipe: " + err.Error()
	}
	cmd := exec.Command(r.bin, args...) //nolint:gosec // 二进制路径来自受控配置
	var stderr tailBuffer
	cmd.Stderr = &stderr
	cmd.Stdin = stdinR
	r.logger.Debug("mosaic encoder starting", "mosaic", r.m.ID, "args", redactArgs(args))
	if err := cmd.Start(); err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		return "ffmpeg start: " + err.Error()
	}
	_ = stdinR.Close() // 子进程持有读端副本
	r.mu.Lock()
	r.cmd = cmd
	r.mu.Unlock()
	r.setRunning(true, "")
	r.publishStarted()
	if r.onStart != nil {
		r.onStart(r.m.ID)
	}

	// 3. 合成循环：按帧率把各格最新帧（或黑帧）拼到画布并写 stdin。
	composeDone := make(chan struct{})
	go func() {
		defer close(composeDone)
		canvas := blackYUV420P(r.m.Width, r.m.Height)
		ticker := time.NewTicker(time.Second / time.Duration(fps))
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
			case <-r.stopCh:
				return
			}
			status := make([]MosaicCellStatus, 0, len(runs))
			for i := range runs {
				cr := &runs[i]
				fresh := false
				if cr.relay != nil {
					fresh = cr.relay.snapshot(cr.snap)
				}
				if fresh {
					blitYUV420P(canvas, cr.snap, r.m.Width, r.m.Height,
						cr.cell.X, cr.cell.Y, cr.cell.W, cr.cell.H)
				} else {
					blitYUV420P(canvas, cr.black, r.m.Width, r.m.Height,
						cr.cell.X, cr.cell.Y, cr.cell.W, cr.cell.H)
				}
				running := false
				if cr.relay != nil {
					running, _ = cr.relay.status()
				}
				status = append(status, MosaicCellStatus{
					CameraID: cr.cell.CameraID, Fresh: fresh, RelayRunning: running,
				})
			}
			r.setCells(status)
			if _, err := stdinW.Write(canvas); err != nil {
				return // 编码进程已退出（EPIPE），本代结束
			}
		}
	}()

	err = cmd.Wait()
	stdinW.Close()
	<-composeDone
	r.mu.Lock()
	if r.cmd == cmd {
		r.cmd = nil
	}
	r.relays = nil
	r.mu.Unlock()
	if err == nil {
		return ""
	}
	msg := err.Error()
	if tail := stderr.Tail(); tail != "" {
		msg += "; ffmpeg: " + tail
	}
	return msg
}
