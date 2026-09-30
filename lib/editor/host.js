// farfield editor — the browser host.
//
// The editor is editor.wasm: hand-written WebAssembly that owns the document,
// Markdown styling, font rasterization, layout and every pixel it shows. This
// file is only its host. It loads the module and the fonts, forwards input,
// and copies the framebuffer to a <canvas> when the editor says it drew.
//
// Text input goes through one invisible <textarea> kept under the caret —
// the only way a browser delivers IME composition, dictation, autocorrect and
// a phone's keyboard — and every character that arrives there is handed to
// the editor and cleared. That textarea is input plumbing, not a view: the
// editor never reads the DOM to show anything.
//
//   FarfieldEditor.mount(element, {
//     wasm: "/static/editor/editor.wasm",   // URL or ArrayBuffer
//     fonts: [url|ArrayBuffer × 6],          // serif ×4, mono ×2
//     value: "…", placeholder: "Write…",
//     field: <textarea name="body">,         // kept in sync for form posts
//     onChange(text), onSave(), onFiles(files) → Promise<string[]>?,
//     onCursor(), onBlur(), onKey(event) → true to swallow the key,
//     page: true   // the page is the document: the host grows with the
//                  // text, the canvas sticks in the viewport, and the
//                  // window's own scroll moves through the document
//   }) → Promise<controller>
(function (global) {
  "use strict";

  var KEY = { ArrowLeft: 1, ArrowRight: 2, ArrowUp: 3, ArrowDown: 4, Home: 5, End: 6,
    PageUp: 7, PageDown: 8, Backspace: 9, Delete: 10, Enter: 11, Tab: 12, Escape: 13 };
  var CMD = { bold: 1, italic: 2, code: 3, link: 4, strike: 5, h1: 6, h2: 7, h3: 8,
    quote: 9, bullets: 10, numbers: 11, codeblock: 12, undo: 13, redo: 14,
    selectAll: 15, rule: 16 };
  var isMac = /Mac|iPhone|iPad|iPod/.test(navigator.platform || navigator.userAgent);

  var enc = new TextEncoder();
  var dec = new TextDecoder();

  function bytes(src) {
    if (src instanceof ArrayBuffer) return Promise.resolve(src);
    return fetch(src).then(function (r) {
      if (!r.ok) throw new Error("editor: could not load " + src);
      return r.arrayBuffer();
    });
  }

  // colour of a CSS custom property as 0xRRGGBBAA, resolved by the browser
  // (so color-mix() and friends work) as the computed colour of a probe
  // element. Not via a canvas: reading pixels back is colour-managed and can
  // come back a unit off, which shows as a seam around the editor.
  function cssColor(el, name, fallback) {
    var v = getComputedStyle(el).getPropertyValue(name).trim();
    if (!v) return fallback;
    var probe = document.createElement("span");
    probe.style.cssText = "position:absolute;visibility:hidden;color:" + v;
    el.appendChild(probe);
    var c = getComputedStyle(probe).color;
    probe.remove();
    // rgb(r, g, b) / rgba(…) or color(srgb r g b) with 0–1 channels
    var m = c.match(/rgba?\(\s*([\d.]+)[,\s]+([\d.]+)[,\s]+([\d.]+)/);
    var rgb = m ? [+m[1], +m[2], +m[3]] : null;
    if (!rgb) {
      m = c.match(/color\(srgb\s+([\d.]+)\s+([\d.]+)\s+([\d.]+)/);
      if (m) rgb = [m[1] * 255, m[2] * 255, m[3] * 255];
    }
    if (!rgb) return fallback;
    rgb = rgb.map(function (n) { return Math.max(0, Math.min(255, Math.round(n))); });
    return ((rgb[0] << 24) | (rgb[1] << 16) | (rgb[2] << 8) | 255) >>> 0;
  }
  function blend(a, b, t) { // mix colour b over a by t (0–1)
    function ch(c, s) { return (c >>> s) & 255; }
    var r = Math.round(ch(a, 24) * (1 - t) + ch(b, 24) * t);
    var g = Math.round(ch(a, 16) * (1 - t) + ch(b, 16) * t);
    var bl = Math.round(ch(a, 8) * (1 - t) + ch(b, 8) * t);
    return ((r << 24) | (g << 16) | (bl << 8) | 255) >>> 0;
  }

  function mount(host, opts) {
    opts = opts || {};
    return Promise.all([
      bytes(opts.wasm || "/static/editor/editor.wasm").then(function (b) { return WebAssembly.instantiate(b, {}); }),
      Promise.all((opts.fonts || []).map(bytes)),
    ]).then(function (res) {
      return start(host, res[0].instance.exports, res[1], opts);
    });
  }

  function start(host, x, fonts, opts) {
    x.init();
    var mem = x.memory;
    function io() { return x.io_ptr(); }
    function put(s) {
      var b = enc.encode(s);
      new Uint8Array(mem.buffer, io(), b.length).set(b);
      return b.length;
    }
    function take(n) { return dec.decode(new Uint8Array(mem.buffer, io(), n)); }

    fonts.forEach(function (f, slot) {
      new Uint8Array(mem.buffer, io(), f.byteLength).set(new Uint8Array(f));
      if (!x.font_load(slot, f.byteLength)) console.warn("editor: font " + slot + " did not load");
    });

    // ── surface ──
    host.classList.add("ff-editor");
    var canvas = document.createElement("canvas");
    canvas.setAttribute("role", "presentation");
    var ctx = canvas.getContext("2d", { alpha: false });
    var sink = document.createElement("textarea");
    sink.setAttribute("aria-label", opts.label || "Editor");
    sink.setAttribute("autocapitalize", "sentences");
    sink.setAttribute("spellcheck", "true");
    sink.className = "ff-editor-sink";
    host.appendChild(canvas);
    host.appendChild(sink);
    var page = !!opts.page;
    if (page) { host.classList.add("ff-editor-page"); x.set_page(1); }

    var W = 0, H = 0, dpr = 1, image = null, fb = 0;
    function stickyTop() { return parseFloat(getComputedStyle(canvas).top) || 0; }
    function resize() {
      var r = host.getBoundingClientRect();
      dpr = Math.min(window.devicePixelRatio || 1, 3);
      var cssH = r.height;
      // in page mode the canvas covers only what the viewport can show
      if (page) cssH = Math.max(120, Math.min(r.height, window.innerHeight - stickyTop()));
      var w = Math.max(1, Math.round(r.width * dpr));
      var h = Math.max(1, Math.round(cssH * dpr));
      if (w === W && h === H) return;
      W = w; H = h;
      canvas.width = W;
      canvas.height = H;
      // exactly W×H device pixels on screen — any other CSS size makes the
      // compositor resample the framebuffer, which softens every glyph
      canvas.style.width = (W / dpr) + "px";
      canvas.style.height = (H / dpr) + "px";
      fb = x.resize(W, H, Math.round(dpr * 100));
      image = null; // memory may have grown; views re-made on next paint
      grow();
      follow();
      paint(true);
    }

    // ── page mode: the host is as tall as the document, the canvas is a
    // sticky window onto it, and the editor's scroll is how far the canvas
    // has slid down inside the host. ──
    var synced = 0, tall = 0;
    function grow() {
      if (!page) return;
      // a short document still gets a generous place to click and write;
      // kept under the viewport so the canvas never has to slide over it
      var h = Math.max(x.doc_height(), Math.round(window.innerHeight * 0.45 * dpr));
      if (h === tall) return;
      tall = h;
      host.style.height = Math.ceil(h / dpr) + "px";
    }
    // snap keeps the canvas on whole device pixels. Layout (a sticky offset
    // of 58.69px, a fractional trackpad scroll) can leave it between pixels,
    // and the compositor then samples it with a half-pixel blur.
    var shiftX = 0, shiftY = 0;
    function snap() {
      var r = canvas.getBoundingClientRect();
      var left = r.left - shiftX, top = r.top - shiftY;
      shiftX = (Math.round(left * dpr) - left * dpr) / dpr;
      shiftY = (Math.round(top * dpr) - top * dpr) / dpr;
      canvas.style.transform = (shiftX || shiftY) ? "translate(" + shiftX + "px," + shiftY + "px)" : "";
    }
    function follow() {
      snap();
      if (!page) return;
      var want = Math.round((canvas.getBoundingClientRect().top - shiftY - host.getBoundingClientRect().top) * dpr);
      var now = x.scroll_top();
      if (now !== synced) {
        // the editor scrolled to reveal the caret: move the window with it
        synced = now;
        if (now !== want) window.scrollBy(0, (now - want) / dpr);
        return;
      }
      if (want !== now) { x.set_scroll(want); synced = x.scroll_top(); }
    }
    if (page) window.addEventListener("scroll", function () { follow(); paint(); }, { passive: true });
    function paint(force) {
      var drew = x.render();
      if (!drew && !force) return;
      if (!image || image.data.buffer !== mem.buffer) {
        image = new ImageData(new Uint8ClampedArray(mem.buffer, fb, W * H * 4), W, H);
      }
      ctx.putImageData(image, 0, 0);
      placeSink();
    }

    // ── theme from the page's tokens ──
    function theme() {
      var bg = cssColor(host, "--paper", cssColor(host, "--surface", 0xf3e5d1ff));
      var ink = cssColor(host, "--ink", 0x0e222dff);
      var accent = cssColor(host, "--accent", 0x0d3560ff);
      var colors = [bg, ink, blend(bg, ink, 0.42), accent, blend(bg, ink, 0.08),
        blend(bg, accent, 0.22), accent, blend(bg, ink, 0.38), blend(bg, ink, 0.25), blend(bg, ink, 0.35)];
      colors.forEach(function (c, i) { x.set_color(i, c); });
      paint(true);
    }

    // ── text sync ──
    var rev = -1;
    function text() { return take(x.get_text()); }
    function changed() {
      var r = x.revision();
      if (r === rev) return;
      rev = r;
      var t = text();
      if (opts.field) opts.field.value = t;
      if (opts.onChange) opts.onChange(t);
    }
    function setText(s) {
      var n = put(s);
      x.set_text(n);
      rev = x.revision();
      if (opts.field) opts.field.value = s;
      if (page) { grow(); resize(); follow(); }
      paint(true);
    }
    function insert(s) {
      if (!s) return;
      x.insert_text(put(s));
      after();
    }
    function after() {
      changed();
      if (page) {
        var t = tall;
        grow();
        if (tall !== t) resize(); // a short document's canvas grows with it
        follow();
      }
      paint();
      if (opts.onCursor) opts.onCursor();
    }

    // ── the input sink sits under the caret, so IME popups appear there ──
    function placeSink() {
      var p = x.caret_rect();
      var v = new Int32Array(mem.buffer, p, 4);
      sink.style.left = (v[0] / dpr) + "px";
      sink.style.top = (canvas.offsetTop + v[1] / dpr) + "px";
      sink.style.height = (v[3] / dpr) + "px";
    }

    function mods(e) {
      var m = 0;
      if (e.shiftKey) m |= 1;
      if (isMac ? e.altKey : e.ctrlKey) m |= 2;
      if (isMac ? e.metaKey : e.ctrlKey) m |= 4;
      return m;
    }

    var composing = false;
    sink.addEventListener("compositionstart", function () { composing = true; });
    sink.addEventListener("compositionend", function (e) {
      composing = false;
      insert(e.data || sink.value);
      sink.value = "";
    });

    sink.addEventListener("beforeinput", function (e) {
      if (composing || e.inputType === "insertCompositionText") return;
      var t = e.inputType;
      if (t === "insertText" || t === "insertReplacementText") {
        e.preventDefault();
        insert(e.data != null ? e.data : (e.dataTransfer && e.dataTransfer.getData("text/plain")));
      } else if (t === "insertLineBreak" || t === "insertParagraph") {
        e.preventDefault(); x.key(KEY.Enter, 0); after();
      } else if (t === "deleteContentBackward") {
        e.preventDefault(); x.key(KEY.Backspace, 0); after();
      } else if (t === "deleteContentForward") {
        e.preventDefault(); x.key(KEY.Delete, 0); after();
      } else if (t === "deleteWordBackward") {
        e.preventDefault(); x.key(KEY.Backspace, 2); after();
      } else if (t === "deleteWordForward") {
        e.preventDefault(); x.key(KEY.Delete, 2); after();
      } else if (t === "deleteSoftLineBackward" || t === "deleteHardLineBackward") {
        e.preventDefault(); x.key(KEY.Backspace, 4); after();
      } else if (t === "historyUndo") {
        e.preventDefault(); x.command(CMD.undo); after();
      } else if (t === "historyRedo") {
        e.preventDefault(); x.command(CMD.redo); after();
      } else if (t !== "insertFromPaste" && t !== "insertFromDrop") {
        e.preventDefault();
      }
    });
    // anything that slipped past beforeinput (older engines) arrives here
    sink.addEventListener("input", function () {
      if (composing || !sink.value) return;
      insert(sink.value);
      sink.value = "";
    });

    function shortcut(e) {
      var k = e.key.toLowerCase();
      var primary = isMac ? e.metaKey : e.ctrlKey;
      if (!primary) return 0;
      if (k === "s") { if (opts.onSave) opts.onSave(); return -1; }
      if (e.shiftKey) {
        if (k === "z") return CMD.redo;
        if (k === "x") return CMD.strike;
        if (k === "7" || k === "&") return CMD.numbers;
        if (k === "8" || k === "*") return CMD.bullets;
        if (k === "9" || k === "(") return CMD.quote;
        if (k === "c" && e.altKey) return CMD.codeblock;
        return 0;
      }
      if (e.altKey) {
        if (e.code === "Digit1") return CMD.h1;
        if (e.code === "Digit2") return CMD.h2;
        if (e.code === "Digit3") return CMD.h3;
        if (e.code === "KeyC") return CMD.codeblock;
        return 0;
      }
      if (k === "b") return CMD.bold;
      if (k === "i") return CMD.italic;
      if (k === "e") return CMD.code;
      if (k === "k") return CMD.link;
      if (k === "z") return CMD.undo;
      if (k === "y") return CMD.redo;
      if (k === "a") return CMD.selectAll;
      return 0;
    }

    sink.addEventListener("keydown", function (e) {
      if (composing || e.isComposing) return;
      if (opts.onKey && opts.onKey(e)) { e.preventDefault(); return; }
      var c = shortcut(e);
      if (c) {
        e.preventDefault();
        if (c > 0) { x.command(c); after(); }
        return;
      }
      var k = KEY[e.key];
      if (!k) return;
      if (k === KEY.Tab && opts.captureTab === false) {
        // A host can let Tab move focus instead of indenting.
        return;
      }
      if (x.key(k, mods(e))) { e.preventDefault(); after(); }
    });

    // clipboard
    sink.addEventListener("copy", function (e) {
      var n = x.get_selection();
      if (!n) return;
      e.preventDefault();
      e.clipboardData.setData("text/plain", take(n));
    });
    sink.addEventListener("cut", function (e) {
      var n = x.get_selection();
      if (!n) return;
      e.preventDefault();
      e.clipboardData.setData("text/plain", take(n));
      x.key(KEY.Backspace, 0);
      after();
    });
    sink.addEventListener("paste", function (e) {
      var dt = e.clipboardData;
      e.preventDefault();
      if (dt.files && dt.files.length && opts.onFiles) return files(dt.files);
      insert(dt.getData("text/plain").replace(/\r\n?/g, "\n"));
    });
    function files(list) {
      Promise.resolve(opts.onFiles(Array.prototype.slice.call(list))).then(function (snips) {
        if (snips && snips.length) insert(snips.join("\n\n"));
      });
    }
    host.addEventListener("dragover", function (e) { e.preventDefault(); });
    host.addEventListener("drop", function (e) {
      e.preventDefault();
      var dt = e.dataTransfer;
      var r = canvas.getBoundingClientRect();
      x.pointer(1, Math.round((e.clientX - r.left) * dpr), Math.round((e.clientY - r.top) * dpr), 0, 1);
      x.pointer(3, 0, 0, 0, 0);
      if (dt.files && dt.files.length && opts.onFiles) files(dt.files);
      else insert(dt.getData("text/plain"));
      focus();
    });

    // pointer
    var down = false;
    function pt(e) {
      var r = canvas.getBoundingClientRect();
      return [Math.round((e.clientX - r.left) * dpr), Math.round((e.clientY - r.top) * dpr)];
    }
    canvas.addEventListener("pointerdown", function (e) {
      if (e.button !== 0) return;
      e.preventDefault();
      var p = pt(e);
      if ((isMac ? e.metaKey : e.ctrlKey)) {
        var n = x.link_at(p[0], p[1]);
        if (n) { window.open(take(n), "_blank", "noopener"); return; }
      }
      focus();
      down = true;
      canvas.setPointerCapture(e.pointerId);
      x.pointer(1, p[0], p[1], mods(e), Math.min(e.detail || 1, 3));
      paint();
    });
    canvas.addEventListener("pointermove", function (e) {
      var p = pt(e);
      if (!down) {
        canvas.style.cursor = ((isMac ? e.metaKey : e.ctrlKey) && x.link_at(p[0], p[1])) ? "pointer" : "text";
        return;
      }
      x.pointer(2, p[0], p[1], mods(e), 0);
      follow();
      paint();
    });
    function up() {
      if (!down) return;
      down = false;
      x.pointer(3, 0, 0, 0, 0);
      paint();
      if (opts.onCursor) opts.onCursor();
    }
    canvas.addEventListener("pointerup", up);
    canvas.addEventListener("pointercancel", up);
    // in page mode the wheel belongs to the page
    if (!page) canvas.addEventListener("wheel", function (e) {
      var before = x.scroll_top();
      var dy = e.deltaMode === 1 ? e.deltaY * 32 : e.deltaMode === 2 ? e.deltaY * H / dpr : e.deltaY;
      x.wheel(Math.round(dy * dpr));
      // only claim the wheel when the editor actually scrolled
      if (x.scroll_top() !== before) { e.preventDefault(); paint(); }
    }, { passive: false });

    function focus() { sink.focus({ preventScroll: true }); }
    sink.addEventListener("focus", function () { x.focus(1); host.classList.add("focused"); paint(); });
    sink.addEventListener("blur", function () {
      x.focus(0);
      host.classList.remove("focused");
      paint();
      if (opts.onBlur) opts.onBlur();
    });

    // ── loop: the caret blinks, so the editor is asked each frame ──
    var running = true;
    function frame(t) {
      if (!running) return;
      x.tick(t | 0);
      paint();
      requestAnimationFrame(frame);
    }

    if (typeof ResizeObserver === "function") new ResizeObserver(resize).observe(host);
    else window.addEventListener("resize", resize);
    window.addEventListener("resize", resize); // devicePixelRatio changes (zoom, monitor)
    var dark = window.matchMedia && matchMedia("(prefers-color-scheme: dark)");
    if (dark && dark.addEventListener) dark.addEventListener("change", theme);
    new MutationObserver(theme).observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme", "class"] });

    if (opts.placeholder) x.set_placeholder(put(opts.placeholder));
    resize();
    setText(opts.value != null ? opts.value : (opts.field ? opts.field.value : ""));
    theme();
    requestAnimationFrame(frame);

    return {
      get value() { return text(); },
      set value(s) { setText(s); },
      insert: function (s) { insert(s); focus(); },
      command: function (name) { if (CMD[name]) { x.command(CMD[name]); after(); focus(); } },
      focus: focus,
      words: function () { return x.word_count(); },
      plain: function (on) { x.set_mode(on ? 1 : 0); after(); },
      // the selection in bytes, and its bounds in CSS pixels within the host
      selection: function () {
        var a = x.sel_start(), b = x.sel_end();
        var v = new Int32Array(mem.buffer, x.sel_rect(), 4);
        var top = canvas.offsetTop;
        return { start: a, end: b, empty: a === b,
          x0: v[0] / dpr, y0: top + v[1] / dpr, x1: v[2] / dpr, y1: top + v[3] / dpr };
      },
      caret: function () {
        var v = new Int32Array(mem.buffer, x.caret_rect(), 4);
        return { x: v[0] / dpr, y: canvas.offsetTop + v[1] / dpr, h: v[3] / dpr };
      },
      // the caret's line up to the caret, and the byte where that line starts
      caretLine: function () { var n = x.caret_line(); return { text: take(n), start: x.line_start() }; },
      // replace bytes a..b (or the selection) with s
      replace: function (s, a, b) {
        if (a != null) x.set_selection(a, b);
        if (s) x.insert_text(put(s));
        else if (x.sel_start() !== x.sel_end()) x.key(KEY.Backspace, 0);
        after();
      },
      run: function (name) { if (CMD[name]) { x.command(CMD[name]); after(); } },
      destroy: function () { running = false; host.innerHTML = ""; },
    };
  }

  global.FarfieldEditor = { mount: mount, commands: CMD };
})(window);
