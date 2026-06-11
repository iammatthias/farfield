// wordle grid behavior — vanilla JS. The server-rendered grid shows restored
// guesses without this file; it adds the typing surface: physical keyboards
// via document keydown, mobile keyboards via the visually-hidden input, Enter
// submits to the stateless guess endpoint, and authed visitors get their
// state persisted after every accepted guess.
(function () {
  "use strict";

  var grid = document.getElementById("wordle-grid");
  if (!grid) return;

  var date = grid.dataset.date;
  var authed = grid.dataset.authed === "true";
  var hard = grid.dataset.hard === "true";
  var over = grid.dataset.over === "true";
  var savedMs = parseInt(grid.dataset.solvems || "0", 10) || 0;
  var loadedAt = Date.now();
  var guesses = [];
  try { guesses = JSON.parse(grid.dataset.guesses || "[]") || []; } catch (e) { guesses = []; }

  var rows = Array.prototype.slice.call(grid.querySelectorAll(".wordle-row"));
  var status = document.getElementById("wordle-status");
  var input = document.getElementById("wordle-input");
  var hardBox = document.getElementById("wordle-hardmode");

  var current = ""; // letters typed into the active (first empty) row
  var busy = false; // a guess is in flight — ignore input until it lands

  function setStatus(msg, alert) {
    if (!status) return;
    status.textContent = msg;
    status.classList.toggle("alert", !!alert);
  }

  function tiles(rowIdx) {
    return Array.prototype.slice.call(rows[rowIdx].querySelectorAll(".tile"));
  }

  // paintCurrent mirrors the typed letters into the active row.
  function paintCurrent() {
    if (guesses.length >= rows.length) return;
    tiles(guesses.length).forEach(function (t, i) {
      t.textContent = i < current.length ? current[i].toUpperCase() : "";
      t.classList.toggle("pending", i < current.length);
    });
  }

  // paintFeedback colors one finished row: g solid fill, y wash+rule, - ghost.
  function paintFeedback(rowIdx, word, fb) {
    tiles(rowIdx).forEach(function (t, i) {
      t.textContent = word[i].toUpperCase();
      t.classList.remove("pending");
      t.classList.add(fb[i] === "g" ? "s-g" : fb[i] === "y" ? "s-y" : "s-a");
    });
  }

  function saveState(solvedNow) {
    if (!authed) return;
    fetch("/wordle/" + date + "/state", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        guesses: guesses,
        hard: hard,
        solveMs: savedMs + (Date.now() - loadedAt),
      }),
    }).catch(function () {
      setStatus((solvedNow ? "solved · " : "") + "save failed", true);
    });
  }

  function submit() {
    if (busy || over || current.length !== 5) {
      if (!busy && !over && current.length < 5) setStatus("five letters needed");
      return;
    }
    busy = true;
    setStatus("checking…");
    fetch("/wordle/" + date + "/guess", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ guess: current, prior: guesses, hard: hard }),
    })
      .then(function (r) {
        if (!r.ok) throw new Error("bad response");
        return r.json();
      })
      .then(function (d) {
        busy = false;
        if (d.invalid) {
          setStatus(d.reason || "not in word list", true);
          return; // the guess is not consumed — the row stays editable
        }
        var word = current;
        current = "";
        paintFeedback(guesses.length, word, d.feedback.join(""));
        guesses.push(word);
        if (hardBox) hardBox.disabled = true; // mode locks once play begins
        if (d.solved) {
          over = true;
          setStatus("solved in " + guesses.length + "/" + rows.length +
            (authed ? " · saving…" : " · log in to keep streaks"));
        } else if (d.over) {
          over = true;
          setStatus("out of guesses · answer " + (d.answer || "").toUpperCase(), true);
        } else {
          setStatus(guesses.length + "/" + rows.length + " guessed");
        }
        if (authed) {
          saveState(d.solved);
          if (d.solved) setStatus("solved in " + guesses.length + "/" + rows.length);
        }
      })
      .catch(function () {
        busy = false;
        setStatus("guess failed — try again", true);
      });
  }

  function handleKey(key) {
    if (over || busy) return;
    if (key === "Enter") { submit(); return; }
    if (key === "Backspace") {
      current = current.slice(0, -1);
      paintCurrent();
      return;
    }
    if (/^[a-zA-Z]$/.test(key) && current.length < 5) {
      current += key.toLowerCase();
      paintCurrent();
    }
  }

  // Physical keyboards: document-level keydown, unless a real field (the
  // hidden input excluded) has focus.
  document.addEventListener("keydown", function (e) {
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    var t = e.target;
    if (t && t !== input && (t.tagName === "INPUT" || t.tagName === "TEXTAREA")) return;
    if (e.key === "Enter" || e.key === "Backspace" || /^[a-zA-Z]$/.test(e.key)) {
      e.preventDefault();
      handleKey(e.key);
    }
  });

  // Mobile keyboards: tapping the grid focuses the hidden input; its input
  // events feed the same path. The field is cleared after every event so it
  // only ever relays the latest keystroke.
  if (input) {
    grid.addEventListener("click", function () { if (!over) input.focus(); });
    input.addEventListener("input", function () {
      var v = input.value;
      input.value = "";
      for (var i = 0; i < v.length; i++) handleKey(v[i]);
    });
    input.addEventListener("keydown", function (e) {
      // letters arrive via the input event; only the control keys need relaying
      if (e.key === "Enter" || e.key === "Backspace") {
        e.preventDefault();
        handleKey(e.key);
      }
    });
  }

  if (hardBox) {
    hardBox.addEventListener("change", function () { hard = hardBox.checked; });
  }
})();
