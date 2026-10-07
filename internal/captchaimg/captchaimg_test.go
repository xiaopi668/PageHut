package captchaimg

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 测试小工具
// ---------------------------------------------------------------------------

// decodePNG 解码 PNG 字节，失败直接终止测试。
func decodePNG(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("PNG 解码失败: %v", err)
	}
	return img
}

// nrgbaAt 返回图像在 (x,y) 处未预乘的 8 位 RGBA，便于跨图像类型比较。
func nrgbaAt(img image.Image, x, y int) color.NRGBA {
	return color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
}

// luminance 返回像素的人眼感知亮度（0-255），完全透明的像素按背景（255）处理。
func luminance(img image.Image, x, y int) int {
	c := nrgbaAt(img, x, y)
	if c.A == 0 {
		return 255
	}
	return (299*int(c.R) + 587*int(c.G) + 114*int(c.B)) / 1000
}

// logASCII 把图像渲染成 ASCII 打到测试日志（取每个 step x step 区块里最暗的像素），
// 便于在没有图形界面的环境里人工核对文字/形状是否可辨认。
func logASCII(t *testing.T, title string, img image.Image, step int) {
	t.Helper()
	b := img.Bounds()
	var sb strings.Builder
	sb.WriteString(title)
	sb.WriteByte('\n')
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			lum := 255
			for dy := 0; dy < step && y+dy < b.Max.Y; dy++ {
				for dx := 0; dx < step && x+dx < b.Max.X; dx++ {
					if l := luminance(img, x+dx, y+dy); l < lum {
						lum = l
					}
				}
			}
			switch {
			case lum < 100:
				sb.WriteByte('#')
			case lum < 185:
				sb.WriteByte('+')
			default:
				sb.WriteByte('.')
			}
		}
		sb.WriteByte('\n')
	}
	t.Log(sb.String())
}

// logAlphaASCII 把图像的 alpha 通道渲染成 ASCII，用于核对拼图块的不透明形状。
func logAlphaASCII(t *testing.T, title string, img image.Image, step int) {
	t.Helper()
	b := img.Bounds()
	var sb strings.Builder
	sb.WriteString(title)
	sb.WriteByte('\n')
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			best := uint8(0)
			for dy := 0; dy < step && y+dy < b.Max.Y; dy++ {
				for dx := 0; dx < step && x+dx < b.Max.X; dx++ {
					if a := nrgbaAt(img, x+dx, y+dy).A; a > best {
						best = a
					}
				}
			}
			switch {
			case best >= 200:
				sb.WriteByte('#')
			case best >= 60:
				sb.WriteByte('+')
			default:
				sb.WriteByte('.')
			}
		}
		sb.WriteByte('\n')
	}
	t.Log(sb.String())
}

// countDark / countOpaque 统计满足条件的像素数量。
func countDark(img image.Image) int {
	b := img.Bounds()
	n := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if luminance(img, x, y) < 120 {
				n++
			}
		}
	}
	return n
}

