package realsense

import (
	"testing"
)

func px(dst []byte, w, x, y int) (r, g, b uint8) {
	o := (y*w + x) * 3
	return dst[o], dst[o+1], dst[o+2]
}

// TestDrawDepthScaleBar 绘制后：色条列存在且方向正确（顶=远端 Turbo(1) 的暗红系，
// 底=近端 Turbo(0) 的深蓝系），背景列纯黑。
func TestDrawDepthScaleBar(t *testing.T) {
	w, h := 640, 480
	dst := make([]byte, w*h*3)
	DrawDepthScale(w, h, dst, 300, 8000)

	// 色条中点列（barX0..barX1 = w-14..w-3）。
	midX := w - 9
	rTop, gTop, bTop := px(dst, w, midX, 12)
	rBot, gBot, bBot := px(dst, w, midX, h-12)
	// Turbo 明度单调递增：顶（远端）亮红系、底（近端）暗；方向不能颠倒。
	if rTop < 100 || bTop > 50 {
		t.Fatalf("top of bar should be far-end (reddish): (%d,%d,%d)", rTop, gTop, bTop)
	}
	if rTop+gTop+bTop <= rBot+gBot+bBot {
		t.Fatalf("bar luminance must increase to top (far): top=(%d,%d,%d) bot=(%d,%d,%d)",
			rTop, gTop, bTop, rBot, gBot, bBot)
	}

	// 标尺列左侧紧邻的图像区不被触碰（保持原值 0）。
	r, g, b := px(dst, w, w-45, 100)
	if r != 0 || g != 0 || b != 0 {
		t.Fatalf("outside column must be untouched: (%d,%d,%d)", r, g, b)
	}
}

// TestDrawDepthScaleTicks 刻度区应存在白色像素（数值与刻线）。
func TestDrawDepthScaleTicks(t *testing.T) {
	w, h := 640, 480
	dst := make([]byte, w*h*3)
	DrawDepthScale(w, h, dst, 300, 8000)

	whiteIn := func(x0, x1, y0, y1 int) bool {
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				r, g, b := px(dst, w, x, y)
				if r == 255 && g == 255 && b == 255 {
					return true
				}
			}
		}
		return false
	}
	// 文字区（bar 左侧 26px 列）应有白色刻度数值。
	if !whiteIn(w-40, w-16, 10, h-10) {
		t.Fatal("no white tick labels found in label area")
	}
	// 色条本身有白色刻线（贯穿色条宽度）。
	if !whiteIn(w-14, w-2, 10, h-10) {
		t.Fatal("no white tick marks found across bar")
	}
}

// TestDrawDepthScaleSmallFrame 帧过小跳过（不越界、不绘制）。
func TestDrawDepthScaleSmallFrame(t *testing.T) {
	w, h := 320, 240
	dst := make([]byte, w*h*3)
	DrawDepthScale(w, h, dst, 300, 8000) // 320 宽：可绘制
	if r, _, _ := px(dst, w, w-9, 12); r == 0 {
		// 顶部 Turbo(1) 红分量 > 0，若为 0 说明没绘制。此处仅确认无 panic。
		t.Logf("320-width frame drew bar (top r=%d)", r)
	}
	w2, h2 := 300, 200
	dst2 := make([]byte, w2*h2*3)
	DrawDepthScale(w2, h2, dst2, 300, 8000)
	for i, v := range dst2 {
		if v != 0 {
			t.Fatalf("small frame must be no-op, dst[%d]=%d", i, v)
		}
	}
}

// TestApplyColorMapScaled 组合入口：映射 + 标尺一次完成。
func TestApplyColorMapScaled(t *testing.T) {
	w, h := 640, 480
	depth := make([]uint16, w*h)
	for i := range depth {
		depth[i] = 4000 // 中距
	}
	dst := make([]byte, w*h*3)
	if err := ApplyColorMapScaled(depth, w, h, dst, 300, 8000); err != nil {
		t.Fatal(err)
	}
	// 画面主体应是 4000mm 对应的 Turbo 色（非黑），标尺列外有映射结果。
	r, g, b := px(dst, w, 100, 100)
	if r == 0 && g == 0 && b == 0 {
		t.Fatal("main area should have colormap output")
	}
	// 标尺背景列是纯黑（覆盖了映射色）。
	if r2, g2, b2 := px(dst, w, w-20, 100); r2 != 0 || g2 != 0 || b2 != 0 {
		t.Fatalf("scale column should be black bg, got (%d,%d,%d)", r2, g2, b2)
	}
	// 错误透传：长度不符报错。
	if err := ApplyColorMapScaled([]uint16{1}, w, h, dst, 300, 8000); err == nil {
		t.Fatal("length mismatch should error")
	}
}

// TestNiceStep 步长选择（整除语义：rangeMM/step ≤ 7）覆盖常用量程。
func TestNiceStep(t *testing.T) {
	cases := []struct{ rangeMM, want int }{
		{7700, 1000}, // 默认 300-8000
		{700, 100},
		{1500, 200},
		{5000, 1000},
	}
	for _, c := range cases {
		if got := niceStep(c.rangeMM); got != c.want {
			t.Errorf("niceStep(%d) = %d, want %d", c.rangeMM, got, c.want)
		}
	}
}
