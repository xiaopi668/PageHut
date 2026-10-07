// Package captchaimg 生成 PageHut 登录/注册环节所需的验证码图片。
//
// 本包只依赖 Go 标准库（image / image/color / image/draw / image/png /
// math / math/rand 等），不引入任何第三方依赖，也不修改 go.mod：
//
//   - NewImage  字符型图形验证码：内置 5x7 点阵数字字形，叠加逐字随机错切、
//     随机上下偏移、干扰曲线与噪点，输出 PNG。
//   - NewSlider 滑动验证码：生成背景图、带透明边缘的拼图块，
//     以及拼图块在背景中的正确坐标。
//
// 随机源由调用方传入，便于测试复现；传 nil 时退化为按当前时间播种的随机源。
package captchaimg

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"math/rand"
	"time"
)

// ---------------------------------------------------------------------------
// 常量
// ---------------------------------------------------------------------------

const (
	// captchaWidth / captchaHeight 是图形验证码画布尺寸。
	captchaWidth  = 160
	captchaHeight = 60

	// glyphCols / glyphRows 是内置点阵字形的列数与行数。
	glyphCols = 5
	glyphRows = 7

	// maxCodeLen 是 NewImage 允许的最大字符数。
	maxCodeLen = 8

	// maxGlyphScale 是点阵放大倍数的上限，避免一两个字符被拉得过大。
	maxGlyphScale = 6

	// glyphMarginX / glyphMarginY 是画布四周预留的边距，
	// 保证错切与上下随机偏移之后字形依然完整可见。
	glyphMarginX = 10
	glyphMarginY = 8

	// glyphSpacing 是相邻字符之间的间距（像素）。
	glyphSpacing = 4

	// maxShear 是单个字符的最大错切比例（每一行像素相对字形垂直中心的水平偏移）。
	maxShear = 0.30

	// sliderHoleShade 是滑动验证码缺口处黑色蒙版的透明度（0-255）。
	sliderHoleShade = 0x59

	// sliderMargin 是滑动验证码拼图块与画布边缘之间保留的边距。
	sliderMargin = 4

	// minPieceSize 是拼图块基础方块的最小边长。
	minPieceSize = 8
)

// ---------------------------------------------------------------------------
// 内置 5x7 点阵字形
// ---------------------------------------------------------------------------

// glyph 表示一个 5 列 x 7 行的点阵字形。
// 每个元素是一行像素，只使用低 5 位：bit4 是最左边的一列，1 表示点亮。
type glyph [glyphRows]uint8

// digitGlyphs 是 0-9 的 5x7 点阵字形表（注释里用 # 展示形状）。
var digitGlyphs = [10]glyph{
	// 0
	{0b01110, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b01110},
	// 1
	{0b00100, 0b01100, 0b00100, 0b00100, 0b00100, 0b00100, 0b01110},
	// 2
	{0b01110, 0b10001, 0b00001, 0b00010, 0b00100, 0b01000, 0b11111},
	// 3
	{0b11111, 0b00010, 0b00100, 0b00010, 0b00001, 0b10001, 0b01110},
	// 4
	{0b00010, 0b00110, 0b01010, 0b10010, 0b11111, 0b00010, 0b00010},
	// 5
	{0b11111, 0b10000, 0b11110, 0b00001, 0b00001, 0b10001, 0b01110},
	// 6
	{0b00110, 0b01000, 0b10000, 0b11110, 0b10001, 0b10001, 0b01110},
	// 7
	{0b11111, 0b00001, 0b00010, 0b00100, 0b01000, 0b01000, 0b01000},
	// 8
	{0b01110, 0b10001, 0b10001, 0b01110, 0b10001, 0b10001, 0b01110},
	// 9
	{0b01110, 0b10001, 0b10001, 0b01111, 0b00001, 0b00010, 0b01100},
}

