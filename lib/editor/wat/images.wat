;; farfield editor — images.
;;
;; A line that is only an image reference — ![alt](url), blob:// or http —
;; shows the image beneath its Markdown, which stays visible (dimmed) as the
;; source of truth. The editor still draws every pixel: the host fetches and
;; decodes an image, scales it to the column in device pixels, and hands over
;; RGBA bytes; this module keeps them, makes room for them in the layout, and
;; blits them into the framebuffer.
;;
;; Host API:
;;   image_alloc(bytes) → ptr      room for w×h×4 RGBA bytes (0: out of memory)
;;   image_put(n, w, h, ptr)       IO[0..n] is the URL exactly as written in
;;                                 the Markdown; the pixels are at ptr
;;
;; Image record (IMGTAB + i*32): 0 url hash  4 url length  8 w  12 h  16 ptr
;; Placement (PLACE + i*20):     0 image   4 x   8 y   12 drawn w   16 drawn h
;; Pixels live on their own heap above the framebuffer's ceiling, so a resize
;; can never grow the framebuffer into them.
(module
  (global $IMGTAB i32 (i32.const 0x1000))  ;; STATIC: 64 records × 32 bytes
  (global $IMG_MAX i32 (i32.const 64))
  (global $PLACE i32 (i32.const 0x8000))   ;; STATIC: 128 placements × 20 bytes
  (global $PLACE_MAX i32 (i32.const 128))
  (global $IMG_HEAP i32 (i32.const 0x05CD0000)) ;; FB + 64 MiB
  (global $img_n (mut i32) (i32.const 0))
  (global $place_n (mut i32) (i32.const 0))
  (global $heap_top (mut i32) (i32.const 0x05CD0000))

  ;; FNV-1a over n bytes at p — images are looked up by the URL's text.
  (func $hash (param $p i32) (param $n i32) (result i32) (local $h i32) (local $e i32)
    (local.set $h (i32.const 0x811C9DC5))
    (local.set $e (i32.add (local.get $p) (local.get $n)))
    (block $done
      (loop $each
        (br_if $done (i32.ge_u (local.get $p) (local.get $e)))
        (local.set $h (i32.mul (i32.xor (local.get $h) (i32.load8_u (local.get $p))) (i32.const 0x01000193)))
        (local.set $p (i32.add (local.get $p) (i32.const 1)))
        (br $each)))
    (local.get $h))

  ;; $image_find: the record for a URL of n bytes at p, or -1.
  (func $image_find (param $p i32) (param $n i32) (result i32) (local $h i32) (local $i i32) (local $r i32)
    (local.set $h (call $hash (local.get $p) (local.get $n)))
    (block $none
      (loop $each
        (br_if $none (i32.ge_s (local.get $i) (global.get $img_n)))
        (local.set $r (i32.add (global.get $IMGTAB) (i32.shl (local.get $i) (i32.const 5))))
        (if (i32.and (i32.eq (i32.load (local.get $r)) (local.get $h))
                     (i32.eq (i32.load offset=4 (local.get $r)) (local.get $n)))
          (then (return (local.get $i))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $each)))
    (i32.const -1))

  (func (export "image_alloc") (param $bytes i32) (result i32) (local $p i32) (local $need i32) (local $have i32)
    (local.set $p (global.get $heap_top))
    (local.set $need (i32.shr_u (i32.add (i32.add (local.get $p) (local.get $bytes)) (i32.const 0xFFFF)) (i32.const 16)))
    (local.set $have (memory.size))
    (if (i32.gt_u (local.get $need) (local.get $have))
      (then (if (i32.lt_s (memory.grow (i32.sub (local.get $need) (local.get $have))) (i32.const 0))
        (then (return (i32.const 0))))))
    (global.set $heap_top (i32.and (i32.add (i32.add (local.get $p) (local.get $bytes)) (i32.const 3)) (i32.const -4)))
    (local.get $p))

  (func (export "image_put") (param $n i32) (param $w i32) (param $h i32) (param $ptr i32) (local $i i32) (local $r i32)
    (if (i32.or (i32.le_s (local.get $w) (i32.const 0)) (i32.le_s (local.get $h) (i32.const 0))) (then (return)))
    (local.set $i (call $image_find (global.get $IO) (local.get $n)))
    (if (i32.lt_s (local.get $i) (i32.const 0))
      (then
        (if (i32.ge_s (global.get $img_n) (global.get $IMG_MAX)) (then (return)))
        (local.set $i (global.get $img_n))
        (global.set $img_n (i32.add (global.get $img_n) (i32.const 1)))))
    (local.set $r (i32.add (global.get $IMGTAB) (i32.shl (local.get $i) (i32.const 5))))
    (i32.store (local.get $r) (call $hash (global.get $IO) (local.get $n)))
    (i32.store offset=4 (local.get $r) (local.get $n))
    (i32.store offset=8 (local.get $r) (local.get $w))
    (i32.store offset=12 (local.get $r) (local.get $h))
    (i32.store offset=16 (local.get $r) (local.get $ptr))
    (global.set $laid_rev (i32.const -1))
    (global.set $dirty (i32.const 1)))

  ;; $image_line: the image record a logical line shows, or -1. The line must
  ;; be exactly ![alt](url), give or take surrounding spaces.
  (func $image_line (param $ls i32) (param $le i32) (result i32) (local $a i32) (local $b i32) (local $p i32)
    (if (global.get $plain) (then (return (i32.const -1))))
    (local.set $a (i32.add (global.get $TEXT) (local.get $ls)))
    (local.set $b (i32.add (global.get $TEXT) (local.get $le)))
    (block $l (loop $trim
      (br_if $l (i32.ge_u (local.get $a) (local.get $b)))
      (br_if $l (i32.ne (i32.load8_u (local.get $a)) (i32.const 32)))
      (local.set $a (i32.add (local.get $a) (i32.const 1))) (br $trim)))
    (block $r (loop $trim
      (br_if $r (i32.le_u (local.get $b) (local.get $a)))
      (br_if $r (i32.ne (i32.load8_u (i32.sub (local.get $b) (i32.const 1))) (i32.const 32)))
      (local.set $b (i32.sub (local.get $b) (i32.const 1))) (br $trim)))
    (if (i32.lt_u (i32.sub (local.get $b) (local.get $a)) (i32.const 6)) (then (return (i32.const -1))))
    (if (i32.or (i32.ne (i32.load8_u (local.get $a)) (i32.const 33))                        ;; !
                (i32.ne (i32.load8_u offset=1 (local.get $a)) (i32.const 91)))             ;; [
      (then (return (i32.const -1))))
    (if (i32.ne (i32.load8_u (i32.sub (local.get $b) (i32.const 1))) (i32.const 41))       ;; )
      (then (return (i32.const -1))))
    ;; the first "](" closes the alt text
    (local.set $p (i32.add (local.get $a) (i32.const 2)))
    (block $found
      (loop $scan
        (if (i32.ge_u (i32.add (local.get $p) (i32.const 1)) (local.get $b)) (then (return (i32.const -1))))
        (br_if $found (i32.and (i32.eq (i32.load8_u (local.get $p)) (i32.const 93))
                               (i32.eq (i32.load8_u offset=1 (local.get $p)) (i32.const 40))))
        (local.set $p (i32.add (local.get $p) (i32.const 1)))
        (br $scan)))
    (local.set $p (i32.add (local.get $p) (i32.const 2)))
    (call $image_find (local.get $p) (i32.sub (i32.sub (local.get $b) (i32.const 1)) (local.get $p))))

  ;; $place_image makes room below logical line ls..le for its image, if it
  ;; has one loaded, and returns the y where the next line starts.
  (func $place_image (param $ls i32) (param $le i32) (param $y i32) (result i32) (local $i i32)
    (local.set $i (call $image_line (local.get $ls) (local.get $le)))
    (if (i32.lt_s (local.get $i) (i32.const 0)) (then (return (local.get $y))))
    (call $image_place (local.get $i) (local.get $y)))

  ;; $image_place places image i at y (with air above and below) and returns
  ;; the y below it.
  (func $image_place (param $i i32) (param $y i32) (result i32)
    (local $r i32) (local $w i32) (local $h i32) (local $dw i32) (local $dh i32) (local $gap i32) (local $pl i32)
    (if (i32.ge_s (global.get $place_n) (global.get $PLACE_MAX)) (then (return (local.get $y))))
    (local.set $r (i32.add (global.get $IMGTAB) (i32.shl (local.get $i) (i32.const 5))))
    (local.set $w (i32.load offset=8 (local.get $r)))
    (local.set $h (i32.load offset=12 (local.get $r)))
    (local.set $dw (call $min (local.get $w) (global.get $col_w)))
    (local.set $dh (i32.div_s (i32.mul (local.get $h) (local.get $dw)) (local.get $w)))
    (local.set $gap (call $dp (i32.const 10)))
    (local.set $pl (i32.add (global.get $PLACE) (i32.mul (global.get $place_n) (i32.const 20))))
    (i32.store (local.get $pl) (local.get $i))
    (i32.store offset=4 (local.get $pl) (global.get $col_x))
    (i32.store offset=8 (local.get $pl) (i32.add (local.get $y) (local.get $gap)))
    (i32.store offset=12 (local.get $pl) (local.get $dw))
    (i32.store offset=16 (local.get $pl) (local.get $dh))
    (global.set $place_n (i32.add (global.get $place_n) (i32.const 1)))
    (i32.add (local.get $y) (i32.add (local.get $dh) (i32.mul (local.get $gap) (i32.const 2)))))

  ;; $draw_images blits every placed image that meets the surface. Pixels are
  ;; stored at the drawn size, so this is a copy; a narrower column than the
  ;; host scaled for falls back to nearest-neighbour sampling.
  (func $draw_images (local $k i32) (local $pl i32) (local $r i32) (local $w i32) (local $h i32) (local $src i32)
    (local $x0 i32) (local $top i32) (local $dw i32) (local $dh i32) (local $j i32) (local $i i32)
    (local $sy i32) (local $sx i32) (local $row i32) (local $c i32) (local $a i32) (local $dst i32)
    (block $done
      (loop $each
        (br_if $done (i32.ge_s (local.get $k) (global.get $place_n)))
        (local.set $pl (i32.add (global.get $PLACE) (i32.mul (local.get $k) (i32.const 20))))
        (local.set $r (i32.add (global.get $IMGTAB) (i32.shl (i32.load (local.get $pl)) (i32.const 5))))
        (local.set $w (i32.load offset=8 (local.get $r)))
        (local.set $h (i32.load offset=12 (local.get $r)))
        (local.set $src (i32.load offset=16 (local.get $r)))
        (local.set $x0 (i32.load offset=4 (local.get $pl)))
        (local.set $top (i32.sub (i32.load offset=8 (local.get $pl)) (global.get $scroll)))
        (local.set $dw (i32.load offset=12 (local.get $pl)))
        (local.set $dh (i32.load offset=16 (local.get $pl)))
        (if (i32.and (i32.lt_s (local.get $top) (global.get $H))
                     (i32.gt_s (i32.add (local.get $top) (local.get $dh)) (i32.const 0)))
          (then
            (local.set $j (call $max (i32.const 0) (i32.sub (global.get $clip_y0) (local.get $top))))
            (block $rows_done
              (loop $rows
                (br_if $rows_done (i32.ge_s (local.get $j) (local.get $dh)))
                (local.set $sy (i32.add (local.get $top) (local.get $j)))
                (br_if $rows_done (i32.ge_s (local.get $sy) (call $min (global.get $H) (global.get $clip_y1))))
                (local.set $row (i32.mul (i32.div_s (i32.mul (local.get $j) (local.get $h)) (local.get $dh)) (local.get $w)))
                (local.set $i (i32.const 0))
                (block $cols_done
                  (loop $cols
                    (br_if $cols_done (i32.ge_s (local.get $i) (local.get $dw)))
                    (local.set $sx (i32.add (local.get $x0) (local.get $i)))
                    (if (i32.and (i32.ge_s (local.get $sx) (i32.const 0)) (i32.lt_s (local.get $sx) (global.get $W)))
                      (then
                        (local.set $c (i32.load (i32.add (local.get $src)
                          (i32.shl (i32.add (local.get $row)
                            (i32.div_s (i32.mul (local.get $i) (local.get $w)) (local.get $dw))) (i32.const 2)))))
                        (local.set $a (i32.shr_u (local.get $c) (i32.const 24)))
                        (local.set $dst (i32.add (global.get $FB)
                          (i32.shl (i32.add (i32.mul (local.get $sy) (global.get $W)) (local.get $sx)) (i32.const 2))))
                        (if (i32.eq (local.get $a) (i32.const 255))
                          (then (i32.store (local.get $dst) (local.get $c)))
                          (else (if (local.get $a)
                            (then (i32.store (local.get $dst) (call $mix (i32.load (local.get $dst)) (local.get $c) (local.get $a)))))))))
                    (local.set $i (i32.add (local.get $i) (i32.const 1)))
                    (br $cols)))
                (local.set $j (i32.add (local.get $j) (i32.const 1)))
                (br $rows)))))
        (local.set $k (i32.add (local.get $k) (i32.const 1)))
        (br $each))))
)
