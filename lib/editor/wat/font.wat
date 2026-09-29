;; farfield editor — TrueType fonts and the glyph rasterizer.
;;
;; No font engine is borrowed from the host: the editor reads TrueType files
;; itself — table directory, cmap (formats 4 and 12), hmtx, loca, glyf with
;; simple and composite glyphs — and turns outlines into anti-aliased coverage
;; with a signed-area accumulation rasterizer (the approach of font-rs): each
;; edge adds the area it covers to an accumulator, and a running sum across a
;; row turns those areas into exact per-pixel coverage. Quadratic curves are
;; flattened to lines with a subdivision count derived from their deviation.
;;
;; Rendered glyphs are cached by (font slot, glyph id, pixel size).
;;
;; A font slot record (FONTREC + slot*64):
;;    0 base    4 length   8 unitsPerEm   12 indexToLocFormat
;;   16 numGlyphs   20 ascender   24 descender   28 lineGap
;;   32 numberOfHMetrics   36 cmap subtable address   40 cmap format
;;   44 loca address   48 glyf address   52 hmtx address   56 loaded
(module
  (global $fonts_used (mut i32) (i32.const 0)) ;; bytes of FONTS in use

  (func $frec (param $slot i32) (result i32)
    (i32.add (global.get $FONTREC) (i32.shl (local.get $slot) (i32.const 6))))

  ;; $find_table returns the address of a table by its four-character tag,
  ;; or 0 when the font has none.
  (func $find_table (param $base i32) (param $tag i32) (result i32)
    (local $n i32) (local $i i32) (local $rec i32)
    (local.set $n (call $u16be (i32.add (local.get $base) (i32.const 4))))
    (block $done
      (loop $scan
        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $rec (i32.add (local.get $base) (i32.add (i32.const 12) (i32.mul (local.get $i) (i32.const 16)))))
        (if (i32.eq (call $u32be (local.get $rec)) (local.get $tag))
          (then (return (i32.add (local.get $base) (call $u32be (i32.add (local.get $rec) (i32.const 8)))))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $scan)))
    (i32.const 0))

  ;; font_load copies a TrueType file from IO into a slot and parses it.
  ;; Returns 1 on success, 0 when the file is not a usable TrueType font or
  ;; there is no room.
  (func (export "font_load") (param $slot i32) (param $n i32) (result i32)
    (local $base i32) (local $r i32) (local $head i32) (local $hhea i32) (local $maxp i32)
    (local $cmap i32) (local $i i32) (local $nsub i32) (local $sub i32) (local $pid i32)
    (local $eid i32) (local $fmt i32) (local $best i32) (local $bestfmt i32)
    (if (i32.ge_u (local.get $slot) (i32.const 8)) (then (return (i32.const 0))))
    (if (i32.gt_u (i32.add (global.get $fonts_used) (local.get $n)) (global.get $FONTS_CAP))
      (then (return (i32.const 0))))
    (local.set $base (i32.add (global.get $FONTS) (global.get $fonts_used)))
    (memory.copy (local.get $base) (global.get $IO) (local.get $n))
    ;; sfnt version 0x00010000 (TrueType) or 'true'
    (local.set $r (call $u32be (local.get $base)))
    (if (i32.and (i32.ne (local.get $r) (i32.const 0x00010000)) (i32.ne (local.get $r) (i32.const 0x74727565)))
      (then (return (i32.const 0))))
    (local.set $head (call $find_table (local.get $base) (i32.const 0x68656164))) ;; 'head'
    (local.set $hhea (call $find_table (local.get $base) (i32.const 0x68686561))) ;; 'hhea'
    (local.set $maxp (call $find_table (local.get $base) (i32.const 0x6D617870))) ;; 'maxp'
    (local.set $cmap (call $find_table (local.get $base) (i32.const 0x636D6170))) ;; 'cmap'
    (if (i32.or (i32.or (i32.eqz (local.get $head)) (i32.eqz (local.get $hhea)))
                (i32.or (i32.eqz (local.get $maxp)) (i32.eqz (local.get $cmap))))
      (then (return (i32.const 0))))
    (local.set $r (call $frec (local.get $slot)))
    (i32.store offset=0 (local.get $r) (local.get $base))
    (i32.store offset=4 (local.get $r) (local.get $n))
    (i32.store offset=8 (local.get $r) (call $u16be (i32.add (local.get $head) (i32.const 18))))
    (i32.store offset=12 (local.get $r) (call $s16be (i32.add (local.get $head) (i32.const 50))))
    (i32.store offset=16 (local.get $r) (call $u16be (i32.add (local.get $maxp) (i32.const 4))))
    (i32.store offset=20 (local.get $r) (call $s16be (i32.add (local.get $hhea) (i32.const 4))))
    (i32.store offset=24 (local.get $r) (call $s16be (i32.add (local.get $hhea) (i32.const 6))))
    (i32.store offset=28 (local.get $r) (call $s16be (i32.add (local.get $hhea) (i32.const 8))))
    (i32.store offset=32 (local.get $r) (call $u16be (i32.add (local.get $hhea) (i32.const 34))))
    (i32.store offset=44 (local.get $r) (call $find_table (local.get $base) (i32.const 0x6C6F6361))) ;; 'loca'
    (i32.store offset=48 (local.get $r) (call $find_table (local.get $base) (i32.const 0x676C7966))) ;; 'glyf'
    (i32.store offset=52 (local.get $r) (call $find_table (local.get $base) (i32.const 0x686D7478))) ;; 'hmtx'
    (if (i32.or (i32.eqz (i32.load offset=44 (local.get $r))) (i32.eqz (i32.load offset=48 (local.get $r))))
      (then (return (i32.const 0)))) ;; CFF outlines are not supported
    ;; Pick a Unicode cmap subtable: full-repertoire format 12 if present,
    ;; else a BMP format 4.
    (local.set $nsub (call $u16be (i32.add (local.get $cmap) (i32.const 2))))
    (block $done
      (loop $scan
        (br_if $done (i32.ge_u (local.get $i) (local.get $nsub)))
        (local.set $pid (call $u16be (i32.add (local.get $cmap) (i32.add (i32.const 4) (i32.mul (local.get $i) (i32.const 8))))))
        (local.set $eid (call $u16be (i32.add (local.get $cmap) (i32.add (i32.const 6) (i32.mul (local.get $i) (i32.const 8))))))
        (local.set $sub (i32.add (local.get $cmap)
          (call $u32be (i32.add (local.get $cmap) (i32.add (i32.const 8) (i32.mul (local.get $i) (i32.const 8)))))))
        (local.set $fmt (call $u16be (local.get $sub)))
        (if (i32.or (i32.eqz (local.get $pid))
                    (i32.and (i32.eq (local.get $pid) (i32.const 3))
                             (i32.or (i32.eq (local.get $eid) (i32.const 1)) (i32.eq (local.get $eid) (i32.const 10)))))
          (then
            (if (i32.eq (local.get $fmt) (i32.const 12))
              (then (local.set $best (local.get $sub)) (local.set $bestfmt (i32.const 12))))
            (if (i32.and (i32.eq (local.get $fmt) (i32.const 4)) (i32.ne (local.get $bestfmt) (i32.const 12)))
              (then (local.set $best (local.get $sub)) (local.set $bestfmt (i32.const 4))))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $scan)))
    (if (i32.eqz (local.get $best)) (then (return (i32.const 0))))
    (i32.store offset=36 (local.get $r) (local.get $best))
    (i32.store offset=40 (local.get $r) (local.get $bestfmt))
    (i32.store offset=56 (local.get $r) (i32.const 1))
    (global.set $fonts_used (i32.add (global.get $fonts_used) (i32.and (i32.add (local.get $n) (i32.const 3)) (i32.const -4))))
    ;; forget cached glyph ids and bitmaps for this slot
    (memory.fill (i32.add (global.get $GMAP) (i32.mul (local.get $slot) (i32.const 0x6000))) (i32.const 0) (i32.const 0x6000))
    (call $gcache_clear)
    (i32.const 1))

  (func $font_loaded (param $slot i32) (result i32)
    (i32.load offset=56 (call $frec (local.get $slot))))

  ;; $cmap_lookup maps a codepoint to a glyph id (0 = .notdef).
  (func $cmap_lookup (param $slot i32) (param $cp i32) (result i32)
    (local $r i32) (local $sub i32) (local $segx2 i32) (local $i i32) (local $end i32) (local $start i32)
    (local $delta i32) (local $ro i32) (local $roa i32) (local $g i32) (local $n i32) (local $grp i32)
    (local.set $r (call $frec (local.get $slot)))
    (local.set $sub (i32.load offset=36 (local.get $r)))
    (if (i32.eq (i32.load offset=40 (local.get $r)) (i32.const 12))
      (then
        (local.set $n (call $u32be (i32.add (local.get $sub) (i32.const 12))))
        (block $done
          (loop $scan
            (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
            (local.set $grp (i32.add (local.get $sub) (i32.add (i32.const 16) (i32.mul (local.get $i) (i32.const 12)))))
            (if (i32.and (i32.ge_u (local.get $cp) (call $u32be (local.get $grp)))
                         (i32.le_u (local.get $cp) (call $u32be (i32.add (local.get $grp) (i32.const 4)))))
              (then (return (i32.add (call $u32be (i32.add (local.get $grp) (i32.const 8)))
                                     (i32.sub (local.get $cp) (call $u32be (local.get $grp)))))))
            (local.set $i (i32.add (local.get $i) (i32.const 1)))
            (br $scan)))
        (return (i32.const 0))))
    ;; format 4
    (if (i32.gt_u (local.get $cp) (i32.const 0xFFFF)) (then (return (i32.const 0))))
    (local.set $segx2 (call $u16be (i32.add (local.get $sub) (i32.const 6))))
    (block $done4
      (loop $seg
        (br_if $done4 (i32.ge_u (local.get $i) (local.get $segx2)))
        (local.set $end (call $u16be (i32.add (local.get $sub) (i32.add (i32.const 14) (local.get $i)))))
        (if (i32.le_u (local.get $cp) (local.get $end))
          (then
            (local.set $start (call $u16be (i32.add (local.get $sub)
              (i32.add (i32.add (i32.const 16) (local.get $segx2)) (local.get $i)))))
            (if (i32.lt_u (local.get $cp) (local.get $start)) (then (return (i32.const 0))))
            (local.set $delta (call $u16be (i32.add (local.get $sub)
              (i32.add (i32.add (i32.const 16) (i32.shl (local.get $segx2) (i32.const 1))) (local.get $i)))))
            (local.set $roa (i32.add (local.get $sub)
              (i32.add (i32.add (i32.const 16) (i32.mul (local.get $segx2) (i32.const 3))) (local.get $i))))
            (local.set $ro (call $u16be (local.get $roa)))
            (if (i32.eqz (local.get $ro))
              (then (return (i32.and (i32.add (local.get $cp) (local.get $delta)) (i32.const 0xFFFF)))))
            (local.set $g (call $u16be (i32.add (local.get $roa)
              (i32.add (local.get $ro) (i32.shl (i32.sub (local.get $cp) (local.get $start)) (i32.const 1))))))
            (if (i32.eqz (local.get $g)) (then (return (i32.const 0))))
            (return (i32.and (i32.add (local.get $g) (local.get $delta)) (i32.const 0xFFFF)))))
        (local.set $i (i32.add (local.get $i) (i32.const 2)))
        (br $seg)))
    (i32.const 0))

  ;; $glyph_id is $cmap_lookup behind a per-slot cache for the first 0x3000
  ;; codepoints (Latin, punctuation, symbols) — layout asks for the same few
  ;; hundred characters over and over.
  (func $glyph_id (param $slot i32) (param $cp i32) (result i32) (local $a i32) (local $g i32)
    (if (i32.ge_u (local.get $cp) (i32.const 0x3000))
      (then (return (call $cmap_lookup (local.get $slot) (local.get $cp)))))
    (local.set $a (i32.add (global.get $GMAP)
      (i32.add (i32.mul (local.get $slot) (i32.const 0x6000)) (i32.shl (local.get $cp) (i32.const 1)))))
    (local.set $g (i32.load16_u (local.get $a)))
    (if (local.get $g) (then (return (i32.sub (local.get $g) (i32.const 1)))))
    (local.set $g (call $cmap_lookup (local.get $slot) (local.get $cp)))
    (i32.store16 (local.get $a) (i32.add (local.get $g) (i32.const 1)))
    (local.get $g))

  ;; $advance_units is a glyph's advance width in font units.
  (func $advance_units (param $slot i32) (param $g i32) (result i32) (local $r i32) (local $nh i32)
    (local.set $r (call $frec (local.get $slot)))
    (local.set $nh (i32.load offset=32 (local.get $r)))
    (if (i32.ge_u (local.get $g) (local.get $nh)) (then (local.set $g (i32.sub (local.get $nh) (i32.const 1)))))
    (call $u16be (i32.add (i32.load offset=52 (local.get $r)) (i32.shl (local.get $g) (i32.const 2)))))

  ;; $advance is a glyph's advance in pixels at a size.
  (func $advance (param $slot i32) (param $g i32) (param $px i32) (result f32)
    (f32.div
      (f32.mul (f32.convert_i32_u (call $advance_units (local.get $slot) (local.get $g)))
               (f32.convert_i32_s (local.get $px)))
      (f32.convert_i32_u (i32.load offset=8 (call $frec (local.get $slot))))))

  ;; $ascent / $descent in pixels at a size (descent positive, downward).
  (func $ascent (param $slot i32) (param $px i32) (result i32)
    (i32.div_s (i32.mul (i32.load offset=20 (call $frec (local.get $slot))) (local.get $px))
               (i32.load offset=8 (call $frec (local.get $slot)))))
  (func $descent (param $slot i32) (param $px i32) (result i32)
    (i32.div_s (i32.mul (i32.sub (i32.const 0) (i32.load offset=24 (call $frec (local.get $slot)))) (local.get $px))
               (i32.load offset=8 (call $frec (local.get $slot)))))

  ;; $glyf_addr returns the address and (in $glyf_n) the length of a glyph's
  ;; outline data. A zero length is an empty glyph (a space).
  (global $glyf_n (mut i32) (i32.const 0))
  (func $glyf_addr (param $slot i32) (param $g i32) (result i32)
    (local $r i32) (local $loca i32) (local $a i32) (local $b i32)
    (local.set $r (call $frec (local.get $slot)))
    (local.set $loca (i32.load offset=44 (local.get $r)))
    (if (i32.ge_u (local.get $g) (i32.load offset=16 (local.get $r)))
      (then (global.set $glyf_n (i32.const 0)) (return (i32.const 0))))
    (if (i32.eqz (i32.load offset=12 (local.get $r)))
      (then
        (local.set $a (i32.shl (call $u16be (i32.add (local.get $loca) (i32.shl (local.get $g) (i32.const 1)))) (i32.const 1)))
        (local.set $b (i32.shl (call $u16be (i32.add (local.get $loca) (i32.add (i32.shl (local.get $g) (i32.const 1)) (i32.const 2)))) (i32.const 1))))
      (else
        (local.set $a (call $u32be (i32.add (local.get $loca) (i32.shl (local.get $g) (i32.const 2)))))
        (local.set $b (call $u32be (i32.add (local.get $loca) (i32.add (i32.shl (local.get $g) (i32.const 2)) (i32.const 4)))))))
    (global.set $glyf_n (i32.sub (local.get $b) (local.get $a)))
    (i32.add (i32.load offset=48 (local.get $r)) (local.get $a)))

  ;; ── the rasterizer ───────────────────────────────────────────────────────
  ;;
  ;; The accumulator is RASTER, f32 per cell, $aw cells wide (glyph width + 2)
  ;; and $ah rows tall. Outline points arrive already in pixel space.

  (global $aw (mut i32) (i32.const 0))
  (global $ah (mut i32) (i32.const 0))
  ;; transform from font units to accumulator pixels: X = x*s + tx, Y = ty - y*s
  (global $sc (mut f32) (f32.const 1))
  (global $tx (mut f32) (f32.const 0))
  (global $ty (mut f32) (f32.const 0))

  (func $acc_add (param $i i32) (param $v f32) (local $a i32)
    (if (i32.or (i32.lt_s (local.get $i) (i32.const 0))
                (i32.ge_s (local.get $i) (i32.mul (global.get $aw) (global.get $ah))))
      (then (return)))
    (local.set $a (i32.add (global.get $RASTER) (i32.shl (local.get $i) (i32.const 2))))
    (f32.store (local.get $a) (f32.add (f32.load (local.get $a)) (local.get $v))))

  (func $line (param $x0 f32) (param $y0 f32) (param $x1 f32) (param $y1 f32)
    (local $dir f32) (local $t f32) (local $dxdy f32) (local $x f32) (local $y i32) (local $yend i32)
    (local $dy f32) (local $xnext f32) (local $d f32) (local $xa f32) (local $xb f32)
    (local $x0f f32) (local $x0i i32) (local $x1c f32) (local $x1i i32) (local $ls i32)
    (local $xmf f32) (local $s f32) (local $fx0 f32) (local $a0 f32) (local $x1f f32) (local $am f32)
    (local $a1 f32) (local $a2 f32) (local $xi i32)
    (if (f32.le (f32.abs (f32.sub (local.get $y0) (local.get $y1))) (f32.const 0.0001)) (then (return)))
    (local.set $dir (f32.const 1))
    (if (f32.gt (local.get $y0) (local.get $y1))
      (then
        (local.set $dir (f32.const -1))
        (local.set $t (local.get $x0)) (local.set $x0 (local.get $x1)) (local.set $x1 (local.get $t))
        (local.set $t (local.get $y0)) (local.set $y0 (local.get $y1)) (local.set $y1 (local.get $t))))
    (local.set $dxdy (f32.div (f32.sub (local.get $x1) (local.get $x0)) (f32.sub (local.get $y1) (local.get $y0))))
    (local.set $x (local.get $x0))
    (if (f32.lt (local.get $y0) (f32.const 0))
      (then (local.set $x (f32.sub (local.get $x) (f32.mul (local.get $y0) (local.get $dxdy))))))
    (local.set $y (i32.trunc_sat_f32_s (f32.max (local.get $y0) (f32.const 0))))
    (local.set $yend (call $min (global.get $ah) (i32.trunc_sat_f32_s (f32.ceil (local.get $y1)))))
    (block $done
      (loop $rows
        (br_if $done (i32.ge_s (local.get $y) (local.get $yend)))
        (local.set $ls (i32.mul (local.get $y) (global.get $aw)))
        (local.set $dy (f32.sub
          (f32.min (f32.convert_i32_s (i32.add (local.get $y) (i32.const 1))) (local.get $y1))
          (f32.max (f32.convert_i32_s (local.get $y)) (local.get $y0))))
        (local.set $xnext (f32.add (local.get $x) (f32.mul (local.get $dxdy) (local.get $dy))))
        (local.set $d (f32.mul (local.get $dy) (local.get $dir)))
        (if (f32.lt (local.get $x) (local.get $xnext))
          (then (local.set $xa (local.get $x)) (local.set $xb (local.get $xnext)))
          (else (local.set $xa (local.get $xnext)) (local.set $xb (local.get $x))))
        (local.set $x0f (f32.floor (local.get $xa)))
        (local.set $x0i (i32.trunc_sat_f32_s (local.get $x0f)))
        (local.set $x1c (f32.ceil (local.get $xb)))
        (local.set $x1i (i32.trunc_sat_f32_s (local.get $x1c)))
        (if (i32.le_s (local.get $x1i) (i32.add (local.get $x0i) (i32.const 1)))
          (then
            (local.set $xmf (f32.sub (f32.mul (f32.const 0.5) (f32.add (local.get $x) (local.get $xnext))) (local.get $x0f)))
            (call $acc_add (i32.add (local.get $ls) (local.get $x0i))
              (f32.sub (local.get $d) (f32.mul (local.get $d) (local.get $xmf))))
            (call $acc_add (i32.add (local.get $ls) (i32.add (local.get $x0i) (i32.const 1)))
              (f32.mul (local.get $d) (local.get $xmf))))
          (else
            (local.set $s (f32.div (f32.const 1) (f32.sub (local.get $xb) (local.get $xa))))
            (local.set $fx0 (f32.sub (local.get $xa) (local.get $x0f)))
            (local.set $a0 (f32.mul (f32.mul (f32.const 0.5) (local.get $s))
              (f32.mul (f32.sub (f32.const 1) (local.get $fx0)) (f32.sub (f32.const 1) (local.get $fx0)))))
            (local.set $x1f (f32.add (f32.sub (local.get $xb) (local.get $x1c)) (f32.const 1)))
            (local.set $am (f32.mul (f32.mul (f32.const 0.5) (local.get $s)) (f32.mul (local.get $x1f) (local.get $x1f))))
            (call $acc_add (i32.add (local.get $ls) (local.get $x0i)) (f32.mul (local.get $d) (local.get $a0)))
            (if (i32.eq (local.get $x1i) (i32.add (local.get $x0i) (i32.const 2)))
              (then
                (call $acc_add (i32.add (local.get $ls) (i32.add (local.get $x0i) (i32.const 1)))
                  (f32.mul (local.get $d) (f32.sub (f32.sub (f32.const 1) (local.get $a0)) (local.get $am)))))
              (else
                (local.set $a1 (f32.mul (local.get $s) (f32.sub (f32.const 1.5) (local.get $fx0))))
                (call $acc_add (i32.add (local.get $ls) (i32.add (local.get $x0i) (i32.const 1)))
                  (f32.mul (local.get $d) (f32.sub (local.get $a1) (local.get $a0))))
                (local.set $xi (i32.add (local.get $x0i) (i32.const 2)))
                (block $mid_done
                  (loop $mid
                    (br_if $mid_done (i32.ge_s (local.get $xi) (i32.sub (local.get $x1i) (i32.const 1))))
                    (call $acc_add (i32.add (local.get $ls) (local.get $xi)) (f32.mul (local.get $d) (local.get $s)))
                    (local.set $xi (i32.add (local.get $xi) (i32.const 1)))
                    (br $mid)))
                (local.set $a2 (f32.add (local.get $a1)
                  (f32.mul (f32.convert_i32_s (i32.sub (i32.sub (local.get $x1i) (local.get $x0i)) (i32.const 3))) (local.get $s))))
                (call $acc_add (i32.add (local.get $ls) (i32.sub (local.get $x1i) (i32.const 1)))
                  (f32.mul (local.get $d) (f32.sub (f32.sub (f32.const 1) (local.get $a2)) (local.get $am))))))
            (call $acc_add (i32.add (local.get $ls) (local.get $x1i)) (f32.mul (local.get $d) (local.get $am)))))
        (local.set $x (local.get $xnext))
        (local.set $y (i32.add (local.get $y) (i32.const 1)))
        (br $rows))))

  ;; $quad flattens a quadratic Bézier into lines; the subdivision count grows
  ;; with the curve's deviation from its chord.
  (func $quad (param $x0 f32) (param $y0 f32) (param $x1 f32) (param $y1 f32) (param $x2 f32) (param $y2 f32)
    (local $devx f32) (local $devy f32) (local $devsq f32) (local $n i32) (local $i i32) (local $t f32)
    (local $px f32) (local $py f32) (local $nx f32) (local $ny f32) (local $ax f32) (local $ay f32) (local $bx f32) (local $by f32)
    (local.set $devx (f32.add (f32.sub (local.get $x0) (f32.mul (f32.const 2) (local.get $x1))) (local.get $x2)))
    (local.set $devy (f32.add (f32.sub (local.get $y0) (f32.mul (f32.const 2) (local.get $y1))) (local.get $y2)))
    (local.set $devsq (f32.add (f32.mul (local.get $devx) (local.get $devx)) (f32.mul (local.get $devy) (local.get $devy))))
    (if (f32.lt (local.get $devsq) (f32.const 0.333))
      (then (call $line (local.get $x0) (local.get $y0) (local.get $x2) (local.get $y2)) (return)))
    (local.set $n (i32.add (i32.const 1)
      (i32.trunc_sat_f32_s (f32.floor (f32.sqrt (f32.sqrt (f32.mul (f32.const 3) (local.get $devsq))))))))
    (local.set $px (local.get $x0))
    (local.set $py (local.get $y0))
    (local.set $i (i32.const 1))
    (block $done
      (loop $step
        (br_if $done (i32.ge_s (local.get $i) (local.get $n)))
        (local.set $t (f32.div (f32.convert_i32_s (local.get $i)) (f32.convert_i32_s (local.get $n))))
        (local.set $ax (f32.add (local.get $x0) (f32.mul (local.get $t) (f32.sub (local.get $x1) (local.get $x0)))))
        (local.set $ay (f32.add (local.get $y0) (f32.mul (local.get $t) (f32.sub (local.get $y1) (local.get $y0)))))
        (local.set $bx (f32.add (local.get $x1) (f32.mul (local.get $t) (f32.sub (local.get $x2) (local.get $x1)))))
        (local.set $by (f32.add (local.get $y1) (f32.mul (local.get $t) (f32.sub (local.get $y2) (local.get $y1)))))
        (local.set $nx (f32.add (local.get $ax) (f32.mul (local.get $t) (f32.sub (local.get $bx) (local.get $ax)))))
        (local.set $ny (f32.add (local.get $ay) (f32.mul (local.get $t) (f32.sub (local.get $by) (local.get $ay)))))
        (call $line (local.get $px) (local.get $py) (local.get $nx) (local.get $ny))
        (local.set $px (local.get $nx))
        (local.set $py (local.get $ny))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $step)))
    (call $line (local.get $px) (local.get $py) (local.get $x2) (local.get $y2)))

  ;; point accessors: PTS holds font-unit x,y (i32) per point, PTSF its flags
  (func $ptx (param $i i32) (result f32)
    (f32.add (f32.mul (f32.convert_i32_s (i32.load (i32.add (global.get $PTS) (i32.shl (local.get $i) (i32.const 3)))))
                      (global.get $sc)) (global.get $tx)))
  (func $pty (param $i i32) (result f32)
    (f32.sub (global.get $ty)
      (f32.mul (f32.convert_i32_s (i32.load offset=4 (i32.add (global.get $PTS) (i32.shl (local.get $i) (i32.const 3)))))
               (global.get $sc))))
  (func $on (param $i i32) (result i32)
    (i32.and (i32.load8_u (i32.add (global.get $PTSF) (local.get $i))) (i32.const 1)))

  ;; $contour draws points s..e as a closed contour, turning off-curve points
  ;; into quadratic segments and inserting the implied on-curve midpoints.
  (func $contour (param $s i32) (param $e i32)
    (local $n i32) (local $k i32) (local $i i32) (local $first i32)
    (local $sx f32) (local $sy f32) (local $cx f32) (local $cy f32) (local $qx f32) (local $qy f32)
    (local $hasq i32) (local $px f32) (local $py f32) (local $synth i32)
    (local.set $n (i32.add (i32.sub (local.get $e) (local.get $s)) (i32.const 1)))
    (if (i32.lt_s (local.get $n) (i32.const 2)) (then (return)))
    ;; start at the first on-curve point; if there is none, at the midpoint
    ;; between the last and first control points
    (local.set $first (i32.const -1))
    (local.set $i (local.get $s))
    (block $found
      (loop $find
        (br_if $found (i32.gt_s (local.get $i) (local.get $e)))
        (if (call $on (local.get $i)) (then (local.set $first (local.get $i)) (br $found)))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $find)))
    (if (i32.ge_s (local.get $first) (i32.const 0))
      (then
        (local.set $sx (call $ptx (local.get $first)))
        (local.set $sy (call $pty (local.get $first))))
      (else
        (local.set $synth (i32.const 1))
        (local.set $first (local.get $e))
        (local.set $sx (f32.mul (f32.const 0.5) (f32.add (call $ptx (local.get $s)) (call $ptx (local.get $e)))))
        (local.set $sy (f32.mul (f32.const 0.5) (f32.add (call $pty (local.get $s)) (call $pty (local.get $e)))))))
    (local.set $cx (local.get $sx))
    (local.set $cy (local.get $sy))
    (local.set $k (i32.const 1))
    (block $done
      (loop $walk
        (br_if $done (i32.gt_s (local.get $k) (local.get $n)))
        ;; point index, cyclic from just after $first
        (local.set $i (i32.add (local.get $s)
          (i32.rem_u (i32.add (i32.sub (local.get $first) (local.get $s)) (local.get $k)) (local.get $n))))
        ;; With a real on-curve start, step n is that start again: close.
        ;; With a synthetic start (no on-curve points), step n is the last
        ;; control point, which still has to be walked before closing.
        (if (i32.and (i32.eq (local.get $k) (local.get $n)) (i32.eqz (local.get $synth)))
          (then
            (call $close (local.get $hasq) (local.get $cx) (local.get $cy) (local.get $qx) (local.get $qy) (local.get $sx) (local.get $sy))
            (br $done)))
        (local.set $px (call $ptx (local.get $i)))
        (local.set $py (call $pty (local.get $i)))
        (if (call $on (local.get $i))
          (then
            (if (local.get $hasq)
              (then (call $quad (local.get $cx) (local.get $cy) (local.get $qx) (local.get $qy) (local.get $px) (local.get $py)))
              (else (call $line (local.get $cx) (local.get $cy) (local.get $px) (local.get $py))))
            (local.set $cx (local.get $px))
            (local.set $cy (local.get $py))
            (local.set $hasq (i32.const 0)))
          (else
            (if (local.get $hasq)
              (then
                (local.set $sx (local.get $sx)) ;; (keep start)
                (call $quad (local.get $cx) (local.get $cy) (local.get $qx) (local.get $qy)
                  (f32.mul (f32.const 0.5) (f32.add (local.get $qx) (local.get $px)))
                  (f32.mul (f32.const 0.5) (f32.add (local.get $qy) (local.get $py))))
                (local.set $cx (f32.mul (f32.const 0.5) (f32.add (local.get $qx) (local.get $px))))
                (local.set $cy (f32.mul (f32.const 0.5) (f32.add (local.get $qy) (local.get $py))))))
            (local.set $qx (local.get $px))
            (local.set $qy (local.get $py))
            (local.set $hasq (i32.const 1))))
        (if (i32.eq (local.get $k) (local.get $n))
          (then
            (call $close (local.get $hasq) (local.get $cx) (local.get $cy) (local.get $qx) (local.get $qy) (local.get $sx) (local.get $sy))
            (br $done)))
        (local.set $k (i32.add (local.get $k) (i32.const 1)))
        (br $walk))))

  (func $close (param $hasq i32) (param $cx f32) (param $cy f32) (param $qx f32) (param $qy f32) (param $sx f32) (param $sy f32)
    (if (local.get $hasq)
      (then (call $quad (local.get $cx) (local.get $cy) (local.get $qx) (local.get $qy) (local.get $sx) (local.get $sy)))
      (else (call $line (local.get $cx) (local.get $cy) (local.get $sx) (local.get $sy)))))

  ;; $outline draws one glyph's outline into the accumulator, offset by
  ;; (dx, dy) font units. Composite glyphs recurse into their components.
  (func $outline (param $slot i32) (param $g i32) (param $dx i32) (param $dy i32) (param $depth i32)
    (local $a i32) (local $nc i32) (local $p i32) (local $npts i32) (local $i i32) (local $f i32)
    (local $rep i32) (local $v i32) (local $s i32) (local $e i32) (local $cf i32) (local $cg i32)
    (local $ax i32) (local $ay i32) (local $txs f32) (local $tys f32)
    (if (i32.gt_s (local.get $depth) (i32.const 4)) (then (return)))
    (local.set $a (call $glyf_addr (local.get $slot) (local.get $g)))
    (if (i32.eqz (global.get $glyf_n)) (then (return)))
    (local.set $nc (call $s16be (local.get $a)))
    (if (i32.lt_s (local.get $nc) (i32.const 0))
      (then
        ;; composite: components with x/y offsets; scales are ignored
        (local.set $p (i32.add (local.get $a) (i32.const 10)))
        (loop $comp
          (local.set $cf (call $u16be (local.get $p)))
          (local.set $cg (call $u16be (i32.add (local.get $p) (i32.const 2))))
          (local.set $p (i32.add (local.get $p) (i32.const 4)))
          (if (i32.and (local.get $cf) (i32.const 1))
            (then
              (local.set $ax (call $s16be (local.get $p)))
              (local.set $ay (call $s16be (i32.add (local.get $p) (i32.const 2))))
              (local.set $p (i32.add (local.get $p) (i32.const 4))))
            (else
              (local.set $ax (i32.load8_s (local.get $p)))
              (local.set $ay (i32.load8_s offset=1 (local.get $p)))
              (local.set $p (i32.add (local.get $p) (i32.const 2)))))
          (if (i32.eqz (i32.and (local.get $cf) (i32.const 2)))
            (then (local.set $ax (i32.const 0)) (local.set $ay (i32.const 0)))) ;; point matching: unsupported
          (if (i32.and (local.get $cf) (i32.const 8)) (then (local.set $p (i32.add (local.get $p) (i32.const 2)))))
          (if (i32.and (local.get $cf) (i32.const 0x40)) (then (local.set $p (i32.add (local.get $p) (i32.const 4)))))
          (if (i32.and (local.get $cf) (i32.const 0x80)) (then (local.set $p (i32.add (local.get $p) (i32.const 8)))))
          (call $outline (local.get $slot) (local.get $cg)
            (i32.add (local.get $dx) (local.get $ax)) (i32.add (local.get $dy) (local.get $ay))
            (i32.add (local.get $depth) (i32.const 1)))
          (br_if $comp (i32.and (local.get $cf) (i32.const 0x20))))
        (return)))
    (if (i32.eqz (local.get $nc)) (then (return)))
    ;; simple glyph
    (local.set $npts (i32.add (call $u16be (i32.add (local.get $a)
      (i32.add (i32.const 10) (i32.shl (i32.sub (local.get $nc) (i32.const 1)) (i32.const 1))))) (i32.const 1)))
    (if (i32.gt_u (local.get $npts) (i32.const 8000)) (then (return)))
    (local.set $p (i32.add (local.get $a) (i32.add (i32.const 10) (i32.shl (local.get $nc) (i32.const 1)))))
    (local.set $p (i32.add (local.get $p) (i32.add (i32.const 2) (call $u16be (local.get $p))))) ;; skip instructions
    ;; flags
    (local.set $i (i32.const 0))
    (block $fdone
      (loop $flags
        (br_if $fdone (i32.ge_u (local.get $i) (local.get $npts)))
        (local.set $f (i32.load8_u (local.get $p)))
        (local.set $p (i32.add (local.get $p) (i32.const 1)))
        (i32.store8 (i32.add (global.get $PTSF) (local.get $i)) (local.get $f))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (if (i32.and (local.get $f) (i32.const 8))
          (then
            (local.set $rep (i32.load8_u (local.get $p)))
            (local.set $p (i32.add (local.get $p) (i32.const 1)))
            (block $rdone
              (loop $r
                (br_if $rdone (i32.or (i32.eqz (local.get $rep)) (i32.ge_u (local.get $i) (local.get $npts))))
                (i32.store8 (i32.add (global.get $PTSF) (local.get $i)) (local.get $f))
                (local.set $i (i32.add (local.get $i) (i32.const 1)))
                (local.set $rep (i32.sub (local.get $rep) (i32.const 1)))
                (br $r)))))
        (br $flags)))
    ;; x coordinates
    (local.set $v (local.get $dx))
    (local.set $i (i32.const 0))
    (block $xdone
      (loop $xs
        (br_if $xdone (i32.ge_u (local.get $i) (local.get $npts)))
        (local.set $f (i32.load8_u (i32.add (global.get $PTSF) (local.get $i))))
        (if (i32.and (local.get $f) (i32.const 2))
          (then
            (if (i32.and (local.get $f) (i32.const 0x10))
              (then (local.set $v (i32.add (local.get $v) (i32.load8_u (local.get $p)))))
              (else (local.set $v (i32.sub (local.get $v) (i32.load8_u (local.get $p))))))
            (local.set $p (i32.add (local.get $p) (i32.const 1))))
          (else
            (if (i32.eqz (i32.and (local.get $f) (i32.const 0x10)))
              (then
                (local.set $v (i32.add (local.get $v) (call $s16be (local.get $p))))
                (local.set $p (i32.add (local.get $p) (i32.const 2)))))))
        (i32.store (i32.add (global.get $PTS) (i32.shl (local.get $i) (i32.const 3))) (local.get $v))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $xs)))
    ;; y coordinates
    (local.set $v (local.get $dy))
    (local.set $i (i32.const 0))
    (block $ydone
      (loop $ys
        (br_if $ydone (i32.ge_u (local.get $i) (local.get $npts)))
        (local.set $f (i32.load8_u (i32.add (global.get $PTSF) (local.get $i))))
        (if (i32.and (local.get $f) (i32.const 4))
          (then
            (if (i32.and (local.get $f) (i32.const 0x20))
              (then (local.set $v (i32.add (local.get $v) (i32.load8_u (local.get $p)))))
              (else (local.set $v (i32.sub (local.get $v) (i32.load8_u (local.get $p))))))
            (local.set $p (i32.add (local.get $p) (i32.const 1))))
          (else
            (if (i32.eqz (i32.and (local.get $f) (i32.const 0x20)))
              (then
                (local.set $v (i32.add (local.get $v) (call $s16be (local.get $p))))
                (local.set $p (i32.add (local.get $p) (i32.const 2)))))))
        (i32.store offset=4 (i32.add (global.get $PTS) (i32.shl (local.get $i) (i32.const 3))) (local.get $v))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $ys)))
    ;; contours
    (local.set $s (i32.const 0))
    (local.set $i (i32.const 0))
    (block $cdone
      (loop $cs
        (br_if $cdone (i32.ge_s (local.get $i) (local.get $nc)))
        (local.set $e (call $u16be (i32.add (local.get $a) (i32.add (i32.const 10) (i32.shl (local.get $i) (i32.const 1))))))
        (call $contour (local.get $s) (local.get $e))
        (local.set $s (i32.add (local.get $e) (i32.const 1)))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $cs))))

  ;; ── the glyph cache ──────────────────────────────────────────────────────
  ;;
  ;; GIDX holds 16384 entries of 32 bytes:
  ;;   0 key+1 (0 = empty)   4 bitmap address   8 width   12 height
  ;;   16 left bearing (px)   20 top (px above baseline)
  ;; Bitmaps are bump-allocated in GBMP. When either fills, everything is
  ;; dropped and re-rendered on demand — a font size change does this anyway.

  (global $gbmp_used (mut i32) (i32.const 0))
  (global $gcount (mut i32) (i32.const 0))

  (func $gcache_clear
    (memory.fill (global.get $GIDX) (i32.const 0) (i32.const 0x80000))
    (global.set $gbmp_used (i32.const 0))
    (global.set $gcount (i32.const 0)))

  ;; $glyph returns the cache entry for a glyph at a pixel size, rendering it
  ;; on first use.
  (func $glyph (param $slot i32) (param $g i32) (param $px i32) (result i32)
    (local $key i32) (local $h i32) (local $e i32) (local $a i32) (local $upem f32)
    (local $xmin i32) (local $ymin i32) (local $xmax i32) (local $ymax i32)
    (local $x0 i32) (local $x1 i32) (local $top i32) (local $bot i32) (local $w i32) (local $ht i32)
    (local $i i32) (local $j i32) (local $acc f32) (local $bm i32) (local $v f32)
    (local.set $key (i32.add (i32.or (i32.or (local.get $g) (i32.shl (local.get $slot) (i32.const 16)))
                                     (i32.shl (local.get $px) (i32.const 19))) (i32.const 1)))
    (local.set $h (i32.and (i32.shr_u (i32.mul (local.get $key) (i32.const 0x9E3779B1)) (i32.const 18)) (i32.const 16383)))
    (block $found
      (loop $probe
        (local.set $e (i32.add (global.get $GIDX) (i32.shl (local.get $h) (i32.const 5))))
        (if (i32.eq (i32.load (local.get $e)) (local.get $key)) (then (return (local.get $e))))
        (br_if $found (i32.eqz (i32.load (local.get $e))))
        (local.set $h (i32.and (i32.add (local.get $h) (i32.const 1)) (i32.const 16383)))
        (br $probe)))
    ;; render it
    (local.set $upem (f32.convert_i32_u (i32.load offset=8 (call $frec (local.get $slot)))))
    (global.set $sc (f32.div (f32.convert_i32_s (local.get $px)) (local.get $upem)))
    (local.set $a (call $glyf_addr (local.get $slot) (local.get $g)))
    (if (i32.lt_u (global.get $glyf_n) (i32.const 10))
      (then
        (i32.store (local.get $e) (local.get $key))
        (i32.store offset=8 (local.get $e) (i32.const 0))
        (i32.store offset=12 (local.get $e) (i32.const 0))
        (global.set $gcount (i32.add (global.get $gcount) (i32.const 1)))
        (return (local.get $e))))
    (local.set $xmin (call $s16be (i32.add (local.get $a) (i32.const 2))))
    (local.set $ymin (call $s16be (i32.add (local.get $a) (i32.const 4))))
    (local.set $xmax (call $s16be (i32.add (local.get $a) (i32.const 6))))
    (local.set $ymax (call $s16be (i32.add (local.get $a) (i32.const 8))))
    (local.set $x0 (i32.trunc_sat_f32_s (f32.floor (f32.mul (f32.convert_i32_s (local.get $xmin)) (global.get $sc)))))
    (local.set $x1 (i32.trunc_sat_f32_s (f32.ceil (f32.mul (f32.convert_i32_s (local.get $xmax)) (global.get $sc)))))
    (local.set $top (i32.trunc_sat_f32_s (f32.ceil (f32.mul (f32.convert_i32_s (local.get $ymax)) (global.get $sc)))))
    (local.set $bot (i32.trunc_sat_f32_s (f32.floor (f32.mul (f32.convert_i32_s (local.get $ymin)) (global.get $sc)))))
    (local.set $w (i32.add (i32.sub (local.get $x1) (local.get $x0)) (i32.const 1)))
    (local.set $ht (i32.sub (local.get $top) (local.get $bot)))
    (if (i32.or (i32.or (i32.le_s (local.get $w) (i32.const 0)) (i32.le_s (local.get $ht) (i32.const 0)))
                (i32.gt_s (i32.mul (i32.add (local.get $w) (i32.const 2)) (i32.add (local.get $ht) (i32.const 1))) (i32.const 196608)))
      (then
        (i32.store (local.get $e) (local.get $key))
        (i32.store offset=8 (local.get $e) (i32.const 0))
        (i32.store offset=12 (local.get $e) (i32.const 0))
        (return (local.get $e))))
    ;; make room
    (if (i32.or (i32.gt_u (i32.add (global.get $gbmp_used) (i32.mul (local.get $w) (local.get $ht)))
                          (i32.sub (global.get $GBMP_END) (global.get $GBMP)))
                (i32.gt_u (global.get $gcount) (i32.const 12000)))
      (then
        (call $gcache_clear)
        (return (call $glyph (local.get $slot) (local.get $g) (local.get $px)))))
    (global.set $aw (i32.add (local.get $w) (i32.const 2)))
    (global.set $ah (i32.add (local.get $ht) (i32.const 1)))
    (memory.fill (global.get $RASTER) (i32.const 0) (i32.shl (i32.mul (global.get $aw) (global.get $ah)) (i32.const 2)))
    (global.set $tx (f32.convert_i32_s (i32.sub (i32.const 0) (local.get $x0))))
    (global.set $ty (f32.convert_i32_s (local.get $top)))
    (call $outline (local.get $slot) (local.get $g) (i32.const 0) (i32.const 0) (i32.const 0))
    ;; accumulate rows into 8-bit coverage
    (local.set $bm (i32.add (global.get $GBMP) (global.get $gbmp_used)))
    (local.set $j (i32.const 0))
    (block $rows_done
      (loop $rows
        (br_if $rows_done (i32.ge_s (local.get $j) (local.get $ht)))
        (local.set $acc (f32.const 0))
        (local.set $i (i32.const 0))
        (block $cols_done
          (loop $cols
            (br_if $cols_done (i32.ge_s (local.get $i) (local.get $w)))
            (local.set $acc (f32.add (local.get $acc)
              (f32.load (i32.add (global.get $RASTER)
                (i32.shl (i32.add (i32.mul (local.get $j) (global.get $aw)) (local.get $i)) (i32.const 2))))))
            (local.set $v (f32.min (f32.abs (local.get $acc)) (f32.const 1)))
            (i32.store8 (i32.add (local.get $bm) (i32.add (i32.mul (local.get $j) (local.get $w)) (local.get $i)))
              (i32.trunc_sat_f32_u (f32.add (f32.mul (local.get $v) (f32.const 255)) (f32.const 0.5))))
            (local.set $i (i32.add (local.get $i) (i32.const 1)))
            (br $cols)))
        (local.set $j (i32.add (local.get $j) (i32.const 1)))
        (br $rows)))
    (global.set $gbmp_used (i32.add (global.get $gbmp_used) (i32.mul (local.get $w) (local.get $ht))))
    (global.set $gcount (i32.add (global.get $gcount) (i32.const 1)))
    (i32.store (local.get $e) (local.get $key))
    (i32.store offset=4 (local.get $e) (local.get $bm))
    (i32.store offset=8 (local.get $e) (local.get $w))
    (i32.store offset=12 (local.get $e) (local.get $ht))
    (i32.store offset=16 (local.get $e) (local.get $x0))
    (i32.store offset=20 (local.get $e) (local.get $top))
    (local.get $e))

  ;; glyph_debug renders one codepoint and exposes its entry — for tests and
  ;; for a host that wants to check its fonts loaded.
  (func (export "glyph_debug") (param $slot i32) (param $cp i32) (param $px i32) (result i32)
    (call $glyph (local.get $slot) (call $glyph_id (local.get $slot) (local.get $cp)) (local.get $px)))
)