// unknownGlyph 是未收录字符的占位字形（空心方块），保证渲染不会失败。
// 目前仅收录 0-9，其它字符一律用它绘制。
var unknownGlyph = glyph{0b11111, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b11111}

// glyphFor 返回字符对应的字形，未收录时返回占位字形。
func glyphFor(ch byte) glyph {
	if ch >= '0' && ch <= '9' {
		return digitGlyphs[ch-'0']
	}
	return unknownGlyph
}

// glyphRowsOf 把字形展开成 7 行文本，供测试输出与查重使用。
func glyphRowsOf(g glyph) [glyphRows]string {
	var rows [glyphRows]string
	for y := 0; y < glyphRows; y++ {
		buf := make([]byte, 0, glyphCols)
		for x := 0; x < glyphCols; x++ {
			if g[y]&(1<<uint(glyphCols-1-x)) != 0 {
				buf = append(buf, '#')
			} else {
				buf = append(buf, '.')
			}
		}
		rows[y] = string(buf)
	}
	return rows
}

// ---------------------------------------------------------------------------
// 配色
// ---------------------------------------------------------------------------

// inkPalette 是文字与干扰线使用的深色系配色，保证与浅色背景对比充足。
var inkPalette = []color.RGBA{
	{R: 0x1F, G: 0x3A, B: 0x5F, A: 0xFF}, // 深蓝
	{R: 0x2E, G: 0x4A, B: 0x2E, A: 0xFF}, // 墨绿
	{R: 0x5A, G: 0x2A, B: 0x2A, A: 0xFF}, // 暗红
	{R: 0x3B, G: 0x2E, B: 0x55, A: 0xFF}, // 深紫
	{R: 0x33, G: 0x33, B: 0x33, A: 0xFF}, // 深灰
	{R: 0x7A, G: 0x4A, B: 0x12, A: 0xFF}, // 赭石
}

// sliderPalette 是滑动验证码背景使用的彩色系配色。
var sliderPalette = []color.RGBA{
	{R: 0x2C, G: 0x6E, B: 0xB5, A: 0xFF}, // 湖蓝
	{R: 0x39, G: 0x8E, B: 0x7A, A: 0xFF}, // 青绿
	{R: 0xE0, G: 0x8A, B: 0x3C, A: 0xFF}, // 橙
	{R: 0xD1, G: 0x5B, B: 0x6B, A: 0xFF}, // 玫红
	{R: 0x6A, G: 0x5C, B: 0xC4, A: 0xFF}, // 蓝紫
	{R: 0x4A, G: 0x5A, B: 0x6A, A: 0xFF}, // 石板灰
	{R: 0xB8, G: 0x9B, B: 0x3E, A: 0xFF}, // 土黄
	{R: 0x3E, G: 0x7C, B: 0x4F, A: 0xFF}, // 森林绿
}

// randInkColor 从深色系里随机取色，并做轻微扰动，避免整幅图颜色统一。
func randInkColor(r *rand.Rand) color.RGBA {
	c := inkPalette[r.Intn(len(inkPalette))]
	c.R = clampUint8(int(c.R) + r.Intn(21) - 10)
	c.G = clampUint8(int(c.G) + r.Intn(21) - 10)
	c.B = clampUint8(int(c.B) + r.Intn(21) - 10)
	c.A = 0xFF
	return c
}

// ---------------------------------------------------------------------------
// 公开 API：字符图形验证码
// ---------------------------------------------------------------------------

