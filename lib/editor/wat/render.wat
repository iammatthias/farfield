;; farfield editor — drawing.
;;
;; Everything the editor shows is drawn here, into an RGBA framebuffer at FB
;; that the host only copies to the screen: fills, glyph coverage blended in
;; colour, the caret, selection, block decorations, the scrollbar. There is no
;; DOM and no canvas drawing API behind it — just bytes.
;;
;; Palette (u32, bytes R G B A in memory):
;;   0 background   1 text   2 syntax (dimmed)   3 accent   4 code panel
;;   5 selection    6 caret  7 rules and bars   8 scrollbar  9 placeholder
(module
  (global $scroll (mut i32) (i32.const 0)) ;; document y at the top of the surface
  (global $dirty (mut i32) (i32.const 1))
  ;; $blink: only the caret changed; render redraws its line alone. The rows
  ;; render last touched are dirty_y..dirty_y+dirty_h, so a host re-uploads
  ;; just those instead of the whole framebuffer on every blink.
  (global $blink (mut i32) (i32.const 0))
  (global $dirty_y (mut i32) (i32.const 0))
  (global $dirty_h (mut i32) (i32.const 0))
  (func (export "dirty_y") (result i32) (global.get $dirty_y))
  (func (export "dirty_h") (result i32) (global.get $dirty_h))
  (func (export "is_dirty") (result i32) (global.get $dirty))
  ;; rows images may be drawn into (render_band narrows it)
  (global $clip_y0 (mut i32) (i32.const 0))
  (global $clip_y1 (mut i32) (i32.const 0x7FFFFFFF))

  (func $color (param $i i32) (result i32)
    (i32.load (i32.add (global.get $PALETTE) (i32.shl (local.get $i) (i32.const 2)))))

  ;; set_color lets the host bring its theme: rgba is 0xRRGGBBAA.
  (func (export "set_color") (param $i i32) (param $rgba i32)
    (if (i32.ge_u (local.get $i) (i32.const 16)) (then (return)))
    (i32.store (i32.add (global.get $PALETTE) (i32.shl (local.get $i) (i32.const 2)))
      (i32.or (i32.or
        (i32.shr_u (local.get $rgba) (i32.const 24))
        (i32.and (i32.shr_u (local.get $rgba) (i32.const 8)) (i32.const 0xFF00)))
        (i32.or
          (i32.and (i32.shl (local.get $rgba) (i32.const 8)) (i32.const 0xFF0000))
          (i32.shl (local.get $rgba) (i32.const 24)))))
    (global.set $dirty (i32.const 1)))

  ;; the default palette: farfield's light theme (Paper, Deep Space, Farfield
  ;; Blue). A host with CSS passes its live tokens instead.
  (func $default_palette
    (i32.store offset=0x100 (i32.const 0) (i32.const 0xFFD1E5F3)) ;; paper #f3e5d1
    (i32.store offset=0x104 (i32.const 0) (i32.const 0xFF2D220E)) ;; ink #0e222d
    (i32.store offset=0x108 (i32.const 0) (i32.const 0xFF8F9797)) ;; syntax, ink 40% on paper
    (i32.store offset=0x10C (i32.const 0) (i32.const 0xFF60350D)) ;; farfield blue #0d3560
    (i32.store offset=0x110 (i32.const 0) (i32.const 0xFFC3D5E2)) ;; code panel, ink 8% on paper
    (i32.store offset=0x114 (i32.const 0) (i32.const 0xFFBDC5CA)) ;; selection, blue 18% on paper
    (i32.store offset=0x118 (i32.const 0) (i32.const 0xFF60350D)) ;; caret
    (i32.store offset=0x11C (i32.const 0) (i32.const 0xFF939BA0)) ;; rules and quote bars
    (i32.store offset=0x120 (i32.const 0) (i32.const 0xFFA9B4BD)) ;; scrollbar
    (i32.store offset=0x124 (i32.const 0) (i32.const 0xFFA3AAAB)) ;; placeholder
    (i32.store offset=0x128 (i32.const 0) (i32.const 0xFF202AA6))) ;; spelling, #a62a20

  ;; $fill paints a rectangle in surface coordinates, clipped to the surface.
  (func $fill (param $x i32) (param $y i32) (param $w i32) (param $h i32) (param $c i32)
    (local $x1 i32) (local $y1 i32) (local $row i32) (local $p i32) (local $e i32)
    (local.set $x1 (call $min (i32.add (local.get $x) (local.get $w)) (global.get $W)))
    (local.set $y1 (call $min (i32.add (local.get $y) (local.get $h)) (global.get $H)))
    (local.set $x (call $max (local.get $x) (i32.const 0)))
    (local.set $y (call $max (local.get $y) (i32.const 0)))
    (if (i32.or (i32.ge_s (local.get $x) (local.get $x1)) (i32.ge_s (local.get $y) (local.get $y1))) (then (return)))
    (local.set $row (local.get $y))
    (block $rows_done
      (loop $rows
        (br_if $rows_done (i32.ge_s (local.get $row) (local.get $y1)))
        (local.set $p (i32.add (global.get $FB)
          (i32.shl (i32.add (i32.mul (local.get $row) (global.get $W)) (local.get $x)) (i32.const 2))))
        (local.set $e (i32.add (local.get $p) (i32.shl (i32.sub (local.get $x1) (local.get $x)) (i32.const 2))))
        (block $px_done
          (loop $px
            (br_if $px_done (i32.ge_u (local.get $p) (local.get $e)))
            (i32.store (local.get $p) (local.get $c))
            (local.set $p (i32.add (local.get $p) (i32.const 4)))
            (br $px)))
        (local.set $row (i32.add (local.get $row) (i32.const 1)))
        (br $rows))))

  ;; $mix blends colour c over d by coverage a (0–255), per channel.
  (func $mix (param $d i32) (param $c i32) (param $a i32) (result i32)
    (local $inv i32)
    (local.set $inv (i32.sub (i32.const 255) (local.get $a)))
    (i32.or (i32.or
      (i32.div_u (i32.add (i32.add (i32.mul (i32.and (local.get $d) (i32.const 0xFF)) (local.get $inv))
                                   (i32.mul (i32.and (local.get $c) (i32.const 0xFF)) (local.get $a))) (i32.const 127)) (i32.const 255))
      (i32.shl (i32.div_u (i32.add (i32.add
        (i32.mul (i32.and (i32.shr_u (local.get $d) (i32.const 8)) (i32.const 0xFF)) (local.get $inv))
        (i32.mul (i32.and (i32.shr_u (local.get $c) (i32.const 8)) (i32.const 0xFF)) (local.get $a))) (i32.const 127)) (i32.const 255)) (i32.const 8)))
      (i32.or
        (i32.shl (i32.div_u (i32.add (i32.add
          (i32.mul (i32.and (i32.shr_u (local.get $d) (i32.const 16)) (i32.const 0xFF)) (local.get $inv))
          (i32.mul (i32.and (i32.shr_u (local.get $c) (i32.const 16)) (i32.const 0xFF)) (local.get $a))) (i32.const 127)) (i32.const 255)) (i32.const 16))
        (i32.const 0xFF000000))))

  ;; $gamma_init builds the coverage curve glyphs are drawn through. Blending
  ;; raw coverage linearly leaves antialiased text thin and grey beside the
  ;; browser's own; browsers lift coverage before compositing. This lifts it by
  ;; x^0.6875 (= x^½ · x^⅛ · x^1/16 — three square roots, no pow in wasm),
  ;; which darkens edges without thickening a stem's solid core.
  (func $gamma_init (local $i i32) (local $x f32) (local $r f32)
    (block $done
      (loop $each
        (br_if $done (i32.gt_u (local.get $i) (i32.const 255)))
        (local.set $x (f32.div (f32.convert_i32_u (local.get $i)) (f32.const 255)))
        (local.set $r (f32.sqrt (local.get $x)))
        (local.set $r (f32.mul (local.get $r) (f32.sqrt (f32.sqrt (local.get $r)))))
        (local.set $r (f32.mul (local.get $r) (f32.sqrt (f32.sqrt (f32.sqrt (f32.sqrt (local.get $x)))))))
        (i32.store8 (i32.add (global.get $GAMMA) (local.get $i))
          (i32.trunc_f32_u (f32.add (f32.mul (local.get $r) (f32.const 255)) (f32.const 0.5))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $each))))

  ;; $draw_glyph blends a cached glyph with its origin at (x, baseline).
  (func $draw_glyph (param $e i32) (param $x i32) (param $base i32) (param $c i32)
    (local $w i32) (local $h i32) (local $bm i32) (local $gx i32) (local $gy i32) (local $i i32) (local $j i32)
    (local $sx i32) (local $sy i32) (local $a i32) (local $p i32)
    (local.set $w (i32.load offset=8 (local.get $e)))
    (local.set $h (i32.load offset=12 (local.get $e)))
    (if (i32.or (i32.eqz (local.get $w)) (i32.eqz (local.get $h))) (then (return)))
    (local.set $bm (i32.load offset=4 (local.get $e)))
    (local.set $gx (i32.add (local.get $x) (i32.load offset=16 (local.get $e))))
    (local.set $gy (i32.sub (local.get $base) (i32.load offset=20 (local.get $e))))
    (block $rows_done
      (loop $rows
        (br_if $rows_done (i32.ge_s (local.get $j) (local.get $h)))
        (local.set $sy (i32.add (local.get $gy) (local.get $j)))
        (if (i32.and (i32.ge_s (local.get $sy) (i32.const 0)) (i32.lt_s (local.get $sy) (global.get $H)))
          (then
            (local.set $i (i32.const 0))
            (block $cols_done
              (loop $cols
                (br_if $cols_done (i32.ge_s (local.get $i) (local.get $w)))
                (local.set $sx (i32.add (local.get $gx) (local.get $i)))
                (local.set $a (i32.load8_u (i32.add (global.get $GAMMA)
                  (i32.load8_u (i32.add (local.get $bm) (i32.add (i32.mul (local.get $j) (local.get $w)) (local.get $i)))))))
                (if (i32.and (i32.ne (local.get $a) (i32.const 0))
                      (i32.and (i32.ge_s (local.get $sx) (i32.const 0)) (i32.lt_s (local.get $sx) (global.get $W))))
                  (then
                    (local.set $p (i32.add (global.get $FB)
                      (i32.shl (i32.add (i32.mul (local.get $sy) (global.get $W)) (local.get $sx)) (i32.const 2))))
                    (i32.store (local.get $p)
                      (if (result i32) (i32.eq (local.get $a) (i32.const 255))
                        (then (i32.or (local.get $c) (i32.const 0xFF000000)))
                        (else (call $mix (i32.load (local.get $p)) (local.get $c) (local.get $a)))))))
                (local.set $i (i32.add (local.get $i) (i32.const 1)))
                (br $cols)))))
        (local.set $j (i32.add (local.get $j) (i32.const 1)))
        (br $rows))))

  ;; $text_color picks a character's colour from its style.
  (func $text_color (param $st i32) (result i32)
    (if (i32.and (local.get $st) (global.get $S_MARK))
      (then (return (call $color (select (i32.const 3) (i32.const 2) (i32.and (local.get $st) (global.get $S_ACCENT)))))))
    (if (i32.and (local.get $st) (i32.or (global.get $S_LINK) (global.get $S_ACCENT))) (then (return (call $color (i32.const 3)))))
    (call $color (i32.const 1)))

  ;; ── selection ────────────────────────────────────────────────────────────

  (global $sel_a (mut i32) (i32.const 0)) ;; anchor
  (global $sel_h (mut i32) (i32.const 0)) ;; head (the caret)
  (func $sel_lo (result i32) (call $min (global.get $sel_a) (global.get $sel_h)))
  (func $sel_hi (result i32) (call $max (global.get $sel_a) (global.get $sel_h)))

  ;; ── placeholder ──────────────────────────────────────────────────────────

  (global $PLACEHOLDER i32 (i32.const 0x3000)) ;; up to 1 KiB of UTF-8
  (global $ph_len (mut i32) (i32.const 0))

  ;; set_placeholder takes UTF-8 from IO: what an empty document says.
  (func (export "set_placeholder") (param $n i32)
    (local.set $n (call $min (local.get $n) (i32.const 1024)))
    (memory.copy (global.get $PLACEHOLDER) (global.get $IO) (local.get $n))
    (global.set $ph_len (local.get $n))
    (global.set $dirty (i32.const 1)))

  ;; ── the frame ────────────────────────────────────────────────────────────

  (global $focused (mut i32) (i32.const 0))
  (global $caret_on (mut i32) (i32.const 1))

  ;; $draw_line paints visual line l at surface y (document y − scroll).
  (func $draw_line (param $l i32)
    (local $r i32) (local $y i32) (local $h i32) (local $base i32) (local $info i32) (local $kind i32)
    (local $p i32) (local $e i32) (local $x f32) (local $st i32) (local $cp i32) (local $cl i32)
    (local $slot i32) (local $px i32) (local $g i32) (local $a f32) (local $xi i32) (local $lo i32) (local $hi i32)
    (local $selx0 i32) (local $selx1 i32) (local $sel i32) (local $aw i32)
    (local.set $r (call $rec (local.get $l)))
    ;; an image line away from the selection: its image is drawn, its source is not
    (if (i32.and (i32.load offset=24 (local.get $r)) (i32.const 0x40000)) (then (return)))
    (local.set $y (i32.sub (i32.load offset=8 (local.get $r)) (global.get $scroll)))
    (local.set $h (i32.load offset=12 (local.get $r)))
    (local.set $base (i32.add (local.get $y) (i32.load offset=16 (local.get $r))))
    (local.set $info (call $line_info (local.get $l)))
    (local.set $kind (i32.and (local.get $info) (i32.const 0xFF)))
    (local.set $p (i32.load (local.get $r)))
    (local.set $e (i32.load offset=4 (local.get $r)))

    ;; block decoration
    (if (i32.or (i32.eq (local.get $kind) (global.get $K_CODE)) (i32.eq (local.get $kind) (global.get $K_FENCE)))
      (then (call $fill (global.get $col_x) (local.get $y) (global.get $col_w) (local.get $h) (call $color (i32.const 4)))))
    (if (i32.eq (local.get $kind) (global.get $K_QUOTE))
      (then (call $fill (global.get $col_x) (local.get $y) (call $max (call $dp (i32.const 3)) (i32.const 2)) (local.get $h)
        (call $color (i32.const 7)))))
    (if (i32.eq (local.get $kind) (global.get $K_RULE))
      (then (call $fill (global.get $col_x) (i32.add (local.get $y) (i32.shr_s (local.get $h) (i32.const 1)))
        (global.get $col_w) (call $max (call $dp (i32.const 1)) (i32.const 1)) (call $color (i32.const 7)))))

    ;; selection behind the text
    (local.set $lo (call $sel_lo))
    (local.set $hi (call $sel_hi))
    (local.set $sel (i32.and (i32.lt_s (local.get $lo) (local.get $hi))
      (i32.and (i32.le_s (local.get $lo) (local.get $e)) (i32.ge_s (local.get $hi) (local.get $p)))))

    (local.set $x (f32.convert_i32_s (i32.load offset=20 (local.get $r))))
    (local.set $selx0 (i32.const -1))
    (block $done
      (loop $chars
        ;; selection edges are found on the way across
        (if (local.get $sel)
          (then
            (if (i32.and (i32.lt_s (local.get $selx0) (i32.const 0)) (i32.ge_s (local.get $p) (local.get $lo)))
              (then (local.set $selx0 (i32.trunc_sat_f32_s (local.get $x)))))
            (if (i32.le_s (local.get $p) (local.get $hi))
              (then (local.set $selx1 (i32.trunc_sat_f32_s (local.get $x)))))))
        (br_if $done (i32.ge_s (local.get $p) (local.get $e)))
        (local.set $cp (call $cp_at (local.get $p)))
        (local.set $cl (global.get $dec_len))
        (local.set $st (call $style_of (local.get $p)))
        (local.set $slot (call $slot_for (local.get $st) (local.get $info)))
        (local.set $px (call $px_for (local.get $st) (local.get $info)))
        (local.set $a (call $cp_adv (local.get $cp) (local.get $st) (local.get $info)))
        (local.set $xi (i32.trunc_sat_f32_s (f32.nearest (local.get $x))))
        (local.set $aw (i32.trunc_sat_f32_s (f32.ceil (local.get $a))))
        ;; inline code gets a tint (code blocks are tinted whole)
        (if (i32.and (i32.ne (i32.and (local.get $st) (global.get $S_CODE)) (i32.const 0))
                     (i32.and (i32.and (i32.ne (local.get $kind) (global.get $K_CODE)) (i32.ne (local.get $kind) (global.get $K_FENCE)))
                              (i32.and (i32.ne (local.get $kind) (global.get $K_PLAIN)) (i32.ne (local.get $kind) (global.get $K_HTML)))))
          (then (call $fill (local.get $xi) (i32.add (local.get $y) (i32.div_s (local.get $h) (i32.const 8)))
            (local.get $aw) (i32.sub (local.get $h) (i32.div_s (local.get $h) (i32.const 4))) (call $color (i32.const 4)))))
        (local.set $x (f32.add (local.get $x) (local.get $a)))
        (local.set $p (i32.add (local.get $p) (local.get $cl)))
        (br $chars)))
    ;; a selection that runs past the end of the line covers the newline too
    (if (local.get $sel)
      (then
        (if (i32.gt_s (local.get $hi) (local.get $e))
          (then (local.set $selx1 (i32.add (i32.trunc_sat_f32_s (local.get $x)) (call $dp (i32.const 6))))))
        (if (i32.ge_s (local.get $selx0) (i32.const 0))
          (then (call $fill (local.get $selx0) (local.get $y) (i32.sub (local.get $selx1) (local.get $selx0)) (local.get $h)
            (call $color (i32.const 5)))))))

    ;; the text itself, over tint and selection
    (local.set $p (i32.load (local.get $r)))
    (local.set $x (f32.convert_i32_s (i32.load offset=20 (local.get $r))))
    (block $tdone
      (loop $text
        (br_if $tdone (i32.ge_s (local.get $p) (local.get $e)))
        (local.set $cp (call $cp_at (local.get $p)))
        (local.set $cl (global.get $dec_len))
        (local.set $st (call $style_of (local.get $p)))
        (local.set $slot (call $slot_for (local.get $st) (local.get $info)))
        (local.set $px (call $px_for (local.get $st) (local.get $info)))
        (local.set $a (call $cp_adv (local.get $cp) (local.get $st) (local.get $info)))
        (local.set $xi (i32.trunc_sat_f32_s (f32.nearest (local.get $x))))
        ;; a concealed syntax character has no width: nothing to draw
        (if (i32.and (i32.gt_u (local.get $cp) (i32.const 0x20)) (f32.gt (local.get $a) (f32.const 0)))
          (then
            (local.set $g (call $resolve (local.get $slot) (local.get $cp)))
            (call $draw_glyph (call $glyph (global.get $res_slot) (local.get $g) (local.get $px))
              (local.get $xi) (local.get $base) (call $text_color (local.get $st)))))
        (if (i32.and (local.get $st) (global.get $S_STRIKE))
          (then (call $fill (local.get $xi) (i32.sub (local.get $base) (i32.div_s (local.get $px) (i32.const 3)))
            (i32.trunc_sat_f32_s (f32.ceil (local.get $a))) (call $max (call $dp (i32.const 1)) (i32.const 1))
            (call $text_color (local.get $st)))))
        (local.set $x (f32.add (local.get $x) (local.get $a)))
        (local.set $p (i32.add (local.get $p) (local.get $cl)))
        (br $text)))
    ;; misspelled words get a wavy underline
    (call $draw_spelling (local.get $l) (i32.load (local.get $r)) (local.get $e) (local.get $base) (local.get $kind)))

  ;; render draws the frame if anything changed. Returns 1 when it drew.
  (func $render (export "render") (result i32)
    (local $l i32) (local $top i32) (local $bottom i32) (local $cx i32) (local $cl i32) (local $r i32)
    (local $th i32) (local $ty i32) (local $p i32) (local $x f32) (local $cp i32) (local $g i32)
    (if (i32.or (i32.eqz (global.get $W)) (i32.eqz (global.get $H))) (then (return (i32.const 0))))
    (call $layout)
    (if (i32.and (i32.eqz (global.get $dirty)) (global.get $blink))
      (then (global.set $blink (i32.const 0)) (return (call $render_caret_line))))
    (if (i32.eqz (global.get $dirty)) (then (return (i32.const 0))))
    (global.set $dirty (i32.const 0))
    (global.set $blink (i32.const 0))
    (global.set $dirty_y (i32.const 0))
    (global.set $dirty_h (global.get $H))
    (call $fill (i32.const 0) (i32.const 0) (global.get $W) (global.get $H) (call $color (i32.const 0)))
    (if (i32.eqz (global.get $nlines)) (then (return (i32.const 1))))
    ;; visible lines only
    (local.set $l (call $line_at_y (global.get $scroll)))
    (local.set $bottom (i32.add (global.get $scroll) (global.get $H)))
    (block $done
      (loop $lines
        (br_if $done (i32.ge_s (local.get $l) (global.get $nlines)))
        (br_if $done (i32.ge_s (i32.load offset=8 (call $rec (local.get $l))) (local.get $bottom)))
        (call $draw_line (local.get $l))
        (local.set $l (i32.add (local.get $l) (i32.const 1)))
        (br $lines)))
    (call $draw_images)
    ;; an empty document shows its placeholder
    (if (i32.and (i32.eqz (global.get $len)) (i32.ne (global.get $ph_len) (i32.const 0)))
      (then
        (local.set $r (call $rec (i32.const 0)))
        (local.set $x (f32.convert_i32_s (i32.load offset=20 (local.get $r))))
        (local.set $p (global.get $PLACEHOLDER))
        (block $pdone
          (loop $ph
            (br_if $pdone (i32.ge_u (local.get $p) (i32.add (global.get $PLACEHOLDER) (global.get $ph_len))))
            (local.set $cp (call $decode (local.get $p)))
            (local.set $p (i32.add (local.get $p) (global.get $dec_len)))
            (local.set $g (call $glyph_id (call $slot_ok (i32.const 2)) (local.get $cp)))
            (call $draw_glyph (call $glyph (call $slot_ok (i32.const 2)) (local.get $g) (global.get $base_px))
              (i32.trunc_sat_f32_s (f32.nearest (local.get $x)))
              (i32.sub (i32.add (i32.load offset=8 (local.get $r)) (i32.load offset=16 (local.get $r))) (global.get $scroll))
              (call $color (i32.const 9)))
            (local.set $x (f32.add (local.get $x) (call $advance (call $slot_ok (i32.const 2)) (local.get $g) (global.get $base_px))))
            (br $ph)))))
    ;; the caret
    (if (i32.and (global.get $focused) (global.get $caret_on))
      (then
        (local.set $cl (call $line_of (global.get $sel_h)))
        (local.set $r (call $rec (local.get $cl)))
        (local.set $cx (call $x_of (global.get $sel_h)))
        (call $fill (local.get $cx) (i32.sub (i32.load offset=8 (local.get $r)) (global.get $scroll))
          (call $max (call $dp (i32.const 2)) (i32.const 1)) (i32.load offset=12 (local.get $r)) (call $color (i32.const 6)))))
    ;; the scrollbar, when there is anything to scroll
    (if (i32.and (i32.gt_s (global.get $doc_h) (global.get $H)) (i32.eqz (global.get $page_mode)))
      (then
        (local.set $th (call $max (call $dp (i32.const 24))
          (i32.div_s (i32.mul (global.get $H) (global.get $H)) (global.get $doc_h))))
        (local.set $ty (i32.div_s (i32.mul (global.get $scroll) (i32.sub (global.get $H) (local.get $th)))
          (call $max (i32.const 1) (i32.sub (global.get $doc_h) (global.get $H)))))
        (call $fill (i32.sub (global.get $W) (call $dp (i32.const 6))) (local.get $ty)
          (call $dp (i32.const 3)) (local.get $th) (call $color (i32.const 8)))))
    (i32.const 1))

  ;; render_band redraws only surface rows y0..y1 — widened to whole visual
  ;; lines, so no glyph is ever drawn twice over an uncleared edge — and
  ;; reports them as dirty_y/dirty_h. A host with a tall surface (page mode's
  ;; tile) redraws what is on screen after an edit and fills in the rest as it
  ;; scrolls into view, instead of the whole surface on every keystroke.
  (func (export "render_band") (param $y0 i32) (param $y1 i32) (result i32)
    (local $l i32) (local $l2 i32) (local $r i32) (local $top i32) (local $bot i32) (local $cl i32)
    (if (i32.or (i32.eqz (global.get $W)) (i32.eqz (global.get $H))) (then (return (i32.const 0))))
    (call $layout)
    (local.set $y0 (call $clamp (local.get $y0) (i32.const 0) (global.get $H)))
    (local.set $y1 (call $clamp (local.get $y1) (i32.const 0) (global.get $H)))
    (if (i32.ge_s (local.get $y0) (local.get $y1)) (then (return (i32.const 0))))
    ;; an empty document (the placeholder) is small: draw it all
    (if (i32.or (i32.eqz (global.get $nlines)) (i32.eqz (global.get $len)))
      (then (global.set $dirty (i32.const 1)) (return (call $render))))
    (local.set $l (call $line_at_y (i32.add (global.get $scroll) (local.get $y0))))
    (local.set $l2 (call $line_at_y (i32.add (global.get $scroll) (i32.sub (local.get $y1) (i32.const 1)))))
    (local.set $r (call $rec (local.get $l)))
    (local.set $top (call $min (local.get $y0) (i32.sub (i32.load offset=8 (local.get $r)) (global.get $scroll))))
    (local.set $r (call $rec (local.get $l2)))
    (local.set $bot (call $max (local.get $y1)
      (i32.sub (i32.add (i32.load offset=8 (local.get $r)) (i32.load offset=12 (local.get $r))) (global.get $scroll))))
    (local.set $top (call $max (local.get $top) (i32.const 0)))
    (local.set $bot (call $min (local.get $bot) (global.get $H)))
    (call $fill (i32.const 0) (local.get $top) (global.get $W) (i32.sub (local.get $bot) (local.get $top)) (call $color (i32.const 0)))
    (local.set $cl (local.get $l))
    (block $done
      (loop $lines
        (br_if $done (i32.gt_s (local.get $cl) (local.get $l2)))
        (call $draw_line (local.get $cl))
        (local.set $cl (i32.add (local.get $cl) (i32.const 1)))
        (br $lines)))
    (global.set $clip_y0 (local.get $top))
    (global.set $clip_y1 (local.get $bot))
    (call $draw_images)
    (global.set $clip_y0 (i32.const 0))
    (global.set $clip_y1 (i32.const 0x7FFFFFFF))
    ;; the caret, when it falls in the band
    (if (i32.and (global.get $focused) (global.get $caret_on))
      (then
        (local.set $cl (call $line_of (global.get $sel_h)))
        (if (i32.and (i32.ge_s (local.get $cl) (local.get $l)) (i32.le_s (local.get $cl) (local.get $l2)))
          (then
            (local.set $r (call $rec (local.get $cl)))
            (call $fill (call $x_of (global.get $sel_h)) (i32.sub (i32.load offset=8 (local.get $r)) (global.get $scroll))
              (call $max (call $dp (i32.const 2)) (i32.const 1)) (i32.load offset=12 (local.get $r)) (call $color (i32.const 6)))))))
    (global.set $dirty (i32.const 0))
    (global.set $blink (i32.const 0))
    (global.set $dirty_y (local.get $top))
    (global.set $dirty_h (i32.sub (local.get $bot) (local.get $top)))
    (i32.const 1))

  ;; $render_caret_line repaints only the caret's visual line — background,
  ;; text, selection, caret — for a blink. Returns 1 when it drew anything.
  (func $render_caret_line (result i32) (local $cl i32) (local $r i32) (local $y i32) (local $h i32)
    (if (i32.eqz (global.get $nlines)) (then (return (i32.const 0))))
    (local.set $cl (call $line_of (global.get $sel_h)))
    (local.set $r (call $rec (local.get $cl)))
    (local.set $y (i32.sub (i32.load offset=8 (local.get $r)) (global.get $scroll)))
    (local.set $h (i32.load offset=12 (local.get $r)))
    (if (i32.or (i32.ge_s (local.get $y) (global.get $H)) (i32.le_s (i32.add (local.get $y) (local.get $h)) (i32.const 0)))
      (then (return (i32.const 0))))
    (call $fill (i32.const 0) (local.get $y) (global.get $W) (local.get $h) (call $color (i32.const 0)))
    (call $draw_line (local.get $cl))
    (if (i32.and (global.get $focused) (global.get $caret_on))
      (then (call $fill (call $x_of (global.get $sel_h)) (local.get $y)
        (call $max (call $dp (i32.const 2)) (i32.const 1)) (local.get $h) (call $color (i32.const 6)))))
    (global.set $dirty_y (call $max (local.get $y) (i32.const 0)))
    (global.set $dirty_h (i32.sub (call $min (i32.add (local.get $y) (local.get $h)) (global.get $H)) (global.get $dirty_y)))
    (i32.const 1))

  (func (export "fb_ptr") (result i32) (global.get $FB))
)
