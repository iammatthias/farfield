// sudoku grid behavior — vanilla JS, progressive: the server-rendered grid
// works without this file; it only adds keyboard movement, the check/clear
// buttons, and continuity between visits. Progress lives in localStorage
// only, keyed by artifact and date (daily:sudoku:YYYY-MM-DD) — nothing is
// ever sent to or tracked by the server; the stateless check endpoint judges
// a grid and forgets it.
(function () {
  "use strict";

  var grid = document.getElementById("sudoku-grid");
  if (!grid) return;

  var date = grid.dataset.date;
  var storeKey = "daily:sudoku:" + date;
  var loadedAt = Date.now();

  var cells = Array.prototype.slice.call(grid.querySelectorAll(".cell"));
  var inputs = cells.map(function (c) { return c.querySelector("input"); });
  var status = document.getElementById("sudoku-status");
  var checkBtn = document.getElementById("sudoku-check");
  var clearBtn = document.getElementById("sudoku-clear");

  function setStatus(msg, alert) {
    if (!status) return;
    status.textContent = msg;
    status.classList.toggle("alert", !!alert);
  }

  function clearConflicts() {
    cells.forEach(function (c) { c.classList.remove("conflict"); });
  }

  // entries builds the 81-char wire string: givens from their fixed text,
  // blanks as '0'.
  function entries() {
    return cells
      .map(function (c, i) {
        var inp = inputs[i];
        var v = inp ? inp.value.trim() : c.textContent.trim();
        return /^[1-9]$/.test(v) ? v : "0";
      })
      .join("");
  }

  function fmt(ms) {
    var sec = Math.floor(ms / 1000);
    return Math.floor(sec / 60) + ":" + String(sec % 60).padStart(2, "0");
  }

  // ── local-only continuity ──────────────────────────────────────────────
  // load/save round-trip {entries, solveMs, solved} through localStorage so
  // a reload restores the board and a solved day keeps its time.

  function load() {
    try {
      var raw = localStorage.getItem(storeKey);
      return raw ? JSON.parse(raw) : null;
    } catch (e) {
      return null;
    }
  }

  function save(state) {
    try {
      localStorage.setItem(storeKey, JSON.stringify(state));
    } catch (e) { /* storage full or blocked — play on without continuity */ }
  }

  var saved = load() || {};
  var savedMs = typeof saved.solveMs === "number" && saved.solveMs > 0 ? saved.solveMs : 0;
  var solved = !!saved.solved;

  // Restore the in-progress board into the editable cells.
  if (typeof saved.entries === "string" && saved.entries.length === 81) {
    inputs.forEach(function (inp, i) {
      if (inp && /^[1-9]$/.test(saved.entries[i])) inp.value = saved.entries[i];
    });
  }
  if (solved) setStatus("solved · " + fmt(savedMs));

  function elapsedMs() {
    return solved ? savedMs : savedMs + (Date.now() - loadedAt);
  }

  function persist() {
    save({ entries: entries(), solveMs: elapsedMs(), solved: solved });
  }

  // moveFocus walks from cell i in steps of delta until it lands on an
  // editable cell, skipping givens; it stops silently at the grid edge.
  // Horizontal steps flow across row ends so ←/→ traverse the whole grid.
  function moveFocus(i, delta) {
    for (var j = i + delta; j >= 0 && j < 81; j += delta) {
      if (inputs[j]) { inputs[j].focus(); inputs[j].select(); return; }
    }
  }

  inputs.forEach(function (inp, idx) {
    if (!inp) return;
    var i = idx;

    inp.addEventListener("keydown", function (e) {
      var k = e.key;
      if (k === "ArrowLeft") { e.preventDefault(); moveFocus(i, -1); return; }
      if (k === "ArrowRight") { e.preventDefault(); moveFocus(i, 1); return; }
      if (k === "ArrowUp") { e.preventDefault(); moveFocus(i, -9); return; }
      if (k === "ArrowDown") { e.preventDefault(); moveFocus(i, 9); return; }
      if (k === "Backspace" || k === "Delete" || k === "0") {
        e.preventDefault();
        inp.value = "";
        cells[i].classList.remove("conflict");
        persist();
        return;
      }
      if (/^[1-9]$/.test(k)) {
        e.preventDefault();
        inp.value = k;
        cells[i].classList.remove("conflict");
        persist();
        moveFocus(i, 1);
      }
    });

    // Mobile and IME paths bypass keydown — sanitize whatever arrived.
    inp.addEventListener("input", function () {
      var m = inp.value.match(/[1-9]/g);
      inp.value = m ? m[m.length - 1] : "";
      cells[i].classList.remove("conflict");
      persist();
      if (inp.value) moveFocus(i, 1);
    });
  });

  if (checkBtn) {
    checkBtn.addEventListener("click", function () {
      clearConflicts();
      var e = entries();
      var empty = (e.match(/0/g) || []).length;
      if (empty > 0) {
        persist();
        setStatus(empty + " empty");
        return;
      }
      setStatus("checking…");
      fetch("/sudoku/" + date + "/check", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ entries: e }),
      })
        .then(function (r) {
          if (!r.ok) { setStatus("check failed", true); return null; }
          return r.json();
        })
        .then(function (d) {
          if (!d) return;
          if (d.solved) {
            if (!solved) { savedMs = savedMs + (Date.now() - loadedAt); solved = true; }
            persist();
            setStatus("solved · " + fmt(savedMs));
            return;
          }
          persist();
          if (d.conflicts && d.conflicts.length) {
            d.conflicts.forEach(function (ci) { cells[ci].classList.add("conflict"); });
            setStatus(d.conflicts.length + " conflict" + (d.conflicts.length === 1 ? "" : "s"), true);
          }
        })
        .catch(function () { setStatus("check failed", true); });
    });
  }

  if (clearBtn) {
    clearBtn.addEventListener("click", function () {
      inputs.forEach(function (inp) { if (inp) inp.value = ""; });
      clearConflicts();
      solved = false;
      savedMs = 0;
      loadedAt = Date.now();
      try { localStorage.removeItem(storeKey); } catch (e) { /* ignore */ }
      setStatus("cleared");
    });
  }
})();