// NewImage 生成图形验证码 PNG。
//
// code 是明文答案（例如 5 位数字）；目前内置 0-9 的 5x7 点阵字形，
// 其它字符会用空心方块占位，调用方应只传数字。
// rnd 由调用方提供以便测试可复现，允许为 nil，
// 此时使用 rand.New(rand.NewSource(time.Now().UnixNano()))。
//
// 返回值是完整的 PNG 文件字节（画布 160x60）。
func NewImage(rnd *rand.Rand, code string) ([]byte, error) {
	if code == "" {
		return nil, errors.New("captchaimg: 验证码内容不能为空")
	}
	if len(code) > maxCodeLen {
		return nil, fmt.Errorf("captchaimg: 验证码长度 %d 超过上限 %d", len(code), maxCodeLen)
	}
	r := newRand(rnd)

	img := image.NewRGBA(image.Rect(0, 0, captchaWidth, captchaHeight))
	// 第一层：浅色渐变背景 + 浅色底纹（位于文字之下，不影响辨认）。
	drawCaptchaBackdrop(r, img)

	n := len(code)
	scale := fitGlyphScale(n, glyphSpacing)
	if scale < 2 {
		return nil, fmt.Errorf("captchaimg: %d 个字符无法在 %dx%d 的画布内清晰绘制",
			n, captchaWidth, captchaHeight)
	}
	glyphW, glyphH := glyphCols*scale, glyphRows*scale
	totalW := n*glyphW + (n-1)*glyphSpacing
	startX := (captchaWidth - totalW) / 2
	startY := (captchaHeight - glyphH) / 2

	// 第二层：逐字绘制，颜色、错切角度、垂直偏移都随机。
	for i := 0; i < n; i++ {
		x := startX + i*(glyphW+glyphSpacing)
		y := startY + randomOffsetY(r, startY, glyphH)
		drawGlyph(img, glyphFor(code[i]), x, y, scale, randInkColor(r), randomShear(r))
	}

	// 第三层：干扰曲线。
	drawInterference(r, img)
	// 第四层：随机噪点（透明度低，只增加识别难度，不盖住文字）。
	drawSpeckles(r, img, captchaWidth*captchaHeight/28)

	return encodePNG(img)
}

// fitGlyphScale 计算点阵放大倍数，使 n 个字符连同间距、边距能放进画布。
func fitGlyphScale(n, spacing int) int {
	availW := captchaWidth - 2*glyphMarginX - spacing*(n-1)
	if availW <= 0 {
		return 0
	}
	scale := availW / (n * glyphCols)
	if h := (captchaHeight - 2*glyphMarginY) / glyphRows; h < scale {
		scale = h
	}
	if scale > maxGlyphScale {
		scale = maxGlyphScale
	}
	return scale
}

// randomOffsetY 返回字符可用的随机垂直偏移，保证字形仍完整落在画布内。
// 偏移上限同时受字形高度的四分之一约束，避免相邻字符高低错落得过于夸张。
func randomOffsetY(r *rand.Rand, startY, glyphH int) int {
	up := startY - glyphMarginY
	down := captchaHeight - glyphMarginY - (startY + glyphH)
	limit := up
	if down < limit {
		limit = down
	}
	if jitter := glyphH / 4; jitter < limit {
		limit = jitter
	}
	if limit <= 0 {
		return 0
	}
	return r.Intn(2*limit+1) - limit
}

// randomShear 返回 [-maxShear, maxShear] 之间的随机错切比例。
func randomShear(r *rand.Rand) float64 {
	return (r.Float64()*2 - 1) * maxShear
}

// drawGlyph 按 scale 倍放大绘制字形，并按 shear 做逐行水平错切。
// shear 表示每行像素相对字形垂直中心的水平偏移比例，用来模拟倾斜，
// 偏移以字形中心为基准，避免字符被推出画布。
func drawGlyph(img *image.RGBA, g glyph, ox, oy, scale int, c color.RGBA, shear float64) {
	glyphH := glyphRows * scale
	for gy := 0; gy < glyphRows; gy++ {
		for gx := 0; gx < glyphCols; gx++ {
			if g[gy]&(1<<uint(glyphCols-1-gx)) == 0 {
				continue
			}
			for py := 0; py < scale; py++ {
				y := oy + gy*scale + py
				dx := int(math.Round(shear * float64(gy*scale+py-glyphH/2)))
				for px := 0; px < scale; px++ {
					setPixel(img, ox+gx*scale+px+dx, y, c)
				}
			}
		}
	}
}

