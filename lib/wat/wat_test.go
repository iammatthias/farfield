package wat

import (
	"context"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// run assembles src, instantiates it in wazero, and returns the module.
func run(t *testing.T, src string) api.Module {
	t.Helper()
	bin, err := Assemble(Source{Name: "test.wat", Text: src})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	t.Cleanup(func() { rt.Close(ctx) })
	mod, err := rt.Instantiate(ctx, bin)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	return mod
}

func call(t *testing.T, m api.Module, name string, args ...uint64) []uint64 {
	t.Helper()
	f := m.ExportedFunction(name)
	if f == nil {
		t.Fatalf("no export %s", name)
	}
	out, err := f.Call(context.Background(), args...)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

func TestFoldedAndFlat(t *testing.T) {
	m := run(t, `(module
	  (func $add (export "add") (param $a i32) (param $b i32) (result i32)
	    (i32.add (local.get $a) (local.get $b)))
	  (func (export "addflat") (param i32 i32) (result i32)
	    local.get 0
	    local.get 1
	    i32.add)
	  (func (export "twice") (param $x i32) (result i32)
	    (call $add (local.get $x) (local.get $x))))`)
	if got := call(t, m, "add", 2, 40)[0]; got != 42 {
		t.Errorf("add = %d", got)
	}
	if got := call(t, m, "addflat", 7, 8)[0]; got != 15 {
		t.Errorf("addflat = %d", got)
	}
	if got := call(t, m, "twice", 21)[0]; got != 42 {
		t.Errorf("twice = %d", got)
	}
}

func TestControlFlowAndLabels(t *testing.T) {
	m := run(t, `(module
	  ;; sum 1..n with a loop, a named block, and br_if
	  (func (export "sum") (param $n i32) (result i32) (local $i i32) (local $s i32)
	    (block $done
	      (loop $next
	        (br_if $done (i32.gt_s (local.get $i) (local.get $n)))
	        (local.set $s (i32.add (local.get $s) (local.get $i)))
	        (local.set $i (i32.add (local.get $i) (i32.const 1)))
	        (br $next)))
	    (local.get $s))
	  (func (export "sign") (param $x i32) (result i32)
	    (if (result i32) (i32.lt_s (local.get $x) (i32.const 0))
	      (then (i32.const -1))
	      (else (if (result i32) (local.get $x) (then (i32.const 1)) (else (i32.const 0))))))
	  (func (export "pick") (param $k i32) (result i32)
	    (block $c (block $b (block $a
	      (br_table $a $b $c (local.get $k)))
	      (return (i32.const 10)))
	      (return (i32.const 20)))
	    (i32.const 30)))`)
	if got := call(t, m, "sum", 100)[0]; got != 5050 {
		t.Errorf("sum = %d", got)
	}
	for in, want := range map[int32]int32{-5: -1, 0: 0, 9: 1} {
		if got := int32(call(t, m, "sign", uint64(uint32(in)))[0]); got != want {
			t.Errorf("sign(%d) = %d", in, got)
		}
	}
	for k, want := range []uint64{10, 20, 30, 30} {
		if got := call(t, m, "pick", uint64(k))[0]; got != want {
			t.Errorf("pick(%d) = %d", k, got)
		}
	}
}

func TestMemoryDataGlobals(t *testing.T) {
	m := run(t, `(module
	  (memory (export "memory") 1)
	  (data (i32.const 16) "hi\00\ff" "\u{2014}")
	  (global $counter (mut i32) (i32.const 5))
	  (global $k (export "k") i32 (i32.const 7))
	  (func (export "bump") (result i32)
	    (global.set $counter (i32.add (global.get $counter) (i32.const 1)))
	    (global.get $counter))
	  (func (export "byte") (param $p i32) (result i32) (i32.load8_u (local.get $p)))
	  (func (export "word") (param $p i32) (result i32) (i32.load16_u offset=1 (local.get $p)))
	  (func (export "fill") (param $p i32) (param $v i32) (param $n i32)
	    (memory.fill (local.get $p) (local.get $v) (local.get $n)))
	  (func (export "copy") (param $d i32) (param $s i32) (param $n i32)
	    (memory.copy (local.get $d) (local.get $s) (local.get $n))))`)
	if got := call(t, m, "byte", 16)[0]; got != 'h' {
		t.Errorf("byte = %d", got)
	}
	if got := call(t, m, "byte", 19)[0]; got != 0xff {
		t.Errorf("byte 19 = %d", got)
	}
	if got := call(t, m, "word", 15)[0]; got != uint64('h')|uint64('i')<<8 {
		t.Errorf("word = %x", got)
	}
	em, _ := m.Memory().Read(20, 3)
	if string(em) != "—" {
		t.Errorf("utf-8 data = %q", em)
	}
	call(t, m, "bump")
	if got := call(t, m, "bump")[0]; got != 7 {
		t.Errorf("bump = %d", got)
	}
	call(t, m, "fill", 100, 0xAB, 4)
	call(t, m, "copy", 200, 100, 4)
	got, _ := m.Memory().Read(200, 4)
	if string(got) != "\xab\xab\xab\xab" {
		t.Errorf("fill/copy = %x", got)
	}
	if g := m.ExportedGlobal("k").Get(); g != 7 {
		t.Errorf("global k = %d", g)
	}
}

func TestNumbers(t *testing.T) {
	m := run(t, `(module
	  (func (export "big") (result i32) (i32.const 0xFFFF_FFFF))
	  (func (export "neg") (result i64) (i64.const -9_000_000_000))
	  (func (export "f") (result f32) (f32.mul (f32.const 1.5) (f32.const 4)))
	  (func (export "d") (result f64) (f64.sqrt (f64.const 2)))
	  (func (export "sat") (result i32) (i32.trunc_sat_f64_s (f64.const 1e20)))
	  (func (export "ext") (result i32) (i32.extend8_s (i32.const 0x80))))`)
	if got := int32(call(t, m, "big")[0]); got != -1 {
		t.Errorf("big = %d", got)
	}
	if got := int64(call(t, m, "neg")[0]); got != -9_000_000_000 {
		t.Errorf("neg = %d", got)
	}
	if got := api.DecodeF32(call(t, m, "f")[0]); got != 6 {
		t.Errorf("f = %v", got)
	}
	if got := api.DecodeF64(call(t, m, "d")[0]); got < 1.414 || got > 1.415 {
		t.Errorf("d = %v", got)
	}
	if got := int32(call(t, m, "sat")[0]); got != 2147483647 {
		t.Errorf("sat = %d", got)
	}
	if got := int32(call(t, m, "ext")[0]); got != -128 {
		t.Errorf("ext = %d", got)
	}
}

func TestMultipleSourcesAndImports(t *testing.T) {
	a := Source{Name: "a.wat", Text: `(module (func $double (param $x i32) (result i32) (i32.shl (local.get $x) (i32.const 1))))`}
	b := Source{Name: "b.wat", Text: `(module (func (export "quad") (param $x i32) (result i32) (call $double (call $double (local.get $x)))))`}
	bin, err := Assemble(a, b)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)
	m, err := rt.Instantiate(ctx, bin)
	if err != nil {
		t.Fatal(err)
	}
	if got := call(t, m, "quad", 5)[0]; got != 20 {
		t.Errorf("quad = %d", got)
	}

	// An import resolves against a host module.
	var seen int32
	_, err = rt.NewHostModuleBuilder("env").NewFunctionBuilder().
		WithFunc(func(v int32) { seen = v }).Export("log").Instantiate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bin, err = Assemble(Source{Name: "i.wat", Text: `(module
	  (import "env" "log" (func $log (param i32)))
	  (func (export "go") (call $log (i32.const 99))))`})
	if err != nil {
		t.Fatal(err)
	}
	m2, err := rt.Instantiate(ctx, bin)
	if err != nil {
		t.Fatal(err)
	}
	call(t, m2, "go")
	if seen != 99 {
		t.Errorf("import saw %d", seen)
	}
}

func TestErrorsNamePlaces(t *testing.T) {
	for src, want := range map[string]string{
		`(module (func (local.get $nope)))`:           "unknown local $nope",
		`(module (func (br $gone)))`:                  "unknown label $gone",
		`(module (func (i32.frobnicate)))`:            "unknown instruction",
		`(module (func (call $missing)))`:             "unknown function $missing",
		`(module (func $a) (func $a))`:                "duplicate function",
		`(module (data (i32.const 0) "unterminated))`: "unterminated string",
	} {
		_, err := Assemble(Source{Name: "e.wat", Text: src})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", src, err, want)
		}
		if err != nil && !strings.Contains(err.Error(), "e.wat:") {
			t.Errorf("error lacks a position: %v", err)
		}
	}
}