func countOpaque(img image.Image) int {
	b := img.Bounds()
	n := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if nrgbaAt(img, x, y).A > 0 {
				n++
			}
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// 点阵字形
// ---------------------------------------------------------------------------

// TestGlyphBitmapNonEmptyAndDistinct 断言 0-9 每个字形都有实心像素、
// 尺寸固定为 5x7，并且两两形状互不相同（避免把两个数字画成同一个样子）。
func TestGlyphBitmapNonEmptyAndDistinct(t *testing.T) {
	seen := make(map[string]byte, len(digitGlyphs))
	rows := make([][glyphRows]string, len(digitGlyphs))

	for d := 0; d < len(digitGlyphs); d++ {
		g := glyphFor(byte('0' + d))
		rows[d] = glyphRowsOf(g)

		on := 0
		for y := 0; y < glyphRows; y++ {
			for x := 0; x < glyphCols; x++ {
				if g[y]&(1<<uint(glyphCols-1-x)) != 0 {
					on++
				}
			}
		}
		if on == 0 {
			t.Fatalf("数字 %d 的字形没有任何实心像素", d)
		}
		if on < 8 {
			t.Fatalf("数字 %d 的字形只有 %d 个实心像素，形状过于残缺", d, on)
		}
		// 每一行都必须有像素，否则字形会出现断裂的横条
		for y := 0; y < glyphRows; y++ {
			if g[y]&0b11111 == 0 {
				t.Fatalf("数字 %d 的第 %d 行为空", d, y)
			}
		}
		key := strings.Join(rows[d][:], "/")
		if prev, ok := seen[key]; ok {
			t.Fatalf("数字 %d 与数字 %d 的字形完全相同", d, prev)
		}
		seen[key] = byte(d)
	}

	if len(seen) != 10 {
		t.Fatalf("字形去重后只剩 %d 种，期望 10 种", len(seen))
	}

	// 未收录的字符使用占位字形，且必须与所有数字都不同
	unknownRows := glyphRowsOf(glyphFor('X'))
	unknown := strings.Join(unknownRows[:], "/")
	if _, ok := seen[unknown]; ok {
		t.Fatal("占位字形与某个数字字形相同")
	}

	// 把点阵打到日志，便于人工核对每个数字的形状
	for d := 0; d < len(digitGlyphs); d++ {
		t.Logf("数字 %d:\n%s", d, strings.Join(rows[d][:], "\n"))
	}
}

// ---------------------------------------------------------------------------
// 图形验证码
// ---------------------------------------------------------------------------

// renderCleanCode 用与本包相同的字形绘制路径渲染一段明文（只画字形，
// 不叠加底纹、干扰线和噪点），用于把 "字形渲染是否正确" 与
// "干扰强度是否合适" 分开核对。布局参数与 NewImage 保持一致。
func renderCleanCode(r *rand.Rand, code string) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, captchaWidth, captchaHeight))
	for y := 0; y < captchaHeight; y++ {
		for x := 0; x < captchaWidth; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF})
		}
	}
	scale := fitGlyphScale(len(code), glyphSpacing)
	if scale < 2 {
		return img
	}
	glyphW, glyphH := glyphCols*scale, glyphRows*scale
	totalW := len(code)*glyphW + (len(code)-1)*glyphSpacing
	startX := (captchaWidth - totalW) / 2
	startY := (captchaHeight - glyphH) / 2
	for i := 0; i < len(code); i++ {
		x := startX + i*(glyphW+glyphSpacing)
		y := startY + randomOffsetY(r, startY, glyphH)
		drawGlyph(img, glyphFor(code[i]), x, y, scale, color.RGBA{A: 0xFF}, randomShear(r))
	}
	return img
}

// TestDrawGlyphMatchesBitmap 断言 drawGlyph 忠实地把点阵字形按 scale 倍放大：
// 无错切时点亮的位置恰好对应一个 scale x scale 的实心方块，其余位置保持背景。
func TestDrawGlyphMatchesBitmap(t *testing.T) {
	const scale = 3
	for _, ch := range []byte{'0', '3', '8', '9', 'X'} {
		g := glyphFor(ch)
		w, h := glyphCols*scale, glyphRows*scale
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		drawGlyph(img, g, 0, 0, scale, color.RGBA{A: 0xFF}, 0)

		for y := 0; y < glyphRows; y++ {
			for x := 0; x < glyphCols; x++ {
				on := g[y]&(1<<uint(glyphCols-1-x)) != 0
				for py := 0; py < scale; py++ {
					for px := 0; px < scale; px++ {
						alpha := nrgbaAt(img, x*scale+px, y*scale+py).A
						if on && alpha != 0xFF {
							t.Fatalf("字形 %q 点阵(%d,%d) 应点亮，(像素 %d,%d) alpha=%d",
								ch, x, y, x*scale+px, y*scale+py, alpha)
						}
						if !on && alpha != 0 {
							t.Fatalf("字形 %q 点阵(%d,%d) 应留空，(像素 %d,%d) alpha=%d",
								ch, x, y, x*scale+px, y*scale+py, alpha)
						}
					}
				}
			}
		}
	}
}

