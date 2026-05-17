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

  // Insert text at the textarea's cursor, then refocus.
  function insert(text) {
    var s = body.selectionStart, e = body.selectionEnd;
    body.value = body.value.slice(0, s) + text + body.value.slice(e);
    body.selectionStart = body.selectionEnd = s + text.length;
    body.focus();
  }

  // Build and open a <dialog>. fill(bodyEl, dialogEl) populates it.
  function openDialog(title, fill) {
    var d = document.createElement("dialog");
    d.className = "embed-dialog";
    var head = document.createElement("div");
    head.className = "embed-head";
    head.innerHTML = "<strong></strong>";
    head.firstChild.textContent = title;
    var close = document.createElement("button");
    close.type = "button";
    close.textContent = "Close";
    close.onclick = function () { d.close(); };
    head.appendChild(close);
    var content = document.createElement("div");
    content.className = "embed-content";
    d.appendChild(head);
    d.appendChild(content);
    document.body.appendChild(d);
    d.addEventListener("close", function () { d.remove(); });
    fill(content, d);
    d.showModal();
    return d;
  }

  function note(parent, msg) {
    var p = document.createElement("p");
    p.className = "muted";
    p.textContent = msg;
    parent.appendChild(p);
    return p;
  }

  // A paginated grid of blob thumbnails. onClick(cid, cell) fires per blob.
  // Returns a wrapper element; selection state lives in the caller.
  function blobGrid(onClick) {
    var wrap = document.createElement("div");
    var grid = document.createElement("div");
    grid.className = "embed-grid";
    wrap.appendChild(grid);
    var loading = note(grid, "Loading blobs…");
    var page = 0, pages = 1;

    var more = document.createElement("button");
    more.type = "button";
    more.className = "embed-row embed-more";
    more.textContent = "Load more";
    more.style.display = "none";
    more.onclick = load;
    wrap.appendChild(more);

    function load() {
      page++;
      fetch(cfg.blobs + "/blobs?page=" + page)
        .then(function (r) { return r.json(); })
        .then(function (data) {
          if (loading) { loading.remove(); loading = null; }
          pages = (data && data.pages) || 1;
          var blobs = (data && data.blobs) || [];
          if (page === 1 && !blobs.length) {
            note(grid, "No blobs yet — upload one.");
            return;
          }
          blobs.forEach(function (b) {
            var cell = document.createElement("button");
            cell.type = "button";
            cell.className = "embed-cell";
            cell.title = b.cid;
            var img = document.createElement("img");
            img.loading = "lazy";
            img.alt = "";
            img.src = cfg.blobs + "/blobs/" + b.cid;
            cell.appendChild(img);
            cell.onclick = function () { onClick(b.cid, cell); };
            grid.appendChild(cell);
          });
          more.style.display = page < pages ? "" : "none";
        })
        .catch(function () {
          if (loading) { loading.remove(); loading = null; }
          note(grid, "Could not load blobs.");
        });
    }

    load();
    return wrap;
  }

  // Upload a file through the host app's proxy; resolves to the new CID.
  function uploadBlob(file) {
    var fd = new FormData();
    fd.append("file", file);
    return fetch("/embed/blob", { method: "POST", body: fd })
      .then(function (r) {
        if (!r.ok) throw new Error("upload failed");
        return r.json();
      })
      .then(function (d) { return d.cid; });
  }

  // A file <input> styled as a button; onDone(cid) after a successful upload.
  function uploadControl(onDone) {
    var label = document.createElement("label");
    label.className = "embed-upload";
    label.textContent = "Upload a file…";
    var input = document.createElement("input");
    input.type = "file";
    input.onchange = function () {
      if (!input.files.length) return;
      label.textContent = "Uploading…";
      uploadBlob(input.files[0])
        .then(function (cid) { label.textContent = "Upload a file…"; onDone(cid); })
        .catch(function () { label.textContent = "Upload failed — try again"; });
    };
    label.appendChild(input);
    return label;
  }

  // ── Embed a single blob ───────────────────────────────────────────────────
  function embedBlob() {
    openDialog("Embed a blob", function (c, d) {
      c.appendChild(uploadControl(function (cid) {
        insert("![](blob://" + cid + ")");
        d.close();
      }));
      c.appendChild(blobGrid(function (cid) {
        insert("![](blob://" + cid + ")");
        d.close();
      }));
    });
  }

  // ── Embed a series ────────────────────────────────────────────────────────
  function embedSeries() {
    openDialog("Embed a series", function (c, d) {
      var tabs = document.createElement("div");
      tabs.className = "embed-tabs";
      var browseBtn = document.createElement("button");
      browseBtn.type = "button";
      browseBtn.textContent = "Pick existing";
      var createBtn = document.createElement("button");
      createBtn.type = "button";
      createBtn.textContent = "Build new";
      tabs.appendChild(browseBtn);
      tabs.appendChild(createBtn);
      c.appendChild(tabs);
      var pane = document.createElement("div");
      c.appendChild(pane);

      function browse() {
        browseBtn.className = "active";
        createBtn.className = "";
        pane.innerHTML = "";
        var list = document.createElement("div");
        list.className = "embed-list";
        note(list, "Loading series…");
        fetch(cfg.content + "/api/series")
          .then(function (r) { return r.json(); })
          .then(function (data) {
            list.innerHTML = "";
            var series = (data && data.series) || [];
            if (!series.length) { note(list, "No series yet — build one."); return; }
            series.forEach(function (s) {
              var row = document.createElement("button");
              row.type = "button";
              row.className = "embed-row";
              row.textContent = s.title || s.rkey;
              row.onclick = function () {
                insert("![](series://" + s.rkey + ")");
                d.close();
              };
              list.appendChild(row);
            });
          })
          .catch(function () { list.innerHTML = ""; note(list, "Could not load series."); });
        pane.appendChild(list);
      }

      function create() {
        createBtn.className = "active";
        browseBtn.className = "";
        pane.innerHTML = "";
        var picked = []; // CIDs, in selection order

        var title = document.createElement("input");
        title.type = "text";
        title.placeholder = "Series title (optional)";
        title.className = "embed-title";
        pane.appendChild(title);

        var grid = blobGrid(function (cid, cell) {
          var i = picked.indexOf(cid);
          if (i >= 0) { picked.splice(i, 1); cell.classList.remove("sel"); }
          else { picked.push(cid); cell.classList.add("sel"); }
          count.textContent = picked.length + " selected";
        });
        pane.appendChild(uploadControl(function (cid) {
          picked.push(cid);
          count.textContent = picked.length + " selected (uploaded — appears on reopen)";
        }));
        pane.appendChild(grid);

        var bar = document.createElement("div");
        bar.className = "embed-actions";
        var count = document.createElement("span");
        count.className = "muted";
        count.textContent = "0 selected";
        var go = document.createElement("button");
        go.type = "button";
        go.textContent = "Create & embed";
        go.onclick = function () {
          if (!picked.length) { count.textContent = "Select at least one blob"; return; }
          go.disabled = true;
          go.textContent = "Creating…";
          fetch("/embed/series", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ title: title.value, cids: picked }),
          })
            .then(function (r) {
              if (!r.ok) throw new Error("create failed");
              return r.json();
            })
            .then(function (s) {
              insert("![](series://" + s.rkey + ")");
              d.close();
            })
            .catch(function () {
              go.disabled = false;
              go.textContent = "Create & embed";
              count.textContent = "Could not create the series.";
            });
        };
        bar.appendChild(count);
        bar.appendChild(go);
        pane.appendChild(bar);
      }

      browseBtn.onclick = browse;
      createBtn.onclick = create;
      browse();
    });
  }

  toolbar.addEventListener("click", function (ev) {
    var btn = ev.target.closest("[data-embed]");
    if (!btn) return;
    ev.preventDefault();
    if (btn.dataset.embed === "blob") embedBlob();
    else if (btn.dataset.embed === "series") embedSeries();
  });
})();
