;; farfield editor — the document buffer.
;;
;; The text is UTF-8 in one contiguous run at TEXT[0 .. len). An edit moves the
;; tail with memory.copy, which is a memmove: for documents this size (an
;; essay is tens of kilobytes) that is faster than anything clever, and every
;; reader — layout, render, search — gets the text as a plain array with no
;; gap to step around.
;;
;; Positions everywhere are byte offsets into this array, always on a UTF-8
;; character boundary.
(module
  (global $len (mut i32) (i32.const 0))
  ;; revision increments on every change, so a host can tell whether the text
  ;; moved since it last looked without copying it out.
  (global $revision (mut i32) (i32.const 0))

  (func $text_len (export "text_len") (result i32) (global.get $len))
  (func (export "revision") (result i32) (global.get $revision))

  (func $byte (param $p i32) (result i32)
    (i32.load8_u (i32.add (global.get $TEXT) (local.get $p))))

  ;; $raw_insert puts n bytes from src at p. Returns 0 when the document is
  ;; full, leaving it untouched.
  (func $raw_insert (param $p i32) (param $src i32) (param $n i32) (result i32)
    (if (i32.gt_u (i32.add (global.get $len) (local.get $n)) (global.get $TEXT_CAP))
      (then (return (i32.const 0))))
    (memory.copy
      (i32.add (global.get $TEXT) (i32.add (local.get $p) (local.get $n)))
      (i32.add (global.get $TEXT) (local.get $p))
      (i32.sub (global.get $len) (local.get $p)))
    (memory.copy (i32.add (global.get $TEXT) (local.get $p)) (local.get $src) (local.get $n))
    (global.set $len (i32.add (global.get $len) (local.get $n)))
    (global.set $revision (i32.add (global.get $revision) (i32.const 1)))
    (i32.const 1))

  ;; $raw_delete removes n bytes at p.
  (func $raw_delete (param $p i32) (param $n i32)
    (memory.copy
      (i32.add (global.get $TEXT) (local.get $p))
      (i32.add (global.get $TEXT) (i32.add (local.get $p) (local.get $n)))
      (i32.sub (global.get $len) (i32.add (local.get $p) (local.get $n))))
    (global.set $len (i32.sub (global.get $len) (local.get $n)))
    (global.set $revision (i32.add (global.get $revision) (i32.const 1))))

  ;; ── UTF-8 ────────────────────────────────────────────────────────────────

  ;; $dec_len holds the byte length of the character $decode just read.
  (global $dec_len (mut i32) (i32.const 1))

  ;; $decode reads the codepoint at address a (not a text position — any
  ;; memory). Malformed input decodes as U+FFFD one byte at a time, so a bad
  ;; byte can never stall a loop.
  (func $decode (param $a i32) (result i32) (local $b i32)
    (local.set $b (i32.load8_u (local.get $a)))
    (if (i32.lt_u (local.get $b) (i32.const 0x80))
      (then (global.set $dec_len (i32.const 1)) (return (local.get $b))))
    (if (i32.eq (i32.and (local.get $b) (i32.const 0xE0)) (i32.const 0xC0))
      (then
        (global.set $dec_len (i32.const 2))
        (return (i32.or
          (i32.shl (i32.and (local.get $b) (i32.const 0x1F)) (i32.const 6))
          (i32.and (i32.load8_u offset=1 (local.get $a)) (i32.const 0x3F))))))
    (if (i32.eq (i32.and (local.get $b) (i32.const 0xF0)) (i32.const 0xE0))
      (then
        (global.set $dec_len (i32.const 3))
        (return (i32.or (i32.or
          (i32.shl (i32.and (local.get $b) (i32.const 0x0F)) (i32.const 12))
          (i32.shl (i32.and (i32.load8_u offset=1 (local.get $a)) (i32.const 0x3F)) (i32.const 6)))
          (i32.and (i32.load8_u offset=2 (local.get $a)) (i32.const 0x3F))))))
    (if (i32.eq (i32.and (local.get $b) (i32.const 0xF8)) (i32.const 0xF0))
      (then
        (global.set $dec_len (i32.const 4))
        (return (i32.or (i32.or (i32.or
          (i32.shl (i32.and (local.get $b) (i32.const 0x07)) (i32.const 18))
          (i32.shl (i32.and (i32.load8_u offset=1 (local.get $a)) (i32.const 0x3F)) (i32.const 12)))
          (i32.shl (i32.and (i32.load8_u offset=2 (local.get $a)) (i32.const 0x3F)) (i32.const 6)))
          (i32.and (i32.load8_u offset=3 (local.get $a)) (i32.const 0x3F))))))
    (global.set $dec_len (i32.const 1))
    (i32.const 0xFFFD))

  ;; $cp_at decodes the character at text position p.
  (func $cp_at (param $p i32) (result i32)
    (call $decode (i32.add (global.get $TEXT) (local.get $p))))

  ;; $next moves one character forward, $prev one back; both stop at the ends.
  (func $next (param $p i32) (result i32)
    (if (i32.ge_u (local.get $p) (global.get $len)) (then (return (global.get $len))))
    (drop (call $cp_at (local.get $p)))
    (call $min (i32.add (local.get $p) (global.get $dec_len)) (global.get $len)))

  (func $prev (param $p i32) (result i32)
    (if (i32.le_s (local.get $p) (i32.const 0)) (then (return (i32.const 0))))
    (local.set $p (i32.sub (local.get $p) (i32.const 1)))
    (block $done
      (loop $back
        (br_if $done (i32.le_s (local.get $p) (i32.const 0)))
        ;; continuation bytes are 10xxxxxx
        (br_if $done (i32.ne (i32.and (call $byte (local.get $p)) (i32.const 0xC0)) (i32.const 0x80)))
        (local.set $p (i32.sub (local.get $p) (i32.const 1)))
        (br $back)))
    (local.get $p))

  ;; $is_word says whether a codepoint belongs to a word, for word motion,
  ;; double-click and word count: letters, digits, underscore, apostrophes
  ;; inside words, and anything non-ASCII that is not punctuation or space.
  (func $is_word (param $c i32) (result i32)
    (if (i32.lt_u (local.get $c) (i32.const 0x80))
      (then (return (i32.or (i32.or (i32.or
        (i32.and (i32.ge_u (local.get $c) (i32.const 0x30)) (i32.le_u (local.get $c) (i32.const 0x39)))
        (i32.and (i32.ge_u (i32.or (local.get $c) (i32.const 0x20)) (i32.const 0x61))
                 (i32.le_u (i32.or (local.get $c) (i32.const 0x20)) (i32.const 0x7A))))
        (i32.eq (local.get $c) (i32.const 0x5F)))
        (i32.eq (local.get $c) (i32.const 0x27))))))
    ;; U+2019 right single quote is an apostrophe inside words
    (if (i32.eq (local.get $c) (i32.const 0x2019)) (then (return (i32.const 1))))
    ;; Latin-1 punctuation and the General Punctuation block are not words
    (if (i32.and (i32.ge_u (local.get $c) (i32.const 0xA0)) (i32.le_u (local.get $c) (i32.const 0xBF)))
      (then (return (i32.const 0))))
    (if (i32.and (i32.ge_u (local.get $c) (i32.const 0x2000)) (i32.le_u (local.get $c) (i32.const 0x206F)))
      (then (return (i32.const 0))))
    (i32.ne (local.get $c) (i32.const 0x3000)))

  (func $is_space (param $c i32) (result i32)
    (i32.or (i32.or (i32.eq (local.get $c) (i32.const 0x20)) (i32.eq (local.get $c) (i32.const 0x09)))
            (i32.or (i32.eq (local.get $c) (i32.const 0xA0)) (i32.eq (local.get $c) (i32.const 0x3000)))))

  ;; ── lines ────────────────────────────────────────────────────────────────

  ;; $line_start is the position just after the previous newline.
  (func $line_start (param $p i32) (result i32)
    (block $done
      (loop $back
        (br_if $done (i32.le_s (local.get $p) (i32.const 0)))
        (br_if $done (i32.eq (call $byte (i32.sub (local.get $p) (i32.const 1))) (i32.const 0x0A)))
        (local.set $p (i32.sub (local.get $p) (i32.const 1)))
        (br $back)))
    (local.get $p))

  ;; $line_end is the position of the next newline (or the end of the text).
  (func $line_end (param $p i32) (result i32)
    (block $done
      (loop $fwd
        (br_if $done (i32.ge_u (local.get $p) (global.get $len)))
        (br_if $done (i32.eq (call $byte (local.get $p)) (i32.const 0x0A)))
        (local.set $p (i32.add (local.get $p) (i32.const 1)))
        (br $fwd)))
    (local.get $p))

  ;; ── words ────────────────────────────────────────────────────────────────

  ;; $word_left and $word_right move the way ⌥← / ⌥→ do: skip spaces and
  ;; punctuation, then the run of word characters.
  (func $word_left (param $p i32) (result i32) (local $q i32)
    (block $skipped
      (loop $skip
        (br_if $skipped (i32.le_s (local.get $p) (i32.const 0)))
        (local.set $q (call $prev (local.get $p)))
        (br_if $skipped (call $is_word (call $cp_at (local.get $q))))
        (local.set $p (local.get $q))
        (br $skip)))
    (block $done
      (loop $run
        (br_if $done (i32.le_s (local.get $p) (i32.const 0)))
        (local.set $q (call $prev (local.get $p)))
        (br_if $done (i32.eqz (call $is_word (call $cp_at (local.get $q)))))
        (local.set $p (local.get $q))
        (br $run)))
    (local.get $p))

  (func $word_right (param $p i32) (result i32)
    (block $skipped
      (loop $skip
        (br_if $skipped (i32.ge_u (local.get $p) (global.get $len)))
        (br_if $skipped (call $is_word (call $cp_at (local.get $p))))
        (local.set $p (call $next (local.get $p)))
        (br $skip)))
    (block $done
      (loop $run
        (br_if $done (i32.ge_u (local.get $p) (global.get $len)))
        (br_if $done (i32.eqz (call $is_word (call $cp_at (local.get $p)))))
        (local.set $p (call $next (local.get $p)))
        (br $run)))
    (local.get $p))

  ;; word_count counts runs of word characters — what an editor's status line
  ;; means by "words".
  (func (export "word_count") (result i32) (local $p i32) (local $n i32) (local $in i32) (local $w i32)
    (block $done
      (loop $scan
        (br_if $done (i32.ge_u (local.get $p) (global.get $len)))
        (local.set $w (call $is_word (call $cp_at (local.get $p))))
        (if (i32.and (local.get $w) (i32.eqz (local.get $in)))
          (then (local.set $n (i32.add (local.get $n) (i32.const 1)))))
        (local.set $in (local.get $w))
        (local.set $p (i32.add (local.get $p) (global.get $dec_len)))
        (br $scan)))
    (local.get $n))
)
