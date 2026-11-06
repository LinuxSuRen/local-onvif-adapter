// Package snapshot 用 ffmpeg 按需抓取摄像头单帧 JPEG，并做短时缓存。
// 两种取帧方式：
//   - JPEGFromStream：从取流服务的 RTSP 地址拉帧（摄像头启用时优先，
//     避免二次独占打开设备 —— Windows dshow / Linux v4l2 均为独占）。
//   - JPEG：直接从设备采集（摄像头禁用、无取流进程时使用）。
package snapshot

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
	"github.com/linuxsuren/local-onvif-adapter/internal/stream"
)

const (
	defaultTimeout = 10 * time.Second
	defaultTTL     = 3 * time.Second
)

type cacheEntry struct {
	data []byte
	at   time.Time
}

// Generator 抓帧生成器。
type Generator struct {
	bin    string
	ttl    time.Duration
	logger *slog.Logger

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// New 创建抓帧生成器。
func New(bin string, logger *slog.Logger) *Generator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Generator{bin: bin, ttl: defaultTTL, logger: logger, cache: map[string]cacheEntry{}}
}

// JPEG 直接从摄像头设备抓取一帧 JPEG。命中缓存且未过期时直接返回。
func (g *Generator) JPEG(cam config.Camera) ([]byte, error) {
	if cam.Type == config.TypeRpicam {
		// CSI 摄像头直抓走 rpicam-still（libcamera 原生输出 JPEG，不经 ffmpeg）。
		return g.rpiStill(cam)
	}
	args := []string{"-hide_banner", "-loglevel", "error"}
	// macOS 屏幕与宇树 A2 源从 stdin 管道读帧（后者由本机取帧泵喂帧），不能加 -nostdin。
	if !(cam.Type == config.TypeScreen && runtime.GOOS == "darwin") && cam.Type != config.TypeUnitreeA2 {
		args = append(args, "-nostdin")
	}
	args = append(args, stream.InputArgs(cam)...)
	args = append(args, "-frames:v", "1", "-q:v", "3", "-f", "image2", "pipe:1")
	if cam.Type == config.TypeScreen && runtime.GOOS == "darwin" {
		// macOS 屏幕直采：InputArgs 给出的是 image2pipe + pipe:，必须由
		// screencapture 循环喂帧（与取流 Runner 同一命令），否则管道立即
		// EOF——"pipe:: End of file" 即此路径缺失喂帧进程。
		return g.capturedWithFeed(cam, args)
	}
	if cam.Type == config.TypeUnitreeA2 {
		// 宇树 A2 直抓：InputArgs 为 image2pipe + pipe:，需本机取帧泵喂帧
		//（与取流 Runner 同一机制）。
		return g.capturedWithPumpFeed(cam, args)
	}
	return g.captured(cam.ID, args)
}

// capturedWithFeed 在单独的 sh 子进程喂 stdin 的条件下抓帧（macOS 屏幕直采）。
func (g *Generator) capturedWithFeed(cam config.Camera, args []string) ([]byte, error) {
	g.mu.Lock()
	if e, ok := g.cache[cam.ID]; ok && time.Since(e.at) < g.ttl {
		g.mu.Unlock()
		return e.data, nil
	}
	g.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	feed := exec.CommandContext(ctx, "/bin/sh", "-c", stream.DarwinScreenFeedCmd(cam)) //nolint:gosec // 固定解释器+受控命令
	feedPipe, err := feed.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("screen feed pipe: %w", err)
	}
	if err := feed.Start(); err != nil {
		return nil, fmt.Errorf("screen feed start: %w", err)
	}
	defer func() { _ = feed.Process.Kill() }()

	ffmpeg := exec.CommandContext(ctx, g.bin, args...) //nolint:gosec // 路径来自受控配置
	ffmpeg.Stdin = feedPipe
	g.logger.Debug("capturing with feed", "camera", cam.ID, "args", args)
	data, err := ffmpeg.Output()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg capture failed: %w", err)
	}
	if err := g.storeFrame(cam.ID, data); err != nil {
		return nil, err
	}
	return data, nil
}

// storeFrame 写入帧缓存（拆出便于带喂帧路径复用）。
func (g *Generator) storeFrame(key string, data []byte) error {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return fmt.Errorf("not a JPEG frame (%d bytes)", len(data))
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cache[key] = cacheEntry{data: data, at: time.Now()}
	return nil
}

