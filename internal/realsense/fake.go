package realsense

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"sync"
	"time"
)

// FakeSource 无硬件时的合成帧源：三路全出，按配置帧率定时产出。
//
// 画面设计原则：每一帧都随序号变化且各流之间有可辨认的对应关系，
// 便于肉眼验证链路（编码、延迟、通道是否接错）：
//   - color：八色彩条背景 + 随帧水平移动的高亮方块（感知延迟/帧率）；
//   - depth：距离斜面渐变 + 与 color 方块同位置同速移动的「近物」凸起
//     （验证 depth/color 空间对应；方块处颜色明显偏近端色）；
//   - ir：color 的灰度化 + 同位置白色方块（对应 IR 双目看到的投影）。
type FakeSource struct {
	cfg Config

	mu     sync.Mutex
	tick   int64  // 已产出帧数（也驱动画面动画）
	cancel func() // Stop 置位；Poll 通过 ctx 感知
	done   chan struct{}
}

// NewFakeSource 创建合成源。
func NewFakeSource(cfg Config) *FakeSource {
	cfg = cfg.WithDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	s := &FakeSource{cfg: cfg, cancel: cancel, done: make(chan struct{})}
	// 用独立 ctx 计时器实现「无消费者也按节奏走、Stop 后 Poll 立即返回」。
	go func() {
		defer close(s.done)
		interval := time.Duration(float64(time.Second) / float64(cfg.Framerate))
		t := time.NewTicker(max(interval, time.Millisecond))
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.mu.Lock()
				s.tick++
				s.mu.Unlock()
			}
		}
	}()
	return s
}

// Streams 声明三路全出。
func (s *FakeSource) Streams() []StreamKind { return []StreamKind{KindColor, KindDepth, KindIR} }

// Poll 阻塞到下一拍并返回该帧画面。
func (s *FakeSource) Poll() (FrameSet, error) {
	s.mu.Lock()
	last := s.tick
	s.mu.Unlock()
	// 等待 tick 递增（首拍立即用 0 号帧也行，但等一拍节奏更均匀）。
	for {
		s.mu.Lock()
		cur := s.tick
		s.mu.Unlock()
		if cur > last {
			return s.render(cur), nil
		}
		select {
		case <-s.done:
			return nil, fmt.Errorf("realsense: fake source stopped")
		case <-time.After(time.Millisecond):
		}
	}
}

// Stop 停止内部计时器。
func (s *FakeSource) Stop() { s.cancel() }

// Config 返回生效配置。
func (s *FakeSource) Config() Config { return s.cfg }

// render 渲染第 n 帧。
func (s *FakeSource) render(n int64) FrameSet {
	w, h := s.cfg.Width, s.cfg.Height
	set := FrameSet{}

	// color：八色彩条 + 移动方块。
	col := image.NewRGBA(image.Rect(0, 0, w, h))
	bars := []color.RGBA{
		{192, 192, 192, 255}, {192, 192, 0, 255}, {0, 192, 192, 255},
		{0, 192, 0, 255}, {192, 0, 192, 255}, {192, 0, 0, 255},
		{0, 0, 192, 255}, {0, 0, 0, 255},
	}
	bw := max(w/len(bars), 1)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			col.SetRGBA(x, y, bars[(x/bw)%len(bars)])
		}
	}
	// 方块尺寸 1/8 画面宽，横向往返移动，周期约 4 秒。
	sz := max(w/8, 4)
	span := max(w-sz, 1)
	// 每帧步长 = w/16 像素；往返用三角波。
	steps := span / max(w/16, 1)
	var fx int
	if steps > 0 {
		p := int(n) % (2 * steps)
		if p >= steps {
			p = 2*steps - p
		}
		fx = p * (span / steps)
	}
	fy := h/2 - sz/2
	for y := fy; y < fy+sz && y < h; y++ {
		for x := fx; x < fx+sz && x < w; x++ {
			col.SetRGBA(x, y, color.RGBA{255, 255, 255, 255})
		}
	}
	set[KindColor] = rgbaFrame(KindColor, w, h, col)

	// depth：横向距离斜面（左近右远，落在 [min,max] 区间中段）+
	// 方块处凸起为最近距离；再加轻量纵向波纹让画面有纹理。
	depth := make([]uint16, w*h)
	rgb := make([]byte, w*h*3)
	minD, maxD := s.cfg.DepthMinMM, s.cfg.DepthMaxMM
	mid := (minD + maxD) / 2
	half := (maxD - minD) / 2
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			d := mid - half*2*(x-w/2)/max(w, 1) + y*half/max(h, 1)/2
			if x >= fx && x < fx+sz && y >= fy && y < fy+sz {
				d = minD // 方块 = 最近
			}
			if d < minD {
				d = minD
			} else if d > maxD {
				d = maxD
			}
			depth[y*w+x] = uint16(d)
		}
	}
	if err := ApplyColorMapScaled(depth, w, h, rgb, minD, maxD); err != nil {
		// 长度均按 w*h 分配，不会走到这里。
		panic(err)
	}
	set[KindDepth] = &Frame{Kind: KindDepth, Width: w, Height: h, Data: rgb}

	// ir：color 灰度化（方块自动为白色高亮）。
	grayDst := make([]byte, w*h*3)
	if err := GrayToRGB(lumaOf(col, w, h), w, h, grayDst); err != nil {
		panic(err)
	}
	set[KindIR] = &Frame{Kind: KindIR, Width: w, Height: h, Data: grayDst}

	return set
}

// rgbaFrame 从 RGBA 图提取 rgb24 帧。
func rgbaFrame(kind StreamKind, w, h int, img *image.RGBA) *Frame {
	data := make([]byte, w*h*3)
	for i := 0; i < w*h; i++ {
		data[i*3] = img.Pix[i*4]
		data[i*3+1] = img.Pix[i*4+1]
		data[i*3+2] = img.Pix[i*4+2]
	}
	return &Frame{Kind: kind, Width: w, Height: h, Data: data}
}

// lumaOf 计算灰度图（ITU-R BT.601）。
func lumaOf(img *image.RGBA, w, h int) []byte {
	gray := make([]byte, w*h)
	for i := 0; i < w*h; i++ {
		r, g, b := img.Pix[i*4], img.Pix[i*4+1], img.Pix[i*4+2]
		gray[i] = uint8((int32(r)*299 + int32(g)*587 + int32(b)*114) / 1000)
	}
	return gray
}
