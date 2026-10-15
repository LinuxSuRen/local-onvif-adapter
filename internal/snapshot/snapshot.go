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
	"os/exec"
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
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	args = append(args, stream.InputArgs(cam)...)
	args = append(args, "-frames:v", "1", "-q:v", "3", "-f", "image2", "pipe:1")
	return g.captured(cam.ID, args)
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
