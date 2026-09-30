;; farfield editor — memory map and shared helpers.
;;
;; The whole editor lives in one linear memory laid out in fixed regions. A
;; region is a base address held in an immutable global; there is no general
;; allocator, because every structure has a known worst case and a fixed home
;; is simpler to reason about (and to inspect from a host) than a heap.
;;
;;   STATIC  0x00000000   64 KiB   palette, font records, small scratch
;;   IO      0x00010000    4 MiB   host ⇄ editor byte exchange (text, fonts)
;;   FONTS   0x00410000    2 MiB   TrueType files, copied in by font_load
;;   GMAP    0x00610000  256 KiB   codepoint → glyph id, per font slot
;;   GIDX    0x00650000  512 KiB   glyph cache index (open addressing)
;;   GBMP    0x006D0000    6 MiB   glyph cache bitmaps (8-bit coverage)
;;   RASTER  0x00CD0000    1 MiB   coverage accumulator + outline points
;;   STYLE   0x00DD0000    4 MiB   one style byte per text byte
;;   LINES   0x011D0000    2 MiB   laid-out visual lines, 32 bytes each
;;   UNDO    0x013D0000    4 MiB   edit log for undo / redo
;;   TEXT    0x017D0000    4 MiB   the document, UTF-8, contiguous
;;   FB      0x01BD0000    …       RGBA framebuffer, grown on resize
(module
  (memory (export "memory") 446)

  (global $STATIC i32 (i32.const 0x00000000))
  (global $IO     i32 (i32.const 0x00010000))
  (global $IO_CAP i32 (i32.const 0x00400000))
  (global $FONTS  i32 (i32.const 0x00410000))
  (global $FONTS_CAP i32 (i32.const 0x00200000))
  (global $GMAP   i32 (i32.const 0x00610000))
  (global $GIDX   i32 (i32.const 0x00650000))
  (global $GBMP   i32 (i32.const 0x006D0000))
  (global $GBMP_END i32 (i32.const 0x00CD0000))
  (global $RASTER i32 (i32.const 0x00CD0000))
  (global $PTS    i32 (i32.const 0x00DA0000)) ;; outline points: x,y i32 pairs
  (global $PTSF   i32 (i32.const 0x00DC0000)) ;; outline point flags, 1 byte each
  (global $STYLE  i32 (i32.const 0x00DD0000))
  (global $LINES  i32 (i32.const 0x011D0000))
  (global $LINES_CAP i32 (i32.const 65536))   ;; entries of 32 bytes
  (global $UNDO   i32 (i32.const 0x013D0000))
  (global $UNDO_CAP i32 (i32.const 0x00400000))
  (global $TEXT   i32 (i32.const 0x017D0000))
  (global $TEXT_CAP i32 (i32.const 0x00400000))
  (global $FB     i32 (i32.const 0x01BD0000))

  ;; STATIC sub-regions — every fixed address below 64 KiB, in one place so a
  ;; new one cannot land on an old one:
  ;;   0x0100 PALETTE      0x0200 CARET_OUT (+16 sel_rect)   0x0300 GAMMA
  ;;   0x0400 FONTREC (8 × 64)   0x1000 IMGTAB (64 × 32, images.wat)
  ;;   0x2000 OPENERS (64 × 12, markdown.wat)   0x3000 PLACEHOLDER (1 KiB)
  ;;   0x4000 SCRATCH (edit.wat)   0x8000 PLACE (128 × 20, images.wat)
  (global $PALETTE i32 (i32.const 0x0100)) ;; 16 colours × u32 (RGBA bytes)
  (global $CARET_OUT i32 (i32.const 0x0200)) ;; x, y, w, h (device px) for the host
  (global $GAMMA i32 (i32.const 0x0300))    ;; 256-byte glyph coverage curve
  (global $FONTREC i32 (i32.const 0x0400)) ;; 8 font slots × 64 bytes

  ;; ── small helpers ────────────────────────────────────────────────────────

  (func $min (param $a i32) (param $b i32) (result i32)
    (select (local.get $a) (local.get $b) (i32.lt_s (local.get $a) (local.get $b))))
  (func $max (param $a i32) (param $b i32) (result i32)
    (select (local.get $a) (local.get $b) (i32.gt_s (local.get $a) (local.get $b))))
  (func $clamp (param $v i32) (param $lo i32) (param $hi i32) (result i32)
    (call $max (local.get $lo) (call $min (local.get $v) (local.get $hi))))
  (func $absf (param $v f32) (result f32) (f32.abs (local.get $v)))

  ;; Big-endian reads, for TrueType tables.
  (func $u16be (param $a i32) (result i32)
    (i32.or
      (i32.shl (i32.load8_u (local.get $a)) (i32.const 8))
      (i32.load8_u offset=1 (local.get $a))))
  (func $s16be (param $a i32) (result i32)
    (i32.extend16_s (call $u16be (local.get $a))))
  (func $u32be (param $a i32) (result i32)
    (i32.or
      (i32.shl (call $u16be (local.get $a)) (i32.const 16))
      (call $u16be (i32.add (local.get $a) (i32.const 2)))))

  ;; io_ptr tells the host where to write input and read output.
  (func (export "io_ptr") (result i32) (global.get $IO))
  (func (export "io_cap") (result i32) (global.get $IO_CAP))
)
