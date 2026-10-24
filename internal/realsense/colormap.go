package realsense

import "fmt"

// Turbo 伪彩色：深度值 → RGB 的确定性映射。
//
// 使用 Google Turbo colormap 的多项式近似（Anton Mikhailov 发布的
// 拟合系数），近端深蓝、远端暗红，全程明度单调递增，比 Jet 的
// 伪轮廓更少。不用 librealsense 内置 colorizer 的原因：其默认
// 直方图均衡化导致映射随场景变化，同一位置颜色不稳定，无法
// 用于目测距离；这里固定 [min,max] 线性映射，颜色可复现。

// turbo 多项式系数（x ∈ [0,1]）。
var (
	turboR = [6]float64{0.13572138, 4.61539260, -42.66032258, 132.13108234, -152.94239396, 59.28637943}
	turboG = [6]float64{0.09140161, 2.19418839, 4.84296658, -14.18503333, 4.27729857, 2.82956604}
	turboB = [6]float64{0.10667330, 12.64194608, -60.58204836, 110.36276771, -89.90310912, 27.34824973}
)

// poly 计算五次多项式 Σ c[i]·x^i。
func poly(c [6]float64, x float64) float64 {
	v := c[5]
	for i := 4; i >= 0; i-- {
		v = v*x + c[i]
	}
	return v
}

// TurboRGB 返回归一化位置 q∈[0,1]（自动钳位）对应的 Turbo 颜色。
func TurboRGB(q float64) (r, g, b uint8) {
	if q < 0 {
		q = 0
	} else if q > 1 {
		q = 1
	}
	r = clamp8(poly(turboR, q) * 255)
	g = clamp8(poly(turboG, q) * 255)
	b = clamp8(poly(turboB, q) * 255)
	return
}

func clamp8(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v + 0.5)
}

// ApplyColorMap 把 16-bit 深度帧映射为 Turbo 伪彩色 rgb24。
// depth 长度必须为 w*h；dst 长度必须为 w*h*3（可预分配复用）。
// 深度值 0 表示无效点（太近/太远/遮挡），映射为纯黑以便与最近可测区分。
func ApplyColorMap(depth []uint16, w, h int, dst []byte, minMM, maxMM int) error {
	if len(depth) != w*h {
		return errLen("depth", w*h, len(depth))
	}
	if len(dst) != w*h*3 {
		return errLen("dst", w*h*3, len(dst))
	}
	if maxMM <= minMM {
		minMM, maxMM = 0, 1
	}
	span := float64(maxMM - minMM)
	for i, d := range depth {
		o := i * 3
		if d == 0 {
			dst[o], dst[o+1], dst[o+2] = 0, 0, 0
			continue
		}
		q := (float64(int32(d)) - float64(minMM)) / span
		if q < 0 {
			q = 0
		} else if q > 1 {
			q = 1
		}
		dst[o], dst[o+1], dst[o+2] = TurboRGB(q)
	}
	return nil
}

// GrayToRGB 把 8-bit 灰度帧展开为 rgb24（IR 流用）。
func GrayToRGB(gray []byte, w, h int, dst []byte) error {
	if len(gray) != w*h {
		return errLen("gray", w*h, len(gray))
	}
	if len(dst) != w*h*3 {
		return errLen("dst", w*h*3, len(dst))
	}
	for i, v := range gray {
		o := i * 3
		dst[o], dst[o+1], dst[o+2] = v, v, v
	}
	return nil
}

func errLen(name string, want, got int) error {
	return fmt.Errorf("realsense: %s length mismatch: want %d got %d", name, want, got)
}
