// farfield editor — page glue for the admin apps.
//
// Any <textarea data-editor> becomes the WebAssembly editor, and the page
// becomes the document: no box, the text runs in the page's own column and
// the window's scroll moves through it. The textarea stays in the form as the
// field that posts (the editor keeps it current), so a page without
// JavaScript — or one where the module fails to load — still edits and saves
// plain Markdown.
//
// Around the text, three quiet pieces of chrome:
//   the strip       one sticky line of formatting commands above the text
//   the selection   a small floating bar over any selection
//   the / menu      type "/" at the start of a line to insert a block
//
// Attributes on the textarea:
//   data-editor            opt in
//   data-editor-upload     URL that takes a multipart "file" and answers
//                          {"cid": …} — enables paste, drop and images
//   data-editor-tools      "full" (default) or "plain" (scrap: no Markdown
//                          chrome at all)
//   data-editor-blobs      the browser-facing blobs URL, so ![](blob://cid)
//                          images show beneath their line
//
// With a form[data-async], ⌘S and [data-doc-save] buttons save in place: a
// URL-encoded POST with Accept: application/json, answered with
// {action, editURL, slug, created, viewURL} or {error}. Words land in every
// .doc-words and save state in every .save-note on the page.
(function () {
  "use strict";
  var script = document.currentScript;
  var base = script && script.src ? script.src.replace(/mount\.js.*$/, "") : "/static/editor/";
  var ver = script && script.src ? (script.src.match(/[?&]v=([^&]+)/) || [])[1] || "" : "";
  var q = ver ? "?v=" + ver : "";
  var FONTS = ["Newsreader16pt-Regular.ttf", "Newsreader16pt-SemiBold.ttf", "Newsreader16pt-Italic.ttf",
    "Newsreader16pt-SemiBoldItalic.ttf", "IBMPlexMono-Regular.ttf", "IBMPlexMono-SemiBold.ttf", "Newsreader16pt-Medium.ttf"];
  var mac = /Mac|iPhone|iPad|iPod/.test(navigator.platform || navigator.userAgent);
  var MOD = mac ? "⌘" : "Ctrl ", ALT = mac ? "⌥" : "Alt ", SHIFT = mac ? "⇧" : "Shift ";

  function el(tag, attrs, text) {
    var n = document.createElement(tag);
    for (var k in attrs || {}) n.setAttribute(k, attrs[k]);
    if (text) n.textContent = text;
    return n;
  }
  function each(sel, fn) { Array.prototype.forEach.call(document.querySelectorAll(sel), fn); }

  function upload(url, file) {
    var fd = new FormData();
    fd.append("file", file);
    return fetch(url, { method: "POST", body: fd }).then(function (r) {
      if (!r.ok) throw new Error("Upload failed");
      return r.json();
    }).then(function (d) { return d.cid; });
  }

  // [command, label, title, class]
  var STRIP = [
    // first, so a phone (which has no ⌘Z) reaches them without scrolling
    ["undo", "↶", "Undo — " + MOD + "Z", "glyph"], ["redo", "↷", "Redo — " + MOD + SHIFT + "Z", "glyph"], null,
    ["h1", "H1", "Heading — " + MOD + ALT + "1"], ["h2", "H2", "Subheading — " + MOD + ALT + "2"],
    ["h3", "H3", "Minor heading — " + MOD + ALT + "3"], null,
    ["bold", "B", "Bold — " + MOD + "B", "b"], ["italic", "I", "Italic — " + MOD + "I", "i"],
    ["strike", "S", "Strikethrough — " + MOD + SHIFT + "X", "s"], ["code", "Code", "Inline code — " + MOD + "E"],
    ["link", "Link", "Link — " + MOD + SHIFT + "K"], null,
    ["quote", "Quote", "Quote — " + MOD + SHIFT + "9"], ["bullets", "List", "Bulleted list — " + MOD + SHIFT + "8"],
    ["numbers", "Numbered", "Numbered list — " + MOD + SHIFT + "7"], ["codeblock", "Block", "Code block — " + MOD + ALT + "C"],
  ];
  var FLOAT = [
    ["bold", "B", "Bold", "b"], ["italic", "I", "Italic", "i"], ["strike", "S", "Strikethrough", "s"],
    ["code", "Code", "Inline code"], ["link", "Link", "Link"], null,
    ["h2", "H2", "Subheading"], ["quote", "Quote", "Quote"],
  ];
  // [command, label, hint, words to match]
  var BLOCKS = [
    ["h1", "Heading", "#", "heading title h1"],
    ["h2", "Subheading", "##", "subheading heading h2"],
    ["h3", "Minor heading", "###", "heading h3"],
    ["bullets", "List", "-", "list bullets unordered"],
    ["numbers", "Numbered list", "1.", "numbered ordered list"],
    ["quote", "Quote", ">", "quote blockquote"],
    ["codeblock", "Code block", "```", "code block fence"],
    ["rule", "Divider", "---", "divider rule line hr"],
    ["image", "Image", "upload", "image photo picture upload"],
  ];

  function buttons(bar, list) {
    list.forEach(function (t) {
      if (!t) { bar.appendChild(el("span", { class: "sep", "aria-hidden": "true" })); return; }
      var b = el("button", { type: "button", "data-cmd": t[0], title: t[2], "aria-label": t[2] }, t[1]);
      if (t[3]) b.className = t[3];
      bar.appendChild(b);
    });
    // keep focus in the editor while clicking
    bar.addEventListener("mousedown", function (e) { if (e.target.closest("button")) e.preventDefault(); });
  }

  // Everything that pins to the top of the window sits above the editor's
  // canvas: the app's top bar, if it sticks, then the strip.
  function pinnedTop() {
    var top = document.querySelector(".bar, .top");
    if (!top) return 0;
    var p = getComputedStyle(top).position;
    return p === "sticky" || p === "fixed" ? top.getBoundingClientRect().height : 0;
  }

  function mountOne(field) {
    var form = field.form;
    var uploadURL = field.getAttribute("data-editor-upload");
    var plain = field.getAttribute("data-editor-tools") === "plain";
    var wrap = el("div", { class: "ff-doc" + (plain ? " ff-doc-plain" : "") });
    var strip = plain ? null : el("div", { class: "ff-strip", role: "toolbar", "aria-label": "Formatting" });
    var host = el("div", { class: "ff-editor" });
    if (strip) wrap.appendChild(strip);
    wrap.appendChild(host);
    var holder = field.closest(".field") || field;
    holder.parentNode.insertBefore(wrap, holder);

    function place() {
      var t = pinnedTop();
      wrap.style.setProperty("--ff-strip-top", t + "px");
      wrap.style.setProperty("--ff-editor-top", (t + (strip ? strip.getBoundingClientRect().height : 0)) + "px");
    }
    place();
    window.addEventListener("resize", place);

    var ed = null;
    var dirty = false, saving = false;
    function words() {
      if (!ed) return;
      var n = ed.words();
      each(".doc-words", function (w) { w.textContent = n.toLocaleString() + (n === 1 ? " word" : " words"); });
    }
    function note(text, href, bad) {
      each(".save-note", function (n) {
        n.textContent = "";
        n.classList.toggle("bad", !!bad);
        if (href) n.appendChild(el("a", { href: href, target: "_blank", rel: "noopener" }, text + " ↗"));
        else n.textContent = text;
      });
    }

    function files(list) {
      if (!uploadURL) return Promise.resolve([]);
      note("Uploading " + list.length + (list.length === 1 ? " image…" : " images…"));
      return Promise.all(list.map(function (f) {
        return upload(uploadURL, f).then(function (cid) { return "![](blob://" + cid + ")"; });
      })).then(function (snips) { note(dirty ? "Unsaved" : ""); return snips; }).catch(function (e) {
        note(e.message || "Upload failed", null, true);
        return [];
      });
    }
    var pick = null;
    if (uploadURL) {
      pick = el("input", { type: "file", accept: "image/*", multiple: "", hidden: "" });
      pick.onchange = function () {
        files(Array.prototype.slice.call(pick.files)).then(function (s) { if (s.length && ed) ed.insert(s.join("\n\n")); });
        pick.value = "";
      };
      wrap.appendChild(pick);
    } else {
      BLOCKS = BLOCKS.filter(function (b) { return b[0] !== "image"; });
    }

    function save() {
      if (!form || saving) return;
      if (!form.hasAttribute("data-async")) { form.requestSubmit(); return; }
      saving = true;
      note("Saving…");
      fetch(form.action, {
        method: "POST",
        headers: { Accept: "application/json" },
        body: new URLSearchParams(new FormData(form)),
      }).then(function (r) {
        return r.json().catch(function () { return {}; }).then(function (d) { return { ok: r.ok, d: d || {} }; });
      }).then(function (res) {
        saving = false;
        if (!res.ok) throw new Error(res.d.error || "Save failed");
        dirty = false;
        if (res.d.action) form.action = res.d.action;
        if (res.d.editURL && location.pathname !== res.d.editURL) history.replaceState(null, "", res.d.editURL);
        var slug = form.querySelector('input[name="slug"]');
        if (slug && res.d.slug && slug.value !== res.d.slug) {
          slug.value = res.d.slug;
          slug.dispatchEvent(new Event("input", { bubbles: true }));
        }
        var at = new Date();
        if (res.d.created && res.d.viewURL) note(form.getAttribute("data-created-label") || "Posted", res.d.viewURL);
        else note("Saved " + at.getHours() + ":" + String(at.getMinutes()).padStart(2, "0"));
        document.dispatchEvent(new CustomEvent("farfield:saved"));
      }).catch(function (err) {
        saving = false;
        note(err.message || "Save failed", null, true);
      });
    }

    // ── the selection bar ──
    var float = plain ? null : el("div", { class: "ff-float", role: "toolbar", "aria-label": "Selection", hidden: "" });
    // on a misspelled word the bar also offers to learn it
    var learnBtn = plain ? null : el("button", { type: "button", class: "learn", hidden: "",
      title: "Stop flagging this word", "aria-label": "Add to dictionary" }, "Add to dictionary");
    if (float) {
      buttons(float, FLOAT);
      float.appendChild(learnBtn);
      learnBtn.addEventListener("click", function () {
        var w = learnBtn.getAttribute("data-word");
        if (w && ed) { ed.learn(w); learnBtn.hidden = true; }
      });
      host.appendChild(float);
    }
    function floatBar() {
      if (!float || !ed) return;
      var s = ed.selection();
      if (s.empty || !host.classList.contains("focused")) { float.hidden = true; return; }
      float.hidden = false;
      var word = ed.spellWord ? ed.spellWord() : "";
      learnBtn.hidden = !word;
      if (word) learnBtn.setAttribute("data-word", word);
      var w = float.offsetWidth, h = float.offsetHeight, W = host.clientWidth;
      var cx = s.y0 + 4 >= s.y1 - 4 || Math.abs(s.y1 - s.y0) < 40 ? (s.x0 + s.x1) / 2 : s.x0 + 60;
      var left = Math.max(0, Math.min(W - w, cx - w / 2));
      var top = s.y0 - h - 10;
      // flip under the selection when it would slide beneath the pinned chrome
      var ceiling = parseFloat(wrap.style.getPropertyValue("--ff-editor-top")) || 0;
      if (host.getBoundingClientRect().top + top < ceiling + 4) top = s.y1 + 10;
      float.style.left = left + "px";
      float.style.top = top + "px";
    }

    // ── the / menu ──
    var menu = plain ? null : el("div", { class: "ff-slash", role: "listbox", "aria-label": "Insert block", hidden: "" });
    if (menu) host.appendChild(menu);
    var slash = null; // {start, len, items, at}
    var dismissed = -1;
    function closeSlash() { if (menu) menu.hidden = true; slash = null; }
    function slashMenu() {
      if (!menu || !ed) return;
      var s = ed.selection();
      var line = s.empty ? ed.caretLine() : null;
      var m = line && /^\/([a-z0-9 ]{0,24})$/i.exec(line.text);
      if (!m || line.start === dismissed) { closeSlash(); if (!m) dismissed = -1; return; }
      var qy = m[1].trim().toLowerCase();
      var items = BLOCKS.filter(function (b) { return !qy || b[3].indexOf(qy) >= 0 || b[1].toLowerCase().indexOf(qy) === 0; });
      if (!items.length) { closeSlash(); return; }
      var at = slash && slash.start === line.start ? Math.min(slash.at, items.length - 1) : 0;
      slash = { start: line.start, len: line.text.length, items: items, at: at };
      menu.textContent = "";
      items.forEach(function (b, i) {
        var o = el("button", { type: "button", role: "option", "data-i": String(i), "aria-selected": i === at ? "true" : "false" });
        o.appendChild(el("span", null, b[1]));
        o.appendChild(el("span", { class: "hint" }, b[2]));
        menu.appendChild(o);
      });
      menu.hidden = false;
      var c = ed.caret();
      var top = c.y + c.h + 6;
      var room = window.innerHeight - (host.getBoundingClientRect().top + top);
      if (room < menu.offsetHeight + 12) top = c.y - menu.offsetHeight - 6;
      menu.style.left = Math.max(0, Math.min(host.clientWidth - menu.offsetWidth, c.x - 12)) + "px";
      menu.style.top = top + "px";
    }
    function choose(i) {
      if (!slash || !ed) return;
      var b = slash.items[i];
      var start = slash.start, len = slash.len;
      closeSlash();
      ed.replace("", start, start + len);
      if (b[0] === "image") { if (pick) pick.click(); return; }
      ed.run(b[0]);
      ed.focus();
    }
    if (menu) {
      menu.addEventListener("mousedown", function (e) { e.preventDefault(); });
      menu.addEventListener("click", function (e) {
        var o = e.target.closest("[data-i]");
        if (o) choose(+o.getAttribute("data-i"));
      });
    }
    function onKey(e) {
      if (!slash) return false;
      var n = slash.items.length;
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        slash.at = (slash.at + (e.key === "ArrowDown" ? 1 : n - 1)) % n;
        Array.prototype.forEach.call(menu.children, function (o, i) { o.setAttribute("aria-selected", i === slash.at ? "true" : "false"); });
        return true;
      }
      if (e.key === "Enter" || e.key === "Tab") { choose(slash.at); return true; }
      if (e.key === "Escape") { dismissed = slash.start; closeSlash(); return true; }
      return false;
    }

    // the Markdown toggle, remembered per browser
    function remembered() { try { return localStorage.getItem("ff-editor-source") === "1"; } catch (e) { return false; } }
    function source(on, quiet) {
      if (!ed || plain) return;
      ed.plain(on);
      var b = strip && strip.querySelector(".mode");
      if (b) b.setAttribute("aria-pressed", on ? "true" : "false");
      wrap.classList.toggle("ff-source", on);
      try { localStorage.setItem("ff-editor-source", on ? "1" : "0"); } catch (e) {}
      if (!quiet) ed.focus();
    }

    function run(cmd) {
      if (!ed) return;
      if (cmd === "image") { if (pick) pick.click(); return; }
      ed.command(cmd);
    }
    [strip, float].forEach(function (bar) {
      if (!bar) return;
      bar.addEventListener("click", function (e) {
        var b = e.target.closest("button[data-cmd]");
        if (b) run(b.getAttribute("data-cmd"));
      });
    });
    if (strip) {
      buttons(strip, STRIP);
      if (uploadURL) buttons(strip, [null, ["image", "Image", "Image — or paste or drop one"]]);
      strip.appendChild(el("span", { class: "hint", "aria-hidden": "true" }, "/ for blocks"));
      // Markdown: the raw source, unstyled and without images — what saves.
      var modeBtn = el("button", { type: "button", class: "mode", "aria-pressed": "false",
        title: "Show the Markdown source", "aria-label": "Show the Markdown source" }, "Markdown");
      strip.appendChild(modeBtn);
      modeBtn.addEventListener("mousedown", function (e) { e.preventDefault(); });
      modeBtn.addEventListener("click", function () { source(modeBtn.getAttribute("aria-pressed") !== "true"); });
    }

    var assets = window.FarfieldEditorAssets || {};
    FarfieldEditor.mount(host, {
      wasm: assets.wasm || base + "editor.wasm" + q,
      dict: plain ? null : (assets.dict || base + "dict/en_US.txt" + q),
      fonts: assets.fonts || FONTS.map(function (f) { return base + "fonts/" + f + q; }),
      field: field,
      page: true,
      blobBase: field.getAttribute("data-editor-blobs") || "",
      placeholder: field.getAttribute("placeholder") || (plain ? "Paste or type…" : "Write, or type / for blocks…"),
      label: (form && form.querySelector('label[for="' + field.id + '"]') || {}).textContent || "Body",
      onChange: function () {
        if (!dirty) note("Unsaved");
        dirty = true;
        words();
      },
      onCursor: function () { floatBar(); slashMenu(); },
      onBlur: function () { if (float) float.hidden = true; closeSlash(); },
      onKey: onKey,
      onSave: save,
      onFiles: uploadURL ? files : null,
    }).then(function (e) {
      ed = e;
      if (plain) e.plain(true);
      else if (remembered()) source(true, true);
      wrap.classList.add("ready");
      holder.classList.add("ff-editor-replaced");
      words();
      place();
      window.addEventListener("scroll", floatBar, { passive: true });
      if (field.hasAttribute("autofocus")) e.focus();
      // the editor's own actions, in the fleet's ⌘K menu
      (window.FarfieldPaletteQueue = window.FarfieldPaletteQueue || []).push(function () {
        var acts = [];
        if (form) acts.push({ title: "Save", sub: "this document", words: "write store", run: save });
        var pub = form && form.querySelector('input[type="checkbox"][name="published"]');
        if (pub) acts.push({ title: pub.checked ? "Unpublish and save" : "Publish and save", sub: "this document",
          words: "publish draft live public", run: function () { pub.checked = !pub.checked; dirty = true; save(); } });
        if (!plain) {
          var on = wrap.classList.contains("ff-source");
          acts.push({ title: on ? "Show formatted" : "Show Markdown", sub: "editor view", words: "source markdown raw toggle",
            run: function () { source(!on); } });
          var w = ed.spellWord();
          if (w) acts.push({ title: "Add “" + w + "” to dictionary", sub: "spelling", words: "learn spelling word",
            run: function () { ed.learn(w); } });
        }
        return acts;
      });
    }).catch(function (err) {
      // keep the plain textarea: the page still works
      console.error(err);
      wrap.remove();
    });

    if (form) {
      each("[data-doc-save]", function (b) {
        if (b.form !== form && !form.contains(b)) return;
        b.addEventListener("click", function (e) { e.preventDefault(); save(); });
      });
      // ⌘S anywhere on the page, not only inside the editor
      document.addEventListener("keydown", function (e) {
        if ((e.metaKey || e.ctrlKey) && !e.shiftKey && !e.altKey && e.key.toLowerCase() === "s") {
          e.preventDefault();
          save();
        }
      });
      form.addEventListener("input", function (e) {
        if (e.target === field) return;
        if (!dirty) note("Unsaved");
        dirty = true;
      });
      form.addEventListener("submit", function () { dirty = false; });
      window.addEventListener("beforeunload", function (e) {
        if (dirty && form.hasAttribute("data-async")) { e.preventDefault(); e.returnValue = ""; }
      });
    }
  }

  function start() { each("textarea[data-editor]", mountOne); }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start);
  else start();
})();
