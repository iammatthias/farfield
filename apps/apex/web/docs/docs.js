// Full view for observation figures. Each figure gets a toggle above its
// plate; the toggle opens the diagram in a <dialog> at reading scale
// (1.5× the viewBox, so 11px labels render ~16px), pannable by scroll,
// touch, or mouse drag. One dialog is shared across figures.
(function () {
  "use strict";

  var dialog, pan, title;

  function build() {
    dialog = document.createElement("dialog");
    dialog.className = "obs-dialog";

    var bar = document.createElement("div");
    bar.className = "obs-dialog-bar";
    title = document.createElement("span");
    title.className = "obs-dialog-title";
    var close = document.createElement("button");
    close.type = "button";
    close.className = "obs-close";
    close.textContent = "close";
    close.addEventListener("click", function () { dialog.close(); });
    bar.appendChild(title);
    bar.appendChild(close);

    pan = document.createElement("div");
    pan.className = "obs-pan";

    dialog.appendChild(bar);
    dialog.appendChild(pan);

    // A press-and-release on the dialog element itself is a click on the
    // backdrop — every real target inside is a child. Both ends of the
    // gesture must land there with no movement between them, so a drag
    // that strays over the backdrop can never dismiss the plate.
    var downOnBackdrop = false, bx = 0, by = 0;
    dialog.addEventListener("pointerdown", function (e) {
      downOnBackdrop = e.target === dialog;
      bx = e.clientX;
      by = e.clientY;
    });
    dialog.addEventListener("pointerup", function (e) {
      if (downOnBackdrop && e.target === dialog &&
          Math.hypot(e.clientX - bx, e.clientY - by) < 6) {
        dialog.close();
      }
      downOnBackdrop = false;
    });

    // Drag-to-pan for mouse only; touch already pans via native overflow
    // scrolling, and fighting it doubles the movement.
    var dragging = false, lx = 0, ly = 0;
    pan.addEventListener("pointerdown", function (e) {
      if (e.pointerType !== "mouse" || e.button !== 0) return;
      dragging = true;
      lx = e.clientX;
      ly = e.clientY;
      pan.classList.add("dragging");
      try { pan.setPointerCapture(e.pointerId); } catch (_) {}
    });
    pan.addEventListener("pointermove", function (e) {
      if (!dragging) return;
      pan.scrollLeft -= e.clientX - lx;
      pan.scrollTop -= e.clientY - ly;
      lx = e.clientX;
      ly = e.clientY;
    });
    function end() {
      dragging = false;
      pan.classList.remove("dragging");
    }
    pan.addEventListener("pointerup", end);
    pan.addEventListener("pointercancel", end);

    document.body.appendChild(dialog);
  }

  function open(fig) {
    var svg = fig.querySelector("svg");
    if (!svg) return;
    if (!dialog) build();

    pan.textContent = "";
    var clone = svg.cloneNode(true);
    var vb = svg.viewBox.baseVal;
    var w = Math.round(vb.width * 1.5) + "px";
    clone.style.width = w;
    clone.style.minWidth = w;
    clone.style.height = "auto";
    pan.appendChild(clone);

    var cap = fig.querySelector("figcaption");
    title.textContent = cap ? cap.textContent : "observation";
    dialog.showModal();
    pan.scrollLeft = 0;
    pan.scrollTop = 0;
  }

  document.querySelectorAll(".observation").forEach(function (fig) {
    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "obs-expand";
    btn.textContent = "full view";
    btn.setAttribute("aria-haspopup", "dialog");
    btn.addEventListener("click", function () { open(fig); });
    fig.insertBefore(btn, fig.firstChild);
  });
})();