// drawCaptchaBackdrop 绘制浅色渐变背景和浅色底纹。
// 底纹颜色很浅（灰度 200 以上），只增加纹理，不影响文字识别。
func drawCaptchaBackdrop(r *rand.Rand, img *image.RGBA) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	c1 := color.RGBA{R: uint8(238 + r.Intn(18)), G: uint8(240 + r.Intn(16)), B: uint8(246 + r.Intn(10)), A: 0xFF}
	c2 := color.RGBA{R: uint8(214 + r.Intn(22)), G: uint8(220 + r.Intn(20)), B: uint8(232 + r.Intn(16)), A: 0xFF}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			t := float64(y)/float64(h-1)*0.8 + float64(x)/float64(w-1)*0.2
			img.SetRGBA(x, y, lerpRGBA(c1, c2, t))
		}
	}
	// 浅色底纹：几段淡灰曲线 + 两三个淡色圆。
	for i := 0; i < 4+r.Intn(3); i++ {
		drawCurve(img,
			randPoint(r, w, h), randPoint(r, w, h), randPoint(r, w, h),
			color.RGBA{R: 0xC8, G: 0xCE, B: 0xDC, A: 0xFF}, 150, 1+r.Intn(2))
	}
	for i := 0; i < 1+r.Intn(2); i++ {
		fillCircle(img, float64(r.Intn(w)), float64(r.Intn(h)),
			float64(10+r.Intn(18)), color.RGBA{R: 0xD8, G: 0xDF, B: 0xEE, A: 0xFF}, 60)
	}
}

// drawInterference 在文字之上叠加若干条随机曲线，进一步干扰机器识别。
// 透明度控制在 120/255 左右，人眼仍能透过线条看清文字。
func drawInterference(r *rand.Rand, img *image.RGBA) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	for i := 0; i < 4+r.Intn(3); i++ {
		drawCurve(img,
			randPoint(r, w, h), randPoint(r, w, h), randPoint(r, w, h),
			randInkColor(r), 120, 1+r.Intn(2))
	}
}

// drawSpeckles 在整幅图上撒随机噪点：深浅交替、透明度较低，
// 既提高机器识别难度，又不会掩盖文字与图形。
func drawSpeckles(r *rand.Rand, img *image.RGBA, count int) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	for i := 0; i < count; i++ {
		x, y := r.Intn(w), r.Intn(h)
		if r.Intn(2) == 0 {
			blendPixel(img, x, y, color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}, uint8(50+r.Intn(80)))
		} else {
			blendPixel(img, x, y, randInkColor(r), uint8(25+r.Intn(45)))
		}
	}
}

// ---------------------------------------------------------------------------
// 公开 API：滑动验证码
// ---------------------------------------------------------------------------

