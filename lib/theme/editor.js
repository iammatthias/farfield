// farfield editor — markdown editing enhancements shared by the admin UIs.
//
// Layer 1 — embed pickers: the host page provides a <textarea id="body">, an
// embed toolbar with buttons (data-embed="blob" / data-embed="series"), and a
// global window.FARFIELD = { blobs, content } carrying the public service
// URLs. Reads go straight to those services (CORS-enabled); writes go through
// the host app's session-gated /embed/* proxy so service keys stay
// server-side. Pasting or dropping a media file into the textarea uploads it
// through the same proxy and inserts the blob:// reference.
//
// Layer 2 — the document editor: when the page carries a [data-doc-open]
// trigger, the script builds a full-screen writing surface over the same
// textarea. With FARFIELD.editdoc set (the host's session-gated
// markdown→editable-HTML endpoint) the surface is rich — a Medium-style
// contenteditable document with a formatting toolbar, live ⌘B/⌘I, markdown
// typing shortcuts (## , - , > , ```), paste/drop upload, links (⌘K), and
// file attachments — serialized back to markdown on save. Markdown source
// stays one toggle away; blocks the rich schema can't represent (tables, raw
// HTML) round-trip verbatim as marked source blocks. A form with data-async
// saves via fetch (⌘S) and stays put.
(function () {
  "use strict";

  var cfg = window.FARFIELD || {};
  var body = document.getElementById("body");
  if (!body) return;

  // ── tiny DOM helper ────────────────────────────────────────────────────
  function el(tag, props, kids) {
    var n = document.createElement(tag);
    props = props || {};
    Object.keys(props).forEach(function (k) {
      if (k === "class") n.className = props[k];
      else if (k === "text") n.textContent = props[k];
      else if (k.slice(0, 2) === "on") n[k.toLowerCase()] = props[k];
      else if (props[k] != null) n.setAttribute(k, props[k]);
    });
    (kids || []).forEach(function (c) { if (c) n.appendChild(c); });
    return n;
  }

  function jsonPost(o) {
    return {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(o),
    };
  }
  function asJSON(r) {
    if (!r.ok) throw new Error("request failed");
    return r.json();
  }

  // afterEdit runs whenever the script itself changes the textarea — the doc
  // editor hooks it to resize, recount, and mark the form dirty.
  var editHooks = [];
  function afterEdit() { editHooks.forEach(function (fn) { fn(); }); }

  // Insert text at the textarea's cursor, then refocus.
  function insert(text) {
    var s = body.selectionStart, e = body.selectionEnd;
    body.value = body.value.slice(0, s) + text + body.value.slice(e);
    body.selectionStart = body.selectionEnd = s + text.length;
    body.focus();
    afterEdit();
  }

  // mediaSink is where picked embeds land: the textarea by default, the rich
  // surface while the document editor is in rich mode.
  var mediaSink = insert;

  function blobURL(cid) { return cfg.blobs + "/blobs/" + cid; }

  function msgRow(text) { return el("div", { class: "embed-msg", text: text }); }

  // ── modal shell ────────────────────────────────────────────────────────
  function openModal(title) {
    var content = el("div", { class: "embed-content" });
    var x = el("button", { type: "button", class: "embed-x",
      "aria-label": "Close", text: "×" });
    var dlg = el("dialog", { class: "embed-modal" }, [
      el("div", { class: "embed-head" }, [el("strong", { text: title }), x]),
      content,
    ]);
    x.onclick = function () { dlg.close(); };
    document.body.appendChild(dlg);
    dlg.addEventListener("close", function () { dlg.remove(); });
    dlg.showModal();
    return { dlg: dlg, content: content };
  }

  // ── upload ─────────────────────────────────────────────────────────────
  function uploadBlob(file) {
    var fd = new FormData();
    fd.append("file", file);
    return fetch("/embed/blob", { method: "POST", body: fd }).then(function (r) {
      if (!r.ok) throw new Error("upload failed");
      return r.json();
    }).then(function (d) { return d.cid; });
  }

  // A drag-and-drop + click upload zone; onDone(cid) per successful upload.
  function uploadZone(onDone) {
    var label = el("span", { class: "embed-drop-text",
      text: "Drop an image here, or click to upload" });
    var input = el("input", { type: "file", accept: "image/*" });
    var zone = el("div", { class: "embed-drop" }, [label, input]);

    function handle(file) {
      if (!file) return;
      zone.classList.add("busy");
      label.textContent = "Uploading…";
      uploadBlob(file).then(function (cid) {
        zone.classList.remove("busy");
        label.textContent = "Drop an image here, or click to upload";
        onDone(cid);
      }).catch(function () {
        zone.classList.remove("busy");
        label.textContent = "Upload failed — click to try again";
      });
    }
    zone.onclick = function () { input.click(); };
    input.onchange = function () { handle(input.files[0]); input.value = ""; };
    zone.ondragover = function (e) { e.preventDefault(); zone.classList.add("over"); };
    zone.ondragleave = function () { zone.classList.remove("over"); };
    zone.ondrop = function (e) {
      e.preventDefault();
      zone.classList.remove("over");
      handle(e.dataTransfer.files[0]);
    };
    return zone;
  }

  // ── blob grid (paginated, infinite scroll) ─────────────────────────────
  // opts: { onPick: fn(cid), order: fn(cid)->number (0 = unselected) }
  function blobGrid(opts) {
    var cells = {};
    var grid = el("div", { class: "embed-grid" });
    var sentinel = el("div", { class: "embed-sentinel" });
    var scroll = el("div", { class: "embed-scroll" }, [grid, sentinel]);
    var msg = msgRow("Loading blobs…");
    scroll.insertBefore(msg, sentinel);
    var page = 0, pages = 1, busy = false, done = false;

    function paint(cell) {
      if (!opts.order) return;
      var n = opts.order(cell.dataset.cid);
      cell.classList.toggle("sel", n > 0);
      cell._badge.textContent = n > 0 ? n : "";
    }
    function makeCell(cid) {
      var b = el("span", { class: "embed-badge" });
      var cell = el("button", { type: "button", class: "embed-cell", title: cid }, [
        el("img", { loading: "lazy", alt: "", src: blobURL(cid) }), b,
      ]);
      cell.dataset.cid = cid;
      cell._badge = b;
      cell.onclick = function () { opts.onPick(cid); };
      cells[cid] = cell;
      paint(cell);
      return cell;
    }
    function load() {
      if (busy || done) return;
      busy = true;
      page++;
      fetch("/embed/blobs?page=" + page).then(function (r) { return r.json(); })
        .then(function (d) {
          busy = false;
          if (msg) { msg.remove(); msg = null; }
          pages = (d && d.pages) || 1;
          var blobs = (d && d.blobs) || [];
          blobs.forEach(function (b) { grid.appendChild(makeCell(b.cid)); });
          if (page === 1 && !blobs.length) {
            scroll.insertBefore(msgRow("No blobs yet — upload one above."), sentinel);
          }
          if (page >= pages || !blobs.length) {
            done = true;
            io.disconnect();
          } else {
            // Re-observe so a sentinel still in view triggers the next page.
            io.unobserve(sentinel);
            io.observe(sentinel);
          }
        })
        .catch(function () {
          busy = false;
          if (msg) msg.textContent = "Could not load blobs.";
        });
    }
    var io = new IntersectionObserver(function (entries) {
      if (entries[0].isIntersecting) load();
    }, { root: scroll, rootMargin: "400px" });
    io.observe(sentinel);

    return {
      el: scroll,
      refresh: function () {
        Object.keys(cells).forEach(function (c) { paint(cells[c]); });
      },
      prepend: function (cid) {
        if (cells[cid]) return;
        grid.insertBefore(makeCell(cid), grid.firstChild);
      },
    };
  }

  // ── embed a single blob ────────────────────────────────────────────────
  function embedBlob() {
    var m = openModal("Embed a blob");
    function pick(cid) { m.dlg.close(); mediaSink("![](blob://" + cid + ")"); }
    var grid = blobGrid({ onPick: pick });
    m.content.appendChild(el("div", { class: "embed-pane" }, [
      el("div", { class: "embed-top" }, [
        uploadZone(pick),
        el("p", { class: "embed-label", text: "Or pick an existing blob" }),
      ]),
      grid.el,
    ]));
  }

  // ── embed a series ─────────────────────────────────────────────────────
  function embedSeries() {
    var m = openModal("Embed a series");
    var pickBtn = el("button", { type: "button", class: "active", text: "Pick existing" });
    var buildBtn = el("button", { type: "button", text: "Build new" });
    var pane = el("div", { class: "embed-pane" });
    m.content.appendChild(el("div", { class: "embed-tabs" }, [pickBtn, buildBtn]));
    m.content.appendChild(pane);

    function show(which) {
      pickBtn.classList.toggle("active", which === "pick");
      buildBtn.classList.toggle("active", which === "build");
      pane.innerHTML = "";
      if (which === "pick") seriesPicker(pane, m);
      else galleryBuilder(pane, m);
    }
    pickBtn.onclick = function () { show("pick"); };
    buildBtn.onclick = function () { show("build"); };
    show("pick");
  }

  // seriesPicker lists existing series as cards with a cover thumbnail.
  function seriesPicker(pane, m) {
    var scroll = el("div", { class: "embed-scroll" });
    var msg = msgRow("Loading series…");
    scroll.appendChild(msg);
    pane.appendChild(scroll);

    fetch("/embed/series").then(function (r) { return r.json(); })
      .then(function (d) {
        msg.remove();
        var series = (d && d.series) || [];
        if (!series.length) {
          scroll.appendChild(msgRow("No series yet — switch to “Build new”."));
          return;
        }
        var cards = el("div", { class: "embed-cards" });
        series.forEach(function (s) {
          var cids = (s.body || "").match(/blob:\/\/[a-z0-9]+/gi) || [];
          var cover = cids.length
            ? el("img", { loading: "lazy", alt: "", src: blobURL(cids[0].slice(7)) })
            : el("div", { class: "embed-cover-empty", text: "no images" });
          var card = el("button", { type: "button", class: "embed-card" }, [
            el("div", { class: "embed-cover" }, [cover]),
            el("div", { class: "embed-card-meta" }, [
              el("strong", { text: s.title || s.slug }),
              el("span", { class: "embed-count",
                text: cids.length + (cids.length === 1 ? " image" : " images") }),
            ]),
          ]);
          card.onclick = function () {
            m.dlg.close();
            mediaSink("![](series://" + s.slug + ")");
          };
          cards.appendChild(card);
        });
        scroll.appendChild(cards);
      })
      .catch(function () { msg.textContent = "Could not load series."; });
  }

  // galleryBuilder assembles a new series: an ordered, editable tray of
  // picked blobs plus a multi-select grid to pick from.
  function galleryBuilder(pane, m) {
    var picked = []; // cids, in display order

    var title = el("input", { type: "text", class: "embed-input",
      placeholder: "Series title (optional)" });
    var tray = el("div", { class: "embed-tray" });
    var count = el("span", { class: "embed-foot-count" });
    var create = el("button", { type: "button", class: "embed-go",
      text: "Create & embed series" });

    var grid = blobGrid({
      order: function (cid) { return picked.indexOf(cid) + 1; },
      onPick: function (cid) {
        var i = picked.indexOf(cid);
        if (i >= 0) picked.splice(i, 1);
        else picked.push(cid);
        sync();
      },
    });

    function move(from, to) {
      if (to < 0 || to >= picked.length || from === to) return;
      picked.splice(to, 0, picked.splice(from, 1)[0]);
      sync();
    }
    function renderTray() {
      tray.innerHTML = "";
      if (!picked.length) {
        tray.appendChild(el("span", { class: "embed-tray-empty",
          text: "Nothing picked yet — choose images below, drag tray items to reorder." }));
        return;
      }
      picked.forEach(function (cid, i) {
        var rm = el("button", { type: "button", class: "embed-tray-rm",
          title: "Remove", text: "×" });
        rm.onclick = function () { picked.splice(i, 1); sync(); };
        var item = el("div", { class: "embed-tray-item", draggable: "true" }, [
          el("img", { alt: "", src: blobURL(cid) }), rm,
        ]);
        item.ondragstart = function (e) {
          e.dataTransfer.setData("text/plain", String(i));
          item.classList.add("dragging");
        };
        item.ondragend = function () { item.classList.remove("dragging"); };
        item.ondragover = function (e) { e.preventDefault(); };
        item.ondrop = function (e) {
          e.preventDefault();
          move(parseInt(e.dataTransfer.getData("text/plain"), 10), i);
        };
        tray.appendChild(item);
      });
    }
    function sync() {
      renderTray();
      grid.refresh();
      count.textContent = picked.length
        ? picked.length + " selected"
        : "Pick at least one image";
      count.classList.remove("err");
    }

    create.onclick = function () {
      if (!picked.length) { count.textContent = "Pick at least one image";
        count.classList.add("err"); return; }
      create.disabled = true;
      create.textContent = "Creating…";
      fetch("/embed/series", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ title: title.value, cids: picked }),
      }).then(function (r) {
        if (!r.ok) throw new Error("create failed");
        return r.json();
      }).then(function (s) {
        m.dlg.close();
        mediaSink("![](series://" + s.slug + ")");
      }).catch(function () {
        create.disabled = false;
        create.textContent = "Create & embed series";
        count.textContent = "Could not create the series.";
        count.classList.add("err");
      });
    };

    pane.appendChild(el("div", { class: "embed-top" }, [
      title,
      el("p", { class: "embed-label", text: "Selected images" }),
      tray,
      uploadZone(function (cid) {
        if (picked.indexOf(cid) < 0) picked.push(cid);
        grid.prepend(cid);
        sync();
      }),
      el("p", { class: "embed-label", text: "Pick images" }),
    ]));
    pane.appendChild(grid.el);
    pane.appendChild(el("div", { class: "embed-foot" }, [count, create]));
    sync();
  }

  // ── embed toolbar wiring ───────────────────────────────────────────────
  var toolbar = document.querySelector(".embed-toolbar");
  if (toolbar) {
    toolbar.addEventListener("click", function (e) {
      var btn = e.target.closest("[data-embed]");
      if (!btn) return;
      e.preventDefault();
      if (btn.dataset.embed === "blob") embedBlob();
      else if (btn.dataset.embed === "series") embedSeries();
    });
  }

  // ── paste / drop media upload into the plain textarea ──────────────────
  var docTriggers = document.querySelectorAll("[data-doc-open]");
  if (toolbar || docTriggers.length) {
    var uploadFiles = function (files) {
      Array.prototype.slice.call(files).forEach(function (f) {
        if (!/^(image|video|audio)\//.test(f.type)) return;
        var tag = "![uploading " + (f.name || "file") + "…]()";
        insert(tag);
        uploadBlob(f).then(function (cid) {
          replaceOnce(tag, "![](blob://" + cid + ")");
        }).catch(function () {
          replaceOnce(tag, "");
        });
      });
    };
    var replaceOnce = function (tag, text) {
      var i = body.value.indexOf(tag);
      if (i < 0) return;
      body.value = body.value.slice(0, i) + text + body.value.slice(i + tag.length);
      afterEdit();
    };
    body.addEventListener("paste", function (e) {
      var files = e.clipboardData && e.clipboardData.files;
      if (files && files.length) { e.preventDefault(); uploadFiles(files); }
    });
    body.addEventListener("dragover", function (e) { e.preventDefault(); });
    body.addEventListener("drop", function (e) {
      var files = e.dataTransfer && e.dataTransfer.files;
      if (files && files.length) { e.preventDefault(); uploadFiles(files); }
    });
  }

  // ── series reorder tray ────────────────────────────────────────────────
  // A [data-series-tray] host renders the body's gallery as draggable
  // thumbnails; a drop rewrites the markdown (whole lines move, so alt text
  // rides along). Only image-only bodies reorder — anything else shows a
  // quiet hint instead of risking prose.
  var trayHost = document.querySelector("[data-series-tray]");
  if (trayHost && cfg.blobs) initSeriesTray(trayHost);

  function initSeriesTray(host) {
    function parseGallery() {
      var lines = body.value.split(/\n+/)
        .map(function (t) { return t.trim(); })
        .filter(Boolean);
      var items = [];
      for (var i = 0; i < lines.length; i++) {
        var m = /^!\[[^\]]*\]\(blob:\/\/([a-z0-9]+)\)$/.exec(lines[i]) ||
          /^blob:\/\/([a-z0-9]+)$/.exec(lines[i]);
        if (!m) return null;
        items.push({ line: lines[i], cid: m[1] });
      }
      return items;
    }
    function apply(items) {
      body.value = items.map(function (it) { return it.line; }).join("\n\n");
      afterEdit();
      render();
    }
    function render() {
      var items = parseGallery();
      host.innerHTML = "";
      if (!items || items.length === 0) {
        host.appendChild(el("p", { class: "hint",
          text: "Reordering needs an image-only series." }));
        return;
      }
      if (items.length === 1) {
        host.appendChild(el("p", { class: "hint",
          text: "Add a second image to reorder." }));
        return;
      }
      var tray = el("div", { class: "embed-tray series-tray" });
      items.forEach(function (it, i) {
        var item = el("div", { class: "embed-tray-item", draggable: "true",
          title: it.cid }, [el("img", { alt: "", src: blobURL(it.cid) })]);
        item.ondragstart = function (e) {
          e.dataTransfer.setData("text/plain", String(i));
          item.classList.add("dragging");
        };
        item.ondragend = function () { item.classList.remove("dragging"); };
        item.ondragover = function (e) { e.preventDefault(); };
        item.ondrop = function (e) {
          e.preventDefault();
          var from = parseInt(e.dataTransfer.getData("text/plain"), 10);
          if (isNaN(from) || from === i) return;
          items.splice(i, 0, items.splice(from, 1)[0]);
          apply(items);
        };
        tray.appendChild(item);
      });
      host.appendChild(tray);
    }
    body.addEventListener("input", render);
    editHooks.push(render);
    render();
  }

  // ── document editor ────────────────────────────────────────────────────
  if (docTriggers.length && window.HTMLDialogElement) initDocEditor();

  function initDocEditor() {
    var form = body.form;
    if (!form) return;
    form.classList.add("doc-enhanced"); // hides the plain-textarea fallback

    var holder = body.parentElement; // the hidden fallback field — parks the textarea
    var titleInput = form.querySelector('input[name="title"]');
    var card = document.querySelector(".doc-card");
    var pageWords = document.querySelector(".doc-words");
    var pageNote = document.querySelector(".save-note");
    var isAsync = form.hasAttribute("data-async");
    var kinds = cfg.embeds || ["blob", "series"];
    var rich = !!cfg.editdoc; // rich surface available?
    var mode = "md";          // current surface: "rich" | "md"
    var savedRange = null;    // last selection inside the rich surface
    var upSeq = 0;

    document.execCommand("defaultParagraphSeparator", false, "p");
    document.execCommand("styleWithCSS", false, false);

    // ── bar (row 1): Done · title · Edit/Markdown · count · state ──
    var doneBtn = el("button", { type: "button", text: "Done" });
    var title = el("span", { class: "doc-title" });
    var editBtn = el("button", { type: "button", text: "Edit" });
    var mdBtn = el("button", { type: "button", text: "Markdown" });
    var count = el("span", { class: "doc-meta" });
    var state = el("span", { class: "doc-meta doc-state" });

    var barKids = [doneBtn, title];
    if (rich) barKids.push(el("div", { class: "seg" }, [editBtn, mdBtn]));
    barKids.push(count, state);

    // ── toolbar (row 2): formatting + embeds ──
    var richBtns = [];
    function tbtn(label, cls, tip, fn, richOnly) {
      var b = el("button", { type: "button",
        class: "tb" + (cls ? " " + cls : ""), title: tip, text: label });
      b.addEventListener("mousedown", function (e) { e.preventDefault(); }); // keep the selection
      b.onclick = fn;
      if (richOnly) richBtns.push(b);
      return b;
    }
    function sep() { return el("span", { class: "tb-sep" }); }

    var tbBold = tbtn("B", "tb-b", "Bold — ⌘B", function () { rcmd("bold"); }, true);
    var tbItal = tbtn("I", "tb-i", "Italic — ⌘I", function () { rcmd("italic"); }, true);
    var tbStrike = tbtn("S", "tb-s", "Strikethrough", function () { rcmd("strikeThrough"); }, true);
    var tbLink = tbtn("Link", "", "Link — ⌘K", toggleLinkPop, true);
    var tbH1 = tbtn("H1", "", "Heading 1 — type # ␣", function () { headCmd("H1"); }, true);
    var tbH2 = tbtn("H2", "", "Heading 2 — type ## ␣", function () { headCmd("H2"); }, true);
    var tbH3 = tbtn("H3", "", "Heading 3 — type ### ␣", function () { headCmd("H3"); }, true);
    var tbQuote = tbtn("Quote", "", "Blockquote — type > ␣", toggleQuote, true);
    var tbUl = tbtn("• List", "", "Bulleted list — type - ␣", function () { rcmd("insertUnorderedList"); }, true);
    var tbOl = tbtn("1. List", "", "Numbered list — type 1. ␣", function () { rcmd("insertOrderedList"); }, true);
    var tbCode = tbtn("{ }", "", "Code block — type ```", toggleCodeBlock, true);
    var tbHr = tbtn("—", "", "Divider — type --- ⏎", function () { rcmd("insertHorizontalRule"); ensureShape(); }, true);

    var toolKids = [tbBold, tbItal, tbStrike, tbLink, sep(),
      tbH1, tbH2, tbH3, sep(), tbQuote, tbUl, tbOl, tbCode, tbHr,
      sep(), tbtn("?", "", "Keyboard shortcuts — ⌘/", toggleKeys, false)];
    if (cfg.blobs) {
      toolKids.push(sep());
      if (kinds.indexOf("blob") >= 0) {
        toolKids.push(tbtn("Image", "", "Insert an image",
          function () { snapshotSel(); embedBlob(); }, false));
        toolKids.push(tbtn("File", "", "Attach a file",
          function () { snapshotSel(); fileInput.click(); }, false));
      }
      if (kinds.indexOf("series") >= 0) {
        toolKids.push(tbtn("Gallery", "", "Insert a gallery",
          function () { snapshotSel(); embedSeries(); }, false));
      }
    }
    var tools = el("div", { class: "doc-tools" }, toolKids);

    var fileInput = el("input", { type: "file" });
    fileInput.style.display = "none";
    fileInput.onchange = function () {
      var f = fileInput.files[0];
      fileInput.value = "";
      if (!f) return;
      if (mode === "rich") richUploadFiles([f]);
      else uploadBlob(f).then(function (cid) {
        insert("[" + (f.name || cid) + "](blob://" + cid + ")");
      });
    };

    // ── link popover ──
    var popInput = el("input", { type: "url", placeholder: "https://…" });
    var popApply = el("button", { type: "button", text: "Apply" });
    var popRemove = el("button", { type: "button", text: "Unlink" });
    var pop = el("div", { class: "doc-pop" }, [popInput, popApply, popRemove]);
    pop.hidden = true;

    // ── alt-text row — appears when an image island is selected ──
    var altFig = null;
    var altInput = el("input", { type: "text",
      placeholder: "Alt text — describe this image" });
    var altApply = el("button", { type: "button", text: "Apply" });
    var altPop = el("div", { class: "doc-pop" }, [
      el("span", { class: "doc-meta", text: "Image" }), altInput, altApply,
    ]);
    altPop.hidden = true;
    function hideAltPop() { altPop.hidden = true; altFig = null; }
    function showAltPop(fig) {
      var im = fig.querySelector("img");
      if (!im) return;
      altFig = fig;
      altInput.value = im.getAttribute("alt") || "";
      altPop.hidden = false;
    }
    altApply.onclick = function () {
      if (altFig) {
        var im = altFig.querySelector("img");
        if (im) im.setAttribute("alt", altInput.value.trim());
        markDirty();
      }
      hideAltPop();
      surface.focus();
    };
    altInput.addEventListener("keydown", function (e) {
      if (e.key === "Enter") { e.preventDefault(); altApply.onclick(); }
      if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); hideAltPop(); surface.focus(); }
    });

    // ── surfaces ──
    var surface = el("div", { class: "doc-rich prose" });
    surface.contentEditable = "true";
    surface.spellcheck = true;
    var col = el("div", { class: "doc-col" });
    var dlg = el("dialog", { class: "doc-editor" }, [
      el("header", { class: "doc-bar" }, barKids),
      tools, pop, altPop, fileInput,
      el("div", { class: "doc-scroll" }, [col]),
    ]);
    // Inside the form, so the textarea keeps submitting from either home.
    form.appendChild(dlg);

    // ── words, dirty, autosize ──
    function currentText() {
      return mode === "rich" ? surface.textContent : body.value;
    }
    function wordsText() {
      var t = currentText().trim();
      var n = t ? t.split(/\s+/).length : 0;
      return n.toLocaleString() + (n === 1 ? " word" : " words");
    }
    function countWords() {
      count.textContent = wordsText();
      if (pageWords) pageWords.textContent = wordsText();
    }
    function autosize() {
      if (!dlg.open || mode !== "md") return;
      body.style.height = "auto";
      body.style.height = body.scrollHeight + "px";
    }

    var dirty = false;
    var saving = false;
    var autosaveTimer = null;
    // Autosave: a quiet save 5s after the last edit. Only when async saves
    // are wired, never mid-save, and not before required fields would pass.
    function scheduleAutosave() {
      if (!isAsync) return;
      clearTimeout(autosaveTimer);
      autosaveTimer = setTimeout(function () {
        if (!dirty || saving) return;
        if (titleInput && titleInput.hasAttribute("required") &&
            !titleInput.value.trim()) return;
        save();
      }, 5000);
    }
    function paintState(text, isDirty, isErr) {
      state.textContent = text;
      state.classList.toggle("dirty", !!(isDirty || isErr));
      if (pageNote) {
        pageNote.textContent = text;
        pageNote.classList.toggle("err", !!isErr);
      }
    }
    function markDirty() {
      dirty = true;
      paintState(isAsync ? "Unsaved — ⌘S saves" : "Unsaved", true);
      scheduleAutosave();
    }
    form.addEventListener("input", markDirty); // contenteditable input bubbles here too
    body.addEventListener("input", function () { autosize(); countWords(); });
    editHooks.push(function () {
      autosize();
      countWords();
      markDirty();
      if (!dlg.open) refreshCard();
    });

    window.addEventListener("beforeunload", function (e) {
      if (!dirty) return;
      e.preventDefault();
      e.returnValue = "";
    });
    form.addEventListener("submit", function () { syncBody(); dirty = false; });

    // ── mode switching ──
    function syncBody() {
      if (mode === "rich") body.value = serialize();
    }
    function setToolsMode() {
      richBtns.forEach(function (b) { b.disabled = mode !== "rich"; });
      editBtn.classList.toggle("on", mode === "rich");
      mdBtn.classList.toggle("on", mode !== "rich");
    }
    function showRich() {
      if (!rich) { showMD(); return; }
      fetch(cfg.editdoc, jsonPost({ body: body.value })).then(asJSON)
        .then(function (d) {
          mode = "rich";
          mediaSink = richInsertMD;
          body.classList.remove("doc-text");
          body.style.height = "";
          holder.appendChild(body);
          surface.innerHTML = d.html || "";
          ensureShape();
          col.appendChild(surface);
          setToolsMode();
          updateEmpty();
          countWords();
          surface.focus();
        })
        .catch(function () { showMD(); });
    }
    function showMD() {
      hidePop();
      hideAltPop();
      syncBody();
      mode = "md";
      mediaSink = insert;
      surface.remove();
      col.appendChild(body);
      body.classList.add("doc-text");
      setToolsMode();
      autosize();
      countWords();
      body.focus();
    }
    editBtn.onclick = function () { if (mode !== "rich") showRich(); };
    mdBtn.onclick = function () { if (mode !== "md") showMD(); };

    // ── keyboard cheat sheet (⌘/) ──
    var keysDlg = null;
    function toggleKeys() {
      if (keysDlg && keysDlg.open) { keysDlg.close(); return; }
      if (!keysDlg) {
        var rows = [
          ["⌘S", "Save"],
          ["⌘B / ⌘I", "Bold / italic"],
          ["⌘K", "Link"],
          ["## ␣", "Heading (#–######)"],
          ["- ␣ / 1. ␣", "List / numbered list"],
          ["> ␣", "Quote"],
          ["``` ", "Code block"],
          ["--- ⏎", "Divider"],
          ["paste / drop", "Upload images & files"],
          ["⌘/", "This sheet"],
        ];
        var dl = el("dl", { class: "doc-keys-list" });
        rows.forEach(function (r) {
          dl.appendChild(el("dt", {}, [el("kbd", { text: r[0] })]));
          dl.appendChild(el("dd", { text: r[1] }));
        });
        var x = el("button", { type: "button", class: "embed-x",
          "aria-label": "Close", text: "×" });
        keysDlg = el("dialog", { class: "doc-keys" }, [
          el("div", { class: "embed-head" }, [
            el("strong", { text: "Keyboard shortcuts" }), x]),
          dl,
        ]);
        x.onclick = function () { keysDlg.close(); };
        dlg.appendChild(keysDlg);
      }
      keysDlg.showModal();
    }
    document.addEventListener("keydown", function (e) {
      if ((e.metaKey || e.ctrlKey) && e.key === "/" && dlg.open) {
        e.preventDefault();
        toggleKeys();
      }
    });

    // ── open / close ──
    function openDoc() {
      title.textContent = (titleInput && titleInput.value.trim()) || "Untitled";
      dlg.showModal();
      if (rich) showRich(); else showMD();
      countWords();
    }
    dlg.addEventListener("close", function () {
      hidePop();
      hideAltPop();
      syncBody();
      mode = "md";
      mediaSink = insert;
      surface.remove();
      body.classList.remove("doc-text");
      body.style.height = "";
      holder.appendChild(body);
      refreshCard();
    });
    doneBtn.onclick = function () { dlg.close(); };

    Array.prototype.forEach.call(docTriggers, function (t) {
      t.addEventListener("click", openDoc);
      if (t.tagName !== "BUTTON") {
        t.addEventListener("keydown", function (e) {
          if (e.key === "Enter" || e.key === " ") { e.preventDefault(); openDoc(); }
        });
      }
    });

    // ── the metadata page's document card ──
    function renderInto(node, emptyText) {
      if (!body.value.trim()) {
        node.innerHTML = "";
        node.appendChild(msgRow(emptyText));
        return;
      }
      fetch(cfg.preview, jsonPost({ body: body.value })).then(asJSON)
        .then(function (d) { node.innerHTML = d.html || ""; })
        .catch(function () {
          node.innerHTML = "";
          node.appendChild(msgRow("Could not render the preview."));
        });
    }
    function refreshCard() {
      countWords();
      if (!card || !cfg.preview) return;
      card.innerHTML = "";
      card.appendChild(el("span", { class: "doc-open-hint", text: "Open editor" }));
      if (!body.value.trim()) {
        card.classList.add("empty");
        card.appendChild(el("p", { text: "Nothing written yet — open the editor to start." }));
        return;
      }
      card.classList.remove("empty");
      var prose = el("div", { class: "prose doc-card-prose" });
      card.appendChild(prose);
      renderInto(prose, "");
    }

    // ── save ──
    function save() {
      if (saving) return;
      syncBody();
      if (!isAsync) { form.requestSubmit(); return; }
      saving = true;
      paintState("Saving…", false);
      // URL-encoded, exactly like a native form post — the handlers parse
      // with ParseForm, which never reads a multipart body.
      fetch(form.action, {
        method: "POST",
        headers: { Accept: "application/json" },
        body: new URLSearchParams(new FormData(form)),
      }).then(function (r) {
        return r.json().then(function (d) { return { ok: r.ok, d: d || {} }; });
      }).then(function (res) {
        saving = false;
        if (!res.ok) throw new Error(res.d.error || "save failed");
        dirty = false;
        if (res.d.action) form.action = res.d.action;
        if (res.d.editURL && location.pathname !== res.d.editURL) {
          history.replaceState(null, "", res.d.editURL);
        }
        var at = new Date();
        paintState("Saved " + at.getHours() + ":" +
          String(at.getMinutes()).padStart(2, "0"), false);
      }).catch(function (err) {
        saving = false;
        paintState(err.message || "Save failed", false, true);
      });
    }
    document.addEventListener("keydown", function (e) {
      if ((e.metaKey || e.ctrlKey) && !e.shiftKey && !e.altKey &&
          e.key.toLowerCase() === "s") {
        e.preventDefault();
        save();
      }
    });

    // ── rich surface: selection, commands, toolbar state ──
    function selNode() {
      var s = window.getSelection();
      if (!s.rangeCount) return null;
      var n = s.anchorNode;
      if (!n || !surface.contains(n)) return null;
      return n.nodeType === 1 ? n : n.parentElement;
    }
    function closestIn(selector) {
      var n = selNode();
      var hit = n ? n.closest(selector) : null;
      return hit && surface.contains(hit) ? hit : null;
    }
    function currentBlock() {
      var n = selNode();
      while (n && n.parentElement !== surface) n = n.parentElement;
      return n;
    }
    // snapshotSel captures the live selection synchronously — selectionchange
    // lags behind a focus steal (popover, picker modal), so anything that
    // moves focus away must snapshot first.
    function snapshotSel() {
      var s = window.getSelection();
      if (s.rangeCount && surface.contains(s.anchorNode)) {
        savedRange = s.getRangeAt(0).cloneRange();
      }
    }
    function restoreSel() {
      var s = window.getSelection();
      // A live selection inside the surface wins; only a stolen focus needs
      // the snapshot put back.
      if (s.rangeCount && surface.contains(s.anchorNode)) {
        surface.focus();
        return;
      }
      surface.focus();
      if (savedRange) {
        s.removeAllRanges();
        s.addRange(savedRange);
      }
    }
    function placeCaret(elm) {
      var r = document.createRange();
      r.selectNodeContents(elm);
      r.collapse(true);
      var s = window.getSelection();
      s.removeAllRanges();
      s.addRange(r);
    }
    function caretAtEnd(elm) {
      var s = window.getSelection();
      if (!s.rangeCount) return false;
      var r = s.getRangeAt(0).cloneRange();
      r.selectNodeContents(elm);
      r.setStart(s.getRangeAt(0).endContainer, s.getRangeAt(0).endOffset);
      return r.toString().trim() === "";
    }

    function rcmd(name, val) {
      if (mode !== "rich") return;
      restoreSel();
      document.execCommand(name, false, val || null);
      afterRichEdit();
      refreshTools();
    }
    function headCmd(tag) {
      var cur = currentBlock();
      rcmd("formatBlock", cur && cur.tagName === tag ? "<p>" : "<" + tag.toLowerCase() + ">");
    }
    function toggleQuote() {
      if (closestIn("blockquote")) rcmd("outdent");
      else rcmd("formatBlock", "<blockquote>");
    }
    function toggleCodeBlock() {
      if (closestIn("pre")) { rcmd("formatBlock", "<p>"); return; }
      rcmd("formatBlock", "<pre>");
      var pre = closestIn("pre");
      if (pre) { pre.setAttribute("data-code", "1"); pre.setAttribute("data-lang", ""); }
    }
    function setOn(b, on) { b.classList.toggle("on", !!on); }
    function refreshTools() {
      if (mode !== "rich") return;
      setOn(tbBold, document.queryCommandState("bold"));
      setOn(tbItal, document.queryCommandState("italic"));
      setOn(tbStrike, document.queryCommandState("strikeThrough"));
      var blk = currentBlock();
      var tag = blk ? blk.tagName : "";
      setOn(tbH1, tag === "H1");
      setOn(tbH2, tag === "H2");
      setOn(tbH3, tag === "H3");
      setOn(tbQuote, !!closestIn("blockquote"));
      var li = closestIn("li");
      var listTag = li && li.parentElement ? li.parentElement.tagName : "";
      setOn(tbUl, listTag === "UL");
      setOn(tbOl, listTag === "OL");
      setOn(tbCode, !!closestIn("pre"));
      setOn(tbLink, !!closestIn("a"));
    }
    document.addEventListener("selectionchange", function () {
      if (mode !== "rich" || !dlg.open) return;
      var s = window.getSelection();
      if (s.rangeCount && surface.contains(s.anchorNode)) {
        savedRange = s.getRangeAt(0).cloneRange();
        refreshTools();
      }
    });

    function ensureShape() {
      if (!surface.firstElementChild) {
        surface.innerHTML = "<p><br></p>";
        return;
      }
      var last = surface.lastElementChild;
      if (/^(FIGURE|HR)$/.test(last.tagName)) {
        surface.appendChild(el("p", {}, [document.createElement("br")]));
      }
    }
    function updateEmpty() {
      var empty = !surface.textContent.trim() &&
        !surface.querySelector("figure,img,hr,pre");
      if (empty) surface.setAttribute("data-empty", "1");
      else surface.removeAttribute("data-empty");
    }
    function afterRichEdit() {
      ensureShape();
      updateEmpty();
      countWords();
      markDirty();
    }

    // ── link popover ──
    function toggleLinkPop() {
      if (!pop.hidden) { hidePop(); return; }
      if (mode !== "rich") return;
      snapshotSel(); // the input steals focus next — keep the target range
      var a = closestIn("a");
      popInput.value = a ? (a.getAttribute("href") || "") : "";
      pop.hidden = false;
      popInput.focus();
    }
    function hidePop() { pop.hidden = true; }
    popApply.onclick = function () {
      var url = popInput.value.trim();
      hidePop();
      restoreSel();
      if (!url) return;
      var a = closestIn("a");
      if (a) a.setAttribute("href", url);
      else document.execCommand("createLink", false, url);
      afterRichEdit();
      refreshTools();
    };
    popRemove.onclick = function () {
      hidePop();
      restoreSel();
      document.execCommand("unlink");
      afterRichEdit();
      refreshTools();
    };
    popInput.addEventListener("keydown", function (e) {
      if (e.key === "Enter") { e.preventDefault(); popApply.onclick(); }
      if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); hidePop(); restoreSel(); }
    });

    // ── rich surface: typing behaviors ──
    surface.addEventListener("input", function (e) {
      if (surface.innerHTML === "" || surface.innerHTML === "<br>") {
        surface.innerHTML = "<p><br></p>";
        placeCaret(surface.firstElementChild);
      }
      updateEmpty();
      countWords();
      if (e && e.inputType === "insertText" && (e.data === " " || e.data === "`")) {
        blockShortcut();
      }
    });

    // blockShortcut turns a just-typed markdown prefix into its block: the
    // prefix is deleted through execCommand so native undo stays coherent.
    function blockShortcut() {
      var blk = currentBlock();
      if (!blk || blk.tagName !== "P") return;
      // The browser writes a trailing space as &nbsp; — normalize before
      // matching the markdown prefixes.
      var t = blk.textContent.replace(/\u00a0/g, " ");
      var run = null;
      var mHead = /^(#{1,6}) $/.exec(t);
      if (mHead) {
        run = function () { document.execCommand("formatBlock", false, "<h" + mHead[1].length + ">"); };
      } else if (/^[-*] $/.test(t)) {
        run = function () { document.execCommand("insertUnorderedList"); };
      } else if (/^1[.)] $/.test(t)) {
        run = function () { document.execCommand("insertOrderedList"); };
      } else if (/^> $/.test(t)) {
        run = function () { document.execCommand("formatBlock", false, "<blockquote>"); };
      } else if (/^``` ?$/.test(t)) {
        run = function () {
          document.execCommand("formatBlock", false, "<pre>");
          var pre = closestIn("pre");
          if (pre) { pre.setAttribute("data-code", "1"); pre.setAttribute("data-lang", ""); }
        };
      }
      if (!run) return;
      var s = window.getSelection();
      if (!s.rangeCount) return;
      var r = document.createRange();
      r.setStart(blk, 0);
      r.setEnd(s.anchorNode, s.anchorOffset);
      s.removeAllRanges();
      s.addRange(r);
      document.execCommand("delete");
      run();
      afterRichEdit();
      refreshTools();
    }

    surface.addEventListener("keydown", function (e) {
      if (e.key === "Escape" && !pop.hidden) {
        e.preventDefault();
        e.stopPropagation();
        hidePop();
        return;
      }
      if ((e.metaKey || e.ctrlKey) && !e.shiftKey && e.key.toLowerCase() === "k") {
        e.preventDefault();
        toggleLinkPop();
        return;
      }
      if (e.key !== "Enter") return;
      var blk = currentBlock();
      if (!blk) return;
      // Enter at the end of a heading starts a paragraph, not another heading.
      if (!e.shiftKey && /^H[1-6]$/.test(blk.tagName) && caretAtEnd(blk)) {
        e.preventDefault();
        // Composed from execCommands so native undo stays coherent.
        document.execCommand("insertParagraph");
        document.execCommand("formatBlock", false, "<p>");
        afterRichEdit();
        return;
      }
      // Enter inside a code/verbatim block stays a newline; a blank last
      // line exits to a fresh paragraph.
      if (blk.tagName === "PRE") {
        e.preventDefault();
        if (caretAtEnd(blk) && /\n$/.test(blk.textContent)) {
          blk.textContent = blk.textContent.replace(/\n+$/, "");
          var np = el("p", {}, [document.createElement("br")]);
          blk.after(np);
          placeCaret(np);
        } else {
          document.execCommand("insertText", false, "\n");
        }
        afterRichEdit();
        return;
      }
      // --- on its own line becomes a divider.
      if (!e.shiftKey && blk.tagName === "P" && /^(---+|\*\*\*+)$/.test(blk.textContent.trim())) {
        e.preventDefault();
        var s2 = window.getSelection();
        var r2 = document.createRange();
        r2.selectNodeContents(blk);
        s2.removeAllRanges();
        s2.addRange(r2);
        document.execCommand("delete");
        document.execCommand("insertHorizontalRule");
        afterRichEdit();
      }
    });

    // Clicking an embed island selects it whole, so Backspace removes it.
    surface.addEventListener("click", function (e) {
      var fig = e.target.closest("figure[contenteditable=false]");
      if (!fig || !surface.contains(fig)) { hideAltPop(); return; }
      var r = document.createRange();
      r.selectNode(fig);
      var s = window.getSelection();
      s.removeAllRanges();
      s.addRange(r);
      if (fig.getAttribute("data-blob")) showAltPop(fig);
      else hideAltPop();
    });

    // ── rich surface: paste & drop ──
    surface.addEventListener("paste", function (e) {
      var files = e.clipboardData && e.clipboardData.files;
      if (files && files.length) {
        e.preventDefault();
        richUploadFiles(files);
        return;
      }
      var txt = e.clipboardData ? e.clipboardData.getData("text/plain") : "";
      if (!txt) return;
      e.preventDefault();
      // Markdown-looking text renders into formatting; plain text stays plain.
      if (rich && /\n|^#{1,6} |[*_`~]|\]\(/.test(txt)) {
        fetch(cfg.editdoc, jsonPost({ body: txt })).then(asJSON)
          .then(function (d) {
            restoreSel();
            document.execCommand("insertHTML", false, d.html || "");
            afterRichEdit();
          })
          .catch(function () { document.execCommand("insertText", false, txt); });
      } else {
        document.execCommand("insertText", false, txt);
      }
    });
    surface.addEventListener("dragover", function (e) { e.preventDefault(); });
    surface.addEventListener("drop", function (e) {
      var files = e.dataTransfer && e.dataTransfer.files;
      if (files && files.length) { e.preventDefault(); richUploadFiles(files); }
    });

    function escHTML(s) {
      return s.replace(/&/g, "&amp;").replace(/</g, "&lt;")
        .replace(/>/g, "&gt;").replace(/"/g, "&quot;");
    }
    function richUploadFiles(files) {
      Array.prototype.slice.call(files).forEach(function (f) {
        var isMedia = /^(image|video|audio)\//.test(f.type);
        var id = "ffup" + (++upSeq);
        restoreSel();
        document.execCommand("insertHTML", false,
          '<figure class="doc-embed" contenteditable="false" id="' + id + '">Uploading ' +
          escHTML(f.name || "file") + "…</figure>");
        uploadBlob(f).then(function (cid) {
          var fig = surface.querySelector("#" + id);
          if (!fig) return;
          if (isMedia) {
            fig.removeAttribute("id");
            fig.setAttribute("data-blob", cid);
            fig.innerHTML = '<img class="blob-media standalone" src="' +
              escHTML(blobURL(cid)) + '" alt="">';
          } else {
            var a = el("a", { class: "blob-file", "data-blob": cid,
              href: blobURL(cid), text: f.name || cid });
            fig.replaceWith(el("p", {}, [a]));
          }
          afterRichEdit();
        }).catch(function () {
          var fig = surface.querySelector("#" + id);
          if (fig) fig.remove();
        });
      });
    }

    // richInsertMD renders a markdown snippet through the host (the single
    // source of embed HTML) and splices it at the caret.
    function richInsertMD(md) {
      fetch(cfg.editdoc, jsonPost({ body: md })).then(asJSON)
        .then(function (d) {
          restoreSel();
          document.execCommand("insertHTML", false, d.html || "");
          afterRichEdit();
        });
    }

    // ── serializer: constrained DOM → markdown ──
    function escText(s) {
      return s.replace(/\u00a0/g, " ").replace(/([\\`*_\[\]])/g, "\\$1");
    }
    function trimNL(s) { return s.replace(/\n+$/, ""); }
    function codeSpan(s) {
      s = s.replace(/\n/g, " ");
      return s.indexOf("`") >= 0 ? "`` " + s + " ``" : "`" + s + "`";
    }
    function wrapMarker(n, m) {
      var s = inlineChildren(n);
      var lead = /^\s*/.exec(s)[0];
      var tail = /\s*$/.exec(s)[0];
      var core = s.slice(lead.length, s.length - tail.length);
      if (!core) return s;
      return lead + m + core + m + tail;
    }
    function inlineChildren(elm) {
      var out = "";
      for (var c = elm.firstChild; c; c = c.nextSibling) out += inlineNode(c);
      return out;
    }
    function inlineNode(n) {
      if (n.nodeType === 3) return escText(n.nodeValue);
      if (n.nodeType !== 1) return "";
      var tag = n.tagName;
      // Under hard wraps (FARFIELD.hardWraps — short-note apps) a <br> is a
      // plain newline; long-form markdown needs the explicit backslash break.
      if (tag === "BR") return cfg.hardWraps ? "\n" : "\\\n";
      if (tag === "STRONG" || tag === "B") return wrapMarker(n, "**");
      if (tag === "EM" || tag === "I") return wrapMarker(n, "*");
      if (tag === "DEL" || tag === "S" || tag === "STRIKE") return wrapMarker(n, "~~");
      if (tag === "CODE") {
        if (n.hasAttribute("data-verbatim")) return n.textContent;
        return codeSpan(n.textContent);
      }
      if (tag === "A") {
        var blob = n.getAttribute("data-blob");
        var href = blob ? "blob://" + blob : (n.getAttribute("href") || "");
        return "[" + inlineChildren(n) + "](" + href + ")";
      }
      if (tag === "IMG") {
        var bcid = n.getAttribute("data-blob");
        var src = bcid ? "blob://" + bcid : (n.getAttribute("src") || "");
        return "![" + (n.getAttribute("alt") || "") + "](" + src + ")";
      }
      if (tag === "VIDEO" || tag === "AUDIO") {
        var mcid = n.getAttribute("data-blob");
        return mcid ? "![](blob://" + mcid + ")" : "";
      }
      if (tag === "INPUT" || tag === "UL" || tag === "OL" || tag === "FIGURE") return "";
      return inlineChildren(n);
    }
    function cleanInline(elm) {
      // A trailing <br> is presentational, not a hard break — drop it so an
      // "empty" paragraph serializes to nothing.
      return inlineChildren(elm).replace(/[ \t]+\n/g, "\n")
        .replace(/\\\n\s*$/, "").trim();
    }
    function listToMD(list, depth) {
      var pad = new Array(depth + 1).join("  ");
      var num = parseInt(list.getAttribute("start") || "1", 10);
      var out = [];
      for (var li = list.firstElementChild; li; li = li.nextElementSibling) {
        if (li.tagName !== "LI") continue;
        var marker = list.tagName === "OL" ? (num++) + ". " : "- ";
        var box = li.querySelector(":scope > input[type=checkbox]");
        if (box) marker += box.checked ? "[x] " : "[ ] ";
        var text = "";
        var nested = [];
        for (var c = li.firstChild; c; c = c.nextSibling) {
          if (c.nodeType === 1 && (c.tagName === "UL" || c.tagName === "OL")) {
            nested.push(c);
            continue;
          }
          if (c.nodeType === 1 && c.tagName === "INPUT") continue;
          text += inlineNode(c);
        }
        out.push(pad + marker + text.trim());
        nested.forEach(function (nl) { out.push(listToMD(nl, depth + 1)); });
      }
      return out.join("\n");
    }
    function isBlockTag(tag) {
      return /^(P|DIV|UL|OL|PRE|BLOCKQUOTE|FIGURE|HR|H[1-6])$/.test(tag);
    }
    function hasBlockChild(n) {
      for (var c = n.firstElementChild; c; c = c.nextElementSibling) {
        if (isBlockTag(c.tagName)) return true;
      }
      return false;
    }
    function escBlockStart(s) {
      // A literal block prefix at line start would re-parse as structure.
      return s.replace(/^(#{1,6} |> |[-*+] |\d+[.)] )/, function (x) { return "\\" + x; });
    }
    // containerToMD serializes an element whose children mix inline runs and
    // block elements — contenteditable happily nests a <ul> or <p> inside a
    // paragraph, and dropping those would lose content.
    function containerToMD(n) {
      var parts = [];
      var run = "";
      function flush() {
        var s = run.replace(/\\\n\s*$/, "").trim();
        run = "";
        if (s) parts.push(escBlockStart(s));
      }
      for (var c = n.firstChild; c; c = c.nextSibling) {
        if (c.nodeType === 1 && isBlockTag(c.tagName)) {
          flush();
          var b = blockToMD(c);
          if (b != null && b !== "") parts.push(b);
        } else {
          run += inlineNode(c);
        }
      }
      flush();
      return parts.length ? parts.join("\n\n") : null;
    }
    function blocksToMD(container) {
      var parts = [];
      for (var c = container.firstChild; c; c = c.nextSibling) {
        var s = blockToMD(c);
        if (s != null && s !== "") parts.push(s);
      }
      return parts.join("\n\n");
    }
    function blockToMD(n) {
      if (n.nodeType === 3) {
        var stray = escText(n.nodeValue).trim();
        return stray || null;
      }
      if (n.nodeType !== 1) return null;
      var tag = n.tagName;
      var mh = /^H([1-6])$/.exec(tag);
      if (mh) {
        var ht = cleanInline(n);
        return ht ? "######".slice(0, +mh[1]) + " " + ht : null;
      }
      if (tag === "P" || tag === "DIV") {
        if (hasBlockChild(n)) return containerToMD(n);
        var s = cleanInline(n);
        if (!s) return null;
        return escBlockStart(s);
      }
      if (tag === "UL" || tag === "OL") return listToMD(n, 0);
      if (tag === "BLOCKQUOTE") {
        return blocksToMD(n).split("\n").map(function (l) {
          return l ? "> " + l : ">";
        }).join("\n");
      }
      if (tag === "PRE") {
        if (n.hasAttribute("data-verbatim")) return trimNL(n.textContent);
        var lang = n.getAttribute("data-lang") || "";
        return "```" + lang + "\n" + trimNL(n.textContent) + "\n```";
      }
      if (tag === "HR") return "---";
      if (tag === "FIGURE") {
        var series = n.getAttribute("data-series");
        if (series) return "![](series://" + series + ")";
        var blob = n.getAttribute("data-blob");
        if (blob) {
          var im = n.querySelector("img");
          var alt = im ? (im.getAttribute("alt") || "") : "";
          return "![" + escText(alt) + "](blob://" + blob + ")";
        }
        return null; // an in-flight upload placeholder
      }
      if (hasBlockChild(n)) return containerToMD(n);
      var fallback = cleanInline(n);
      return fallback || null;
    }
    function serialize() {
      return blocksToMD(surface);
    }
  }
})();
