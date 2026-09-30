;; farfield editor — editing: the host-facing API.
;;
;; A host (a browser page, a desktop window) owns nothing but a surface and an
;; event loop. It hands the editor keys, text, pointer and wheel events; the
;; editor changes the document, and render() draws the result into FB.
;;
;; Keys (key code, then mods):
;;   1 ←  2 →  3 ↑  4 ↓  5 Home  6 End  7 PageUp  8 PageDown
;;   9 Backspace  10 Delete  11 Enter  12 Tab  13 Escape
;; mods: 1 shift   2 word (⌥ on a Mac, Ctrl elsewhere)   4 command (⌘ / Ctrl)
;;
;; Commands:
;;   1 bold  2 italic  3 code  4 link  5 strike  6 H1  7 H2  8 H3
;;   9 quote  10 bullets  11 numbers  12 code block  13 undo  14 redo
;;   15 select all  16 rule  17 select word  18 select line
(module
  (global $goal_x (mut i32) (i32.const -1)) ;; remembered x for vertical motion
  ;; page mode: the host page is the document. The column fills the surface
  ;; (the page supplies margins), there is no scrollbar, and the host drives
  ;; $scroll from the window's own scroll position.
  (global $page_mode (mut i32) (i32.const 0))

  ;; ── setup ────────────────────────────────────────────────────────────────

  (func (export "init")
    (call $default_palette)
    (call $gamma_init)
    (call $gcache_clear)
    (global.set $len (i32.const 0))
    (global.set $sel_a (i32.const 0))
    (global.set $sel_h (i32.const 0))
    (call $undo_reset)
    (global.set $dirty (i32.const 1)))

  ;; resize sets the surface size in device pixels and the device pixel ratio
  ;; (× 100), growing memory for the framebuffer. Returns the framebuffer
  ;; address, or 0 when the memory could not grow.
  (func (export "resize") (param $w i32) (param $h i32) (param $dpr i32) (result i32)
    (local $need i32) (local $have i32) (local $measure i32)
    (local.set $w (call $clamp (local.get $w) (i32.const 1) (i32.const 8192)))
    (local.set $h (call $clamp (local.get $h) (i32.const 1) (i32.const 8192)))
    (local.set $need (i32.shr_u (i32.add (i32.add (global.get $FB) (i32.mul (i32.mul (local.get $w) (local.get $h)) (i32.const 4)))
                                         (i32.const 0xFFFF)) (i32.const 16)))
    (local.set $have (memory.size))
    (if (i32.gt_u (local.get $need) (local.get $have))
      (then (if (i32.lt_s (memory.grow (i32.sub (local.get $need) (local.get $have))) (i32.const 0))
        (then (return (i32.const 0))))))
    (if (i32.ne (local.get $dpr) (global.get $dpr)) (then (call $gcache_clear)))
    (global.set $W (local.get $w))
    (global.set $H (local.get $h))
    (global.set $dpr (call $clamp (local.get $dpr) (i32.const 50) (i32.const 400)))
    (global.set $base_px (call $dp (i32.const 19)))
    (global.set $mono_px (call $dp (i32.const 15)))
    ;; a reading measure: ~70 characters, centred, never tighter than the
    ;; gutters allow on a phone
    (local.set $measure (call $dp (i32.const 700)))
    (if (global.get $page_mode) (then (local.set $measure (local.get $w))))
    (global.set $col_w (call $min (local.get $measure)
      (i32.sub (local.get $w) (i32.mul (call $dp (select (i32.const 0) (i32.const 20) (global.get $page_mode))) (i32.const 2)))))
    (global.set $col_w (call $max (global.get $col_w) (call $dp (i32.const 120))))
    (global.set $col_x (i32.div_s (i32.sub (local.get $w) (global.get $col_w)) (i32.const 2)))
    (global.set $pad_top (call $dp (select (i32.const 6) (i32.const 28) (global.get $page_mode))))
    (global.set $laid_w (i32.const -1))
    (global.set $dirty (i32.const 1))
    (call $layout)
    (call $clamp_scroll)
    (global.get $FB))

  ;; ── text in, text out ────────────────────────────────────────────────────

  ;; set_text replaces the document with n bytes from IO and forgets history.
  (func (export "set_text") (param $n i32) (result i32)
    (if (i32.gt_u (local.get $n) (global.get $TEXT_CAP)) (then (return (i32.const 0))))
    (memory.copy (global.get $TEXT) (global.get $IO) (local.get $n))
    (global.set $len (local.get $n))
    (global.set $revision (i32.add (global.get $revision) (i32.const 1)))
    (global.set $sel_a (i32.const 0))
    (global.set $sel_h (i32.const 0))
    (global.set $scroll (i32.const 0))
    (call $undo_reset)
    (global.set $dirty (i32.const 1))
    (i32.const 1))

  ;; get_text copies the document into IO and returns its length.
  (func (export "get_text") (result i32)
    (memory.copy (global.get $IO) (global.get $TEXT) (call $min (global.get $len) (global.get $IO_CAP)))
    (call $min (global.get $len) (global.get $IO_CAP)))

  ;; get_selection copies the selected text into IO and returns its length.
  (func $get_selection (export "get_selection") (result i32) (local $n i32)
    (local.set $n (call $min (i32.sub (call $sel_hi) (call $sel_lo)) (global.get $IO_CAP)))
    (memory.copy (global.get $IO) (i32.add (global.get $TEXT) (call $sel_lo)) (local.get $n))
    (local.get $n))

  (func (export "sel_start") (result i32) (call $sel_lo))
  (func (export "sel_end") (result i32) (call $sel_hi))
  (func (export "set_selection") (param $a i32) (param $h i32)
    (global.set $sel_a (call $snap (local.get $a)))
    (global.set $sel_h (call $snap (local.get $h)))
    (call $moved))

  ;; $snap clamps a position into the text and back onto a character boundary.
  (func $snap (param $p i32) (result i32)
    (local.set $p (call $clamp (local.get $p) (i32.const 0) (global.get $len)))
    (block $ok
      (loop $back
        (br_if $ok (i32.le_s (local.get $p) (i32.const 0)))
        (br_if $ok (i32.ge_s (local.get $p) (global.get $len)))
        (br_if $ok (i32.ne (i32.and (call $byte (local.get $p)) (i32.const 0xC0)) (i32.const 0x80)))
        (local.set $p (i32.sub (local.get $p) (i32.const 1)))
        (br $back)))
    (local.get $p))

  ;; ── undo ─────────────────────────────────────────────────────────────────
  ;;
  ;; The log is a sequence of records, each:
  ;;   0 kind (1 insert, 2 delete)   4 pos   8 len   12 group
  ;;   16 caret before   20 anchor before   24 … len bytes of text … then the
  ;;   record's total size as a trailing u32, so the log can be walked back.
  ;; Undo walks back one group; redo walks forward one. A new edit drops
  ;; whatever redo was left.

  (global $u_pos (mut i32) (i32.const 0)) ;; end of the applied records
  (global $u_end (mut i32) (i32.const 0)) ;; end of all records (redo beyond u_pos)
  (global $group (mut i32) (i32.const 0))
  (global $typing (mut i32) (i32.const 0)) ;; 1 while a run of typing can merge
  (global $last_edit_ms (mut i32) (i32.const 0))
  (global $now_ms (mut i32) (i32.const 0))

  (func $undo_reset
    (global.set $u_pos (i32.const 0))
    (global.set $u_end (i32.const 0))
    (global.set $group (i32.add (global.get $group) (i32.const 1)))
    (global.set $typing (i32.const 0)))

  ;; $new_group starts a new undo step for the next edit.
  (func $new_group
    (global.set $group (i32.add (global.get $group) (i32.const 1)))
    (global.set $typing (i32.const 0)))

  (func $log (param $kind i32) (param $p i32) (param $n i32) (param $src i32) (local $r i32) (local $size i32)
    (local.set $size (i32.and (i32.add (i32.add (i32.const 28) (local.get $n)) (i32.const 3)) (i32.const -4)))
    (global.set $u_end (global.get $u_pos))
    (if (i32.gt_u (i32.add (global.get $u_pos) (local.get $size)) (global.get $UNDO_CAP))
      (then ;; out of room: history starts over from here
        (global.set $u_pos (i32.const 0))
        (global.set $u_end (i32.const 0))
        (if (i32.gt_u (local.get $size) (global.get $UNDO_CAP)) (then (return)))))
    (local.set $r (i32.add (global.get $UNDO) (global.get $u_pos)))
    (i32.store offset=0 (local.get $r) (local.get $kind))
    (i32.store offset=4 (local.get $r) (local.get $p))
    (i32.store offset=8 (local.get $r) (local.get $n))
    (i32.store offset=12 (local.get $r) (global.get $group))
    (i32.store offset=16 (local.get $r) (global.get $sel_h))
    (i32.store offset=20 (local.get $r) (global.get $sel_a))
    (memory.copy (i32.add (local.get $r) (i32.const 24)) (local.get $src) (local.get $n))
    (i32.store (i32.sub (i32.add (local.get $r) (local.get $size)) (i32.const 4)) (local.get $size))
    (global.set $u_pos (i32.add (global.get $u_pos) (local.get $size)))
    (global.set $u_end (global.get $u_pos)))

  ;; $insert and $delete are the only ways the document changes; both log.
  (func $insert (param $p i32) (param $src i32) (param $n i32) (result i32)
    (if (i32.eqz (local.get $n)) (then (return (i32.const 1))))
    (if (i32.gt_u (i32.add (global.get $len) (local.get $n)) (global.get $TEXT_CAP)) (then (return (i32.const 0))))
    (call $log (i32.const 1) (local.get $p) (local.get $n) (local.get $src))
    (drop (call $raw_insert (local.get $p) (local.get $src) (local.get $n)))
    (global.set $dirty (i32.const 1))
    (i32.const 1))

  (func $delete (param $p i32) (param $n i32)
    (if (i32.le_s (local.get $n) (i32.const 0)) (then (return)))
    (call $log (i32.const 2) (local.get $p) (local.get $n) (i32.add (global.get $TEXT) (local.get $p)))
    (call $raw_delete (local.get $p) (local.get $n))
    (global.set $dirty (i32.const 1)))

  (func $undo (local $r i32) (local $size i32) (local $g i32) (local $kind i32)
    (if (i32.eqz (global.get $u_pos)) (then (return)))
    (local.set $g (i32.const -1))
    (block $done
      (loop $back
        (br_if $done (i32.eqz (global.get $u_pos)))
        (local.set $size (i32.load (i32.add (global.get $UNDO) (i32.sub (global.get $u_pos) (i32.const 4)))))
        (local.set $r (i32.add (global.get $UNDO) (i32.sub (global.get $u_pos) (local.get $size))))
        (if (i32.eq (local.get $g) (i32.const -1)) (then (local.set $g (i32.load offset=12 (local.get $r)))))
        (br_if $done (i32.ne (i32.load offset=12 (local.get $r)) (local.get $g)))
        (local.set $kind (i32.load (local.get $r)))
        (if (i32.eq (local.get $kind) (i32.const 1))
          (then (call $raw_delete (i32.load offset=4 (local.get $r)) (i32.load offset=8 (local.get $r))))
          (else (drop (call $raw_insert (i32.load offset=4 (local.get $r)) (i32.add (local.get $r) (i32.const 24))
                  (i32.load offset=8 (local.get $r))))))
        (global.set $sel_h (i32.load offset=16 (local.get $r)))
        (global.set $sel_a (i32.load offset=20 (local.get $r)))
        (global.set $u_pos (i32.sub (global.get $u_pos) (local.get $size)))
        (br $back)))
    (call $new_group)
    (global.set $dirty (i32.const 1))
    (call $moved))

  (func $redo (local $r i32) (local $size i32) (local $g i32) (local $kind i32) (local $p i32) (local $n i32)
    (if (i32.ge_u (global.get $u_pos) (global.get $u_end)) (then (return)))
    (local.set $g (i32.load offset=12 (i32.add (global.get $UNDO) (global.get $u_pos))))
    (block $done
      (loop $fwd
        (br_if $done (i32.ge_u (global.get $u_pos) (global.get $u_end)))
        (local.set $r (i32.add (global.get $UNDO) (global.get $u_pos)))
        (br_if $done (i32.ne (i32.load offset=12 (local.get $r)) (local.get $g)))
        (local.set $kind (i32.load (local.get $r)))
        (local.set $p (i32.load offset=4 (local.get $r)))
        (local.set $n (i32.load offset=8 (local.get $r)))
        (if (i32.eq (local.get $kind) (i32.const 1))
          (then
            (drop (call $raw_insert (local.get $p) (i32.add (local.get $r) (i32.const 24)) (local.get $n)))
            (global.set $sel_h (i32.add (local.get $p) (local.get $n))))
          (else
            (call $raw_delete (local.get $p) (local.get $n))
            (global.set $sel_h (local.get $p))))
        (global.set $sel_a (global.get $sel_h))
        (local.set $size (i32.and (i32.add (i32.add (i32.const 28) (local.get $n)) (i32.const 3)) (i32.const -4)))
        (global.set $u_pos (i32.add (global.get $u_pos) (local.get $size)))
        (br $fwd)))
    (call $new_group)
    (global.set $dirty (i32.const 1))
    (call $moved))

  ;; ── editing primitives ───────────────────────────────────────────────────

  ;; $delete_selection removes the selection, leaving the caret where it was.
  (func $delete_selection (result i32) (local $lo i32) (local $hi i32)
    (local.set $lo (call $sel_lo))
    (local.set $hi (call $sel_hi))
    (if (i32.eq (local.get $lo) (local.get $hi)) (then (return (i32.const 0))))
    (call $delete (local.get $lo) (i32.sub (local.get $hi) (local.get $lo)))
    (global.set $sel_a (local.get $lo))
    (global.set $sel_h (local.get $lo))
    (i32.const 1))

  ;; $replace puts n bytes at src in place of the selection; the caret ends
  ;; after them.
  (func $replace (param $src i32) (param $n i32) (result i32) (local $p i32)
    (drop (call $delete_selection))
    (local.set $p (global.get $sel_h))
    (if (i32.eqz (call $insert (local.get $p) (local.get $src) (local.get $n))) (then (return (i32.const 0))))
    (global.set $sel_h (i32.add (local.get $p) (local.get $n)))
    (global.set $sel_a (global.get $sel_h))
    (call $moved)
    (i32.const 1))

  ;; SCRATCH holds short strings the editor inserts on its own behalf.
  (global $SCRATCH i32 (i32.const 0x4000))

  (func $put (param $at i32) (param $c i32) (result i32)
    (i32.store8 (i32.add (global.get $SCRATCH) (local.get $at)) (local.get $c))
    (i32.add (local.get $at) (i32.const 1)))

  ;; insert_text: typed or pasted text (n bytes in IO) replaces the selection.
  ;; Typing coalesces into one undo step per word; a paste is its own step.
  (func (export "insert_text") (param $n i32) (result i32) (local $space i32)
    (if (i32.eqz (local.get $n)) (then (return (i32.const 1))))
    (local.set $space (i32.and (i32.eq (local.get $n) (i32.const 1))
      (call $is_space (i32.load8_u (global.get $IO)))))
    (if (i32.or (i32.or (i32.ne (local.get $n) (i32.const 1)) (i32.eqz (global.get $typing)))
                (i32.or (i32.ne (call $sel_lo) (call $sel_hi))
                        (i32.gt_u (i32.sub (global.get $now_ms) (global.get $last_edit_ms)) (i32.const 1500))))
      (then (call $new_group)))
    (if (i32.eqz (call $replace (global.get $IO) (local.get $n))) (then (return (i32.const 0))))
    ;; a word ends at a space: the next word is the next undo step
    (global.set $typing (i32.and (i32.eq (local.get $n) (i32.const 1)) (i32.eqz (local.get $space))))
    (global.set $last_edit_ms (global.get $now_ms))
    (i32.const 1))

  ;; ── motion ───────────────────────────────────────────────────────────────

  ;; $moved: the caret moved — keep it in view, show it, forget typing runs.
  (func $moved
    (global.set $caret_on (i32.const 1))
    (global.set $dirty (i32.const 1))
    (call $layout)
    (call $reveal))

  (func $set_caret (param $p i32) (param $extend i32)
    (global.set $sel_h (local.get $p))
    (if (i32.eqz (local.get $extend)) (then (global.set $sel_a (local.get $p))))
    (global.set $typing (i32.const 0))
    (call $moved))

  ;; $vertical moves the caret by visual lines, holding its x.
  (func $vertical (param $dir i32) (param $extend i32) (local $l i32)
    (call $layout)
    (if (i32.lt_s (global.get $goal_x) (i32.const 0)) (then (global.set $goal_x (call $x_of (global.get $sel_h)))))
    (local.set $l (i32.add (call $line_of (global.get $sel_h)) (local.get $dir)))
    (if (i32.lt_s (local.get $l) (i32.const 0))
      (then (call $set_caret (i32.const 0) (local.get $extend)) (return)))
    (if (i32.ge_s (local.get $l) (global.get $nlines))
      (then (call $set_caret (global.get $len) (local.get $extend)) (return)))
    (call $set_caret (call $pos_in_line (local.get $l) (global.get $goal_x)) (local.get $extend)))

  (func $page (param $dir i32) (param $extend i32) (local $y i32) (local $l i32)
    (call $layout)
    (if (i32.lt_s (global.get $goal_x) (i32.const 0)) (then (global.set $goal_x (call $x_of (global.get $sel_h)))))
    (local.set $l (call $line_of (global.get $sel_h)))
    (local.set $y (i32.add (i32.load offset=8 (call $rec (local.get $l)))
      (i32.mul (local.get $dir) (i32.sub (global.get $H) (call $dp (i32.const 40))))))
    (global.set $scroll (i32.add (global.get $scroll) (i32.mul (local.get $dir) (i32.sub (global.get $H) (call $dp (i32.const 40))))))
    (call $clamp_scroll)
    (call $set_caret (call $pos_in_line (call $line_at_y (local.get $y)) (global.get $goal_x)) (local.get $extend)))

  ;; visual line boundaries, for Home / End
  (func $vline_start (param $p i32) (result i32) (i32.load (call $rec (call $line_of (local.get $p)))))
  (func $vline_end (param $p i32) (result i32) (local $r i32)
    (local.set $r (call $rec (call $line_of (local.get $p))))
    (if (i32.and (i32.shr_u (i32.load offset=24 (local.get $r)) (i32.const 16)) (i32.const 2))
      (then (return (i32.load offset=4 (local.get $r)))))
    (call $max (i32.load (local.get $r)) (call $prev (i32.load offset=4 (local.get $r)))))

  ;; ── keys ─────────────────────────────────────────────────────────────────

  ;; key handles a named key. Returns 1 when the editor consumed it.
  (func (export "key") (param $k i32) (param $mods i32) (result i32)
    (local $shift i32) (local $word i32) (local $cmd i32) (local $p i32) (local $vertical i32)
    (call $layout)
    (local.set $shift (i32.and (local.get $mods) (i32.const 1)))
    (local.set $word (i32.and (local.get $mods) (i32.const 2)))
    (local.set $cmd (i32.and (local.get $mods) (i32.const 4)))
    (local.set $vertical (i32.or (i32.or (i32.eq (local.get $k) (i32.const 3)) (i32.eq (local.get $k) (i32.const 4)))
                                 (i32.or (i32.eq (local.get $k) (i32.const 7)) (i32.eq (local.get $k) (i32.const 8)))))
    (if (i32.eqz (local.get $vertical)) (then (global.set $goal_x (i32.const -1))))
    (block $unhandled
      (block $done
        ;; ← →
        (if (i32.eq (local.get $k) (i32.const 1))
          (then
            (if (i32.and (i32.ne (call $sel_lo) (call $sel_hi)) (i32.eqz (local.get $shift)))
              (then (call $set_caret (call $sel_lo) (i32.const 0)) (br $done)))
            (local.set $p (if (result i32) (local.get $cmd) (then (call $vline_start (global.get $sel_h)))
              (else (if (result i32) (local.get $word) (then (call $word_left (global.get $sel_h)))
                (else (call $prev (global.get $sel_h)))))))
            (call $set_caret (local.get $p) (local.get $shift)) (br $done)))
        (if (i32.eq (local.get $k) (i32.const 2))
          (then
            (if (i32.and (i32.ne (call $sel_lo) (call $sel_hi)) (i32.eqz (local.get $shift)))
              (then (call $set_caret (call $sel_hi) (i32.const 0)) (br $done)))
            (local.set $p (if (result i32) (local.get $cmd) (then (call $vline_end (global.get $sel_h)))
              (else (if (result i32) (local.get $word) (then (call $word_right (global.get $sel_h)))
                (else (call $next (global.get $sel_h)))))))
            (call $set_caret (local.get $p) (local.get $shift)) (br $done)))
        ;; ↑ ↓ (⌘ goes to the ends of the document)
        (if (i32.eq (local.get $k) (i32.const 3))
          (then
            (if (local.get $cmd) (then (call $set_caret (i32.const 0) (local.get $shift)) (br $done)))
            (if (local.get $word) (then (call $set_caret (call $line_start (call $prev (call $line_start (global.get $sel_h)))) (local.get $shift)) (br $done)))
            (call $vertical (i32.const -1) (local.get $shift)) (br $done)))
        (if (i32.eq (local.get $k) (i32.const 4))
          (then
            (if (local.get $cmd) (then (call $set_caret (global.get $len) (local.get $shift)) (br $done)))
            (if (local.get $word) (then (call $set_caret (call $line_end (call $next (call $line_end (global.get $sel_h)))) (local.get $shift)) (br $done)))
            (call $vertical (i32.const 1) (local.get $shift)) (br $done)))
        (if (i32.eq (local.get $k) (i32.const 5))
          (then (call $set_caret (if (result i32) (local.get $cmd) (then (i32.const 0)) (else (call $vline_start (global.get $sel_h))))
            (local.get $shift)) (br $done)))
        (if (i32.eq (local.get $k) (i32.const 6))
          (then (call $set_caret (if (result i32) (local.get $cmd) (then (global.get $len)) (else (call $vline_end (global.get $sel_h))))
            (local.get $shift)) (br $done)))
        (if (i32.eq (local.get $k) (i32.const 7)) (then (call $page (i32.const -1) (local.get $shift)) (br $done)))
        (if (i32.eq (local.get $k) (i32.const 8)) (then (call $page (i32.const 1) (local.get $shift)) (br $done)))
        ;; ⌫ ⌦
        (if (i32.eq (local.get $k) (i32.const 9))
          (then
            (call $new_group)
            (if (call $delete_selection) (then (call $moved) (br $done)))
            (local.set $p (if (result i32) (local.get $cmd) (then (call $vline_start (global.get $sel_h)))
              (else (if (result i32) (local.get $word) (then (call $word_left (global.get $sel_h)))
                (else (call $backspace_target (global.get $sel_h)))))))
            (call $delete (local.get $p) (i32.sub (global.get $sel_h) (local.get $p)))
            (call $set_caret (local.get $p) (i32.const 0)) (br $done)))
        (if (i32.eq (local.get $k) (i32.const 10))
          (then
            (call $new_group)
            (if (call $delete_selection) (then (call $moved) (br $done)))
            (local.set $p (if (result i32) (local.get $word) (then (call $word_right (global.get $sel_h)))
              (else (call $next (global.get $sel_h)))))
            (call $delete (global.get $sel_h) (i32.sub (local.get $p) (global.get $sel_h)))
            (call $moved) (br $done)))
        (if (i32.eq (local.get $k) (i32.const 11)) (then (call $enter) (br $done)))
        (if (i32.eq (local.get $k) (i32.const 12)) (then (call $tab (local.get $shift)) (br $done)))
        (if (i32.eq (local.get $k) (i32.const 13))
          (then (if (i32.ne (call $sel_lo) (call $sel_hi))
            (then (call $set_caret (global.get $sel_h) (i32.const 0)) (br $done))) (br $unhandled)))
        (br $unhandled))
      (return (i32.const 1)))
    (i32.const 0))

  ;; $backspace_target: one character back — or, at the end of a list or
  ;; quote marker, the whole marker, so ⌫ on "- " ends the list in one stroke.
  (func $backspace_target (param $p i32) (result i32) (local $ls i32) (local $info i32)
    (local.set $ls (call $line_start (local.get $p)))
    (global.set $in_fence (i32.const 0))
    (local.set $info (call $classify (local.get $ls) (call $line_end (local.get $ls))))
    (if (i32.and (i32.eq (local.get $p) (i32.add (local.get $ls) (global.get $prefix_len)))
                 (i32.or (i32.or (i32.eq (i32.and (local.get $info) (i32.const 0xFF)) (global.get $K_BULLET))
                                 (i32.eq (i32.and (local.get $info) (i32.const 0xFF)) (global.get $K_ORDER)))
                         (i32.eq (i32.and (local.get $info) (i32.const 0xFF)) (global.get $K_QUOTE))))
      (then (return (i32.add (local.get $ls) (global.get $indent_len)))))
    (call $prev (local.get $p)))

  ;; $enter breaks the line and carries the block along: a list item makes
  ;; the next item (numbered lists count up), a quote stays quoted, code
  ;; keeps its indentation. Enter on an empty item ends the list instead.
  (func $enter (local $ls i32) (local $le i32) (local $info i32) (local $kind i32) (local $n i32)
    (local $i i32) (local $num i32) (local $q i32) (local $c i32)
    (call $new_group)
    (drop (call $delete_selection))
    (local.set $ls (call $line_start (global.get $sel_h)))
    (local.set $le (call $line_end (global.get $sel_h)))
    (call $layout) ;; fence state for this line comes from the layout pass
    (local.set $info (call $line_info (call $line_of (local.get $ls))))
    (global.set $in_fence (i32.eq (i32.and (local.get $info) (i32.const 0xFF)) (global.get $K_CODE)))
    (local.set $info (call $classify (local.get $ls) (local.get $le)))
    (local.set $kind (i32.and (local.get $info) (i32.const 0xFF)))
    ;; an item with nothing after its marker: remove the marker, end the list
    (if (i32.and (i32.or (i32.or (i32.eq (local.get $kind) (global.get $K_BULLET)) (i32.eq (local.get $kind) (global.get $K_ORDER)))
                         (i32.eq (local.get $kind) (global.get $K_QUOTE)))
                 (i32.eq (local.get $le) (i32.add (local.get $ls) (global.get $prefix_len))))
      (then
        (call $delete (local.get $ls) (global.get $prefix_len))
        (call $set_caret (local.get $ls) (i32.const 0))
        (return)))
    (local.set $n (call $put (i32.const 0) (i32.const 0x0A)))
    (if (i32.or (i32.eq (local.get $kind) (global.get $K_BULLET)) (i32.eq (local.get $kind) (global.get $K_QUOTE)))
      (then
        ;; copy the marker (and its indentation) as typed
        (local.set $i (local.get $ls))
        (block $copied
          (loop $copy
            (br_if $copied (i32.ge_s (local.get $i) (i32.add (local.get $ls) (global.get $prefix_len))))
            (local.set $n (call $put (local.get $n) (call $byte (local.get $i))))
            (local.set $i (i32.add (local.get $i) (i32.const 1)))
            (br $copy)))))
    (if (i32.eq (local.get $kind) (global.get $K_ORDER))
      (then
        ;; indentation, then the number plus one, then the same delimiter
        (local.set $i (local.get $ls))
        (block $ind (loop $sp
          (br_if $ind (i32.ne (call $byte (local.get $i)) (i32.const 0x20)))
          (local.set $n (call $put (local.get $n) (i32.const 0x20)))
          (local.set $i (i32.add (local.get $i) (i32.const 1)))
          (br $sp)))
        (block $digits (loop $d
          (local.set $c (call $byte (local.get $i)))
          (br_if $digits (i32.or (i32.lt_u (local.get $c) (i32.const 0x30)) (i32.gt_u (local.get $c) (i32.const 0x39))))
          (local.set $num (i32.add (i32.mul (local.get $num) (i32.const 10)) (i32.sub (local.get $c) (i32.const 0x30))))
          (local.set $i (i32.add (local.get $i) (i32.const 1)))
          (br $d)))
        (local.set $n (call $put_int (local.get $n) (i32.add (local.get $num) (i32.const 1))))
        (local.set $n (call $put (local.get $n) (call $byte (local.get $i))))
        (local.set $n (call $put (local.get $n) (i32.const 0x20)))))
    (if (i32.eq (local.get $kind) (global.get $K_CODE))
      (then ;; keep the line's indentation
        (local.set $i (local.get $ls))
        (block $ind2 (loop $sp2
          (br_if $ind2 (i32.ge_s (local.get $i) (local.get $le)))
          (local.set $c (call $byte (local.get $i)))
          (br_if $ind2 (i32.and (i32.ne (local.get $c) (i32.const 0x20)) (i32.ne (local.get $c) (i32.const 0x09))))
          (local.set $n (call $put (local.get $n) (local.get $c)))
          (local.set $i (i32.add (local.get $i) (i32.const 1)))
          (br $sp2)))))
    (drop (call $replace (global.get $SCRATCH) (local.get $n)))
    (call $new_group))

  (func $put_int (param $at i32) (param $v i32) (result i32) (local $d i32) (local $div i32)
    (local.set $div (i32.const 1000000))
    (local.set $d (i32.const 0))
    (block $done
      (loop $digits
        (if (i32.or (i32.ge_s (local.get $v) (local.get $div)) (i32.or (local.get $d) (i32.eq (local.get $div) (i32.const 1))))
          (then
            (local.set $at (call $put (local.get $at) (i32.add (i32.const 0x30) (i32.rem_u (i32.div_u (local.get $v) (local.get $div)) (i32.const 10)))))
            (local.set $d (i32.const 1))))
        (br_if $done (i32.eq (local.get $div) (i32.const 1)))
        (local.set $div (i32.div_u (local.get $div) (i32.const 10)))
        (br $digits)))
    (local.get $at))

  ;; $tab indents (or with shift outdents) every line the selection touches
  ;; by two spaces — nesting a list item, or shifting a block of code.
  (func $tab (param $out i32) (local $p i32) (local $end i32) (local $a i32) (local $h i32) (local $delta i32) (local $first i32)
    (call $new_group)
    (local.set $p (call $line_start (call $sel_lo)))
    (local.set $end (call $sel_hi))
    (local.set $a (global.get $sel_a))
    (local.set $h (global.get $sel_h))
    (local.set $first (i32.const 1))
    (block $done
      (loop $lines
        (if (local.get $out)
          (then
            (local.set $delta (call $min (call $run (local.get $p) (global.get $len) (i32.const 0x20)) (i32.const 2)))
            (call $delete (local.get $p) (local.get $delta))
            (local.set $delta (i32.sub (i32.const 0) (local.get $delta))))
          (else
            (drop (call $put (call $put (i32.const 0) (i32.const 0x20)) (i32.const 0x20)))
            (drop (call $insert (local.get $p) (global.get $SCRATCH) (i32.const 2)))
            (local.set $delta (i32.const 2))))
        ;; carry the selection ends along with the text they sit in
        (if (i32.ge_s (local.get $a) (local.get $p)) (then (local.set $a (call $max (local.get $p) (i32.add (local.get $a) (local.get $delta))))))
        (if (i32.ge_s (local.get $h) (local.get $p)) (then (local.set $h (call $max (local.get $p) (i32.add (local.get $h) (local.get $delta))))))
        (local.set $end (i32.add (local.get $end) (local.get $delta)))
        (local.set $p (call $line_end (local.get $p)))
        (br_if $done (i32.ge_s (local.get $p) (local.get $end)))
        (br_if $done (i32.ge_u (local.get $p) (global.get $len)))
        (local.set $p (i32.add (local.get $p) (i32.const 1)))
        (br $lines)))
    (global.set $sel_a (local.get $a))
    (global.set $sel_h (local.get $h))
    (call $new_group)
    (call $moved))

  ;; ── commands ─────────────────────────────────────────────────────────────

  (func (export "command") (param $c i32) (result i32)
    (call $layout)
    (global.set $goal_x (i32.const -1))
    (if (i32.eq (local.get $c) (i32.const 1)) (then (call $wrap (i32.const 0x2A) (i32.const 2)) (return (i32.const 1))))
    (if (i32.eq (local.get $c) (i32.const 2)) (then (call $wrap (i32.const 0x2A) (i32.const 1)) (return (i32.const 1))))
    (if (i32.eq (local.get $c) (i32.const 3)) (then (call $wrap (i32.const 0x60) (i32.const 1)) (return (i32.const 1))))
    (if (i32.eq (local.get $c) (i32.const 4)) (then (call $link) (return (i32.const 1))))
    (if (i32.eq (local.get $c) (i32.const 5)) (then (call $wrap (i32.const 0x7E) (i32.const 2)) (return (i32.const 1))))
    (if (i32.and (i32.ge_u (local.get $c) (i32.const 6)) (i32.le_u (local.get $c) (i32.const 8)))
      (then (call $heading (i32.sub (local.get $c) (i32.const 5))) (return (i32.const 1))))
    (if (i32.eq (local.get $c) (i32.const 9)) (then (call $prefix_lines (i32.const 1)) (return (i32.const 1))))
    (if (i32.eq (local.get $c) (i32.const 10)) (then (call $prefix_lines (i32.const 2)) (return (i32.const 1))))
    (if (i32.eq (local.get $c) (i32.const 11)) (then (call $prefix_lines (i32.const 3)) (return (i32.const 1))))
    (if (i32.eq (local.get $c) (i32.const 12)) (then (call $code_block) (return (i32.const 1))))
    (if (i32.eq (local.get $c) (i32.const 13)) (then (call $undo) (return (i32.const 1))))
    (if (i32.eq (local.get $c) (i32.const 14)) (then (call $redo) (return (i32.const 1))))
    (if (i32.eq (local.get $c) (i32.const 15))
      (then (global.set $sel_a (i32.const 0)) (global.set $sel_h (global.get $len)) (call $moved) (return (i32.const 1))))
    (if (i32.eq (local.get $c) (i32.const 16)) (then (call $rule) (return (i32.const 1))))
    (if (i32.eq (local.get $c) (i32.const 17)) (then (call $select_word (global.get $sel_h)) (return (i32.const 1))))
    (if (i32.eq (local.get $c) (i32.const 18)) (then (call $select_line (global.get $sel_h)) (return (i32.const 1))))
    (i32.const 0))

  ;; $is_wrapped: is [lo, hi) enclosed by n copies of c on each side?
  (func $is_wrapped (param $lo i32) (param $hi i32) (param $c i32) (param $n i32) (result i32) (local $i i32)
    (if (i32.or (i32.lt_s (i32.sub (local.get $lo) (local.get $n)) (i32.const 0))
                (i32.gt_s (i32.add (local.get $hi) (local.get $n)) (global.get $len)))
      (then (return (i32.const 0))))
    (block $no
      (loop $check
        (br_if $no (i32.ge_s (local.get $i) (local.get $n)))
        (if (i32.or (i32.ne (call $byte (i32.sub (i32.sub (local.get $lo) (i32.const 1)) (local.get $i))) (local.get $c))
                    (i32.ne (call $byte (i32.add (local.get $hi) (local.get $i))) (local.get $c)))
          (then (return (i32.const 0))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $check)))
    ;; for single * (italic), a third * outside means it is really bold
    (if (i32.eq (local.get $n) (i32.const 1))
      (then
        (if (i32.and (i32.ge_s (i32.sub (local.get $lo) (i32.const 2)) (i32.const 0))
                     (i32.lt_s (i32.add (local.get $hi) (i32.const 1)) (global.get $len)))
          (then (if (i32.and (i32.eq (call $byte (i32.sub (local.get $lo) (i32.const 2))) (local.get $c))
                             (i32.eq (call $byte (i32.add (local.get $hi) (i32.const 1))) (local.get $c)))
            (then (return (i32.const 0))))))))
    (i32.const 1))

  ;; $wrap toggles an inline marker (** for bold, * italic, ` code, ~~ strike)
  ;; around the selection — or, with no selection, inserts a pair with the
  ;; caret between.
  (func $wrap (param $c i32) (param $n i32) (local $lo i32) (local $hi i32) (local $i i32)
    (call $new_group)
    (local.set $lo (call $sel_lo))
    (local.set $hi (call $sel_hi))
    (if (call $is_wrapped (local.get $lo) (local.get $hi) (local.get $c) (local.get $n))
      (then
        (call $delete (local.get $hi) (local.get $n))
        (call $delete (i32.sub (local.get $lo) (local.get $n)) (local.get $n))
        (global.set $sel_a (i32.sub (local.get $lo) (local.get $n)))
        (global.set $sel_h (i32.sub (local.get $hi) (local.get $n)))
        (call $new_group) (call $moved) (return)))
    (block $filled (loop $f
      (br_if $filled (i32.ge_s (local.get $i) (local.get $n)))
      (drop (call $put (local.get $i) (local.get $c)))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $f)))
    (drop (call $insert (local.get $hi) (global.get $SCRATCH) (local.get $n)))
    (drop (call $insert (local.get $lo) (global.get $SCRATCH) (local.get $n)))
    (global.set $sel_a (i32.add (local.get $lo) (local.get $n)))
    (global.set $sel_h (i32.add (local.get $hi) (local.get $n)))
    (call $new_group)
    (call $moved))

  ;; $link turns the selection into [selection](…) with the caret waiting for
  ;; the destination. A selected URL becomes the destination instead.
  (func $link (local $lo i32) (local $hi i32) (local $url i32) (local $n i32)
    (call $new_group)
    (local.set $lo (call $sel_lo))
    (local.set $hi (call $sel_hi))
    (local.set $url (i32.and (i32.lt_s (local.get $lo) (local.get $hi))
      (i32.or (call $starts (local.get $lo) (local.get $hi) (i32.const 0x3A707474) (i32.const 0x68))
              (call $starts (local.get $lo) (local.get $hi) (i32.const 0x3A626F6C) (i32.const 0x62)))))
    (if (local.get $url)
      (then
        ;; [](url) with the caret inside the brackets
        (drop (call $put (i32.const 0) (i32.const 0x29)))
        (drop (call $insert (local.get $hi) (global.get $SCRATCH) (i32.const 1)))
        (drop (call $put (call $put (call $put (i32.const 0) (i32.const 0x5B)) (i32.const 0x5D)) (i32.const 0x28)))
        (drop (call $insert (local.get $lo) (global.get $SCRATCH) (i32.const 3)))
        (global.set $sel_a (i32.add (local.get $lo) (i32.const 1)))
        (global.set $sel_h (global.get $sel_a))
        (call $new_group) (call $moved) (return)))
    (local.set $n (call $put (call $put (i32.const 0) (i32.const 0x5D)) (i32.const 0x28)))
    (local.set $n (call $put (local.get $n) (i32.const 0x29)))
    (drop (call $insert (local.get $hi) (global.get $SCRATCH) (local.get $n)))
    (drop (call $put (i32.const 0) (i32.const 0x5B)))
    (drop (call $insert (local.get $lo) (global.get $SCRATCH) (i32.const 1)))
    ;; caret between the parentheses
    (global.set $sel_a (i32.add (local.get $hi) (i32.const 3)))
    (global.set $sel_h (global.get $sel_a))
    (call $new_group)
    (call $moved))

  ;; $each_line calls back for each line start the selection touches; the
  ;; line operations below all walk the same way.
  (global $op_a (mut i32) (i32.const 0))
  (global $op_h (mut i32) (i32.const 0))

  (func $shift_sel (param $at i32) (param $delta i32)
    (if (i32.ge_s (global.get $op_a) (local.get $at))
      (then (global.set $op_a (call $max (local.get $at) (i32.add (global.get $op_a) (local.get $delta))))))
    (if (i32.ge_s (global.get $op_h) (local.get $at))
      (then (global.set $op_h (call $max (local.get $at) (i32.add (global.get $op_h) (local.get $delta)))))))

  ;; $heading sets every touched line to heading level n — or clears it when
  ;; the line already is one.
  (func $heading (param $n i32) (local $p i32) (local $end i32) (local $info i32) (local $old i32) (local $cnt i32) (local $i i32)
    (call $new_group)
    (global.set $op_a (global.get $sel_a))
    (global.set $op_h (global.get $sel_h))
    (local.set $p (call $line_start (call $sel_lo)))
    (local.set $end (call $sel_hi))
    (block $done
      (loop $lines
        (global.set $in_fence (i32.const 0))
        (local.set $info (call $classify (local.get $p) (call $line_end (local.get $p))))
        (local.set $old (i32.const 0))
        (if (i32.eq (i32.and (local.get $info) (i32.const 0xFF)) (global.get $K_HEAD))
          (then (local.set $old (global.get $prefix_len))))
        (local.set $cnt (i32.const 0))
        (if (i32.ne (i32.shr_u (local.get $info) (i32.const 8)) (local.get $n))
          (then
            (local.set $i (i32.const 0))
            (block $h (loop $hash
              (br_if $h (i32.ge_s (local.get $i) (local.get $n)))
              (local.set $cnt (call $put (local.get $cnt) (i32.const 0x23)))
              (local.set $i (i32.add (local.get $i) (i32.const 1)))
              (br $hash)))
            (local.set $cnt (call $put (local.get $cnt) (i32.const 0x20)))))
        (if (local.get $old)
          (then (call $delete (local.get $p) (local.get $old)) (call $shift_sel (local.get $p) (i32.sub (i32.const 0) (local.get $old)))
                (local.set $end (i32.sub (local.get $end) (local.get $old)))))
        (if (local.get $cnt)
          (then (drop (call $insert (local.get $p) (global.get $SCRATCH) (local.get $cnt)))
                (call $shift_sel (local.get $p) (local.get $cnt))
                (local.set $end (i32.add (local.get $end) (local.get $cnt)))))
        (local.set $p (call $line_end (local.get $p)))
        (br_if $done (i32.ge_s (local.get $p) (local.get $end)))
        (br_if $done (i32.ge_u (local.get $p) (global.get $len)))
        (local.set $p (i32.add (local.get $p) (i32.const 1)))
        (br $lines)))
    (global.set $sel_a (global.get $op_a))
    (global.set $sel_h (global.get $op_h))
    (call $new_group)
    (call $moved))

  ;; $prefix_lines toggles a block marker on every touched line:
  ;; 1 quote "> ", 2 bullet "- ", 3 numbered "1. ", "2. ", …
  ;; If every line already has it, it comes off; otherwise it goes on.
  (func $prefix_lines (param $which i32)
    (local $p i32) (local $end i32) (local $info i32) (local $all i32) (local $want i32) (local $kind i32)
    (local $cnt i32) (local $num i32) (local $pass i32) (local $plen i32)
    (call $new_group)
    (global.set $op_a (global.get $sel_a))
    (global.set $op_h (global.get $sel_h))
    (local.set $want (if (result i32) (i32.eq (local.get $which) (i32.const 1)) (then (global.get $K_QUOTE))
      (else (if (result i32) (i32.eq (local.get $which) (i32.const 2)) (then (global.get $K_BULLET)) (else (global.get $K_ORDER))))))
    ;; pass 0 decides, pass 1 edits
    (local.set $all (i32.const 1))
    (block $passes
      (loop $pass_loop
        (local.set $p (call $line_start (call $sel_lo)))
        (local.set $end (call $sel_hi))
        (local.set $num (i32.const 1))
        (block $done
          (loop $lines
            (global.set $in_fence (i32.const 0))
            (local.set $info (call $classify (local.get $p) (call $line_end (local.get $p))))
            (local.set $kind (i32.and (local.get $info) (i32.const 0xFF)))
            (local.set $plen (global.get $prefix_len))
            (if (i32.eqz (local.get $pass))
              (then (if (i32.ne (local.get $kind) (local.get $want)) (then (local.set $all (i32.const 0)))))
              (else
                ;; take off any existing list/quote marker
                (if (i32.or (i32.or (i32.eq (local.get $kind) (global.get $K_QUOTE)) (i32.eq (local.get $kind) (global.get $K_BULLET)))
                            (i32.eq (local.get $kind) (global.get $K_ORDER)))
                  (then
                    (call $delete (local.get $p) (local.get $plen))
                    (call $shift_sel (local.get $p) (i32.sub (i32.const 0) (local.get $plen)))
                    (local.set $end (i32.sub (local.get $end) (local.get $plen)))))
                (if (i32.eqz (local.get $all))
                  (then
                    (local.set $cnt (i32.const 0))
                    (if (i32.eq (local.get $which) (i32.const 1))
                      (then (local.set $cnt (call $put (call $put (i32.const 0) (i32.const 0x3E)) (i32.const 0x20)))))
                    (if (i32.eq (local.get $which) (i32.const 2))
                      (then (local.set $cnt (call $put (call $put (i32.const 0) (i32.const 0x2D)) (i32.const 0x20)))))
                    (if (i32.eq (local.get $which) (i32.const 3))
                      (then (local.set $cnt (call $put (call $put (call $put_int (i32.const 0) (local.get $num)) (i32.const 0x2E)) (i32.const 0x20)))
                            (local.set $num (i32.add (local.get $num) (i32.const 1)))))
                    (drop (call $insert (local.get $p) (global.get $SCRATCH) (local.get $cnt)))
                    (call $shift_sel (local.get $p) (local.get $cnt))
                    (local.set $end (i32.add (local.get $end) (local.get $cnt)))))))
            (local.set $p (call $line_end (local.get $p)))
            (br_if $done (i32.ge_s (local.get $p) (local.get $end)))
            (br_if $done (i32.ge_u (local.get $p) (global.get $len)))
            (local.set $p (i32.add (local.get $p) (i32.const 1)))
            (br $lines)))
        (br_if $passes (local.get $pass))
        (local.set $pass (i32.const 1))
        (br $pass_loop)))
    (global.set $sel_a (global.get $op_a))
    (global.set $sel_h (global.get $op_h))
    (call $new_group)
    (call $moved))

  ;; $code_block fences the touched lines with ``` above and below.
  (func $code_block (local $lo i32) (local $hi i32) (local $n i32)
    (call $new_group)
    (local.set $lo (call $line_start (call $sel_lo)))
    (local.set $hi (call $line_end (call $sel_hi)))
    (local.set $n (call $put (call $put (call $put (call $put (i32.const 0) (i32.const 0x0A)) (i32.const 0x60)) (i32.const 0x60)) (i32.const 0x60)))
    (drop (call $insert (local.get $hi) (global.get $SCRATCH) (local.get $n)))
    (local.set $n (call $put (call $put (call $put (call $put (i32.const 0) (i32.const 0x60)) (i32.const 0x60)) (i32.const 0x60)) (i32.const 0x0A)))
    (drop (call $insert (local.get $lo) (global.get $SCRATCH) (local.get $n)))
    (global.set $sel_a (i32.add (global.get $sel_a) (i32.const 4)))
    (global.set $sel_h (i32.add (global.get $sel_h) (i32.const 4)))
    (if (i32.eq (call $sel_lo) (call $sel_hi))
      (then ;; empty: caret inside the fence
        (global.set $sel_h (i32.add (local.get $lo) (i32.const 4)))
        (global.set $sel_a (global.get $sel_h))))
    (call $new_group)
    (call $moved))

  (func $rule (local $n i32) (local $p i32)
    (call $new_group)
    (drop (call $delete_selection))
    (local.set $p (call $line_end (global.get $sel_h)))
    (local.set $n (call $put (call $put (call $put (call $put (call $put (i32.const 0) (i32.const 0x0A)) (i32.const 0x0A))
      (i32.const 0x2D)) (i32.const 0x2D)) (i32.const 0x2D)))
    (local.set $n (call $put (call $put (local.get $n) (i32.const 0x0A)) (i32.const 0x0A)))
    (drop (call $insert (local.get $p) (global.get $SCRATCH) (local.get $n)))
    (call $set_caret (i32.add (local.get $p) (local.get $n)) (i32.const 0))
    (call $new_group))

  ;; ── selection by unit ────────────────────────────────────────────────────

  (func $select_word (param $p i32) (local $a i32) (local $b i32)
    (local.set $a (local.get $p))
    (local.set $b (local.get $p))
    (if (i32.and (i32.lt_s (local.get $p) (global.get $len)) (call $is_word (call $cp_at (local.get $p))))
      (then
        (local.set $b (call $word_right (local.get $p)))
        (if (i32.and (i32.gt_s (local.get $p) (i32.const 0)) (call $is_word (call $cp_at (call $prev (local.get $p)))))
          (then (local.set $a (call $word_left (local.get $p))))))
      (else (local.set $b (call $next (local.get $p)))))
    (global.set $sel_a (local.get $a))
    (global.set $sel_h (local.get $b))
    (call $moved))

  (func $select_line (param $p i32)
    (global.set $sel_a (call $line_start (local.get $p)))
    (global.set $sel_h (call $min (i32.add (call $line_end (local.get $p)) (i32.const 1)) (global.get $len)))
    (call $moved))

  ;; ── pointer ──────────────────────────────────────────────────────────────

  (global $drag (mut i32) (i32.const 0))      ;; 0 none, 1 chars, 2 words, 3 lines
  (global $drag_origin (mut i32) (i32.const 0))

  ;; $pos_at: the text position under a surface point.
  (func $pos_at (param $x i32) (param $y i32) (result i32)
    (call $layout)
    (if (i32.eqz (global.get $nlines)) (then (return (i32.const 0))))
    (call $pos_in_line (call $line_at_y (i32.add (local.get $y) (global.get $scroll))) (local.get $x)))

  ;; pointer: kind 1 down, 2 move (while held), 3 up. clicks is the click
  ;; count the host saw (2 selects words, 3 lines). mods as for key.
  (func (export "pointer") (param $kind i32) (param $x i32) (param $y i32) (param $mods i32) (param $clicks i32)
    (local $p i32) (local $a i32) (local $b i32)
    (global.set $goal_x (i32.const -1))
    (local.set $p (call $pos_at (local.get $x) (local.get $y)))
    (if (i32.eq (local.get $kind) (i32.const 1))
      (then
        (call $new_group)
        (if (i32.ge_s (local.get $clicks) (i32.const 3))
          (then (global.set $drag (i32.const 3)) (global.set $drag_origin (local.get $p)) (call $select_line (local.get $p)) (return)))
        (if (i32.eq (local.get $clicks) (i32.const 2))
          (then (global.set $drag (i32.const 2)) (global.set $drag_origin (local.get $p)) (call $select_word (local.get $p)) (return)))
        (global.set $drag (i32.const 1))
        (call $set_caret (local.get $p) (i32.and (local.get $mods) (i32.const 1)))
        (global.set $drag_origin (global.get $sel_a))
        (return)))
    (if (i32.eq (local.get $kind) (i32.const 2))
      (then
        (if (i32.eqz (global.get $drag)) (then (return)))
        ;; dragging past the edge scrolls
        (if (i32.lt_s (local.get $y) (i32.const 0))
          (then (global.set $scroll (i32.add (global.get $scroll) (i32.div_s (local.get $y) (i32.const 2))))))
        (if (i32.gt_s (local.get $y) (global.get $H))
          (then (global.set $scroll (i32.add (global.get $scroll) (i32.div_s (i32.sub (local.get $y) (global.get $H)) (i32.const 2))))))
        (call $clamp_scroll)
        (local.set $p (call $pos_at (local.get $x) (local.get $y)))
        (if (i32.eq (global.get $drag) (i32.const 1))
          (then (global.set $sel_a (global.get $drag_origin)) (global.set $sel_h (local.get $p))))
        (if (i32.ge_s (global.get $drag) (i32.const 2))
          (then
            ;; extend by whole words / lines, keeping the original unit selected
            (if (i32.eq (global.get $drag) (i32.const 2))
              (then (call $select_word (global.get $drag_origin)))
              (else (call $select_line (global.get $drag_origin))))
            (local.set $a (global.get $sel_a))
            (local.set $b (global.get $sel_h))
            (if (i32.eq (global.get $drag) (i32.const 2))
              (then (call $select_word (local.get $p)))
              (else (call $select_line (local.get $p))))
            (if (i32.lt_s (local.get $p) (global.get $drag_origin))
              (then (global.set $sel_a (local.get $b)))
              (else (global.set $sel_a (local.get $a))))))
        (global.set $caret_on (i32.const 1))
        (global.set $dirty (i32.const 1))
        (return)))
    (global.set $drag (i32.const 0)))

  ;; link_at: if the point is on a link, copy its destination into IO and
  ;; return the length (the host opens it on ⌘-click); else 0.
  (func (export "link_at") (param $x i32) (param $y i32) (result i32)
    (local $p i32) (local $s i32) (local $e i32) (local $ls i32) (local $le i32) (local $q i32) (local $n i32)
    (local.set $p (call $pos_at (local.get $x) (local.get $y)))
    (call $layout)
    (if (i32.ge_s (local.get $p) (global.get $len)) (then (return (i32.const 0))))
    (if (i32.eqz (i32.and (call $style_of (local.get $p)) (global.get $S_LINK))) (then (return (i32.const 0))))
    (local.set $ls (call $line_start (local.get $p)))
    (local.set $le (call $line_end (local.get $p)))
    ;; the extent of the link run
    (local.set $s (local.get $p))
    (block $l (loop $back
      (br_if $l (i32.le_s (local.get $s) (local.get $ls)))
      (br_if $l (i32.eqz (i32.and (call $style_of (i32.sub (local.get $s) (i32.const 1))) (global.get $S_LINK))))
      (local.set $s (i32.sub (local.get $s) (i32.const 1)))
      (br $back)))
    (local.set $e (local.get $p))
    (block $r (loop $fwd
      (br_if $r (i32.ge_s (local.get $e) (local.get $le)))
      (br_if $r (i32.eqz (i32.and (call $style_of (local.get $e)) (global.get $S_LINK))))
      (local.set $e (i32.add (local.get $e) (i32.const 1)))
      (br $fwd)))
    ;; [text](dest): the destination follows "]("
    (if (i32.and (i32.lt_s (i32.add (local.get $e) (i32.const 1)) (local.get $le))
                 (i32.and (i32.eq (call $byte (local.get $e)) (i32.const 0x5D))
                          (i32.eq (call $byte (i32.add (local.get $e) (i32.const 1))) (i32.const 0x28))))
      (then
        (local.set $s (i32.add (local.get $e) (i32.const 2)))
        (local.set $q (local.get $s))
        (block $c (loop $close
          (br_if $c (i32.ge_s (local.get $q) (local.get $le)))
          (br_if $c (i32.eq (call $byte (local.get $q)) (i32.const 0x29)))
          (br_if $c (i32.eq (call $byte (local.get $q)) (i32.const 0x20)))
          (local.set $q (i32.add (local.get $q) (i32.const 1)))
          (br $close)))
        (local.set $e (local.get $q))))
    (local.set $n (i32.sub (local.get $e) (local.get $s)))
    (memory.copy (global.get $IO) (i32.add (global.get $TEXT) (local.get $s)) (local.get $n))
    (local.get $n))

  ;; ── scrolling, focus, time ───────────────────────────────────────────────

  (func $clamp_scroll
    (global.set $scroll (call $clamp (global.get $scroll) (i32.const 0)
      (call $max (i32.const 0) (i32.sub (global.get $doc_h) (global.get $H)))))
    (global.set $dirty (i32.const 1)))

  ;; $reveal scrolls just enough to keep the caret's line in view.
  (func $reveal (local $r i32) (local $top i32) (local $bot i32) (local $margin i32)
    (if (i32.eqz (global.get $nlines)) (then (return)))
    (local.set $r (call $rec (call $line_of (global.get $sel_h))))
    (local.set $margin (call $dp (i32.const 24)))
    (local.set $top (i32.sub (i32.load offset=8 (local.get $r)) (local.get $margin)))
    (local.set $bot (i32.add (i32.add (i32.load offset=8 (local.get $r)) (i32.load offset=12 (local.get $r))) (local.get $margin)))
    (if (i32.lt_s (local.get $top) (global.get $scroll)) (then (global.set $scroll (local.get $top))))
    (if (i32.gt_s (local.get $bot) (i32.add (global.get $scroll) (global.get $H)))
      (then (global.set $scroll (i32.sub (local.get $bot) (global.get $H)))))
    (call $clamp_scroll))

  (func (export "wheel") (param $dy i32)
    (call $layout)
    (global.set $scroll (i32.add (global.get $scroll) (local.get $dy)))
    (call $clamp_scroll))

  (func (export "set_scroll") (param $y i32)
    (call $layout)
    (if (i32.ne (local.get $y) (global.get $scroll)) (then (global.set $dirty (i32.const 1))))
    (global.set $scroll (local.get $y))
    (call $clamp_scroll))
  ;; set_page turns page mode on or off; the host calls resize() after.
  (func (export "set_page") (param $on i32)
    (global.set $page_mode (local.get $on))
    (global.set $dirty (i32.const 1)))

  (func (export "scroll_top") (result i32) (global.get $scroll))
  (func (export "doc_height") (result i32) (call $layout) (global.get $doc_h))

  (func (export "focus") (param $on i32)
    (global.set $focused (local.get $on))
    (global.set $caret_on (i32.const 1))
    (global.set $dirty (i32.const 1)))

  ;; tick advances the clock (ms). The caret blinks while idle; the host
  ;; calls render() afterwards and only paints when it returns 1.
  (func (export "tick") (param $ms i32) (local $on i32)
    (global.set $now_ms (local.get $ms))
    (if (i32.eqz (global.get $focused)) (then (return)))
    (local.set $on (i32.lt_u (i32.rem_u (i32.sub (local.get $ms) (global.get $last_edit_ms)) (i32.const 1060)) (i32.const 620)))
    ;; stay solid for a moment after any edit or move
    (if (i32.lt_u (i32.sub (local.get $ms) (global.get $last_edit_ms)) (i32.const 600)) (then (local.set $on (i32.const 1))))
    (if (i32.ne (local.get $on) (global.get $caret_on))
      (then (global.set $caret_on (local.get $on)) (global.set $dirty (i32.const 1)))))

  ;; sel_rect writes the selection's bounds in surface pixels to STATIC+0x210:
  ;; x of its start, top of its first line, x of its end, bottom of its last
  ;; line — where the host floats its selection bar. Returns the address.
  (func (export "sel_rect") (result i32) (local $o i32) (local $a i32) (local $b i32)
    (call $layout)
    (local.set $o (i32.add (global.get $CARET_OUT) (i32.const 16)))
    (local.set $a (call $rec (call $line_of (call $sel_lo))))
    (local.set $b (call $rec (call $line_of (call $sel_hi))))
    (i32.store offset=0 (local.get $o) (call $x_of (call $sel_lo)))
    (i32.store offset=4 (local.get $o) (i32.sub (i32.load offset=8 (local.get $a)) (global.get $scroll)))
    (i32.store offset=8 (local.get $o) (call $x_of (call $sel_hi)))
    (i32.store offset=12 (local.get $o)
      (i32.sub (i32.add (i32.load offset=8 (local.get $b)) (i32.load offset=12 (local.get $b))) (global.get $scroll)))
    (local.get $o))

  ;; caret_line copies the text of the caret's line, up to the caret, into IO
  ;; and returns its length; line_start is where that line begins. Together
  ;; they let a host notice "/…" typed at the start of a line.
  (func (export "caret_line") (result i32) (local $s i32) (local $n i32)
    (local.set $s (call $line_start (global.get $sel_h)))
    (local.set $n (i32.sub (global.get $sel_h) (local.get $s)))
    (memory.copy (global.get $IO) (i32.add (global.get $TEXT) (local.get $s)) (local.get $n))
    (local.get $n))
  (func (export "line_start") (result i32) (call $line_start (global.get $sel_h)))

  ;; caret_rect writes the caret's surface rectangle (x, y, w, h) to STATIC so
  ;; the host can put its input sink — and the IME's candidate window — there.
  (func (export "caret_rect") (result i32) (local $r i32)
    (call $layout)
    (local.set $r (call $rec (call $line_of (global.get $sel_h))))
    (i32.store offset=0 (global.get $CARET_OUT) (call $x_of (global.get $sel_h)))
    (i32.store offset=4 (global.get $CARET_OUT) (i32.sub (i32.load offset=8 (local.get $r)) (global.get $scroll)))
    (i32.store offset=8 (global.get $CARET_OUT) (call $max (call $dp (i32.const 2)) (i32.const 1)))
    (i32.store offset=12 (global.get $CARET_OUT) (i32.load offset=12 (local.get $r)))
    (global.get $CARET_OUT))
)