// NewSlider 生成滑动验证码。
//
//	bgPNG    背景图（宽 width、高 height）
//	piecePNG 拼图块（尺寸恰好 pieceSize*2 x pieceSize*2，周围透明，
//	         形状与背景上的缺口完全一致）
//	targetX  拼图块左边缘在背景中的正确横坐标
//	targetY  拼图块上边缘的正确纵坐标（前端把它作为固定纵向位置）
//
// 透明边距已经包含在 piecePNG 内部：调用方只要把 piecePNG 整体绘制到
// 背景的 (targetX, targetY) 位置（例如 canvas 的 drawImage(piece, targetX, targetY)），
// 就能与背景上的缺口完全重合。拼图形状相对图片左上角有 bump 像素的内缩，
// 因此可见形状的左边缘位于 targetX+bump，判断拖动位置请统一使用 targetX。
//
// rnd 由调用方提供以便测试可复现，允许为 nil。
func NewSlider(rnd *rand.Rand, width, height, pieceSize int) (bgPNG, piecePNG []byte, targetX, targetY int, err error) {
	if pieceSize < minPieceSize {
		return nil, nil, 0, 0, fmt.Errorf("captchaimg: 拼图块尺寸 %d 小于最小值 %d", pieceSize, minPieceSize)
	}
	canvasSize := pieceSize * 2
	if width < canvasSize+2*sliderMargin || height < canvasSize+2*sliderMargin {
		return nil, nil, 0, 0, fmt.Errorf("captchaimg: 背景 %dx%d 太小，放不下 %d 像素的拼图块（每边至少留 %d 像素边距）",
			width, height, canvasSize, sliderMargin)
	}
	r := newRand(rnd)

	// 1. 背景：渐变底色 + 随机半透明几何图形 + 轻噪点。
	bg := image.NewRGBA(image.Rect(0, 0, width, height))
	drawSliderBackdrop(r, bg)

	// 2. 拼图形状蒙版：基础方块 + 上边凸起半圆 + 右边凸起半圆，带抗锯齿。
	bump := pieceSize / 5
	if bump < 2 {
		bump = 2
	}
	mask := buildPieceMask(pieceSize, bump, canvasSize)

	// 3. 随机目标位置，保证整个拼图块（含凸起）完整落在背景内并留出边距。
	targetX = sliderMargin + r.Intn(width-canvasSize-2*sliderMargin+1)
	targetY = sliderMargin + r.Intn(height-canvasSize-2*sliderMargin+1)

	// 4. 抠出拼图块：源是与 piecePNG 等大的背景区域，
	//    蒙版之外保持完全透明，蒙版之内逐像素拷贝背景内容。
	piece := image.NewRGBA(image.Rect(0, 0, canvasSize, canvasSize))
	draw.DrawMask(piece, piece.Bounds(), bg, image.Pt(targetX, targetY), mask, image.Point{}, draw.Src)

	// 5. 背景上的缺口：半透明压暗 + 浅色描边，让用户看得出该拼到哪里。
	hole := image.Rect(targetX, targetY, targetX+canvasSize, targetY+canvasSize)
	draw.DrawMask(bg, hole, image.NewUniform(color.RGBA{A: sliderHoleShade}), image.Point{}, mask, image.Point{}, draw.Over)
	strokeMask(bg, mask, targetX, targetY)

	if bgPNG, err = encodePNG(bg); err != nil {
		return nil, nil, 0, 0, err
	}
	if piecePNG, err = encodePNG(piece); err != nil {
		return nil, nil, 0, 0, err
	}
	return bgPNG, piecePNG, targetX, targetY, nil
}

// drawSliderBackdrop 绘制滑动验证码背景：渐变底色 + 若干随机半透明几何图形 +
// 淡色曲线纹理 + 轻噪点，避免背景过于单调而被模板匹配。
func drawSliderBackdrop(r *rand.Rand, img *image.RGBA) {
	w, h := img.Rect.Dx(), img.Rect.Dy()

	// 渐变底色：随机取两种颜色，随机选择竖直或水平为主的渐变方向。
	i1 := r.Intn(len(sliderPalette))
	i2 := (i1 + 1 + r.Intn(len(sliderPalette)-1)) % len(sliderPalette)
	c1, c2 := sliderPalette[i1], sliderPalette[i2]
	vertical := r.Intn(2) == 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var t float64
			if vertical {
				t = float64(y)/float64(h-1)*0.85 + float64(x)/float64(w-1)*0.15
			} else {
				t = float64(x)/float64(w-1)*0.85 + float64(y)/float64(h-1)*0.15
			}
			img.SetRGBA(x, y, lerpRGBA(c1, c2, t))
		}
	}

	// 随机半透明圆 / 矩形 / 三角形。
	for i := 0; i < 8+r.Intn(6); i++ {
		c := sliderPalette[r.Intn(len(sliderPalette))]
		alpha := uint8(30 + r.Intn(50))
		switch r.Intn(3) {
		case 0:
			radius := 8 + r.Intn(10+h/2)
			fillCircle(img, float64(r.Intn(w)), float64(r.Intn(h)), float64(radius), c, alpha)
		case 1:
			rw := 16 + r.Intn(16+w/2)
			rh := 16 + r.Intn(16+h/2)
			x0 := r.Intn(w) - rw/2
			y0 := r.Intn(h) - rh/2
			fillRect(img, image.Rect(x0, y0, x0+rw, y0+rh), c, alpha)
		default:
			fillTriangle(img, randPoint(r, w, h), randPoint(r, w, h), randPoint(r, w, h), c, alpha)
		}
	}

	// 淡色曲线纹理，增加背景层次。
	for i := 0; i < 3+r.Intn(3); i++ {
		drawCurve(img, randPoint(r, w, h), randPoint(r, w, h), randPoint(r, w, h),
			color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}, uint8(40+r.Intn(30)), 1+r.Intn(2))
	}

	drawSpeckles(r, img, w*h/220)
}

