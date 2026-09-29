// farfield editor — page glue for the admin apps.
//
// Any <textarea data-editor> becomes the WebAssembly editor. The textarea
// stays in the form as the field that posts (the editor keeps it current), so
// a page without JavaScript — or one where the module fails to load — still
// edits and saves plain Markdown.
//
// Attributes on the textarea:
//   data-editor            opt in
//   data-editor-upload     URL that takes a multipart "file" and answers
//                          {"cid": …} — enables paste, drop and the image button
//   data-editor-tools      "full" (default) or "plain" (scrap: no Markdown bar)
//
// With a form[data-async], ⌘S and [data-doc-save] buttons save in place: a
// URL-encoded POST with Accept: application/json, answered with
// {action, editURL, slug, created, viewURL} or {error}. Words land in every
// .doc-words and save state in every .save-note on the page.
(function () {
  "use strict";
  var script = document.currentScript;
  var base = script ? script.src.replace(/mount\.js.*$/, "") : "/static/editor/";
  var ver = script ? (script.src.match(/[?&]v=([^&]+)/) || [])[1] || "" : "";
  var q = ver ? "?v=" + ver : "";
  var FONTS = ["Newsreader16pt-Regular.ttf", "Newsreader16pt-SemiBold.ttf", "Newsreader16pt-Italic.ttf",
    "Newsreader16pt-SemiBoldItalic.ttf", "IBMPlexMono-Regular.ttf", "IBMPlexMono-SemiBold.ttf"];

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
      if (!r.ok) throw new Error("upload failed");
      return r.json();
    }).then(function (d) { return d.cid; });
  }

  var TOOLS = [
    ["bold", "B", "Bold — ⌘B"], ["italic", "I", "Italic — ⌘I"], ["strike", "S", "Strikethrough — ⌘⇧X"],
    ["code", "code", "Inline code — ⌘E"], ["link", "link", "Link — ⌘K"], null,
    ["h1", "H1", "Heading 1 — ⌘⌥1"], ["h2", "H2", "Heading 2 — ⌘⌥2"], ["h3", "H3", "Heading 3 — ⌘⌥3"], null,
    ["quote", "quote", "Quote — ⌘⇧9"], ["bullets", "• list", "Bulleted list — ⌘⇧8"],
    ["numbers", "1. list", "Numbered list — ⌘⇧7"], ["codeblock", "block", "Code block — ⌘⌥C"],
    ["rule", "rule", "Horizontal rule"],
  ];

  function mountOne(field) {
    var form = field.form;
    var uploadURL = field.getAttribute("data-editor-upload");
    var plain = field.getAttribute("data-editor-tools") === "plain";
    var wrap = el("div", { class: "ff-editor-wrap" });
    var bar = plain ? null : el("div", { class: "ff-editor-bar", role: "toolbar", "aria-label": "Formatting" });
    var host = el("div", { class: "ff-editor" + (plain ? " ff-editor-plain" : "") });
    if (bar) wrap.appendChild(bar);
    wrap.appendChild(host);
    var holder = field.closest(".field") || field;
    holder.parentNode.insertBefore(wrap, holder);

    var ed = null;
    var dirty = false, saving = false;
    function words() {
      if (!ed) return;
      var n = ed.words();
      each(".doc-words", function (w) { w.textContent = n + (n === 1 ? " word" : " words"); });
    }
    function note(text, href, bad) {
      each(".save-note", function (n) {
        n.textContent = "";
        n.classList.toggle("bad", !!bad);
        if (href) {
          var a = el("a", { href: href, target: "_blank", rel: "noopener" }, text + " ↗");
          n.appendChild(a);
        } else n.textContent = text;
      });
    }

    function files(list) {
      if (!uploadURL) return Promise.resolve([]);
      note("Uploading…");
      return Promise.all(list.map(function (f) {
        return upload(uploadURL, f).then(function (cid) { return "![](blob://" + cid + ")"; });
      })).then(function (snips) { note(""); return snips; }).catch(function (e) {
        note(e.message || "Upload failed", null, true);
        return [];
      });
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

    FarfieldEditor.mount(host, {
      wasm: base + "editor.wasm" + q,
      fonts: FONTS.map(function (f) { return base + "fonts/" + f + q; }),
      field: field,
      placeholder: field.getAttribute("placeholder") || (plain ? "Paste or type…" : "Write…"),
      label: (form && form.querySelector('label[for="' + field.id + '"]') || {}).textContent || "Body",
      onChange: function () { dirty = true; words(); },
      onSave: save,
      onFiles: uploadURL ? files : null,
    }).then(function (e) {
      ed = e;
      if (plain) e.plain(true);
      wrap.classList.add("ready");
      holder.classList.add("ff-editor-replaced");
      words();
      if (field.hasAttribute("autofocus")) e.focus();
    }).catch(function (err) {
      // keep the plain textarea: the page still works
      console.error(err);
      wrap.remove();
    });

    if (bar) {
      TOOLS.forEach(function (t) {
        if (!t) { bar.appendChild(el("span", { class: "sep" })); return; }
        var b = el("button", { type: "button", "data-cmd": t[0], title: t[2], "aria-label": t[2] }, t[1]);
        bar.appendChild(b);
      });
      if (uploadURL) {
        var pick = el("input", { type: "file", accept: "image/*", multiple: "", hidden: "" });
        var img = el("button", { type: "button", title: "Image — or paste / drop one", "aria-label": "Insert image" }, "image");
        img.onclick = function () { pick.click(); };
        pick.onchange = function () {
          files(Array.prototype.slice.call(pick.files)).then(function (s) { if (s.length && ed) ed.insert(s.join("\n\n")); });
          pick.value = "";
        };
        bar.appendChild(el("span", { class: "sep" }));
        bar.appendChild(img);
        bar.appendChild(pick);
      }
      // keep focus in the editor while clicking the bar
      bar.addEventListener("mousedown", function (e) { if (e.target.closest("button")) e.preventDefault(); });
      bar.addEventListener("click", function (e) {
        var b = e.target.closest("button[data-cmd]");
        if (b && ed) ed.command(b.getAttribute("data-cmd"));
      });
    }

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
      form.addEventListener("input", function (e) { if (e.target !== field) dirty = true; });
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
