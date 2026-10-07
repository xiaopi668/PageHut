package web

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"pagehut/internal/captchaimg"
)

// 滑动验证码画布尺寸（与前端 JS 保持一致）。
const (
	sliderWidth  = 320
	sliderHeight = 160
	sliderPiece  = 42
)

// handleCaptchaImage 输出图形验证码 PNG，答案保存在服务端挑战存储里，
// cookie 只带一个随机 id。
func (w *Web) handleCaptchaImage(rw http.ResponseWriter, r *http.Request) {
	st, err := w.settings()
	if err != nil || st.CaptchaProvider != captchaImage {
		http.NotFound(rw, r)
		return
	}
	code := randDigits(captchaImageLen)
	png, err := captchaimg.NewImage(nil, code)
	if err != nil {
		log.Printf("[web] 生成图形验证码失败: %v", err)
		http.Error(rw, "captcha error", http.StatusInternalServerError)
		return
	}
	id := captchas.put(code)
	setCookie(rw, captchaCookie, id, int(captchaTTL/time.Second), true, isSecureRequest(r))
	rw.Header().Set("Content-Type", "image/png")
	rw.Header().Set("Cache-Control", "no-store")
	rw.Write(png)
}

// handleCaptchaSlider 输出滑动验证码：背景与拼图块以 data URI 返回，
// 目标横坐标只存在服务端。
func (w *Web) handleCaptchaSlider(rw http.ResponseWriter, r *http.Request) {
	st, err := w.settings()
	if err != nil || st.CaptchaProvider != captchaSlider {
		http.NotFound(rw, r)
		return
	}
	bg, piece, targetX, targetY, err := captchaimg.NewSlider(nil, sliderWidth, sliderHeight, sliderPiece)
	if err != nil {
		log.Printf("[web] 生成滑动验证码失败: %v", err)
		http.Error(rw, "captcha error", http.StatusInternalServerError)
		return
	}
	id := captchas.put(strconv.Itoa(targetX))
	setCookie(rw, captchaCookie, id, int(captchaTTL/time.Second), true, isSecureRequest(r))
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(rw).Encode(map[string]any{
		"bg":        "data:image/png;base64," + base64.StdEncoding.EncodeToString(bg),
		"piece":     "data:image/png;base64," + base64.StdEncoding.EncodeToString(piece),
		"y":         targetY,
		"width":     sliderWidth,
		"height":    sliderHeight,
		"size":      sliderPiece,
		"tolerance": sliderTolerance,
	})
}

// randDigits 生成 n 位数字串（丢弃偏置区间，避免取模偏置）。
func randDigits(n int) string {
	out := make([]byte, 0, n)
	for len(out) < n {
		for _, b := range randBytes(n) {
			if b >= 250 { // 250 = 25*10
				continue
			}
			out = append(out, '0'+b%10)
			if len(out) == n {
				break
			}
		}
	}
	return string(out)
}
