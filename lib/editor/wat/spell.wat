;; farfield editor — spelling.
;;
;; A word list (one word per line) is loaded into DICT as an open-addressed
;; table of FNV-1a hashes of each lowercased word. Words are checked as they
;; are drawn — nothing is stored per byte — and a misspelling gets a wavy
;; underline in the spelling colour (palette 10).
;;
;; What is not checked matters as much as the list. Skipped:
;;   - lines that are not prose (code, fences, HTML, plain mode)
;;   - code, links and Markdown syntax (S_CODE | S_LINK | S_MARK)
;;   - capitalised words (names, acronyms, sentence starts) and 1-letter words
;;   - words touching digits, _ @ / \ # = or a dot inside a name (domains)
;;   - words with non-ASCII letters, and the word being typed at the caret
;;
;; Host API:
;;   dict_load(n)  IO[0..n] is a newline-separated word list
;;   dict_add(n)   IO[0..n] is one word to accept (a personal dictionary)
;;   dict_has(n)   1 if the word in IO[0..n] is known (or spelling is off)
;;   set_spell(on) turn checking on or off (on once a list is loaded)
(module
  (global $spell_on (mut i32) (i32.const 0))
  (global $dict_n (mut i32) (i32.const 0))

  (func $lower (param $c i32) (result i32)
    (select (i32.add (local.get $c) (i32.const 32)) (local.get $c)
      (i32.and (i32.ge_u (local.get $c) (i32.const 65)) (i32.le_u (local.get $c) (i32.const 90)))))

  ;; $whash: FNV-1a of n bytes at p, lowercased; never 0 (0 marks an empty slot)
  (func $whash (param $p i32) (param $n i32) (result i32) (local $h i32) (local $e i32)
    (local.set $h (i32.const 0x811C9DC5))
    (local.set $e (i32.add (local.get $p) (local.get $n)))
    (block $done
      (loop $each
        (br_if $done (i32.ge_u (local.get $p) (local.get $e)))
        (local.set $h (i32.mul (i32.xor (local.get $h) (call $lower (i32.load8_u (local.get $p)))) (i32.const 0x01000193)))
        (local.set $p (i32.add (local.get $p) (i32.const 1)))
        (br $each)))
    (select (i32.const 1) (local.get $h) (i32.eqz (local.get $h))))

  (func $dict_put (param $h i32) (local $s i32) (local $a i32) (local $v i32)
    ;; keep the table under three-quarters full
    (if (i32.ge_u (global.get $dict_n) (i32.shr_u (i32.mul (global.get $DICT_SLOTS) (i32.const 3)) (i32.const 2)))
      (then (return)))
    (local.set $s (i32.and (local.get $h) (i32.sub (global.get $DICT_SLOTS) (i32.const 1))))
    (block $done
      (loop $probe
        (local.set $a (i32.add (global.get $DICT) (i32.shl (local.get $s) (i32.const 2))))
        (local.set $v (i32.load (local.get $a)))
        (if (i32.eqz (local.get $v))
          (then
            (i32.store (local.get $a) (local.get $h))
            (global.set $dict_n (i32.add (global.get $dict_n) (i32.const 1)))
            (br $done)))
        (br_if $done (i32.eq (local.get $v) (local.get $h)))
        (local.set $s (i32.and (i32.add (local.get $s) (i32.const 1)) (i32.sub (global.get $DICT_SLOTS) (i32.const 1))))
        (br $probe))))

  (func $dict_get (param $h i32) (result i32) (local $s i32) (local $v i32)
    (local.set $s (i32.and (local.get $h) (i32.sub (global.get $DICT_SLOTS) (i32.const 1))))
    (loop $probe
      (local.set $v (i32.load (i32.add (global.get $DICT) (i32.shl (local.get $s) (i32.const 2)))))
      (if (i32.eqz (local.get $v)) (then (return (i32.const 0))))
      (if (i32.eq (local.get $v) (local.get $h)) (then (return (i32.const 1))))
      (local.set $s (i32.and (i32.add (local.get $s) (i32.const 1)) (i32.sub (global.get $DICT_SLOTS) (i32.const 1))))
      (br $probe))
    (i32.const 0))

  (func (export "dict_load") (param $n i32) (result i32) (local $i i32) (local $start i32) (local $c i32)
    (local.set $n (call $min (local.get $n) (global.get $IO_CAP)))
    (block $done
      (loop $each
        (local.set $c (if (result i32) (i32.lt_u (local.get $i) (local.get $n))
          (then (i32.load8_u (i32.add (global.get $IO) (local.get $i)))) (else (i32.const 10))))
        (if (i32.or (i32.eq (local.get $c) (i32.const 10)) (i32.eq (local.get $c) (i32.const 13)))
          (then
            (if (i32.gt_u (local.get $i) (local.get $start))
              (then (call $dict_put (call $whash (i32.add (global.get $IO) (local.get $start))
                (i32.sub (local.get $i) (local.get $start))))))
            (local.set $start (i32.add (local.get $i) (i32.const 1)))))
        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $each)))
    (global.set $spell_on (i32.ne (global.get $dict_n) (i32.const 0)))
    (global.set $dirty (i32.const 1))
    (global.get $dict_n))

  (func (export "dict_add") (param $n i32)
    (if (i32.eqz (local.get $n)) (then (return)))
    (call $dict_put (call $whash (global.get $IO) (local.get $n)))
    (global.set $dirty (i32.const 1)))

  (func (export "dict_has") (param $n i32) (result i32)
    (if (i32.eqz (global.get $spell_on)) (then (return (i32.const 1))))
    (call $dict_get (call $whash (global.get $IO) (local.get $n))))

  (func (export "set_spell") (param $on i32)
    (global.set $spell_on (i32.and (i32.ne (local.get $on) (i32.const 0)) (i32.ne (global.get $dict_n) (i32.const 0))))
    (global.set $dirty (i32.const 1)))

  (func $is_letter (param $c i32) (result i32)
    (i32.or (i32.and (i32.ge_u (local.get $c) (i32.const 97)) (i32.le_u (local.get $c) (i32.const 122)))
            (i32.and (i32.ge_u (local.get $c) (i32.const 65)) (i32.le_u (local.get $c) (i32.const 90)))))

  ;; $word_char: letters, the ASCII apostrophe, and any non-ASCII byte (so a
  ;; word with an accent or a curly apostrophe stays one word — and is skipped)
  (func $word_char (param $c i32) (result i32)
    (i32.or (i32.or (call $is_letter (local.get $c)) (i32.eq (local.get $c) (i32.const 39)))
            (i32.ge_u (local.get $c) (i32.const 0x80))))

  ;; $sticky: a neighbour that makes the run part of something else — a
  ;; number, an identifier, a path, an address
  (func $sticky (param $c i32) (result i32)
    (i32.or (i32.or (i32.and (i32.ge_u (local.get $c) (i32.const 48)) (i32.le_u (local.get $c) (i32.const 57)))
                    (i32.or (i32.eq (local.get $c) (i32.const 95)) (i32.eq (local.get $c) (i32.const 64))))
            (i32.or (i32.or (i32.eq (local.get $c) (i32.const 47)) (i32.eq (local.get $c) (i32.const 92)))
                    (i32.or (i32.eq (local.get $c) (i32.const 35)) (i32.eq (local.get $c) (i32.const 61))))))

  ;; $misspelled: is the word run [a, b) of the text a spelling mistake?
  (func $misspelled (param $a i32) (param $b i32) (result i32) (local $p i32) (local $c i32)
    ;; neighbours: identifiers, paths and domains are not words
    (if (i32.gt_s (local.get $a) (i32.const 0))
      (then
        (local.set $c (call $byte (i32.sub (local.get $a) (i32.const 1))))
        (if (call $sticky (local.get $c)) (then (return (i32.const 0))))
        (if (i32.and (i32.eq (local.get $c) (i32.const 46)) (i32.gt_s (local.get $a) (i32.const 1)))
          (then (if (call $is_letter (call $byte (i32.sub (local.get $a) (i32.const 2)))) (then (return (i32.const 0))))))))
    (if (i32.lt_s (local.get $b) (global.get $len))
      (then
        (local.set $c (call $byte (local.get $b)))
        (if (call $sticky (local.get $c)) (then (return (i32.const 0))))
        (if (i32.and (i32.eq (local.get $c) (i32.const 46)) (i32.lt_s (i32.add (local.get $b) (i32.const 1)) (global.get $len)))
          (then (if (call $is_letter (call $byte (i32.add (local.get $b) (i32.const 1)))) (then (return (i32.const 0))))))))
    ;; trim apostrophes, and a possessive 's
    (block $l (loop $t
      (br_if $l (i32.ge_s (local.get $a) (local.get $b)))
      (br_if $l (i32.ne (call $byte (local.get $a)) (i32.const 39)))
      (local.set $a (i32.add (local.get $a) (i32.const 1))) (br $t)))
    (if (i32.and (i32.ge_s (i32.sub (local.get $b) (local.get $a)) (i32.const 3))
          (i32.and (i32.eq (call $byte (i32.sub (local.get $b) (i32.const 2))) (i32.const 39))
                   (i32.eq (call $lower (call $byte (i32.sub (local.get $b) (i32.const 1)))) (i32.const 115))))
      (then (local.set $b (i32.sub (local.get $b) (i32.const 2)))))
    (block $r (loop $t
      (br_if $r (i32.le_s (local.get $b) (local.get $a)))
      (br_if $r (i32.ne (call $byte (i32.sub (local.get $b) (i32.const 1))) (i32.const 39)))
      (local.set $b (i32.sub (local.get $b) (i32.const 1))) (br $t)))
    (if (i32.lt_s (i32.sub (local.get $b) (local.get $a)) (i32.const 2)) (then (return (i32.const 0))))
    ;; capitalised: a name, an acronym or a sentence's first word — let it be
    (local.set $c (call $byte (local.get $a)))
    (if (i32.and (i32.ge_u (local.get $c) (i32.const 65)) (i32.le_u (local.get $c) (i32.const 90))) (then (return (i32.const 0))))
    ;; non-ASCII letters, or syntax / code / links inside the run
    (local.set $p (local.get $a))
    (block $ok
      (loop $scan
        (br_if $ok (i32.ge_s (local.get $p) (local.get $b)))
        (if (i32.ge_u (call $byte (local.get $p)) (i32.const 0x80)) (then (return (i32.const 0))))
        (if (i32.and (call $style_of (local.get $p))
              (i32.or (global.get $S_CODE) (i32.or (global.get $S_LINK) (global.get $S_MARK))))
          (then (return (i32.const 0))))
        (local.set $p (i32.add (local.get $p) (i32.const 1)))
        (br $scan)))
    (i32.eqz (call $dict_get (call $whash (i32.add (global.get $TEXT) (local.get $a)) (i32.sub (local.get $b) (local.get $a))))))

  ;; $draw_spelling underlines misspelled words on visual line l, whose text
  ;; is [p, e), drawn with its baseline at surface y `base`.
  (func $draw_spelling (param $l i32) (param $p i32) (param $e i32) (param $base i32) (param $kind i32)
    (local $a i32) (local $b i32) (local $x0 i32) (local $x1 i32) (local $i i32) (local $ph i32) (local $d i32)
    (local $amp i32) (local $per i32) (local $th i32) (local $y i32) (local $col i32)
    (if (i32.or (i32.eqz (global.get $spell_on)) (global.get $plain)) (then (return)))
    (if (i32.eqz (i32.or (i32.or (i32.eq (local.get $kind) (global.get $K_PARA)) (i32.eq (local.get $kind) (global.get $K_HEAD)))
                         (i32.or (i32.eq (local.get $kind) (global.get $K_QUOTE))
                                 (i32.or (i32.eq (local.get $kind) (global.get $K_BULLET)) (i32.eq (local.get $kind) (global.get $K_ORDER))))))
      (then (return)))
    (local.set $amp (call $max (i32.const 1) (call $dp (i32.const 1))))
    (local.set $per (call $max (i32.const 4) (call $dp (i32.const 4))))
    (local.set $th (call $max (i32.const 1) (call $dp (i32.const 1))))
    ;; below the face's descent line, so the wave clears g, y and p
    (local.set $y (i32.add (local.get $base)
      (i32.sub (call $descent (call $slot_for (i32.const 0) (call $line_info (local.get $l))) (call $line_px (call $line_info (local.get $l))))
               (i32.sub (i32.const 0) (call $dp (i32.const 1))))))
    (local.set $col (call $color (i32.const 10)))
    (block $done
      (loop $words
        ;; next run of word characters
        (block $found
          (loop $skip
            (br_if $done (i32.ge_s (local.get $p) (local.get $e)))
            (br_if $found (call $word_char (call $byte (local.get $p))))
            (local.set $p (i32.add (local.get $p) (i32.const 1)))
            (br $skip)))
        (local.set $a (local.get $p))
        (block $end
          (loop $run
            (br_if $end (i32.ge_s (local.get $p) (local.get $e)))
            (br_if $end (i32.eqz (call $word_char (call $byte (local.get $p)))))
            (local.set $p (i32.add (local.get $p) (i32.const 1)))
            (br $run)))
        (local.set $b (local.get $p))
        ;; leave the word being typed alone
        (if (i32.eqz (i32.and (global.get $focused)
              (i32.and (i32.ge_s (global.get $sel_h) (local.get $a)) (i32.le_s (global.get $sel_h) (local.get $b)))))
          (then
            (if (call $misspelled (local.get $a) (local.get $b))
              (then
                (local.set $x0 (call $x_of (local.get $a)))
                (local.set $x1 (call $x_of (local.get $b)))
                ;; a wave: one pixel column at a time, a triangle of height amp
                (local.set $i (i32.const 0))
                (block $wdone
                  (loop $wave
                    (br_if $wdone (i32.ge_s (local.get $i) (i32.sub (local.get $x1) (local.get $x0))))
                    (local.set $ph (i32.rem_u (local.get $i) (local.get $per)))
                    (local.set $d (select (local.get $ph) (i32.sub (local.get $per) (local.get $ph))
                      (i32.lt_u (local.get $ph) (i32.shr_u (local.get $per) (i32.const 1)))))
                    (call $fill (i32.add (local.get $x0) (local.get $i))
                      (i32.add (local.get $y) (i32.div_u (i32.mul (local.get $d) (i32.shl (local.get $amp) (i32.const 1))) (local.get $per)))
                      (i32.const 1) (local.get $th) (local.get $col))
                    (local.set $i (i32.add (local.get $i) (i32.const 1)))
                    (br $wave)))))))
        (br $words))))
)
