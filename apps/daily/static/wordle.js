// wordle grid behavior — vanilla JS. The server renders an empty grid; this
// file adds the typing surface: physical keyboards via document keydown,
// mobile keyboards via the visually-hidden input, Enter submits to the
// stateless guess endpoint. Continuity between visits lives in localStorage
// only, keyed by artifact and date (daily:wordle:YYYY-MM-DD) — guesses,
// scored feedback, the hard-mode flag, and completion; nothing is ever sent
// to or tracked by the server beyond each one-shot guess call.
(function () {
  "use strict";

  var grid = document.getElementById("wordle-grid");
  if (!grid) return;

  var date = grid.dataset.date;
  var storeKey = "daily:wordle:" + date;
  var loadedAt = Date.now();

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

  // ── local-only continuity ──────────────────────────────────────────────
  // load/save round-trip the day's game through localStorage so a reload
  // repaints the board without asking the server anything.

  function load() {
    try {
      var raw = localStorage.getItem(storeKey);
      return raw ? JSON.parse(raw) : null;
    } catch (e) {
      return null;
    }
  }

  function saveState() {
    try {
      localStorage.setItem(storeKey, JSON.stringify({
        guesses: guesses,
        feedback: feedback,
        hard: hard,
        solved: solvedGame,
        over: over,
        answer: answer,
        solveMs: solvedGame ? savedMs : savedMs + (Date.now() - loadedAt),
      }));
    } catch (e) { /* storage full or blocked — play on without continuity */ }
  }

  var saved = load() || {};
  var guesses = Array.isArray(saved.guesses) ? saved.guesses : [];
  var feedback = Array.isArray(saved.feedback) ? saved.feedback : [];
  var hard = !!saved.hard;
  var solvedGame = !!saved.solved;
  var over = !!saved.over;
  var answer = typeof saved.answer === "string" ? saved.answer : "";
  var savedMs = typeof saved.solveMs === "number" && saved.solveMs > 0 ? saved.solveMs : 0;

  // A stored game only restores if guesses and feedback line up — anything
  // odd degrades to a fresh board rather than a broken one.
  if (guesses.length !== feedback.length || guesses.length > rows.length) {
    guesses = [];
    feedback = [];
    solvedGame = false;
    over = false;
    answer = "";
  }

  function fmt(ms) {
    var sec = Math.floor(ms / 1000);
    return Math.floor(sec / 60) + ":" + String(sec % 60).padStart(2, "0");
  }

  // Repaint the restored game.
  guesses.forEach(function (g, i) { paintFeedback(i, g, feedback[i]); });
  if (hardBox) {
    hardBox.checked = hard;
    hardBox.disabled = over || guesses.length > 0; // mode locks once play begins
  }
  if (solvedGame) {
    setStatus("solved in " + guesses.length + "/" + rows.length +
      (savedMs > 0 ? " · " + fmt(savedMs) : ""));
  } else if (over) {
    setStatus("out of guesses" + (answer ? " · answer " + answer.toUpperCase() : ""), true);
  } else if (guesses.length > 0) {
    setStatus(guesses.length + "/" + rows.length + " guessed");
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
        var fb = d.feedback.join("");
        current = "";
        paintFeedback(guesses.length, word, fb);
        guesses.push(word);
        feedback.push(fb);
        if (hardBox) hardBox.disabled = true; // mode locks once play begins
        if (d.solved) {
          over = true;
          solvedGame = true;
          savedMs = savedMs + (Date.now() - loadedAt);
          loadedAt = Date.now();
          setStatus("solved in " + guesses.length + "/" + rows.length +
            " · " + fmt(savedMs));
        } else if (d.over) {
          over = true;
          answer = d.answer || "";
          setStatus("out of guesses · answer " + answer.toUpperCase(), true);
        } else {
          setStatus(guesses.length + "/" + rows.length + " guessed");
        }
        saveState();
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
    hardBox.addEventListener("change", function () {
      hard = hardBox.checked;
      saveState();
    });
  }
})();