// capturedWithPumpFeed 在本机取帧泵喂 stdin 的条件下抓帧（宇树 A2 直采）：
// 泵就绪即拿首帧退出，不需要等泵进程结束；超时由 ctx 兜底。
func (g *Generator) capturedWithPumpFeed(cam config.Camera, args []string) ([]byte, error) {
	g.mu.Lock()
	if e, ok := g.cache[cam.ID]; ok && time.Since(e.at) < g.ttl {
		g.mu.Unlock()
		return e.data, nil
	}
	g.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	pr, pw, err := pipeWithPump(cam.FramerateOrDefault())
	if err != nil {
		return nil, err
	}
	defer pr.Close()

	ffmpeg := exec.CommandContext(ctx, g.bin, args...) //nolint:gosec // 路径来自受控配置
	ffmpeg.Stdin = pr
	g.logger.Debug("capturing with unitree pump", "camera", cam.ID, "args", args)
	data, err := ffmpeg.Output()
	pw.close() // 抓帧完成即拆泵（关写端 + 终止泵进程）
	if err != nil {
		return nil, fmt.Errorf("ffmpeg capture failed: %w", err)
	}
	if err := g.storeFrame(cam.ID, data); err != nil {
		return nil, err
	}
	return data, nil
}

// pumpCloser 打包取帧泵与其管道写端：close 先关写端再终止泵进程，
// 让脚本因 stdout 管道破裂退出。
type pumpCloser struct {
	pw   *os.File
	pump *stream.UnitreePump
}

func (c pumpCloser) close() {
	_ = c.pw.Close()
	c.pump.Close()
}

// pipeWithPump 建立「本机取帧泵 → 管道写端」并返回读端与清理句柄。
func pipeWithPump(fps int) (*os.File, pumpCloser, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, pumpCloser{}, fmt.Errorf("pipe: %w", err)
	}
	var diag bytes.Buffer
	pump, err := stream.StartUnitreePump(fps, pw, &diag)
	if err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return nil, pumpCloser{}, fmt.Errorf("宇树取帧泵: %w", err)
	}
	go func() { _ = pump.Wait(); _ = pw.Close() }() // 泵退出兜底关写端
	return pr, pumpCloser{pw: pw, pump: pump}, nil
}

// rpiStill 用 rpicam-still 抓取一帧 JPEG（禁用预览与签名信息）。
func (g *Generator) rpiStill(cam config.Camera) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	args := []string{"-n", "-t", "1000", "-o", "-"}
	if cam.Width > 0 && cam.Height > 0 {
		args = append(args, "--width", strconv.Itoa(cam.Width), "--height", strconv.Itoa(cam.Height))
	}
	if src := strings.TrimSpace(cam.Source); src != "" {
		args = append(args, "--camera", src)
	}
	g.logger.Debug("rpicam-still capturing", "camera", cam.ID, "args", args)
	data, err := exec.CommandContext(ctx, "rpicam-still", args...).Output() //nolint:gosec // 固定工具名
	if err != nil {
		return nil, fmt.Errorf("rpicam-still capture failed: %w", err)
	}
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, fmt.Errorf("rpicam-still produced non-JPEG output (%d bytes)", len(data))
	}
	return data, nil
}

// JPEGFromStream 从 RTSP 取流地址拉取一帧 JPEG（与 JPEG 共享同一缓存键）。
func (g *Generator) JPEGFromStream(camID, rtspURL string) ([]byte, error) {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin",
		"-rtsp_transport", "tcp", "-i", rtspURL,
		"-frames:v", "1", "-q:v", "3", "-f", "image2", "pipe:1"}
	return g.captured(camID, args)
}

// captured 执行抓帧命令并写缓存。
func (g *Generator) captured(key string, args []string) ([]byte, error) {
	g.mu.Lock()
	if e, ok := g.cache[key]; ok && time.Since(e.at) < g.ttl {
		g.mu.Unlock()
		return e.data, nil
	}
	g.mu.Unlock()

	data, err := g.run(args)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.cache[key] = cacheEntry{data: data, at: time.Now()}
	g.mu.Unlock()
	return data, nil
}

func (g *Generator) run(args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	g.logger.Debug("snapshot capturing", "args", args)
	cmd := exec.CommandContext(ctx, g.bin, args...) //nolint:gosec // 二进制路径来自受控配置
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		tail := errBuf.String()
		if len(tail) > 300 {
			tail = tail[len(tail)-300:]
		}
		return nil, fmt.Errorf("ffmpeg capture failed: %w; %s", err, tail)
	}
	data := out.Bytes()
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, fmt.Errorf("ffmpeg capture produced non-JPEG output (%d bytes)", len(data))
	}
	return data, nil
}