// buildPieceMask 生成拼图形状的抗锯齿蒙版（尺寸 size x size）。
//
// 形状 = 边长 pieceSize 的基础方块，加上上边中点和右边中点的半圆凸起；
// 整个形状在蒙版内的偏移为 (bump, bump)，即形状的凸起刚好贴着图片左上角，
// 因此在把蒙版区域整体抠成 piecePNG 之后，透明边距即为凸起本身，
// 调用方按 (targetX, targetY) 摆放图片时缺口能精确对齐。
func buildPieceMask(pieceSize, bump, size int) *image.Alpha {
	mask := image.NewAlpha(image.Rect(0, 0, size, size))
	const sub = 4 // 每个像素做 4x4 超采样，得到平滑边缘
	side := float64(pieceSize)
	radius := float64(bump)
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			hits := 0
			for sy := 0; sy < sub; sy++ {
				for sx := 0; sx < sub; sx++ {
					lx := float64(px-bump) + (float64(sx)+0.5)/sub
					ly := float64(py-bump) + (float64(sy)+0.5)/sub
					if insidePiece(lx, ly, side, radius) {
						hits++
					}
				}
			}
			if hits == 0 {
				continue
			}
			mask.SetAlpha(px, py, color.Alpha{A: uint8(hits * 255 / (sub * sub))})
		}
	}
	return mask
}

// insidePiece 判断以基础方块左上角为原点的局部坐标 (lx,ly) 是否落在拼图形状内。
func insidePiece(lx, ly, side, radius float64) bool {
	if lx >= 0 && lx < side && ly >= 0 && ly < side {
		return true
	}
	if dx, dy := lx-side/2, ly; dx*dx+dy*dy <= radius*radius { // 上边凸起
		return true
	}
	if dx, dy := lx-side, ly-side/2; dx*dx+dy*dy <= radius*radius { // 右边凸起
		return true
	}
	return false
}

// strokeMask 用浅色沿蒙版轮廓描一圈边，让背景上的缺口更醒目。
func strokeMask(dst *image.RGBA, mask *image.Alpha, ox, oy int) {
	b := mask.Bounds()
	edge := color.RGBA{R: 0xF2, G: 0xF6, B: 0xFF, A: 0xFF}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if mask.AlphaAt(x, y).A < 128 {
				continue
			}
			// 四邻域都是实心像素则属于内部，不描边。
			if mask.AlphaAt(x-1, y).A >= 128 && mask.AlphaAt(x+1, y).A >= 128 &&
				mask.AlphaAt(x, y-1).A >= 128 && mask.AlphaAt(x, y+1).A >= 128 {
				continue
			}
			blendPixel(dst, ox+x, oy+y, edge, 170)
		}
	}
}

// ---------------------------------------------------------------------------
// 基础绘图工具
// ---------------------------------------------------------------------------

