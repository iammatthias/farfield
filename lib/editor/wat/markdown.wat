;; farfield editor — Markdown, read as you type.
;;
;; The editor never hides the source: every character you typed stays on
;; screen, and Markdown shapes how it looks. A heading is set large and bold,
;; **strong** is bold, `code` is monospaced on a tint, a link reads in the
;; accent colour — and the syntax that did it (#, **, `, the (url)) is still
;; there, dimmed. Nothing to toggle, nothing that round-trips through a
;; different model of the document: what you see is exactly the file.
;;
;; Line kinds (the block structure) come from $classify; inline styles are
;; written into STYLE, one byte per text byte:
;;   bit 0 bold   bit 1 italic   bit 2 code   bit 3 link
;;   bit 4 marker (dimmed syntax)   bit 5 strike   bit 6 heading   bit 7 accent
(module
  ;; line kinds
  (global $K_PARA  i32 (i32.const 0))
  (global $K_HEAD  i32 (i32.const 1))
  (global $K_QUOTE i32 (i32.const 2))
  (global $K_BULLET i32 (i32.const 3))
  (global $K_ORDER i32 (i32.const 4))
  (global $K_FENCE i32 (i32.const 5)) ;; a ``` line itself
  (global $K_CODE  i32 (i32.const 6)) ;; a line inside a fence
  (global $K_RULE  i32 (i32.const 7))
  (global $K_BLANK i32 (i32.const 8))
  (global $K_PLAIN i32 (i32.const 9)) ;; plain-text mode: mono, no Markdown

  ;; $plain switches Markdown off: every line is plain monospaced text. For
  ;; pastes and code, where # and * are content, not syntax.
  (global $plain (mut i32) (i32.const 0))
  (func (export "set_mode") (param $plain i32)
    (global.set $plain (i32.ne (local.get $plain) (i32.const 0)))
    (global.set $laid_rev (i32.const -1))
    (global.set $dirty (i32.const 1)))

  ;; style bits
  (global $S_BOLD i32 (i32.const 1))
  (global $S_ITAL i32 (i32.const 2))
  (global $S_CODE i32 (i32.const 4))
  (global $S_LINK i32 (i32.const 8))
  (global $S_MARK i32 (i32.const 16))
  (global $S_STRIKE i32 (i32.const 32))
  (global $S_HEAD i32 (i32.const 64))
  (global $S_ACCENT i32 (i32.const 128))

  ;; fence state carried from line to line during a layout pass
  (global $in_fence (mut i32) (i32.const 0))
  ;; $prefix_len: bytes of block syntax at the start of the classified line
  (global $prefix_len (mut i32) (i32.const 0))
  ;; $indent_len: leading spaces before a list marker
  (global $indent_len (mut i32) (i32.const 0))

  (func $style_set (param $a i32) (param $b i32) (param $bits i32) (local $p i32)
    (local.set $p (local.get $a))
    (block $done
      (loop $each
        (br_if $done (i32.ge_s (local.get $p) (local.get $b)))
        (i32.store8 (i32.add (global.get $STYLE) (local.get $p))
          (i32.or (i32.load8_u (i32.add (global.get $STYLE) (local.get $p))) (local.get $bits)))
        (local.set $p (i32.add (local.get $p) (i32.const 1)))
        (br $each))))

  (func $style_of (param $p i32) (result i32)
    (i32.load8_u (i32.add (global.get $STYLE) (local.get $p))))

  ;; $run counts repeats of byte c starting at p, stopping at e.
  (func $run (param $p i32) (param $e i32) (param $c i32) (result i32) (local $n i32)
    (block $done
      (loop $count
        (br_if $done (i32.ge_s (i32.add (local.get $p) (local.get $n)) (local.get $e)))
        (br_if $done (i32.ne (call $byte (i32.add (local.get $p) (local.get $n))) (local.get $c)))
        (local.set $n (i32.add (local.get $n) (i32.const 1)))
        (br $count)))
    (local.get $n))

  ;; $classify reads the line [s, e) and returns kind | level<<8. It sets
  ;; $prefix_len to the block syntax length and advances the fence state.
  (func $classify (param $s i32) (param $e i32) (result i32)
    (local $p i32) (local $c i32) (local $n i32) (local $q i32)
    (global.set $prefix_len (i32.const 0))
    (global.set $indent_len (i32.const 0))
    (if (global.get $plain) (then (return (global.get $K_PLAIN))))
    ;; fences: ``` or ~~~ (three or more) toggle code
    (local.set $p (local.get $s))
    (block $ws (loop $lead
      (br_if $ws (i32.ge_s (local.get $p) (local.get $e)))
      (br_if $ws (i32.ne (call $byte (local.get $p)) (i32.const 0x20)))
      (local.set $p (i32.add (local.get $p) (i32.const 1)))
      (br $lead)))
    (global.set $indent_len (i32.sub (local.get $p) (local.get $s)))
    (local.set $c (if (result i32) (i32.lt_s (local.get $p) (local.get $e))
      (then (call $byte (local.get $p))) (else (i32.const 0))))
    (if (i32.or (i32.eq (local.get $c) (i32.const 0x60)) (i32.eq (local.get $c) (i32.const 0x7E)))
      (then
        (if (i32.ge_s (call $run (local.get $p) (local.get $e) (local.get $c)) (i32.const 3))
          (then
            (global.set $in_fence (i32.eqz (global.get $in_fence)))
            (global.set $prefix_len (i32.sub (local.get $e) (local.get $s)))
            (return (global.get $K_FENCE))))))
    (if (global.get $in_fence) (then (return (global.get $K_CODE))))
    (if (i32.ge_s (local.get $p) (local.get $e)) (then (return (global.get $K_BLANK))))
    ;; ATX heading: 1–6 '#' then a space (or end of line)
    (if (i32.eq (local.get $c) (i32.const 0x23))
      (then
        (local.set $n (call $run (local.get $p) (local.get $e) (i32.const 0x23)))
        (local.set $q (i32.add (local.get $p) (local.get $n)))
        (if (i32.and (i32.le_s (local.get $n) (i32.const 6))
              (i32.or (i32.ge_s (local.get $q) (local.get $e)) (i32.eq (call $byte (local.get $q)) (i32.const 0x20))))
          (then
            (global.set $prefix_len (i32.sub (call $min (i32.add (local.get $q) (i32.const 1)) (local.get $e)) (local.get $s)))
            (return (i32.or (global.get $K_HEAD) (i32.shl (local.get $n) (i32.const 8))))))))
    ;; blockquote
    (if (i32.eq (local.get $c) (i32.const 0x3E))
      (then
        (local.set $q (i32.add (local.get $p) (i32.const 1)))
        (if (i32.and (i32.lt_s (local.get $q) (local.get $e)) (i32.eq (call $byte (local.get $q)) (i32.const 0x20)))
          (then (local.set $q (i32.add (local.get $q) (i32.const 1)))))
        (global.set $prefix_len (i32.sub (local.get $q) (local.get $s)))
        (return (global.get $K_QUOTE))))
    ;; thematic break: three or more of - * _ and nothing else but spaces
    (if (i32.or (i32.or (i32.eq (local.get $c) (i32.const 0x2D)) (i32.eq (local.get $c) (i32.const 0x2A)))
                (i32.eq (local.get $c) (i32.const 0x5F)))
      (then
        (if (call $is_rule (local.get $p) (local.get $e) (local.get $c))
          (then
            (global.set $prefix_len (i32.sub (local.get $e) (local.get $s)))
            (return (global.get $K_RULE))))))
    ;; bullet: - * + then a space
    (if (i32.or (i32.or (i32.eq (local.get $c) (i32.const 0x2D)) (i32.eq (local.get $c) (i32.const 0x2A)))
                (i32.eq (local.get $c) (i32.const 0x2B)))
      (then
        (local.set $q (i32.add (local.get $p) (i32.const 1)))
        (if (i32.and (i32.lt_s (local.get $q) (local.get $e)) (i32.eq (call $byte (local.get $q)) (i32.const 0x20)))
          (then
            (global.set $prefix_len (i32.sub (i32.add (local.get $q) (i32.const 1)) (local.get $s)))
            (return (global.get $K_BULLET))))))
    ;; ordered: digits then . or ) then a space
    (if (i32.and (i32.ge_u (local.get $c) (i32.const 0x30)) (i32.le_u (local.get $c) (i32.const 0x39)))
      (then
        (local.set $q (local.get $p))
        (block $digits (loop $d
          (br_if $digits (i32.ge_s (local.get $q) (local.get $e)))
          (local.set $n (call $byte (local.get $q)))
          (br_if $digits (i32.or (i32.lt_u (local.get $n) (i32.const 0x30)) (i32.gt_u (local.get $n) (i32.const 0x39))))
          (local.set $q (i32.add (local.get $q) (i32.const 1)))
          (br $d)))
        (if (i32.lt_s (i32.add (local.get $q) (i32.const 1)) (local.get $e))
          (then
            (local.set $n (call $byte (local.get $q)))
            (if (i32.and (i32.or (i32.eq (local.get $n) (i32.const 0x2E)) (i32.eq (local.get $n) (i32.const 0x29)))
                         (i32.eq (call $byte (i32.add (local.get $q) (i32.const 1))) (i32.const 0x20)))
              (then
                (global.set $prefix_len (i32.sub (i32.add (local.get $q) (i32.const 2)) (local.get $s)))
                (return (global.get $K_ORDER))))))))
    (global.get $K_PARA))

  (func $is_rule (param $p i32) (param $e i32) (param $c i32) (result i32) (local $n i32) (local $b i32)
    (block $done
      (loop $scan
        (br_if $done (i32.ge_s (local.get $p) (local.get $e)))
        (local.set $b (call $byte (local.get $p)))
        (if (i32.eq (local.get $b) (local.get $c))
          (then (local.set $n (i32.add (local.get $n) (i32.const 1))))
          (else (if (i32.ne (local.get $b) (i32.const 0x20)) (then (return (i32.const 0))))))
        (local.set $p (i32.add (local.get $p) (i32.const 1)))
        (br $scan)))
    (i32.ge_s (local.get $n) (i32.const 3)))

  ;; $style_line writes STYLE for the line [s, e) of the given kind.
  (func $style_line (param $s i32) (param $e i32) (param $info i32) (local $kind i32) (local $ps i32)
    (local.set $kind (i32.and (local.get $info) (i32.const 0xFF)))
    (memory.fill (i32.add (global.get $STYLE) (local.get $s)) (i32.const 0) (i32.sub (local.get $e) (local.get $s)))
    (local.set $ps (i32.add (local.get $s) (global.get $prefix_len)))
    (if (i32.eq (local.get $kind) (global.get $K_FENCE))
      (then (call $style_set (local.get $s) (local.get $e) (i32.or (global.get $S_CODE) (global.get $S_MARK))) (return)))
    (if (i32.or (i32.eq (local.get $kind) (global.get $K_CODE)) (i32.eq (local.get $kind) (global.get $K_PLAIN)))
      (then (call $style_set (local.get $s) (local.get $e) (global.get $S_CODE)) (return)))
    (if (i32.eq (local.get $kind) (global.get $K_RULE))
      (then (call $style_set (local.get $s) (local.get $e) (global.get $S_MARK)) (return)))
    (if (i32.eq (local.get $kind) (global.get $K_HEAD))
      (then
        (call $style_set (local.get $s) (local.get $ps) (global.get $S_MARK))
        (call $style_set (local.get $ps) (local.get $e) (global.get $S_HEAD))))
    (if (i32.eq (local.get $kind) (global.get $K_QUOTE))
      (then (call $style_set (local.get $s) (local.get $ps) (i32.or (global.get $S_MARK) (global.get $S_ACCENT)))
            (call $style_set (local.get $ps) (local.get $e) (global.get $S_ITAL))))
    (if (i32.or (i32.eq (local.get $kind) (global.get $K_BULLET)) (i32.eq (local.get $kind) (global.get $K_ORDER)))
      (then (call $style_set (local.get $s) (local.get $ps) (global.get $S_ACCENT))))
    (call $inline (local.get $ps) (local.get $e)))

  ;; ── inline spans ─────────────────────────────────────────────────────────
  ;;
  ;; Order matters, as it does in CommonMark: code spans first (nothing is
  ;; parsed inside them), then links, then emphasis — which skips anything
  ;; already claimed as code or as a link's destination.

  (global $OPENERS i32 (i32.const 0x2000)) ;; emphasis opener stack: 64 × (pos, char, len)
  (global $nopen (mut i32) (i32.const 0))

  (func $inline (param $s i32) (param $e i32)
    (call $code_spans (local.get $s) (local.get $e))
    (call $links (local.get $s) (local.get $e))
    (call $autolinks (local.get $s) (local.get $e))
    (call $emphasis (local.get $s) (local.get $e)))

  ;; `code` — a run of n backticks closed by the next run of exactly n.
  (func $code_spans (param $s i32) (param $e i32) (local $p i32) (local $n i32) (local $q i32) (local $m i32)
    (local.set $p (local.get $s))
    (block $done
      (loop $scan
        (br_if $done (i32.ge_s (local.get $p) (local.get $e)))
        (if (i32.ne (call $byte (local.get $p)) (i32.const 0x60))
          (then (local.set $p (i32.add (local.get $p) (i32.const 1))) (br $scan)))
        (local.set $n (call $run (local.get $p) (local.get $e) (i32.const 0x60)))
        (local.set $q (i32.add (local.get $p) (local.get $n)))
        (block $closed
          (loop $find
            (if (i32.ge_s (local.get $q) (local.get $e))
              (then ;; unclosed: the run is literal
                (local.set $p (i32.add (local.get $p) (local.get $n)))
                (br $scan)))
            (if (i32.eq (call $byte (local.get $q)) (i32.const 0x60))
              (then
                (local.set $m (call $run (local.get $q) (local.get $e) (i32.const 0x60)))
                (br_if $closed (i32.eq (local.get $m) (local.get $n)))
                (local.set $q (i32.add (local.get $q) (local.get $m)))
                (br $find)))
            (local.set $q (i32.add (local.get $q) (i32.const 1)))
            (br $find)))
        (call $style_set (local.get $p) (i32.add (local.get $p) (local.get $n)) (i32.or (global.get $S_CODE) (global.get $S_MARK)))
        (call $style_set (i32.add (local.get $p) (local.get $n)) (local.get $q) (global.get $S_CODE))
        (call $style_set (local.get $q) (i32.add (local.get $q) (local.get $n)) (i32.or (global.get $S_CODE) (global.get $S_MARK)))
        (local.set $p (i32.add (local.get $q) (local.get $n)))
        (br $scan))))

  ;; [text](destination) and ![alt](destination). The brackets, parens and
  ;; destination are syntax; the text reads as a link.
  (func $links (param $s i32) (param $e i32)
    (local $p i32) (local $close i32) (local $end i32) (local $img i32) (local $depth i32) (local $c i32)
    (local.set $p (local.get $s))
    (block $done
      (loop $scan
        (br_if $done (i32.ge_s (local.get $p) (local.get $e)))
        (if (i32.or (i32.ne (call $byte (local.get $p)) (i32.const 0x5B))
                    (i32.and (call $style_of (local.get $p)) (global.get $S_CODE)))
          (then (local.set $p (i32.add (local.get $p) (i32.const 1))) (br $scan)))
        (local.set $img (i32.and (i32.gt_s (local.get $p) (local.get $s))
          (i32.eq (call $byte (i32.sub (local.get $p) (i32.const 1))) (i32.const 0x21))))
        ;; find the matching ]
        (local.set $close (i32.add (local.get $p) (i32.const 1)))
        (local.set $depth (i32.const 1))
        (block $found
          (loop $find
            (if (i32.ge_s (local.get $close) (local.get $e))
              (then (local.set $p (i32.add (local.get $p) (i32.const 1))) (br $scan)))
            (local.set $c (call $byte (local.get $close)))
            (if (i32.eq (local.get $c) (i32.const 0x5B)) (then (local.set $depth (i32.add (local.get $depth) (i32.const 1)))))
            (if (i32.eq (local.get $c) (i32.const 0x5D))
              (then
                (local.set $depth (i32.sub (local.get $depth) (i32.const 1)))
                (br_if $found (i32.eqz (local.get $depth)))))
            (local.set $close (i32.add (local.get $close) (i32.const 1)))
            (br $find)))
        ;; then ( … )
        (if (i32.or (i32.ge_s (i32.add (local.get $close) (i32.const 1)) (local.get $e))
                    (i32.ne (call $byte (i32.add (local.get $close) (i32.const 1))) (i32.const 0x28)))
          (then (local.set $p (i32.add (local.get $p) (i32.const 1))) (br $scan)))
        (local.set $end (i32.add (local.get $close) (i32.const 2)))
        (block $paren
          (loop $find2
            (if (i32.ge_s (local.get $end) (local.get $e))
              (then (local.set $p (i32.add (local.get $p) (i32.const 1))) (br $scan)))
            (br_if $paren (i32.eq (call $byte (local.get $end)) (i32.const 0x29)))
            (local.set $end (i32.add (local.get $end) (i32.const 1)))
            (br $find2)))
        (if (local.get $img)
          (then (call $style_set (i32.sub (local.get $p) (i32.const 1)) (local.get $p) (global.get $S_MARK))))
        (call $style_set (local.get $p) (i32.add (local.get $p) (i32.const 1)) (global.get $S_MARK))
        (call $style_set (i32.add (local.get $p) (i32.const 1)) (local.get $close)
          (if (result i32) (local.get $img) (then (global.get $S_ITAL)) (else (global.get $S_LINK))))
        (call $style_set (local.get $close) (i32.add (local.get $end) (i32.const 1)) (global.get $S_MARK))
        (local.set $p (i32.add (local.get $end) (i32.const 1)))
        (br $scan))))

  ;; bare http(s):// and blob:// references read as links too
  (func $autolinks (param $s i32) (param $e i32) (local $p i32) (local $q i32)
    (local.set $p (local.get $s))
    (block $done
      (loop $scan
        (br_if $done (i32.ge_s (local.get $p) (local.get $e)))
        (if (i32.and
              (i32.eqz (i32.and (call $style_of (local.get $p)) (i32.or (global.get $S_CODE) (global.get $S_MARK))))
              (i32.or (call $starts (local.get $p) (local.get $e) (i32.const 0x3A707474) (i32.const 0x68))  ;; "http:" via h + "ttp:"
                      (call $starts (local.get $p) (local.get $e) (i32.const 0x3A626F6C) (i32.const 0x62)))) ;; "blob:"
          (then
            (local.set $q (local.get $p))
            (block $end (loop $word
              (br_if $end (i32.ge_s (local.get $q) (local.get $e)))
              (br_if $end (i32.le_u (call $byte (local.get $q)) (i32.const 0x20)))
              (local.set $q (i32.add (local.get $q) (i32.const 1)))
              (br $word)))
            (call $style_set (local.get $p) (local.get $q) (global.get $S_LINK))
            (local.set $p (local.get $q))
            (br $scan)))
        (local.set $p (i32.add (local.get $p) (i32.const 1)))
        (br $scan))))

  ;; $starts: does the text at p begin with byte c0 followed by the four
  ;; bytes of w (little-endian), or with "https:"? Only used for URL schemes.
  (func $starts (param $p i32) (param $e i32) (param $w i32) (param $c0 i32) (result i32) (local $a i32)
    (if (i32.gt_s (i32.add (local.get $p) (i32.const 5)) (local.get $e)) (then (return (i32.const 0))))
    (if (i32.ne (call $byte (local.get $p)) (local.get $c0)) (then (return (i32.const 0))))
    (local.set $a (i32.add (global.get $TEXT) (i32.add (local.get $p) (i32.const 1))))
    (if (i32.eq (i32.load align=1 (local.get $a)) (local.get $w)) (then (return (i32.const 1))))
    ;; https:
    (if (i32.and (i32.eq (local.get $c0) (i32.const 0x68))
          (i32.le_s (i32.add (local.get $p) (i32.const 6)) (local.get $e)))
      (then (return (i32.and (i32.eq (i32.load align=1 (local.get $a)) (i32.const 0x73707474))
                             (i32.eq (i32.load8_u offset=4 (local.get $a)) (i32.const 0x3A))))))
    (i32.const 0))

  ;; Emphasis: runs of * _ ~ pair up as a stack of openers. A run can open if
  ;; the next character is not a space, close if the previous one is not;
  ;; `_` inside a word (snake_case) does neither. Two delimiters make bold (or
  ;; strike for ~~), one italic, three both.
  (func $emphasis (param $s i32) (param $e i32)
    (local $p i32) (local $c i32) (local $n i32) (local $before i32) (local $after i32)
    (local $open i32) (local $close i32) (local $i i32) (local $o i32) (local $ol i32) (local $use i32) (local $bits i32)
    (global.set $nopen (i32.const 0))
    (local.set $p (local.get $s))
    (block $done
      (loop $scan
        (br_if $done (i32.ge_s (local.get $p) (local.get $e)))
        (local.set $c (call $byte (local.get $p)))
        (if (i32.or
              (i32.and (i32.and (i32.ne (local.get $c) (i32.const 0x2A)) (i32.ne (local.get $c) (i32.const 0x5F)))
                       (i32.ne (local.get $c) (i32.const 0x7E)))
              (i32.and (call $style_of (local.get $p)) (i32.or (i32.or (global.get $S_CODE) (global.get $S_MARK)) (global.get $S_LINK))))
          (then (local.set $p (i32.add (local.get $p) (i32.const 1))) (br $scan)))
        (local.set $n (call $run (local.get $p) (local.get $e) (local.get $c)))
        (local.set $before (if (result i32) (i32.gt_s (local.get $p) (local.get $s))
          (then (call $byte (i32.sub (local.get $p) (i32.const 1)))) (else (i32.const 0x20))))
        (local.set $after (if (result i32) (i32.lt_s (i32.add (local.get $p) (local.get $n)) (local.get $e))
          (then (call $byte (i32.add (local.get $p) (local.get $n)))) (else (i32.const 0x20))))
        (local.set $open (i32.gt_u (local.get $after) (i32.const 0x20)))
        (local.set $close (i32.gt_u (local.get $before) (i32.const 0x20)))
        (if (i32.eq (local.get $c) (i32.const 0x5F))
          (then ;; intraword underscore is literal
            (if (call $is_word (local.get $before)) (then (local.set $open (i32.const 0))))
            (if (call $is_word (local.get $after)) (then (local.set $close (i32.const 0))))))
        (if (i32.and (i32.eq (local.get $c) (i32.const 0x7E)) (i32.ne (local.get $n) (i32.const 2)))
          (then (local.set $p (i32.add (local.get $p) (local.get $n))) (br $scan)))
        ;; try to close against the nearest opener of the same character
        (if (local.get $close)
          (then
            (local.set $i (global.get $nopen))
            (block $nomatch
              (loop $search
                (br_if $nomatch (i32.le_s (local.get $i) (i32.const 0)))
                (local.set $i (i32.sub (local.get $i) (i32.const 1)))
                (local.set $o (i32.add (global.get $OPENERS) (i32.mul (local.get $i) (i32.const 12))))
                (if (i32.eq (i32.load offset=4 (local.get $o)) (local.get $c))
                  (then
                    (local.set $ol (i32.load offset=8 (local.get $o)))
                    (local.set $use (call $min (call $min (local.get $ol) (local.get $n)) (i32.const 3)))
                    (local.set $bits (i32.const 0))
                    (if (i32.eq (local.get $c) (i32.const 0x7E))
                      (then (local.set $bits (global.get $S_STRIKE)))
                      (else
                        (if (i32.ge_s (local.get $use) (i32.const 2)) (then (local.set $bits (global.get $S_BOLD))))
                        (if (i32.ne (local.get $use) (i32.const 2)) (then (local.set $bits (i32.or (local.get $bits) (global.get $S_ITAL)))))))
                    ;; the delimiters are syntax; what they enclose is styled
                    (call $style_set (i32.sub (i32.add (i32.load (local.get $o)) (local.get $ol)) (local.get $use))
                                     (i32.add (i32.load (local.get $o)) (local.get $ol)) (global.get $S_MARK))
                    (call $style_set (i32.add (i32.load (local.get $o)) (local.get $ol)) (local.get $p) (local.get $bits))
                    (call $style_set (local.get $p) (i32.add (local.get $p) (local.get $use)) (global.get $S_MARK))
                    (global.set $nopen (local.get $i)) ;; drop this opener and any inside it
                    (local.set $p (i32.add (local.get $p) (local.get $n)))
                    (br $scan)))
                (br $search)))))
        (if (i32.and (local.get $open) (i32.lt_s (global.get $nopen) (i32.const 64)))
          (then
            (local.set $o (i32.add (global.get $OPENERS) (i32.mul (global.get $nopen) (i32.const 12))))
            (i32.store (local.get $o) (local.get $p))
            (i32.store offset=4 (local.get $o) (local.get $c))
            (i32.store offset=8 (local.get $o) (local.get $n))
            (global.set $nopen (i32.add (global.get $nopen) (i32.const 1)))))
        (local.set $p (i32.add (local.get $p) (local.get $n)))
        (br $scan))))
)
