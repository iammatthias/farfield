// sudoku grid behavior — vanilla JS, progressive: the server-rendered grid
// works without this file; it only adds keyboard movement, the check/clear
// buttons, and solve-state saves for authed visitors.
(function () {
  "use strict";

  var grid = document.getElementById("sudoku-grid");
  if (!grid) return;

  var date = grid.dataset.date;
  var authed = grid.dataset.authed === "true";
  var savedMs = parseInt(grid.dataset.solvems || "0", 10) || 0;
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
        return;
      }
      if (/^[1-9]$/.test(k)) {
        e.preventDefault();
        inp.value = k;
        cells[i].classList.remove("conflict");
        moveFocus(i, 1);
      }
    });

    // Mobile and IME paths bypass keydown — sanitize whatever arrived.
    inp.addEventListener("input", function () {
      var m = inp.value.match(/[1-9]/g);
      inp.value = m ? m[m.length - 1] : "";
      cells[i].classList.remove("conflict");
      if (inp.value) moveFocus(i, 1);
    });
  });

  if (checkBtn) {
    checkBtn.addEventListener("click", function () {
      clearConflicts();
      var e = entries();
      var empty = (e.match(/0/g) || []).length;
      if (!authed) {
        setStatus(empty > 0
          ? empty + " empty · log in to save & check"
          : "complete · log in to save & check");
        return;
      }
      if (empty > 0) setStatus("saving · " + empty + " empty…");
      else setStatus("checking…");
      fetch("/sudoku/" + date + "/state", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ entries: e, solveMs: savedMs + (Date.now() - loadedAt) }),
      })
        .then(function (r) {
          if (r.status === 401) { setStatus("session expired · log in again", true); return null; }
          if (!r.ok) { setStatus("save failed", true); return null; }
          return r.json();
        })
        .then(function (d) {
          if (!d) return;
          if (d.solved) {
            setStatus("saved · solved in " + fmt(savedMs + (Date.now() - loadedAt)));
            return;
          }
          if (d.conflicts && d.conflicts.length) {
            d.conflicts.forEach(function (ci) { cells[ci].classList.add("conflict"); });
            setStatus("saved · " + d.conflicts.length + " conflict" + (d.conflicts.length === 1 ? "" : "s"), true);
            return;
          }
          setStatus(empty > 0 ? "saved · " + empty + " empty" : "saved");
        })
        .catch(function () { setStatus("save failed", true); });
    });
  }

  if (clearBtn) {
    clearBtn.addEventListener("click", function () {
      inputs.forEach(function (inp) { if (inp) inp.value = ""; });
      clearConflicts();
      setStatus(authed ? "cleared · not saved yet" : "cleared");
    });
  }
})();