// pointF 是浮点坐标点，用于几何图形绘制。
type pointF struct{ x, y float64 }

// newRand 返回可用的随机源：调用方传 nil 时按当前时间播种。
func newRand(rnd *rand.Rand) *rand.Rand {
	if rnd != nil {
		return rnd
	}
	return rand.New(rand.NewSource(time.Now().UnixNano()))
}

// randPoint 返回画布内的一个随机浮点坐标。
func randPoint(r *rand.Rand, w, h int) pointF {
	return pointF{x: float64(r.Intn(w)), y: float64(r.Intn(h))}
}

// encodePNG 把图像编码为 PNG 字节。
func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("captchaimg: PNG 编码失败: %w", err)
	}
	return buf.Bytes(), nil
}

// setPixel 直接把颜色写到 (x,y)，越界时忽略。
func setPixel(img *image.RGBA, x, y int, c color.RGBA) {
	if !(image.Point{X: x, Y: y}.In(img.Rect)) {
		return
	}
	i := img.PixOffset(x, y)
	img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, 0xFF
}

// blendPixel 把颜色 c 以 alpha 不透明度叠加到 (x,y)，越界或完全透明时忽略。
// 目标画布都是不透明的，因此叠加后 alpha 保持 255。
func blendPixel(img *image.RGBA, x, y int, c color.RGBA, alpha uint8) {
	if alpha == 0 || !(image.Point{X: x, Y: y}.In(img.Rect)) {
		return
	}
	if alpha == 0xFF {
		setPixel(img, x, y, c)
		return
	}
	a := float64(alpha) / 255
	i := img.PixOffset(x, y)
	p := img.Pix[i : i+4 : i+4]
	p[0] = clampUint8(int(float64(c.R)*a + float64(p[0])*(1-a) + 0.5))
	p[1] = clampUint8(int(float64(c.G)*a + float64(p[1])*(1-a) + 0.5))
	p[2] = clampUint8(int(float64(c.B)*a + float64(p[2])*(1-a) + 0.5))
	p[3] = 0xFF
}

// drawLine 用 Bresenham 算法画一条带厚度的直线，越界像素自动忽略。
func drawLine(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA, alpha uint8, thickness int) {
	if thickness < 1 {
		thickness = 1
	}
	half := thickness / 2
	dx := absInt(x1 - x0)
	dy := -absInt(y1 - y0)
	sx, sy := 1, 1
	if x0 > x1 {
		sx = -1
	}
	if y0 > y1 {
		sy = -1
	}
	errAcc := dx + dy
	for {
		for oy := -half; oy < thickness-half; oy++ {
			for ox := -half; ox < thickness-half; ox++ {
				blendPixel(img, x0+ox, y0+oy, c, alpha)
			}
		}
		if x0 == x1 && y0 == y1 {
			return
		}
		e2 := 2 * errAcc
		if e2 >= dy {
			errAcc += dy
			x0 += sx
		}
		if e2 <= dx {
			errAcc += dx
			y0 += sy
		}
	}
}

// drawCurve 用二次贝塞尔曲线绘制平滑的干扰线 / 底纹（采样后分段画直线）。
func drawCurve(img *image.RGBA, p0, ctrl, p1 pointF, c color.RGBA, alpha uint8, thickness int) {
	const steps = 24
	prevX, prevY := int(math.Round(p0.x)), int(math.Round(p0.y))
	for i := 1; i <= steps; i++ {
		t := float64(i) / steps
		mt := 1 - t
		x := mt*mt*p0.x + 2*mt*t*ctrl.x + t*t*p1.x
		y := mt*mt*p0.y + 2*mt*t*ctrl.y + t*t*p1.y
		curX, curY := int(math.Round(x)), int(math.Round(y))
		drawLine(img, prevX, prevY, curX, curY, c, alpha, thickness)
		prevX, prevY = curX, curY
	}
}

