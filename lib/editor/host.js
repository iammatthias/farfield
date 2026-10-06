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
//     blobBase: "https://blobs…",  // resolves ![](blob://cid) images
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
    // ── sizing ──
    // In a box, the canvas is the box. In page mode the host is as tall as
    // the document and the canvas is a TILE inside it — about two screens
    // tall, absolutely positioned. The browser scrolls the tile with the page
    // on its compositor, with no script or wasm in the loop; the editor
    // re-renders only when the view nears the tile's edge (retile).
    var TILE = 2.5;                     // tile height, in viewport heights
    var TILE_BYTES = 32 * 1024 * 1024;  // framebuffer ceiling for the tile
    function viewH() { return (window.visualViewport && window.visualViewport.height) || window.innerHeight; }
    function resize() {
      var r = host.getBoundingClientRect();
      dpr = Math.min(window.devicePixelRatio || 1, 3);
      var w = Math.max(1, Math.round(r.width * dpr));
      var cssH = r.height;
      if (page) {
        cssH = Math.min(r.height, window.innerHeight * TILE);
        cssH = Math.min(cssH, TILE_BYTES / (w * 4) / dpr);
        cssH = Math.max(120, cssH);
      }
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
      retile(true);
      paint(true);
    }

    var tall = 0;
    function grow() {
      if (!page) return;
      // a short document still gets a generous place to click and write
      var h = Math.max(x.doc_height(), Math.round(window.innerHeight * 0.45 * dpr));
      if (h === tall) return;
      tall = h;
      host.style.height = Math.ceil(h / dpr) + "px";
    }

    // retile moves the tile to centre on what is visible when the view has
    // come within a quarter screen of its edge (or when forced), and tells
    // the editor its new top. Between retiles, scrolling costs nothing.
    var tileTop = 0, lastVis = 0; // css px from the host's top
    function retile(force) {
      if (!page) { snap(); return; }
      var hostTop = host.getBoundingClientRect().top;
      var vh = viewH(), th = H / dpr, hostH = tall / dpr;
      // retile when within m of an edge, and place the tile with twice that
      // margin, so a fresh tile is never already "near" (it once retiled on
      // every frame, sitting exactly on the threshold)
      var visTop = -hostTop, visBot = visTop + vh, m = vh * 0.1;
      var down = visTop >= lastVis;
      lastVis = visTop;
      var near = visTop < tileTop + (tileTop > 0 ? m : 0) ||
        visBot > tileTop + th - (tileTop + th < hostH ? m : 0);
      if (force || near) {
        // lay the tile ahead of the scroll: most of it is runway in the
        // direction of travel, so a fling crosses few retiles
        var t = force ? visTop - (th - vh) / 2 : (down ? visTop - 2 * m : visBot + 2 * m - th);
        t = Math.max(0, Math.min(t, hostH - th));
        tileTop = Math.round(Math.max(0, t) * dpr) / dpr;
        canvas.style.top = tileTop + "px";
        x.set_scroll(Math.round(tileTop * dpr));
        snap();
        paint(true);
      }
    }
    // the editor may scroll itself (revealing the caret inside the tile);
    // in page mode the window owns scrolling, so its scroll is put back
    function pin() { if (page && x.scroll_top() !== Math.round(tileTop * dpr)) x.set_scroll(Math.round(tileTop * dpr)); }

    // reveal scrolls the window just enough to keep the caret in view — below
    // the pinned chrome and above the on-screen keyboard.
    function reveal() {
      if (!page) return;
      var v = new Int32Array(mem.buffer, x.caret_rect(), 4);
      var docY = (v[1] + x.scroll_top()) / dpr, ch = v[3] / dpr;
      pin();
      var top = parseFloat(getComputedStyle(host).getPropertyValue("--ff-editor-top")) || 0;
      var y = host.getBoundingClientRect().top + docY, pad = 24;
      var vv = window.visualViewport, bottom = vv ? vv.offsetTop + vv.height : window.innerHeight;
      if (y < top + pad) window.scrollBy(0, y - top - pad);
      else if (y + ch > bottom - pad) window.scrollBy(0, y + ch - bottom + pad);
      retile(false);
    }

    // snap keeps the canvas on whole device pixels; layout can leave it
    // between pixels, and the compositor then samples it with a blur.
    var shiftX = 0, shiftY = 0;
    function snap() {
      var r = canvas.getBoundingClientRect();
      var left = r.left - shiftX, top = r.top - shiftY;
      shiftX = (Math.round(left * dpr) - left * dpr) / dpr;
      shiftY = (Math.round(top * dpr) - top * dpr) / dpr;
      canvas.style.transform = (shiftX || shiftY) ? "translate(" + shiftX + "px," + shiftY + "px)" : "";
    }
    var lastScroll = -1e9; // when the page last scrolled (touch uses it)
    if (page) {
      var ticking = false, settle = 0;
      var onScroll = function () {
        lastScroll = performance.now();
        if (!ticking) {
          ticking = true;
          requestAnimationFrame(function () { ticking = false; retile(false); paint(); });
        }
        // re-snap once scrolling settles (mid-scroll the fraction is moot)
        clearTimeout(settle);
        settle = setTimeout(snap, 120);
      };
      window.addEventListener("scroll", onScroll, { passive: true });
      if (window.visualViewport) window.visualViewport.addEventListener("resize", onScroll);
    }
    function upload(dy, dh) {
      if (dh <= 0) return;
      if (!image || image.data.buffer !== mem.buffer) {
        image = new ImageData(new Uint8ClampedArray(mem.buffer, fb, W * H * 4), W, H);
      }
      ctx.putImageData(image, 0, 0, 0, dy, W, dh);
    }
    // In page mode the tile is taller than the screen, so drawing all of it
    // on every keystroke is waste. `valid` is the band of tile rows that is
    // current: an edit redraws what is on screen (plus half a screen either
    // side), and scrolling fills in the rest as it comes into view.
    var valid = { a: 0, b: 0 }, lastCaretY = -1;
    function wanted() {
      var vh = viewH(), visTop = -host.getBoundingClientRect().top;
      var a = Math.floor((visTop - tileTop - vh * 0.5) * dpr);
      var b = Math.ceil((visTop - tileTop + vh * 1.5) * dpr);
      return { a: Math.max(0, Math.min(H, a)), b: Math.max(0, Math.min(H, b)) };
    }
    function band(a, b) {
      if (b <= a || !x.render_band(a, b)) return null;
      var dy = x.dirty_y(), dh = x.dirty_h();
      upload(dy, dh);
      return { a: dy, b: dy + dh };
    }
    function paint(force) {
      if (!page) {
        var drew = x.render();
        if (!drew && !force) return;
        // upload only the rows the editor touched (a blink is one line)
        if (force) upload(0, H); else upload(x.dirty_y(), x.dirty_h());
        placeSink();
        return;
      }
      if (force) valid = { a: 0, b: 0 };
      var n = wanted(), got;
      if (valid.b <= valid.a) {
        got = band(n.a, n.b);
        if (got) valid = got;
      } else if (x.is_dirty()) {
        // An edit (or a caret move, which can conceal or reveal the line it
        // left) only changes layout from the caret's line down: redraw from
        // the higher of the old and new caret lines to the bottom of what is
        // visible — above the keyboard, on a phone. Rows above stay valid.
        var cy = new Int32Array(mem.buffer, x.caret_rect(), 4)[1];
        if (lastCaretY < 0) lastCaretY = cy;
        var from = Math.max(0, Math.min(cy, lastCaretY) - Math.round(40 * dpr));
        var vv = window.visualViewport;
        var visBot = (vv ? vv.offsetTop + vv.height : window.innerHeight) - host.getBoundingClientRect().top - tileTop;
        var to = Math.min(H, Math.ceil(visBot * dpr) + Math.round(40 * dpr));
        if (from < valid.a || from > valid.b) from = n.a; // no valid rows to keep above
        got = band(from, Math.max(to, from + 1));
        if (got) valid = { a: Math.min(valid.a, got.a), b: got.b };
        lastCaretY = cy;
      } else {
        if (x.render()) upload(x.dirty_y(), x.dirty_h()); // a caret blink
        if (n.a < valid.a && (got = band(n.a, valid.a))) valid.a = Math.min(valid.a, got.a);
        if (n.b > valid.b && (got = band(valid.b, n.b))) valid.b = Math.max(valid.b, got.b);
      }
      placeSink();
    }

    // ── theme from the page's tokens ──
    function theme() {
      var bg = cssColor(host, "--paper", cssColor(host, "--surface", 0xf3e5d1ff));
      var ink = cssColor(host, "--ink", 0x0e222dff);
      var accent = cssColor(host, "--accent", 0x0d3560ff);
      var colors = [bg, ink, blend(bg, ink, 0.42), accent, blend(bg, ink, 0.08),
        blend(bg, accent, 0.22), accent, blend(bg, ink, 0.38), blend(bg, ink, 0.25), blend(bg, ink, 0.35),
        cssColor(host, "--bad", 0xa62a20ff)];
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
      images(t);
      if (opts.onChange) opts.onChange(t);
    }

    // ── images: a line that is only ![alt](url) shows the image beneath it.
    // The editor draws it; the host's part is the network and the decoder —
    // fetch once, scale to the column in device pixels, hand over RGBA. ──
    var IMAGE_LINE = /^[ \t]*!\[[^\]\n]*\]\(([^)\s]+)\)[ \t]*$/gm;
    var requested = {};
    function imageSrc(url) {
      var m = /^blob:\/\/([a-z0-9]+)$/.exec(url);
      if (m) return opts.blobBase ? opts.blobBase.replace(/\/+$/, "") + "/blobs/" + m[1] : null;
      return /^https?:\/\//.test(url) ? url : null;
    }
    function images(t) {
      if (typeof createImageBitmap !== "function") return;
      var m;
      IMAGE_LINE.lastIndex = 0;
      while ((m = IMAGE_LINE.exec(t))) {
        var url = m[1];
        if (requested[url]) continue;
        requested[url] = true;
        var src = imageSrc(url);
        if (src) loadImage(url, src);
      }
    }
    function loadImage(url, src) {
      fetch(src, { mode: "cors" }).then(function (r) {
        if (!r.ok) throw new Error(r.status);
        return r.blob();
      }).then(function (b) { return createImageBitmap(b); }).then(function (bmp) {
        // the column, in device pixels, and no taller than most of a screen
        // (a hidden editor has no width yet: assume the reading measure)
        var col = W > 100 ? W : Math.round(760 * dpr);
        var tw = Math.min(bmp.width, col);
        var th = Math.round(bmp.height * tw / bmp.width);
        var maxH = Math.round(window.innerHeight * 0.8 * dpr);
        if (th > maxH) { tw = Math.max(1, Math.round(tw * maxH / th)); th = maxH; }
        var c = document.createElement("canvas");
        c.width = tw; c.height = th;
        var g = c.getContext("2d");
        g.imageSmoothingQuality = "high";
        g.drawImage(bmp, 0, 0, tw, th);
        if (bmp.close) bmp.close();
        var px = g.getImageData(0, 0, tw, th).data;
        var ptr = x.image_alloc(px.length);
        if (!ptr) return;
        new Uint8Array(mem.buffer, ptr, px.length).set(px);
        x.image_put(put(url), tw, th, ptr);
        image = null; // memory may have grown
        // an image can shift everything after it: refresh the whole tile
        changed(); reflow(); paint(true);
      }).catch(function () { /* not an image, or unreachable: the Markdown stays */ });
    }
    function setText(s) {
      var n = put(s);
      x.set_text(n);
      rev = x.revision();
      if (opts.field) opts.field.value = s;
      images(s);
      if (page) { grow(); resize(); retile(true); }
      paint(true);
    }
    function insert(s) {
      if (!s) return;
      x.insert_text(put(s));
      after();
    }
    // reflow: the document's height can change without an edit (an image
    // line reveals its source when the caret reaches it), so the page
    // re-measures after any edit or caret move
    function reflow() {
      if (!page) return;
      var t = tall;
      grow();
      if (tall !== t) resize(); // a short document's canvas grows with it
      reveal();
    }
    function after() {
      changed();
      reflow();
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
        if (k === "k") return CMD.link;
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
      // ⌘K belongs to the fleet menu when the page has one: leave the key to
      // bubble to it. Without the menu it is still Link, as is ⌘⇧K always.
      if (k === "k") return window.FarfieldPalette ? 0 : CMD.link;
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
    var lastClick = null, clickRun = 0;
    function pt(e) {
      var r = canvas.getBoundingClientRect();
      return [Math.round((e.clientX - r.left) * dpr), Math.round((e.clientY - r.top) * dpr)];
    }
    // Touch: a finger that moves is scrolling, and the browser owns that. A
    // tap places the caret (and brings up the keyboard); a double-tap or a
    // long press selects the word. Nothing happens on touchdown, so starting
    // a scroll never moves the caret or opens the keyboard.
    var touch = null, lastTap = null;
    // A tap's own default action (the synthetic click after touchend) moves
    // focus to the page, which would close the keyboard the tap just opened;
    // cancel it — for taps only, so scrolling stays native.
    var tapped = false;
    canvas.addEventListener("touchend", function (e) {
      if (tapped) { tapped = false; if (e.cancelable) e.preventDefault(); }
    }, { passive: false });
    function isTouch(e) { return e.pointerType === "touch" || e.pointerType === "pen"; }
    canvas.addEventListener("pointerdown", function (e) {
      if (!isTouch(e)) return;
      // a touch that lands while the page is still moving stops a fling —
      // it is not a tap, and must not open the keyboard
      touch = { id: e.pointerId, x: e.clientX, y: e.clientY, t: e.timeStamp, moved: false,
        sy: window.scrollY, fling: performance.now() - lastScroll < 250 };
    });
    canvas.addEventListener("pointermove", function (e) {
      if (touch && touch.id === e.pointerId &&
          Math.abs(e.clientX - touch.x) + Math.abs(e.clientY - touch.y) > 10) touch.moved = true;
    });
    canvas.addEventListener("pointercancel", function () { touch = null; });
    canvas.addEventListener("pointerup", function (e) {
      if (!touch || touch.id !== e.pointerId) return;
      var t = touch; touch = null;
      // a tap is a touch that neither moved nor scrolled the page
      if (t.moved || t.fling || Math.abs(window.scrollY - t.sy) > 2) return;
      var clicks = 1;
      if (e.timeStamp - t.t > 450) clicks = 2; // long press: the word
      else if (lastTap && e.timeStamp - lastTap.t < 320 &&
          Math.abs(e.clientX - lastTap.x) + Math.abs(e.clientY - lastTap.y) < 30) clicks = 2;
      lastTap = clicks === 1 ? { t: e.timeStamp, x: e.clientX, y: e.clientY } : null;
      tapped = true;
      var p = pt(e);
      // hit-test against the layout the tap was aimed at, then focus: focusing
      // reveals the caret line's Markdown, which changes the layout
      x.pointer(1, p[0], p[1], 0, clicks);
      x.pointer(3, 0, 0, 0, 0);
      focus();
      after();
    });

    canvas.addEventListener("pointerdown", function (e) {
      if (e.button !== 0) return;
      if (isTouch(e)) return;
      e.preventDefault();
      var p = pt(e);
      if ((isMac ? e.metaKey : e.ctrlKey)) {
        var n = x.link_at(p[0], p[1]);
        if (n) { window.open(take(n), "_blank", "noopener"); return; }
      }
      down = true;
      canvas.setPointerCapture(e.pointerId);
      // Count clicks here: pointer events carry detail 0 in Chrome and
      // Firefox, so e.detail never said "double". 2 selects the word, 3 the
      // line; dragging after either extends by whole words or lines.
      var now = e.timeStamp;
      clickRun = (lastClick && now - lastClick.t < 400 &&
        Math.abs(e.clientX - lastClick.x) + Math.abs(e.clientY - lastClick.y) < 6) ? clickRun % 3 + 1 : 1;
      lastClick = { t: now, x: e.clientX, y: e.clientY };
      x.pointer(1, p[0], p[1], mods(e), clickRun);
      focus(); // after the hit-test: focusing changes what is concealed
      paint();
    });
    canvas.addEventListener("pointermove", function (e) {
      if (isTouch(e)) return;
      var p = pt(e);
      if (!down) {
        canvas.style.cursor = ((isMac ? e.metaKey : e.ctrlKey) && x.link_at(p[0], p[1])) ? "pointer" : "text";
        return;
      }
      x.pointer(2, p[0], p[1], mods(e), 0);
      pin();
      // dragging a selection past the viewport's edge scrolls the page
      if (page) {
        if (e.clientY < 40) window.scrollBy(0, -16);
        else if (e.clientY > window.innerHeight - 40) window.scrollBy(0, 16);
      }
      paint();
    });
    function up() {
      if (!down) return;
      down = false;
      x.pointer(3, 0, 0, 0, 0);
      reflow();
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

    // focus the input sink, and tell the editor even if the browser holds the
    // focus event back (a page that does not itself have focus never fires it)
    function focus() {
      sink.focus({ preventScroll: true });
      if (document.activeElement === sink && !host.classList.contains("focused")) {
        x.focus(1);
        host.classList.add("focused");
      }
    }
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

    // ── spelling: the word list loads after the first paint (a missing list
    // just means no squiggles), then the words this browser has learned ──
    var LEARNED = "ff-editor-words";
    function learned() {
      try { return JSON.parse(localStorage.getItem(LEARNED) || "[]"); } catch (e) { return []; }
    }
    // Suggestions need the words themselves: the engine's table holds only
    // hashes, which can say a word is known but not which known word is
    // near — and a two-edit search over hashes would surface collisions as
    // nonsense. The list is kept as bytes and opened as a set on first use.
    var dictBytes = null, wordSet = null;
    function wordList() {
      if (!wordSet && dictBytes) {
        wordSet = new Set(dec.decode(dictBytes).toLowerCase().split("\n"));
        learned().forEach(function (w) { wordSet.add(w); });
      }
      return wordSet;
    }
    var ROWS = ["qwertyuiop", "asdfghjkl", "zxcvbnm"];
    function near(a, b) {
      for (var r = 0; r < 3; r++) {
        var i = ROWS[r].indexOf(a);
        if (i < 0) continue;
        for (var s = Math.max(0, r - 1); s <= Math.min(2, r + 1); s++) {
          var j = ROWS[s].indexOf(b);
          if (j >= 0 && Math.abs(j - i) <= 1) return true;
        }
      }
      return false;
    }
    var ABC = "abcdefghijklmnopqrstuvwxyz'", VOWEL = "aeiou";
    // every string one edit from w, each with a cost: the slips people make
    // (swapped letters, a neighbouring key, a doubled or undoubled letter, the
    // wrong vowel) cost less than an arbitrary letter
    function edits(w, emit) {
      for (var i = 0; i <= w.length; i++) {
        var a = w.slice(0, i), b = w.slice(i);
        if (b) emit(a + b.slice(1), b[0] === w[i - 1] || b[0] === b[1] ? 1 : 2);
        if (b.length > 1) emit(a + b[1] + b[0] + b.slice(2), 1);
        for (var k = 0; k < ABC.length; k++) {
          var c = ABC[k];
          if (b && c !== b[0]) {
            emit(a + c + b.slice(1), near(b[0], c) ||
              (VOWEL.indexOf(c) >= 0 && VOWEL.indexOf(b[0]) >= 0) ? 1.5 : 3);
          }
          emit(a + c + b, c === w[i - 1] || c === b[0] ? 1.5 : 2.5);
        }
      }
    }
    function suggest(word, max) {
      var set = wordList();
      if (!set || word.length > 20) return [];
      var tail = "", w = word.toLowerCase();
      if (/'s$/.test(w)) { tail = "'s"; w = w.slice(0, -2); }
      var best = new Map();
      function keep(c, cost) {
        if (c === w || !set.has(c)) return;
        if (c[0] !== w[0]) cost += 0.5;
        if (c.length !== w.length) cost += 0.2;
        if (!best.has(c) || best.get(c) > cost) best.set(c, cost);
      }
      var first = [];
      edits(w, function (c, cost) { first.push([c, cost]); keep(c, cost); });
      // two edits only when one finds nothing, and only for words long
      // enough that two slips are plausible
      if (!best.size && w.length >= 4 && w.length <= 14) {
        first.forEach(function (p) { edits(p[0], function (c, cost) { keep(c, p[1] + cost + 2); }); });
      }
      return Array.from(best.entries())
        .sort(function (p, q) { return p[1] - q[1] || (p[0] < q[0] ? -1 : 1); })
        .slice(0, max || 3)
        .map(function (p) { return p[0] + tail; });
    }

    // The word list is ~1 MB (~300 KB on the wire). Fetched at mount it
    // competes with the fonts and wasm the first paint is waiting on, for
    // marks nobody needs in the first second — so a URL is fetched once the
    // page goes idle (or after 1.5 s, where requestIdleCallback is missing).
    // The squiggles arrive a moment later; nothing else changes. Bytes
    // handed in directly cost nothing to wait for and load at once.
    function loadDict() {
      if (!running) return; // destroyed before the page went idle
      bytes(opts.dict).then(function (b) {
        if (!running || b.byteLength > x.io_cap()) return;
        dictBytes = new Uint8Array(b.slice(0));
        new Uint8Array(mem.buffer, io(), b.byteLength).set(new Uint8Array(b));
        x.dict_load(b.byteLength);
        learned().forEach(function (w) { x.dict_add(put(w)); });
        paint(true);
      }).catch(function () { /* no list, no squiggles */ });
    }
    if (opts.dict instanceof ArrayBuffer) {
      loadDict();
    } else if (opts.dict) {
      if (window.requestIdleCallback) requestIdleCallback(loadDict, { timeout: 1500 });
      else setTimeout(loadDict, 1500);
    }

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
      // the selection, when it is one word the dictionary does not know
      spellWord: function () {
        if (x.sel_start() === x.sel_end()) return "";
        var w = take(x.get_selection()).trim();
        if (!/^[a-z][A-Za-z']+$/.test(w)) return "";
        return x.dict_has(put(w)) ? "" : w;
      },
      // up to max known words close to w, likeliest first
      suggest: function (w, max) { return suggest(w, max); },
      // accept a word from now on, in this browser
      learn: function (w) {
        // the engine looks words up with a possessive 's stripped; learn the same
        w = w.toLowerCase().replace(/'s$/, "");
        x.dict_add(put(w));
        if (wordSet) wordSet.add(w);
        var l = learned();
        if (l.indexOf(w) < 0) {
          l.push(w);
          try { localStorage.setItem(LEARNED, JSON.stringify(l)); } catch (e) { /* this session only */ }
        }
        after();
      },
      destroy: function () { running = false; host.innerHTML = ""; },
    };
  }

  global.FarfieldEditor = { mount: mount, commands: CMD };
})(window);