// TestDrawGlyphShear 断言错切是逐行水平偏移：实心像素总数不变，
// 每一行相对上一行的偏移单调不减（正错切时越往下越靠右）。
func TestDrawGlyphShear(t *testing.T) {
	const scale = 2
	g := glyphFor('4')
	w, h := 120, glyphRows*scale
	plain := image.NewRGBA(image.Rect(0, 0, w, h))
	drawGlyph(plain, g, 30, 0, scale, color.RGBA{A: 0xFF}, 0)

	const shear = 0.5
	skewed := image.NewRGBA(image.Rect(0, 0, w, h))
	drawGlyph(skewed, g, 30, 0, scale, color.RGBA{A: 0xFF}, shear)

	if a, b := countOpaque(plain), countOpaque(skewed); a != b {
		t.Fatalf("错切前后实心像素数不一致：%d vs %d（可能被画布裁掉）", a, b)
	}
	// 逐行比较：错切后的每一行应当恰好是未错切那一行整体水平平移，
	// 且平移量随行号单调不减（正错切时越往下越靠右）。
	prevShift, moved, seen := 0, false, 0
	for y := 0; y < h; y++ {
		plainMin, skewedMin := -1, -1
		for x := 0; x < w; x++ {
			if plainMin < 0 && nrgbaAt(plain, x, y).A != 0 {
				plainMin = x
			}
			if skewedMin < 0 && nrgbaAt(skewed, x, y).A != 0 {
				skewedMin = x
			}
		}
		if plainMin < 0 || skewedMin < 0 {
			t.Fatalf("第 %d 行在错切前后出现空行，字形被破坏", y)
		}
		shift := skewedMin - plainMin
		if seen > 0 {
			if shift < prevShift {
				t.Fatalf("第 %d 行的水平平移量 %d 小于上一行的 %d，错切方向不单调", y, shift, prevShift)
			}
			if shift > prevShift {
				moved = true
			}
		}
		prevShift = shift
		seen++
	}
	if !moved {
		t.Fatal("错切系数非零，但各行没有任何水平位移")
	}
}

// TestNewImageProducesValidPNG 断言输出是合法 PNG、尺寸正确，
// 并且画面里确实有足够多的深色像素（说明字形被画出来了）。
func TestNewImageProducesValidPNG(t *testing.T) {
	codes := []string{"0", "7", "38625", "01234567"}
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			data, err := NewImage(rand.New(rand.NewSource(20240501)), code)
			if err != nil {
				t.Fatalf("NewImage(%q) 返回错误: %v", code, err)
			}
			img := decodePNG(t, data)
			if got := img.Bounds(); got.Dx() != captchaWidth || got.Dy() != captchaHeight {
				t.Fatalf("画布尺寸 = %dx%d，期望 %dx%d", got.Dx(), got.Dy(), captchaWidth, captchaHeight)
			}
			if _, ok := img.(*image.RGBA); !ok {
				// 只是提示，PNG 解码器可能返回 NRGBA 等类型
				t.Logf("解码后的具体类型为 %T（不影响结果）", img)
			}
			dark := countDark(img)
			total := captchaWidth * captchaHeight
			if dark*100/total < 3 {
				t.Fatalf("深色像素只有 %d/%d，字形可能没被画出来", dark, total)
			}
			if dark*100/total > 30 {
				t.Fatalf("深色像素达到 %d/%d，画面可能被干扰线糊住", dark, total)
			}
		})
	}
}

