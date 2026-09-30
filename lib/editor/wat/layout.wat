;; farfield editor — layout.
;;
;; The document is laid out into visual lines: each logical line (text between
;; newlines) is classified, styled, and wrapped at word boundaries to the
;; column width. The column is a comfortable reading measure centred in the
;; surface, not the full width of a wide window.
;;
;; A visual line record (LINES + i*32):
;;    0 start byte   4 end byte (excludes the newline)   8 y   12 height
;;   16 baseline (from y)   20 x of the text start   24 info   28 logical start
;; info = kind | level<<8 | flags<<16, flags: 1 first visual line of its
;; logical line, 2 last.
;;
;; Font slots: 0 serif regular, 1 serif semibold, 2 serif italic, 3 serif
;; semibold italic, 4 mono regular, 5 mono semibold, 6 serif medium (H2).
;; A missing slot falls back to 0.
(module
  (global $W (mut i32) (i32.const 0))       ;; surface size, device px
  (global $H (mut i32) (i32.const 0))
  (global $dpr (mut i32) (i32.const 100))   ;; device pixel ratio × 100
  (global $base_px (mut i32) (i32.const 17))
  (global $mono_px (mut i32) (i32.const 15))
  (global $col_x (mut i32) (i32.const 0))   ;; left edge of the text column
  (global $col_w (mut i32) (i32.const 0))   ;; width of the text column
  (global $pad_top (mut i32) (i32.const 0))
  (global $nlines (mut i32) (i32.const 0))
  (global $doc_h (mut i32) (i32.const 0))   ;; laid-out content height
  (global $laid_rev (mut i32) (i32.const -1)) ;; text revision the layout is for
  (global $laid_w (mut i32) (i32.const -1))
  ;; the logical lines the selection touched at layout time: an image line
  ;; shows its source only while the selection is on it
  (global $laid_lo (mut i32) (i32.const -1))
  (global $laid_hi (mut i32) (i32.const -1))
  (global $laid_focus (mut i32) (i32.const -1))

  ;; css → device px
  (func $dp (param $css i32) (result i32)
    (i32.div_s (i32.add (i32.mul (local.get $css) (global.get $dpr)) (i32.const 50)) (i32.const 100)))

  (func $rec (param $i i32) (result i32)
    (i32.add (global.get $LINES) (i32.shl (local.get $i) (i32.const 5))))

  ;; ── style → font ─────────────────────────────────────────────────────────

  (func $slot_ok (param $s i32) (result i32)
    (select (local.get $s) (i32.const 0) (call $font_loaded (local.get $s))))

  ;; $slot_for picks the face for style st on a line of info. Headings follow
  ;; the brand's weights: a large H1 in Newsreader Regular, H2 in Medium, and
  ;; SemiBold only at the smaller heading sizes — never a bold display serif.
  (func $slot_for (param $st i32) (param $info i32) (result i32) (local $s i32) (local $kind i32) (local $level i32)
    (local.set $kind (i32.and (local.get $info) (i32.const 0xFF)))
    (if (i32.or (i32.and (local.get $st) (global.get $S_CODE))
                (i32.or (i32.eq (local.get $kind) (global.get $K_CODE)) (i32.eq (local.get $kind) (global.get $K_FENCE))))
      (then (return (call $slot_ok (select (i32.const 5) (i32.const 4) (i32.and (local.get $st) (global.get $S_BOLD)))))))
    (local.set $s (i32.and (local.get $st) (i32.const 3))) ;; bold | italic → 0..3
    (if (i32.and (local.get $st) (global.get $S_HEAD))
      (then
        (local.set $level (i32.and (i32.shr_u (local.get $info) (i32.const 8)) (i32.const 0x7F)))
        ;; italic headings keep the italic face
        (if (i32.and (local.get $st) (global.get $S_ITAL)) (then (return (call $slot_ok (local.get $s)))))
        (if (i32.eq (local.get $level) (i32.const 1)) (then (return (call $slot_ok (local.get $s)))))
        (if (i32.eq (local.get $level) (i32.const 2))
          (then (return (call $slot_ok (select (i32.const 1) (i32.const 6) (i32.and (local.get $st) (global.get $S_BOLD)))))))
        (local.set $s (i32.or (local.get $s) (i32.const 1)))))
    (call $slot_ok (local.get $s)))

  ;; $tracking: letter-spacing in px for a line — display sizes tighten, as
  ;; large type should (the brand sets Newsreader display at −0.025em).
  (func $tracking (param $info i32) (param $px i32) (result f32) (local $level i32)
    (if (i32.ne (i32.and (local.get $info) (i32.const 0xFF)) (global.get $K_HEAD)) (then (return (f32.const 0))))
    (local.set $level (i32.and (i32.shr_u (local.get $info) (i32.const 8)) (i32.const 0x7F)))
    (if (i32.eq (local.get $level) (i32.const 1))
      (then (return (f32.mul (f32.convert_i32_s (local.get $px)) (f32.const -0.022)))))
    (if (i32.eq (local.get $level) (i32.const 2))
      (then (return (f32.mul (f32.convert_i32_s (local.get $px)) (f32.const -0.014)))))
    (f32.const 0))

  ;; heading scale, percent, by level
  (func $head_pct (param $level i32) (result i32)
    (if (i32.eq (local.get $level) (i32.const 1)) (then (return (i32.const 188))))
    (if (i32.eq (local.get $level) (i32.const 2)) (then (return (i32.const 150))))
    (if (i32.eq (local.get $level) (i32.const 3)) (then (return (i32.const 125))))
    (if (i32.eq (local.get $level) (i32.const 4)) (then (return (i32.const 110))))
    (i32.const 100))

  ;; $px_for: the pixel size of a character with style st on a line of info.
  (func $px_for (param $st i32) (param $info i32) (result i32) (local $kind i32)
    (local.set $kind (i32.and (local.get $info) (i32.const 0xFF)))
    (if (i32.or (i32.and (local.get $st) (global.get $S_CODE))
                (i32.or (i32.eq (local.get $kind) (global.get $K_CODE)) (i32.eq (local.get $kind) (global.get $K_FENCE))))
      (then (return (global.get $mono_px))))
    (if (i32.eq (local.get $kind) (global.get $K_HEAD))
      (then (return (i32.div_s (i32.mul (global.get $base_px)
        (call $head_pct (i32.and (i32.shr_u (local.get $info) (i32.const 8)) (i32.const 0x7F)))) (i32.const 100)))))
    (global.get $base_px))

  ;; $line_px: the size that sets a line's height.
  (func $line_px (param $info i32) (result i32)
    (call $px_for (i32.const 0) (local.get $info)))

  ;; $cp_adv: advance of codepoint cp in style st on a line of info, in px.
  ;; A tab is four spaces; a glyph the face lacks comes from $resolve.
  (func $cp_adv (param $cp i32) (param $st i32) (param $info i32) (result f32)
    (local $slot i32) (local $px i32) (local $g i32)
    ;; syntax on a concealed line (info bit 15) takes no room
    (if (i32.and (i32.ne (i32.and (local.get $info) (i32.const 0x8000)) (i32.const 0))
                 (i32.ne (i32.and (local.get $st) (global.get $S_MARK)) (i32.const 0)))
      (then (return (f32.const 0))))
    (local.set $slot (call $slot_for (local.get $st) (local.get $info)))
    (local.set $px (call $px_for (local.get $st) (local.get $info)))
    (if (i32.eq (local.get $cp) (i32.const 9))
      (then (return (f32.mul (f32.const 4)
        (call $advance (local.get $slot) (call $glyph_id (local.get $slot) (i32.const 0x20)) (local.get $px))))))
    (local.set $g (call $resolve (local.get $slot) (local.get $cp)))
    (f32.add (call $advance (global.get $res_slot) (local.get $g) (local.get $px))
             (call $tracking (local.get $info) (local.get $px))))

  ;; $resolve finds a glyph for cp: in the requested slot, else the serif,
  ;; else the mono (which carries arrows and symbols the serif lacks). The
  ;; slot it came from is left in $res_slot; a glyph no face has resolves to
  ;; the requested slot's .notdef.
  (global $res_slot (mut i32) (i32.const 0))
  (func $resolve (param $slot i32) (param $cp i32) (result i32) (local $g i32)
    (global.set $res_slot (local.get $slot))
    (local.set $g (call $glyph_id (local.get $slot) (local.get $cp)))
    (if (local.get $g) (then (return (local.get $g))))
    (if (i32.and (i32.ne (local.get $slot) (i32.const 0)) (call $font_loaded (i32.const 0)))
      (then
        (local.set $g (call $glyph_id (i32.const 0) (local.get $cp)))
        (if (local.get $g) (then (global.set $res_slot (i32.const 0)) (return (local.get $g))))))
    (if (i32.and (i32.ne (local.get $slot) (i32.const 4)) (call $font_loaded (i32.const 4)))
      (then
        (local.set $g (call $glyph_id (i32.const 4) (local.get $cp)))
        (if (local.get $g) (then (global.set $res_slot (i32.const 4)) (return (local.get $g))))))
    (i32.const 0))

  ;; $adv_at: advance of the character at text position p on a line of info.
  (func $adv_at (param $p i32) (param $info i32) (result f32)
    (call $cp_adv (call $cp_at (local.get $p)) (call $style_of (local.get $p)) (local.get $info)))

  ;; ── laying out ───────────────────────────────────────────────────────────

  (func $line_height (param $info i32) (result i32) (local $px i32) (local $kind i32)
    (local.set $px (call $line_px (local.get $info)))
    (local.set $kind (i32.and (local.get $info) (i32.const 0xFF)))
    (if (i32.eq (local.get $kind) (global.get $K_HEAD))
      (then (return (i32.div_s (i32.mul (local.get $px) (i32.const 130)) (i32.const 100)))))
    (if (i32.or (i32.eq (local.get $kind) (global.get $K_CODE)) (i32.eq (local.get $kind) (global.get $K_FENCE)))
      (then (return (i32.div_s (i32.mul (local.get $px) (i32.const 160)) (i32.const 100)))))
    (i32.div_s (i32.mul (local.get $px) (i32.const 165)) (i32.const 100)))

  ;; $indent_for: extra x for a line's text beyond the column edge.
  (func $indent_for (param $info i32) (result i32)
    (if (i32.eq (i32.and (local.get $info) (i32.const 0xFF)) (global.get $K_QUOTE))
      (then (return (call $dp (i32.const 18)))))
    (if (i32.or (i32.eq (i32.and (local.get $info) (i32.const 0xFF)) (global.get $K_CODE))
                (i32.eq (i32.and (local.get $info) (i32.const 0xFF)) (global.get $K_FENCE)))
      (then (return (call $dp (i32.const 14)))))
    (i32.const 0))

  (func $emit (param $s i32) (param $e i32) (param $y i32) (param $h i32) (param $info i32)
    (param $x0 i32) (param $flags i32) (param $ls i32) (local $r i32) (local $px i32) (local $slot i32) (local $asc i32) (local $desc i32)
    (if (i32.ge_u (global.get $nlines) (global.get $LINES_CAP)) (then (return)))
    (local.set $r (call $rec (global.get $nlines)))
    (local.set $px (call $line_px (local.get $info)))
    (local.set $slot (call $slot_for (select (global.get $S_HEAD) (i32.const 0)
      (i32.eq (i32.and (local.get $info) (i32.const 0xFF)) (global.get $K_HEAD))) (local.get $info)))
    (local.set $asc (call $ascent (local.get $slot) (local.get $px)))
    (local.set $desc (call $descent (local.get $slot) (local.get $px)))
    (i32.store offset=0 (local.get $r) (local.get $s))
    (i32.store offset=4 (local.get $r) (local.get $e))
    (i32.store offset=8 (local.get $r) (local.get $y))
    (i32.store offset=12 (local.get $r) (local.get $h))
    (i32.store offset=16 (local.get $r)
      (i32.add (i32.div_s (i32.sub (local.get $h) (i32.add (local.get $asc) (local.get $desc))) (i32.const 2)) (local.get $asc)))
    (i32.store offset=20 (local.get $r) (local.get $x0))
    (i32.store offset=24 (local.get $r) (i32.or (local.get $info) (i32.shl (local.get $flags) (i32.const 16))))
    (i32.store offset=28 (local.get $r) (local.get $ls))
    (global.set $nlines (i32.add (global.get $nlines) (i32.const 1))))

  ;; $layout rebuilds the line table when the text or the width changed.
  (func $layout (local $p i32) (local $ls i32) (local $le i32) (local $info i32) (local $y i32) (local $h i32) (local $img i32) (local $y2 i32)
    (local $x0 i32) (local $avail f32) (local $start i32) (local $i i32) (local $x f32) (local $a f32)
    (local $brk i32) (local $cp i32) (local $cl i32) (local $first i32) (local $kind i32) (local $prevkind i32)
    (if (i32.and (i32.and (i32.eq (global.get $laid_rev) (global.get $revision)) (i32.eq (global.get $laid_w) (global.get $W)))
                 (i32.or (global.get $plain)
                   (i32.and (i32.eq (global.get $laid_focus) (global.get $focused))
                   (i32.and (i32.eq (global.get $laid_lo) (call $line_start (call $sel_lo)))
                            (i32.eq (global.get $laid_hi) (call $line_start (call $sel_hi)))))))
      (then (return)))
    (global.set $laid_lo (call $line_start (call $sel_lo)))
    (global.set $laid_focus (global.get $focused))
    (global.set $laid_hi (call $line_start (call $sel_hi)))
    (global.set $laid_rev (global.get $revision))
    (global.set $laid_w (global.get $W))
    (global.set $nlines (i32.const 0))
    (global.set $in_fence (i32.const 0))
    (global.set $place_n (i32.const 0))
    (local.set $y (global.get $pad_top))
    (local.set $prevkind (i32.const -1))
    (block $all_done
      (loop $lines
        (local.set $ls (local.get $p))
        (local.set $le (call $line_end (local.get $p)))
        (local.set $info (call $classify (local.get $ls) (local.get $le)))
        (call $style_line (local.get $ls) (local.get $le) (local.get $info))
        (local.set $kind (i32.and (local.get $info) (i32.const 0xFF)))
        ;; an image line away from the selection shows only its image: one
        ;; record (flag 4, drawn without text) as tall as the image
        (local.set $img (call $image_line (local.get $ls) (local.get $le)))
        (if (i32.and (i32.ge_s (local.get $img) (i32.const 0))
                     (i32.or (i32.gt_s (call $sel_lo) (local.get $le)) (i32.lt_s (call $sel_hi) (local.get $ls))))
          (then
            (local.set $y2 (call $image_place (local.get $img) (local.get $y)))
            (call $emit (local.get $ls) (local.get $le) (local.get $y) (i32.sub (local.get $y2) (local.get $y))
              (local.get $info) (global.get $col_x) (i32.const 7) (local.get $ls))
            (local.set $y (local.get $y2))
            (local.set $prevkind (local.get $kind))
            (br_if $all_done (i32.ge_u (local.get $le) (global.get $len)))
            (local.set $p (i32.add (local.get $le) (i32.const 1)))
            (br $lines)))
        ;; a prose line away from the selection reads as the finished text:
        ;; its Markdown syntax (S_MARK) is concealed — no width, not drawn —
        ;; and comes back when the caret or selection reaches the line
        (if (i32.and (i32.eqz (global.get $plain))
              (i32.and (i32.or (i32.or (i32.eq (local.get $kind) (global.get $K_PARA)) (i32.eq (local.get $kind) (global.get $K_HEAD)))
                               (i32.or (i32.eq (local.get $kind) (global.get $K_QUOTE))
                                       (i32.or (i32.eq (local.get $kind) (global.get $K_BULLET)) (i32.eq (local.get $kind) (global.get $K_ORDER)))))
                       (i32.or (i32.eqz (global.get $focused))
                         (i32.or (i32.gt_s (call $sel_lo) (local.get $le)) (i32.lt_s (call $sel_hi) (local.get $ls))))))
          (then (local.set $info (i32.or (local.get $info) (i32.const 0x8000)))))
        ;; a heading gets air above it, unless it opens the document
        (if (i32.and (i32.eq (local.get $kind) (global.get $K_HEAD)) (i32.gt_s (global.get $nlines) (i32.const 0)))
          (then (local.set $y (i32.add (local.get $y) (i32.div_s (call $line_px (local.get $info)) (i32.const 2))))))
        (local.set $h (call $line_height (local.get $info)))
        (local.set $x0 (i32.add (global.get $col_x) (call $indent_for (local.get $info))))
        (local.set $avail (f32.convert_i32_s (i32.sub (i32.add (global.get $col_x) (global.get $col_w)) (local.get $x0))))
        ;; wrap
        (local.set $start (local.get $ls))
        (local.set $i (local.get $ls))
        (local.set $x (f32.const 0))
        (local.set $brk (i32.const -1))
        (local.set $first (i32.const 1))
        (block $wrapped
          (loop $chars
            (br_if $wrapped (i32.ge_s (local.get $i) (local.get $le)))
            (local.set $cp (call $cp_at (local.get $i)))
            (local.set $cl (global.get $dec_len))
            (local.set $a (call $cp_adv (local.get $cp) (call $style_of (local.get $i)) (local.get $info)))
            (if (i32.and (f32.gt (f32.add (local.get $x) (local.get $a)) (local.get $avail))
                         (i32.gt_s (local.get $i) (local.get $start)))
              (then
                (if (i32.le_s (local.get $brk) (local.get $start)) (then (local.set $brk (local.get $i))))
                (call $emit (local.get $start) (local.get $brk) (local.get $y) (local.get $h) (local.get $info)
                  (local.get $x0) (local.get $first) (local.get $ls))
                (local.set $y (i32.add (local.get $y) (local.get $h)))
                (local.set $first (i32.const 0))
                (local.set $start (local.get $brk))
                (local.set $i (local.get $brk))
                (local.set $x (f32.const 0))
                (local.set $brk (i32.const -1))
                ;; continuation lines of a list item hang under its text
                (if (i32.or (i32.eq (local.get $kind) (global.get $K_BULLET)) (i32.eq (local.get $kind) (global.get $K_ORDER)))
                  (then
                    (local.set $x0 (i32.add (global.get $col_x) (call $prefix_width (local.get $ls) (local.get $info))))
                    (local.set $avail (f32.convert_i32_s (i32.sub (i32.add (global.get $col_x) (global.get $col_w)) (local.get $x0))))))
                (br $chars)))
            (if (call $is_space (local.get $cp)) (then (local.set $brk (i32.add (local.get $i) (local.get $cl)))))
            (local.set $x (f32.add (local.get $x) (local.get $a)))
            (local.set $i (i32.add (local.get $i) (local.get $cl)))
            (br $chars)))
        (call $emit (local.get $start) (local.get $le) (local.get $y) (local.get $h) (local.get $info)
          (local.get $x0) (i32.or (i32.const 2) (local.get $first)) (local.get $ls))
        (local.set $y (i32.add (local.get $y) (local.get $h)))
        ;; an image line makes room for its image
        (local.set $y (call $place_image (local.get $ls) (local.get $le) (local.get $y)))
        (local.set $prevkind (local.get $kind))
        (br_if $all_done (i32.ge_u (local.get $le) (global.get $len)))
        (local.set $p (i32.add (local.get $le) (i32.const 1)))
        (br $lines)))
    (global.set $doc_h (i32.add (local.get $y) (call $dp (i32.const 120)))))

  ;; $prefix_width measures a list item's marker, so wrapped lines hang under
  ;; the text rather than the bullet. $classify just ran for this line, so
  ;; $prefix_len is its marker length.
  (func $prefix_width (param $ls i32) (param $info i32) (result i32) (local $p i32) (local $x f32)
    (local.set $p (local.get $ls))
    (block $done
      (loop $m
        (br_if $done (i32.ge_s (local.get $p) (i32.add (local.get $ls) (global.get $prefix_len))))
        (local.set $x (f32.add (local.get $x) (call $adv_at (local.get $p) (local.get $info))))
        (local.set $p (call $next (local.get $p)))
        (br $m)))
    (i32.trunc_sat_f32_s (local.get $x)))

  ;; ── geometry queries ─────────────────────────────────────────────────────

  ;; $line_of: the visual line holding position p. At a wrap point the
  ;; position belongs to the line it starts.
  (func $line_of (param $p i32) (result i32) (local $lo i32) (local $hi i32) (local $mid i32)
    (local.set $hi (i32.sub (global.get $nlines) (i32.const 1)))
    (block $done
      (loop $bs
        (br_if $done (i32.ge_s (local.get $lo) (local.get $hi)))
        (local.set $mid (i32.shr_u (i32.add (i32.add (local.get $lo) (local.get $hi)) (i32.const 1)) (i32.const 1)))
        (if (i32.le_s (i32.load (call $rec (local.get $mid))) (local.get $p))
          (then (local.set $lo (local.get $mid)))
          (else (local.set $hi (i32.sub (local.get $mid) (i32.const 1)))))
        (br $bs)))
    (local.get $lo))

  ;; $line_at_y: the visual line at document y (clamped to the first/last).
  (func $line_at_y (param $y i32) (result i32) (local $lo i32) (local $hi i32) (local $mid i32)
    (local.set $hi (i32.sub (global.get $nlines) (i32.const 1)))
    (block $done
      (loop $bs
        (br_if $done (i32.ge_s (local.get $lo) (local.get $hi)))
        (local.set $mid (i32.shr_u (i32.add (i32.add (local.get $lo) (local.get $hi)) (i32.const 1)) (i32.const 1)))
        (if (i32.le_s (i32.load offset=8 (call $rec (local.get $mid))) (local.get $y))
          (then (local.set $lo (local.get $mid)))
          (else (local.set $hi (i32.sub (local.get $mid) (i32.const 1)))))
        (br $bs)))
    (local.get $lo))

  (func $line_info (param $l i32) (result i32)
    (i32.and (i32.load offset=24 (call $rec (local.get $l))) (i32.const 0xFFFF)))

  ;; $x_of: the x of position p on its visual line.
  (func $x_of (param $p i32) (result i32) (local $l i32) (local $r i32) (local $q i32) (local $x f32) (local $info i32)
    (local.set $l (call $line_of (local.get $p)))
    (local.set $r (call $rec (local.get $l)))
    (local.set $info (call $line_info (local.get $l)))
    (local.set $q (i32.load (local.get $r)))
    (block $done
      (loop $walk
        (br_if $done (i32.ge_s (local.get $q) (local.get $p)))
        (br_if $done (i32.ge_s (local.get $q) (i32.load offset=4 (local.get $r))))
        (local.set $x (f32.add (local.get $x) (call $adv_at (local.get $q) (local.get $info))))
        (local.set $q (call $next (local.get $q)))
        (br $walk)))
    (i32.add (i32.load offset=20 (local.get $r)) (i32.trunc_sat_f32_s (f32.nearest (local.get $x)))))

  ;; $pos_in_line: the position on visual line l nearest to x.
  (func $pos_in_line (param $l i32) (param $x i32) (result i32)
    (local $r i32) (local $q i32) (local $e i32) (local $cx f32) (local $a f32) (local $info i32) (local $tx f32)
    (local.set $r (call $rec (local.get $l)))
    (local.set $info (call $line_info (local.get $l)))
    (local.set $q (i32.load (local.get $r)))
    (local.set $e (i32.load offset=4 (local.get $r)))
    (local.set $tx (f32.convert_i32_s (i32.sub (local.get $x) (i32.load offset=20 (local.get $r)))))
    (block $done
      (loop $walk
        (br_if $done (i32.ge_s (local.get $q) (local.get $e)))
        (local.set $a (call $adv_at (local.get $q) (local.get $info)))
        (br_if $done (f32.lt (local.get $tx) (f32.add (local.get $cx) (f32.mul (local.get $a) (f32.const 0.5)))))
        (local.set $cx (f32.add (local.get $cx) (local.get $a)))
        (local.set $q (call $next (local.get $q)))
        (br $walk)))
    ;; the end of a wrapped (not last) line is the next line's start; stay on
    ;; this one by landing before its trailing space instead
    (if (i32.and (i32.ge_s (local.get $q) (local.get $e))
                 (i32.eqz (i32.and (i32.shr_u (i32.load offset=24 (local.get $r)) (i32.const 16)) (i32.const 2))))
      (then (return (call $max (i32.load (local.get $r)) (call $prev (local.get $e))))))
    (local.get $q))
)
