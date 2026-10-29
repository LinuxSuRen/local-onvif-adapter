// 深度伪彩色帧的内嵌距离色标尺（OSD）。
//
// 在深度帧右缘绘制垂直 Turbo 色条 + 毫米刻度数值：底部为最近距离
//（depth-min）、顶部为最远（depth-max），与 ApplyColorMap 的映射方向
// 一致——画面中某像素的颜色在色条上等高位置读数即该处距离。
//
// 字形采用 3x5 点阵手绘位图（2x 放大绘制），零外部字体依赖；
// 帧宽不足 320 时跳过（窄幅画面放不下 40px 标尺列）。
package realsense

// DrawDepthScale 在 rgb24 帧右缘绘制距离色标尺（原地修改 dst）。
// 布局（自右向左）：2px 边距 + 12px Turbo 色条 + 26px 刻度文字区。
func DrawDepthScale(w, h int, dst []byte, minMM, maxMM int) {
	if w < 320 || h < 120 {
		return // 画面过小放不下标尺，跳过
	}
	if len(dst) != w*h*3 {
		return
	}
	if maxMM <= minMM {
		minMM, maxMM = 0, 1
	}

	// 背景列（纯黑，保证文字与色条在任意画面上都可读）。
	const colW = 40
	for y := 0; y < h; y++ {
		for x := w - colW; x < w; x++ {
			o := (y*w + x) * 3
			dst[o], dst[o+1], dst[o+2] = 0, 0, 0
		}
	}

	// Turbo 色条：底部=minMM（近），顶部=maxMM（远），与帧内像素同向。
	barX0, barX1 := w-14, w-3 // 12px 宽
	barY0, barY1 := 10, h-10
	span := float64(maxMM - minMM)
	for y := barY0; y <= barY1; y++ {
		q := float64(y-barY0) / float64(barY1-barY0) // 0(顶)=远 1(底)=近
		r, g, b := TurboRGB(1 - q)
		for x := barX0; x <= barX1; x++ {
			o := (y*w + x) * 3
			dst[o], dst[o+1], dst[o+2] = r, g, b
		}
	}

	// "mm" 单位标注在色条顶端上方。
	labelMM(barX0-2, 4, w, dst)

	// 刻度：取"整齐步长"使刻度数 3-7 个，白线横贯色条 + 右对齐数值。
	step := niceStep(maxMM - minMM)
	first := ((minMM + step - 1) / step) * step
	for mm := first; mm <= maxMM; mm += step {
		q := 1 - float64(mm-minMM)/span // 1=底部(近) 0=顶部(远)
		y := barY0 + int(q*float64(barY1-barY0)+0.5)
		for x := barX0 - 2; x <= barX1; x++ {
			o := (y*w + x) * 3
			dst[o], dst[o+1], dst[o+2] = 255, 255, 255
		}
		drawNumber(mm, barX0-6, y, w, dst) // 右对齐到色条左侧
	}
}

// niceStep 为量程选择整齐的刻度步长（100/200/250/500 序列）。
func niceStep(rangeMM int) int {
	for _, s := range []int{100, 200, 250, 500, 1000, 2000, 2500, 5000, 10000} {
		if rangeMM/s <= 7 {
			return s
		}
	}
	return 10000
}

// 3x5 点阵字形（每行 3 bit，高位在左）。只需 0-9 与 m。
var glyphs = map[rune][5]uint8{
	'0': {0b111, 0b101, 0b101, 0b101, 0b111},
	'1': {0b010, 0b110, 0b010, 0b010, 0b111},
	'2': {0b111, 0b001, 0b111, 0b100, 0b111},
	'3': {0b111, 0b001, 0b011, 0b001, 0b111},
	'4': {0b101, 0b101, 0b111, 0b001, 0b001},
	'5': {0b111, 0b100, 0b111, 0b001, 0b111},
	'6': {0b111, 0b100, 0b111, 0b101, 0b111},
	'7': {0b111, 0b001, 0b001, 0b010, 0b010},
	'8': {0b111, 0b101, 0b111, 0b101, 0b111},
	'9': {0b111, 0b101, 0b111, 0b001, 0b111},
	'm': {0b000, 0b000, 0b101, 0b111, 0b101},
}

// drawText 以 2x 放大绘制点阵文字（白色），(xRight, yCenter) 为文字
// 右边缘与垂直中心。越界部分自动裁剪。
func drawText(s string, xRight, yCenter, w int, dst []byte) {
	const scale = 2
	gw, gh := 3*scale, 5*scale
	totalW := len([]rune(s)) * (gw + scale)
	x0 := xRight - totalW
	y0 := yCenter - gh/2
	for i, r := range s {
		gl, ok := glyphs[r]
		if !ok {
			continue
		}
		gx := x0 + i*(gw+scale)
		for row := 0; row < 5; row++ {
			bits := gl[row]
			for col := 0; col < 3; col++ {
				if bits&(1<<(2-col)) == 0 {
					continue
				}
				for sy := 0; sy < scale; sy++ {
					for sx := 0; sx < scale; sx++ {
						x, y := gx+col*scale+sx, y0+row*scale+sy
						if x < 0 || x >= w || y < 0 {
							continue
						}
						o := (y*w + x) * 3
						if o+2 >= len(dst) {
							continue
						}
						dst[o], dst[o+1], dst[o+2] = 255, 255, 255
					}
				}
			}
		}
	}
}

func drawNumber(mm int, xRight, yCenter, w int, dst []byte) {
	if mm < 0 {
		return
	}
	s := ""
	for mm > 0 {
		s = string(rune('0'+mm%10)) + s
		mm /= 10
	}
	drawText(s, xRight, yCenter, w, dst)
}

func labelMM(xRight, yTop int, w int, dst []byte) {
	drawText("mm", xRight, yTop+7, w, dst)
}

// ApplyColorMapScaled 映射伪彩色并叠加距离色标尺（深度流出图统一入口）。
func ApplyColorMapScaled(depth []uint16, w, h int, dst []byte, minMM, maxMM int) error {
	if err := ApplyColorMap(depth, w, h, dst, minMM, maxMM); err != nil {
		return err
	}
	DrawDepthScale(w, h, dst, minMM, maxMM)
	return nil
}
