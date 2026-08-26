// farfield meta band — the chip row that replaced the editor sidebar.
//
// Each chip is a button whose popover holds the REAL form control, so the form
// submits the same fields it always did and there is nothing to mirror. This
// script only manages presentation: which popover is open, what a chip's label
// says about the value behind it, the publish state chip, and the assist
// button. It knows nothing about saving — editor.js owns that.
(function () {
  "use strict";

  var band = document.querySelector("[data-band]");
  if (!band) return;
  var form = band.closest("form");

  // ── popovers: one open at a time; esc or outside closes ────────────────
  var open = null; // { wrap, chip, pop }
  function close() {
    if (!open) return;
    open.pop.hidden = true;
    open.chip.classList.remove("open");
    open = null;
  }
  function openPop(wrap, chip, pop) {
    close();
    pop.hidden = false;
    chip.classList.add("open");
    open = { wrap: wrap, chip: chip, pop: pop };
    var field = pop.querySelector("[data-chip-value]");
    if (field) field.focus({ preventScroll: true });
  }
  band.addEventListener("click", function (e) {
    var chip = e.target.closest("[data-chip]");
    if (!chip) return;
    var wrap = chip.closest(".chip-wrap");
    var pop = wrap.querySelector(".chip-pop");
    if (open && open.chip === chip) close();
    else openPop(wrap, chip, pop);
  });
  document.addEventListener("click", function (e) {
    if (open && !open.wrap.contains(e.target)) close();
  });
  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape" && open) {
      e.preventDefault();
      e.stopPropagation();
      var chip = open.chip;
      close();
      chip.focus({ preventScroll: true });
    }
  }, true);

  // ── chip labels follow their values ────────────────────────────────────
  // A filled chip shows its value; an empty one invites. Excerpt is the
  // exception — its value is a paragraph, so the chip only says whether it
  // exists (data-filled-label / data-empty-label).
  function labelFor(chip, field) {
    var v = (field.tagName === "SELECT")
      ? (field.selectedOptions[0] ? field.selectedOptions[0].textContent : "")
      : field.value;
    v = v.trim();
    if (chip.hasAttribute("data-filled-label")) {
      return v ? chip.getAttribute("data-filled-label")
        : chip.getAttribute("data-empty-label");
    }
    return v || ("+ " + chip.getAttribute("data-chip"));
  }
  band.querySelectorAll(".chip-wrap").forEach(function (wrap) {
    var chip = wrap.querySelector("[data-chip]");
    var field = wrap.querySelector("[data-chip-value]");
    if (!chip || !field) return;
    function sync() {
      var v = (field.tagName === "SELECT") ? "x"
        : field.value.trim();
      chip.textContent = labelFor(chip, field);
      chip.classList.toggle("filled", !!v);
      chip.classList.toggle("empty", !v);
    }
    field.addEventListener("input", sync);
    field.addEventListener("change", sync);
    // Enter applies (closes) rather than submitting the whole form.
    if (field.tagName !== "TEXTAREA") {
      field.addEventListener("keydown", function (e) {
        if (e.key === "Enter") { e.preventDefault(); close(); chip.focus({ preventScroll: true }); }
      });
    }
  });

  // ── the publish state chip ─────────────────────────────────────────────
  // The chip IS the checkbox: tap toggles, save commits. The pill's pub note
  // says "will publish"/"will unpublish" whenever the toggle differs from
  // what is saved, so nothing is a surprise.
  var stateChip = form.querySelector("[data-state-chip]");
  var check = form.querySelector('input[name="published"]');
  var pubNote = form.querySelector("[data-pub-note]");
  if (stateChip && check) {
    var savedState = check.checked;
    function paintChip() {
      stateChip.textContent = check.checked ? "published" : "draft";
      stateChip.classList.toggle("state-live", check.checked);
      stateChip.classList.toggle("state-draft", !check.checked);
      stateChip.title = check.checked
        ? (savedState ? "public — tap to unpublish on save"
          : "goes public on save · the site rebuilds")
        : "draft — a 404 to everyone else";
      if (pubNote) {
        pubNote.textContent = check.checked === savedState ? ""
          : (check.checked ? "will publish" : "will unpublish");
      }
    }
    stateChip.addEventListener("click", function () {
      check.checked = !check.checked;
      paintChip();
      // The form's dirty tracking must hear the change.
      check.dispatchEvent(new Event("input", { bubbles: true }));
    });
    // A completed save makes the current state the saved state.
    form.addEventListener("submit", function () { savedState = check.checked; paintChip(); });
    document.addEventListener("farfield:saved", function () { savedState = check.checked; paintChip(); });
    paintChip();
  }

  // ── assist ─────────────────────────────────────────────────────────────
  // Proposes into the tags and excerpt fields; nothing is saved until the
  // author saves. Manual on purpose — metadata that writes itself on every
  // save stops being read.
  var assistBtns = form.querySelectorAll("[data-assist]");
  if (assistBtns.length) {
    var note = form.querySelector("[data-assist-note]");
    var say = function (t) { if (note) note.textContent = t; };
    assistBtns.forEach(function (btn) {
      btn.addEventListener("click", function () {
        var body = document.getElementById("body");
        var bodyText = body ? body.value : "";
        var surface = form.querySelector(".doc-rich");
        // In rich mode the textarea is stale until save serializes it; the
        // surface's text is close enough for suggestion purposes.
        if (surface && surface.isConnected) bodyText = surface.textContent;
        if (!bodyText.trim()) { say("write something first"); return; }
        assistBtns.forEach(function (b) { b.disabled = true; });
        say("reading…");
        fetch("/assist", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            title: (document.getElementById("title") || {}).value || "",
            body: bodyText,
          }),
        }).then(function (r) {
          return r.json().then(function (d) { return { ok: r.ok, d: d }; });
        }).then(function (res) {
          if (!res.ok) { say(res.d.error || "assist failed"); return; }
          ["tags", "excerpt"].forEach(function (id) {
            var f = document.getElementById(id);
            if (!f) return;
            var v = id === "tags"
              ? (res.d.tags || []).join(", ")
              : (res.d.excerpt || "");
            if (!v) return;
            f.value = v;
            f.dispatchEvent(new Event("input", { bubbles: true }));
          });
          say("suggested — edit freely, then save");
        }).catch(function () {
          say("assist unreachable");
        }).finally(function () {
          assistBtns.forEach(function (b) { b.disabled = false; });
        });
      });
    });
  }
})();
