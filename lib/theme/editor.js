// farfield editor — embed blobs and series into a markdown body.
//
// The host page provides a <textarea id="body">, an embed toolbar with
// buttons (data-embed="blob" / data-embed="series"), and a global
// window.FARFIELD = { blobs, content } carrying the public service URLs.
// Reads go straight to those services (CORS-enabled); writes go through the
// host app's session-gated /embed/* proxy so service keys stay server-side.
(function () {
  "use strict";

  var cfg = window.FARFIELD || {};
  var body = document.getElementById("body");
  var toolbar = document.querySelector(".embed-toolbar");
  if (!body || !toolbar) return;

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

  // Insert text at the textarea's cursor, then refocus.
  function insert(text) {
    var s = body.selectionStart, e = body.selectionEnd;
    body.value = body.value.slice(0, s) + text + body.value.slice(e);
    body.selectionStart = body.selectionEnd = s + text.length;
    body.focus();
  }

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
    function pick(cid) { insert("![](blob://" + cid + ")"); m.dlg.close(); }
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
            insert("![](series://" + s.slug + ")");
            m.dlg.close();
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
        insert("![](series://" + s.slug + ")");
        m.dlg.close();
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

  toolbar.addEventListener("click", function (e) {
    var btn = e.target.closest("[data-embed]");
    if (!btn) return;
    e.preventDefault();
    if (btn.dataset.embed === "blob") embedBlob();
    else if (btn.dataset.embed === "series") embedSeries();
  });
})();