// TestNewImageDeterministicAndRandom 断言同一个种子结果一致（可复现），
// 不同种子结果不同（具备随机性）。
func TestNewImageDeterministicAndRandom(t *testing.T) {
	first, err := NewImage(rand.New(rand.NewSource(99)), "38625")
	if err != nil {
		t.Fatalf("NewImage 返回错误: %v", err)
	}
	second, err := NewImage(rand.New(rand.NewSource(99)), "38625")
	if err != nil {
		t.Fatalf("NewImage 返回错误: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("相同种子生成的图片不一致，随机性不可复现")
	}

	distinct := map[string]bool{}
	for seed := int64(1); seed <= 8; seed++ {
		data, err := NewImage(rand.New(rand.NewSource(seed)), "38625")
		if err != nil {
			t.Fatalf("NewImage 返回错误: %v", err)
		}
		distinct[string(data)] = true
	}
	if len(distinct) != 8 {
		t.Fatalf("8 个不同种子只生成了 %d 种图片，随机性不足", len(distinct))
	}

	// 不同明文必须得到不同图片
	a, err := NewImage(rand.New(rand.NewSource(5)), "11111")
	if err != nil {
		t.Fatalf("NewImage 返回错误: %v", err)
	}
	b, err := NewImage(rand.New(rand.NewSource(5)), "00000")
	if err != nil {
		t.Fatalf("NewImage 返回错误: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("不同验证码内容生成了完全相同的图片")
	}
	// nil 随机源也必须可用
	if _, err := NewImage(nil, "38625"); err != nil {
		t.Fatalf("rnd 为 nil 时 NewImage 返回错误: %v", err)
	}
}

// TestNewImageErrors 覆盖参数校验分支。
func TestNewImageErrors(t *testing.T) {
	if _, err := NewImage(rand.New(rand.NewSource(1)), ""); err == nil {
		t.Fatal("空验证码应当返回错误")
	}
	if _, err := NewImage(rand.New(rand.NewSource(1)), "012345678"); err == nil {
		t.Fatal("超过 8 位的验证码应当返回错误")
	}
}

// TestNewImageWritesSample 把样例 PNG 写到临时目录，并打印 ASCII 预览，
// 方便人工确认 "人眼可辨认"（自动化测试无法真正看图，字形正确性由
// TestGlyphBitmapNonEmptyAndDistinct 保证）。
func TestNewImageWritesSample(t *testing.T) {
	dir := t.TempDir()
	const code = "38625"
	data, err := NewImage(rand.New(rand.NewSource(20260808)), code)
	if err != nil {
		t.Fatalf("NewImage 返回错误: %v", err)
	}
	path := filepath.Join(dir, "captcha.png")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("写入临时样例失败: %v", err)
	}
	t.Logf("样例 PNG 已写入 %s（%d 字节，明文 %s）", path, len(data), code)
	logASCII(t, "图形验证码 ASCII 预览（# = 深色文字/干扰线）:", decodePNG(t, data), 1)

	// 再渲染一张只有字形的干净版本，方便人工确认 5 个数字本身是否可辨认
	clean := renderCleanCode(rand.New(rand.NewSource(20260808)), code)
	if countDark(clean) == 0 {
		t.Fatal("干净渲染版本没有任何深色像素")
	}
	logASCII(t, "同一明文的无干扰渲染预览（# = 字形，仅用于人工核对）:", clean, 1)
}

// ---------------------------------------------------------------------------
// 滑动验证码
// ---------------------------------------------------------------------------

// TestNewSliderProducesValidPNG 断言背景图与拼图块都能解码、尺寸正确、
// targetX/targetY 落在合理范围内、拼图块存在不透明像素且周围透明。
func TestNewSliderProducesValidPNG(t *testing.T) {
	cases := []struct{ w, h, piece int }{
		{320, 160, 50},
		{280, 150, 40},
		{240, 240, 60},
		{24, 24, minPieceSize}, // 刚好满足最小尺寸的边界情况
	}
	for _, tc := range cases {
		bgData, pieceData, tx, ty, err := NewSlider(rand.New(rand.NewSource(20240501)), tc.w, tc.h, tc.piece)
		if err != nil {
			t.Fatalf("NewSlider(%d,%d,%d) 返回错误: %v", tc.w, tc.h, tc.piece, err)
		}
		bg := decodePNG(t, bgData)
		piece := decodePNG(t, pieceData)

		if got := bg.Bounds(); got.Dx() != tc.w || got.Dy() != tc.h {
			t.Fatalf("背景尺寸 = %dx%d，期望 %dx%d", got.Dx(), got.Dy(), tc.w, tc.h)
		}
		canvas := tc.piece * 2
		if got := piece.Bounds(); got.Dx() != canvas || got.Dy() != canvas {
			t.Fatalf("拼图块尺寸 = %dx%d，期望 %dx%d", got.Dx(), got.Dy(), canvas, canvas)
		}
		if tx < sliderMargin || tx+canvas > tc.w-sliderMargin+1 {
			t.Fatalf("targetX=%d 超出合理范围，画布宽 %d、拼图块宽 %d", tx, tc.w, canvas)
		}
		if ty < sliderMargin || ty+canvas > tc.h-sliderMargin+1 {
			t.Fatalf("targetY=%d 超出合理范围，画布高 %d、拼图块高 %d", ty, tc.h, canvas)
		}
		if opaque := countOpaque(piece); opaque == 0 {
			t.Fatal("拼图块没有任何不透明像素")
		} else if opaque > canvas*canvas*3/4 {
			t.Fatalf("拼图块不透明像素 %d 过多，可能没有留出透明边缘", opaque)
		}
		// 拼图块四角应当完全透明（凸起只在上边中点和右边中点）
		for _, p := range []image.Point{{X: 0, Y: 0}, {X: canvas - 1, Y: 0}, {X: 0, Y: canvas - 1}, {X: canvas - 1, Y: canvas - 1}} {
			if a := nrgbaAt(piece, p.X, p.Y).A; a != 0 {
				t.Fatalf("拼图块 (%d,%d) 处 alpha=%d，期望完全透明", p.X, p.Y, a)
			}
		}
	}
}

// TestNewSliderPieceAlignment 通过比对背景缺口处被压暗后的颜色与拼图块颜色，
// 验证 (targetX,targetY) 就是拼图块在背景中的正确位置。
//
// 缺口处的处理是叠加一层 alpha=sliderHoleShade 的黑色，
// 因此背景像素应当等于拼图块像素乘以 (255-sliderHoleShade)/255。
func TestNewSliderPieceAlignment(t *testing.T) {
	bgData, pieceData, tx, ty, err := NewSlider(rand.New(rand.NewSource(20260808)), 320, 160, 50)
	if err != nil {
		t.Fatalf("NewSlider 返回错误: %v", err)
	}
	bg := decodePNG(t, bgData)
	piece := decodePNG(t, pieceData)
	canvas := piece.Bounds().Dx()

	factor := float64(255-sliderHoleShade) / 255
	checked, maxDiff := 0, 0
	for y := 1; y < canvas-1; y++ {
		for x := 1; x < canvas-1; x++ {
			// 只校验 "8 邻域都完全不透明" 的内部像素：
			// 边缘像素带有抗锯齿，并且会被缺口描边修改。
			interior := true
			for dy := -1; dy <= 1 && interior; dy++ {
				for dx := -1; dx <= 1; dx++ {
					if nrgbaAt(piece, x+dx, y+dy).A != 255 {
						interior = false
						break
					}
				}
			}
			if !interior {
				continue
			}
			p := nrgbaAt(piece, x, y)
			b := nrgbaAt(bg, tx+x, ty+y)
			for _, diff := range []int{
				absInt(int(b.R) - int(float64(p.R)*factor+0.5)),
				absInt(int(b.G) - int(float64(p.G)*factor+0.5)),
				absInt(int(b.B) - int(float64(p.B)*factor+0.5)),
			} {
				if diff > maxDiff {
					maxDiff = diff
				}
				if diff > 6 {
					t.Fatalf("拼图块像素 (%d,%d) 与背景缺口对不上：块=%v 背景=%v，通道偏差 %d 超过容差",
						x, y, p, b, diff)
				}
			}
			checked++
		}
	}
	if checked < 200 {
		t.Fatalf("参与对齐校验的拼图内部像素只有 %d 个，样本过少", checked)
	}
	t.Logf("对齐校验通过：%d 个内部像素，最大通道偏差 %d", checked, maxDiff)
}

// TestNewSliderRandomness 断言多次调用得到的坐标与图片内容会变化。
func TestNewSliderRandomness(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	xs := map[int]bool{}
	ys := map[int]bool{}
	images := map[string]bool{}
	const rounds = 12
	for i := 0; i < rounds; i++ {
		bg, piece, tx, ty, err := NewSlider(r, 320, 160, 50)
		if err != nil {
			t.Fatalf("NewSlider 返回错误: %v", err)
		}
		xs[tx] = true
		ys[ty] = true
		images[string(bg)+string(piece)] = true
	}
	if len(xs) < 2 {
		t.Fatalf("%d 次调用的 targetX 只有 %d 种取值，缺乏随机性", rounds, len(xs))
	}
	if len(ys) < 2 {
		t.Fatalf("%d 次调用的 targetY 只有 %d 种取值，缺乏随机性", rounds, len(ys))
	}
	if len(images) != rounds {
		t.Fatalf("%d 次调用只生成了 %d 种图片", rounds, len(images))
	}

	// 相同种子必须可复现
	a1, p1, x1, y1, err := NewSlider(rand.New(rand.NewSource(3)), 320, 160, 50)
	if err != nil {
		t.Fatalf("NewSlider 返回错误: %v", err)
	}
	a2, p2, x2, y2, err := NewSlider(rand.New(rand.NewSource(3)), 320, 160, 50)
	if err != nil {
		t.Fatalf("NewSlider 返回错误: %v", err)
	}
	if !bytes.Equal(a1, a2) || !bytes.Equal(p1, p2) || x1 != x2 || y1 != y2 {
		t.Fatal("相同种子生成的滑动验证码不一致，随机性不可复现")
	}

	// nil 随机源也必须可用
	if _, _, _, _, err := NewSlider(nil, 320, 160, 50); err != nil {
		t.Fatalf("rnd 为 nil 时 NewSlider 返回错误: %v", err)
	}
}

// TestNewSliderBoundsAcrossSeeds 在多个种子下检查坐标始终把拼图块完整包在背景内。
func TestNewSliderBoundsAcrossSeeds(t *testing.T) {
	const w, h, piece = 300, 170, 45
	canvas := piece * 2
	for seed := int64(0); seed < 40; seed++ {
		_, _, tx, ty, err := NewSlider(rand.New(rand.NewSource(seed)), w, h, piece)
		if err != nil {
			t.Fatalf("NewSlider 返回错误: %v", err)
		}
		if tx < sliderMargin || tx+canvas > w-sliderMargin {
			t.Fatalf("seed=%d 的 targetX=%d 会让拼图块超出背景（宽 %d）", seed, tx, w)
		}
		if ty < sliderMargin || ty+canvas > h-sliderMargin {
			t.Fatalf("seed=%d 的 targetY=%d 会让拼图块超出背景（高 %d）", seed, ty, h)
		}
	}
}

// TestNewSliderErrors 覆盖参数校验分支。
func TestNewSliderErrors(t *testing.T) {
	if _, _, _, _, err := NewSlider(rand.New(rand.NewSource(1)), 320, 160, minPieceSize-1); err == nil {
		t.Fatal("拼图块过小时应当返回错误")
	}
	if _, _, _, _, err := NewSlider(rand.New(rand.NewSource(1)), 100, 100, 60); err == nil {
		t.Fatal("背景过小时应当返回错误")
	}
	if _, _, _, _, err := NewSlider(rand.New(rand.NewSource(1)), 100, 400, 60); err == nil {
		t.Fatal("背景宽度不足时应当返回错误")
	}
}

// TestNewSliderWritesSample 把样例 PNG 写到临时目录，并打印背景/拼图块的 ASCII 预览。
func TestNewSliderWritesSample(t *testing.T) {
	dir := t.TempDir()
	bgData, pieceData, tx, ty, err := NewSlider(rand.New(rand.NewSource(20260808)), 320, 160, 50)
	if err != nil {
		t.Fatalf("NewSlider 返回错误: %v", err)
	}
	bgPath := filepath.Join(dir, "slider_bg.png")
	piecePath := filepath.Join(dir, "slider_piece.png")
	if err := os.WriteFile(bgPath, bgData, 0o644); err != nil {
		t.Fatalf("写入背景样例失败: %v", err)
	}
	if err := os.WriteFile(piecePath, pieceData, 0o644); err != nil {
		t.Fatalf("写入拼图块样例失败: %v", err)
	}
	t.Logf("样例已写入 %s（%d 字节）与 %s（%d 字节），targetX=%d targetY=%d",
		bgPath, len(bgData), piecePath, len(pieceData), tx, ty)

	logASCII(t, "滑动背景 ASCII 预览（缺口处被压暗）:", decodePNG(t, bgData), 4)
	logAlphaASCII(t, "拼图块 alpha 形状（# = 不透明）:", decodePNG(t, pieceData), 2)
}
