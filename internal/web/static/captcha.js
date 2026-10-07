// PageHut 人机验证前端：图形验证码刷新 + 滑动拼图。
// 安全说明：滑块的目标横坐标只保存在服务端，这里提交的只是用户最终拖到的位置；
// 校验、限次与过期都由服务端处理。
(function () {
  'use strict';

  // ---------- 图形验证码：点击刷新 ----------
  function setupImageRefresh() {
    var imgs = document.querySelectorAll('img.captcha-img');
    Array.prototype.forEach.call(imgs, function (img) {
      img.addEventListener('click', function () {
        var base = img.getAttribute('data-refresh') || img.src;
        img.src = base + (base.indexOf('?') >= 0 ? '&' : '?') + 't=' + Date.now();
      });
    });
  }

  // ---------- 滑动拼图 ----------
  function setupSlider(box) {
    var endpoint = box.getAttribute('data-endpoint');
    var bg = box.querySelector('.slider-bg');
    var piece = box.querySelector('.slider-piece');
    var thumb = box.querySelector('.slider-thumb');
    var track = box.querySelector('.slider-track');
    var canvas = box.querySelector('.slider-canvas');
    var input = box.querySelector('input[name="captcha_x"]');
    if (!endpoint || !bg || !piece || !thumb || !track || !canvas || !input) return;

    var state = { max: 0, x: 0, dragging: false, startX: 0, startLeft: 0 };

    function render() {
      piece.style.left = state.x + 'px';
      var travel = track.clientWidth - thumb.offsetWidth;
      thumb.style.left = (state.max > 0 ? state.x / state.max : 0) * Math.max(0, travel) + 'px';
      input.value = String(Math.round(state.x));
    }

    function load() {
      input.value = '';
      piece.style.visibility = 'hidden';
      var req = new XMLHttpRequest();
      req.open('GET', endpoint, true);
      req.withCredentials = true;
      req.onload = function () {
        if (req.status !== 200) return;
        var d;
        try { d = JSON.parse(req.responseText); } catch (e) { return; }
        bg.src = d.bg;
        piece.src = d.piece;
        canvas.style.width = d.width + 'px';
        canvas.style.height = d.height + 'px';
        state.max = d.width - d.size;
        state.x = 0;
        piece.style.top = d.y + 'px';
        piece.style.visibility = 'visible';
        render();
      };
      req.send();
    }

    function pointerX(e) {
      if (e.touches && e.touches.length) return e.touches[0].clientX;
      return e.clientX;
    }

    function onDown(e) {
      state.dragging = true;
      state.startX = pointerX(e);
      state.startLeft = state.x;
      if (e.preventDefault) e.preventDefault();
    }

    function onMove(e) {
      if (!state.dragging) return;
      var travel = track.clientWidth - thumb.offsetWidth;
      if (travel <= 0 || state.max <= 0) return;
      var x = state.startLeft + (pointerX(e) - state.startX) * (state.max / travel);
      if (x < 0) x = 0;
      if (x > state.max) x = state.max;
      state.x = x;
      render();
      if (e.preventDefault) e.preventDefault();
    }

    function onUp() { state.dragging = false; }

    thumb.addEventListener('mousedown', onDown);
    thumb.addEventListener('touchstart', onDown, { passive: false });
    document.addEventListener('mousemove', onMove);
    document.addEventListener('touchmove', onMove, { passive: false });
    document.addEventListener('mouseup', onUp);
    document.addEventListener('touchend', onUp);

    load();
  }

  function init() {
    setupImageRefresh();
    Array.prototype.forEach.call(document.querySelectorAll('.slider-captcha'), setupSlider);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
