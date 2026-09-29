// Package snapshot 用 ffmpeg 按需抓取摄像头单帧 JPEG，并做短时缓存。
package snapshot

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/linuxsuren/onvif-local/internal/config"
	"github.com/linuxsuren/onvif-local/internal/stream"
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

// JPEG 抓取一帧 JPEG。命中缓存且未过期时直接返回。
func (g *Generator) JPEG(cam config.Camera) ([]byte, error) {
	g.mu.Lock()
	if e, ok := g.cache[cam.ID]; ok && time.Since(e.at) < g.ttl {
		g.mu.Unlock()
		return e.data, nil
	}
	g.mu.Unlock()

	data, err := g.capture(cam)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.cache[cam.ID] = cacheEntry{data: data, at: time.Now()}
	g.mu.Unlock()
	return data, nil
}

func (g *Generator) capture(cam config.Camera) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	args = append(args, stream.InputArgs(cam)...)
	args = append(args, "-frames:v", "1", "-q:v", "3", "-f", "image2", "pipe:1")

	g.logger.Debug("snapshot capturing", "camera", cam.ID, "args", args)
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