// fillRect 用半透明颜色填充矩形，超出画布的部分自动裁剪。
func fillRect(img *image.RGBA, rect image.Rectangle, c color.RGBA, alpha uint8) {
	rect = rect.Intersect(img.Rect)
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			blendPixel(img, x, y, c, alpha)
		}
	}
}

// fillCircle 填充圆形，边缘用覆盖率做一像素抗锯齿。
func fillCircle(img *image.RGBA, cx, cy, radius float64, c color.RGBA, alpha uint8) {
	if radius <= 0 {
		return
	}
	b := img.Rect
	minX := maxInt(b.Min.X, int(math.Floor(cx-radius)))
	maxX := minInt(b.Max.X-1, int(math.Ceil(cx+radius)))
	minY := maxInt(b.Min.Y, int(math.Floor(cy-radius)))
	maxY := minInt(b.Max.Y-1, int(math.Ceil(cy+radius)))
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			d := math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy)
			cov := radius - d + 0.5
			if cov <= 0 {
				continue
			}
			if cov > 1 {
				cov = 1
			}
			blendPixel(img, x, y, c, uint8(float64(alpha)*cov+0.5))
		}
	}
}

// fillTriangle 用半平面判定填充三角形（2x2 超采样抗锯齿），顶点为浮点坐标。
func fillTriangle(img *image.RGBA, p0, p1, p2 pointF, c color.RGBA, alpha uint8) {
	b := img.Rect
	minX := maxInt(b.Min.X, int(math.Floor(math.Min(p0.x, math.Min(p1.x, p2.x)))))
	maxX := minInt(b.Max.X-1, int(math.Ceil(math.Max(p0.x, math.Max(p1.x, p2.x)))))
	minY := maxInt(b.Min.Y, int(math.Floor(math.Min(p0.y, math.Min(p1.y, p2.y)))))
	maxY := minInt(b.Max.Y-1, int(math.Ceil(math.Max(p0.y, math.Max(p1.y, p2.y)))))
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			hits := 0
			for sy := 0; sy < 2; sy++ {
				for sx := 0; sx < 2; sx++ {
					px := float64(x) + 0.25 + 0.5*float64(sx)
					py := float64(y) + 0.25 + 0.5*float64(sy)
					if pointInTriangle(px, py, p0, p1, p2) {
						hits++
					}
				}
			}
			if hits == 0 {
				continue
			}
			blendPixel(img, x, y, c, uint8(int(alpha)*hits/4))
		}
	}
}

// pointInTriangle 用叉积符号判断点是否在三角形内（含边界）。
func pointInTriangle(px, py float64, a, b, c pointF) bool {
	d1 := crossProduct(px, py, a, b)
	d2 := crossProduct(px, py, b, c)
	d3 := crossProduct(px, py, c, a)
	hasNeg := d1 < 0 || d2 < 0 || d3 < 0
	hasPos := d1 > 0 || d2 > 0 || d3 > 0
	return !(hasNeg && hasPos)
}

// crossProduct 返回 (p-a) 与 (b-a) 的叉积 z 分量。
func crossProduct(px, py float64, a, b pointF) float64 {
	return (b.x-a.x)*(py-a.y) - (b.y-a.y)*(px-a.x)
}

// lerpRGBA 在两个颜色之间线性插值，t 会被限制到 [0,1]。
func lerpRGBA(a, b color.RGBA, t float64) color.RGBA {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	return color.RGBA{
		R: clampUint8(int(float64(a.R) + (float64(b.R)-float64(a.R))*t + 0.5)),
		G: clampUint8(int(float64(a.G) + (float64(b.G)-float64(a.G))*t + 0.5)),
		B: clampUint8(int(float64(a.B) + (float64(b.B)-float64(a.B))*t + 0.5)),
		A: 0xFF,
	}
}

// clampUint8 把整数限制到 [0,255]。
func clampUint8(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// minInt / maxInt / absInt 是小工具函数，避免引入 math 之外的依赖。
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
