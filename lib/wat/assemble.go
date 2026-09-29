// Package wat assembles the WebAssembly text format into a binary module.
//
// It exists so farfield's WebAssembly is written by hand — instruction by
// instruction, in .wat — while the tooling to build it stays Go, with no wabt
// or other toolchain to install. It covers the part of the text format a
// hand-written module needs: functions with named params and locals, imports,
// one memory, globals, exports, active data segments, a start function, and
// every instruction in both the flat and the folded (S-expression) form, with
// named block labels.
//
// A module may be split across several source files: Assemble concatenates
// the fields of every (module …) it is given into one module, so a large
// program can be organised by concern without a linker.
package wat

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
)

type valType byte

const (
	tI32 valType = 0x7F
	tI64 valType = 0x7E
	tF32 valType = 0x7D
	tF64 valType = 0x7C
)

func parseValType(s string) (valType, bool) {
	switch s {
	case "i32":
		return tI32, true
	case "i64":
		return tI64, true
	case "f32":
		return tF32, true
	case "f64":
		return tF64, true
	}
	return 0, false
}

type funcType struct {
	params, results []valType
}

func (t funcType) key() string {
	return fmt.Sprint(t.params, "->", t.results)
}

type function struct {
	name       string
	typ        funcType
	typeIdx    uint32
	imported   bool
	module     string // import module/name
	field      string
	locals     []valType         // declared locals (excluding params)
	names      map[string]uint32 // param and local names → index
	localNames []string          // index → name, for the name section
	body       []node
	pos        pos
}

type global struct {
	name string
	typ  valType
	mut  bool
	init node
}

type export struct {
	name string
	kind byte // 0 func, 2 memory, 3 global
	ref  string
}

type data struct {
	offset node
	bytes  []byte
}

type module struct {
	types     []funcType
	typeIdx   map[string]uint32
	funcs     []*function
	funcIdx   map[string]uint32
	globals   []*global
	globalIdx map[string]uint32
	memMin    uint32
	memMax    uint32
	hasMax    bool
	hasMem    bool
	exports   []export
	datas     []data
	start     string
}

// Source is one input file.
type Source struct {
	Name string
	Text string
}

// Assemble builds one binary module from the fields of every (module …) in
// the sources, in order.
func Assemble(sources ...Source) ([]byte, error) {
	m := &module{
		typeIdx:   map[string]uint32{},
		funcIdx:   map[string]uint32{},
		globalIdx: map[string]uint32{},
	}
	var fields []node
	for _, s := range sources {
		tops, err := parseSexprs(s.Name, s.Text)
		if err != nil {
			return nil, err
		}
		for _, t := range tops {
			if t.head() != "module" {
				return nil, fmt.Errorf("%s: top level must be (module …)", t.pos)
			}
			rest := t.list[1:]
			if len(rest) > 0 && !rest[0].isList && strings.HasPrefix(rest[0].atom, "$") {
				rest = rest[1:] // module name
			}
			fields = append(fields, rest...)
		}
	}

	// Imports come first in the function index space, so collect them before
	// defined functions regardless of source order.
	var imports, defined []node
	for _, f := range fields {
		switch f.head() {
		case "import":
			imports = append(imports, f)
		case "func":
			defined = append(defined, f)
		}
	}
	for _, f := range imports {
		if err := m.declareImport(f); err != nil {
			return nil, err
		}
	}
	for _, f := range defined {
		if err := m.declareFunc(f); err != nil {
			return nil, err
		}
	}
	for _, f := range fields {
		var err error
		switch f.head() {
		case "import", "func":
		case "memory":
			err = m.declareMemory(f)
		case "global":
			err = m.declareGlobal(f)
		case "export":
			err = m.declareExport(f)
		case "data":
			err = m.declareData(f)
		case "start":
			if len(f.list) != 2 {
				err = fmt.Errorf("%s: (start $func)", f.pos)
			} else {
				m.start = f.list[1].atom
			}
		case "type":
			// Function types are interned automatically from signatures.
		default:
			err = fmt.Errorf("%s: unsupported module field %q", f.pos, f.head())
		}
		if err != nil {
			return nil, err
		}
	}
	return m.encode()
}

func (m *module) internType(t funcType) uint32 {
	if i, ok := m.typeIdx[t.key()]; ok {
		return i
	}
	i := uint32(len(m.types))
	m.types = append(m.types, t)
	m.typeIdx[t.key()] = i
	return i
}

// parseSig reads (param …), (result …) and (local …) clauses starting at
// fields[i], returning the index of the first field that is none of them.
func parseSig(fn *function, fields []node, i int) (int, error) {
	for ; i < len(fields); i++ {
		f := fields[i]
		switch f.head() {
		case "param", "local":
			isParam := f.head() == "param"
			if isParam && len(fn.locals) > 0 {
				return 0, fmt.Errorf("%s: param after local", f.pos)
			}
			args := f.list[1:]
			if len(args) == 2 && strings.HasPrefix(args[0].atom, "$") {
				vt, ok := parseValType(args[1].atom)
				if !ok {
					return 0, fmt.Errorf("%s: bad type %q", f.pos, args[1].atom)
				}
				idx := uint32(len(fn.typ.params) + len(fn.locals))
				if _, dup := fn.names[args[0].atom]; dup {
					return 0, fmt.Errorf("%s: duplicate local %s", f.pos, args[0].atom)
				}
				fn.names[args[0].atom] = idx
				fn.localNames = append(fn.localNames, args[0].atom)
				if isParam {
					fn.typ.params = append(fn.typ.params, vt)
				} else {
					fn.locals = append(fn.locals, vt)
				}
				continue
			}
			for _, a := range args {
				vt, ok := parseValType(a.atom)
				if !ok {
					return 0, fmt.Errorf("%s: bad type %q", f.pos, a.atom)
				}
				fn.localNames = append(fn.localNames, "")
				if isParam {
					fn.typ.params = append(fn.typ.params, vt)
				} else {
					fn.locals = append(fn.locals, vt)
				}
			}
		case "result":
			if len(fn.locals) > 0 {
				return 0, fmt.Errorf("%s: result after local", f.pos)
			}
			for _, a := range f.list[1:] {
				vt, ok := parseValType(a.atom)
				if !ok {
					return 0, fmt.Errorf("%s: bad type %q", f.pos, a.atom)
				}
				fn.typ.results = append(fn.typ.results, vt)
			}
		default:
			return i, nil
		}
	}
	return i, nil
}

func (m *module) addFunc(fn *function) error {
	idx := uint32(len(m.funcs))
	if fn.name != "" {
		if _, dup := m.funcIdx[fn.name]; dup {
			return fmt.Errorf("%s: duplicate function %s", fn.pos, fn.name)
		}
		m.funcIdx[fn.name] = idx
	}
	fn.typeIdx = m.internType(fn.typ)
	m.funcs = append(m.funcs, fn)
	return nil
}

// (import "mod" "field" (func $name (param …) (result …)))
func (m *module) declareImport(f node) error {
	if len(f.list) != 4 || !f.list[1].isStr || !f.list[2].isStr || f.list[3].head() != "func" {
		return fmt.Errorf("%s: only (import \"m\" \"f\" (func …)) is supported", f.pos)
	}
	desc := f.list[3].list[1:]
	fn := &function{imported: true, module: string(f.list[1].str), field: string(f.list[2].str),
		names: map[string]uint32{}, pos: f.pos}
	i := 0
	if len(desc) > 0 && strings.HasPrefix(desc[0].atom, "$") {
		fn.name = desc[0].atom
		i = 1
	}
	if _, err := parseSig(fn, desc, i); err != nil {
		return err
	}
	return m.addFunc(fn)
}

// (func $name (export "x")* (param …)* (result …)* (local …)* instr*)
func (m *module) declareFunc(f node) error {
	fn := &function{names: map[string]uint32{}, pos: f.pos}
	rest := f.list[1:]
	i := 0
	if i < len(rest) && strings.HasPrefix(rest[i].atom, "$") {
		fn.name = rest[i].atom
		i++
	}
	for i < len(rest) && rest[i].head() == "export" {
		if fn.name == "" {
			fn.name = fmt.Sprintf("$__anon%d", len(m.funcs))
		}
		m.exports = append(m.exports, export{name: string(rest[i].list[1].str), kind: 0, ref: fn.name})
		i++
	}
	i, err := parseSig(fn, rest, i)
	if err != nil {
		return err
	}
	fn.body = rest[i:]
	return m.addFunc(fn)
}

// (memory (export "memory")? min max?)
func (m *module) declareMemory(f node) error {
	if m.hasMem {
		return fmt.Errorf("%s: one memory only", f.pos)
	}
	m.hasMem = true
	var nums []uint32
	for _, a := range f.list[1:] {
		if a.head() == "export" {
			m.exports = append(m.exports, export{name: string(a.list[1].str), kind: 2})
			continue
		}
		if strings.HasPrefix(a.atom, "$") {
			continue
		}
		v, err := strconv.ParseUint(a.atom, 0, 32)
		if err != nil {
			return fmt.Errorf("%s: bad memory size %q", f.pos, a.atom)
		}
		nums = append(nums, uint32(v))
	}
	if len(nums) == 0 {
		return fmt.Errorf("%s: memory needs a minimum", f.pos)
	}
	m.memMin = nums[0]
	if len(nums) > 1 {
		m.memMax, m.hasMax = nums[1], true
	}
	return nil
}

// (global $g (export "x")? (mut i32)|i32 (init))
func (m *module) declareGlobal(f node) error {
	g := &global{}
	rest := f.list[1:]
	i := 0
	if i < len(rest) && strings.HasPrefix(rest[i].atom, "$") {
		g.name = rest[i].atom
		i++
	}
	for i < len(rest) && rest[i].head() == "export" {
		m.exports = append(m.exports, export{name: string(rest[i].list[1].str), kind: 3, ref: g.name})
		i++
	}
	if i >= len(rest) {
		return fmt.Errorf("%s: global needs a type", f.pos)
	}
	if rest[i].head() == "mut" {
		g.mut = true
		vt, ok := parseValType(rest[i].list[1].atom)
		if !ok {
			return fmt.Errorf("%s: bad global type", f.pos)
		}
		g.typ = vt
	} else {
		vt, ok := parseValType(rest[i].atom)
		if !ok {
			return fmt.Errorf("%s: bad global type", f.pos)
		}
		g.typ = vt
	}
	i++
	if i >= len(rest) {
		return fmt.Errorf("%s: global needs an initializer", f.pos)
	}
	g.init = rest[i]
	if g.name != "" {
		if _, dup := m.globalIdx[g.name]; dup {
			return fmt.Errorf("%s: duplicate global %s", f.pos, g.name)
		}
		m.globalIdx[g.name] = uint32(len(m.globals))
	}
	m.globals = append(m.globals, g)
	return nil
}

// (export "name" (func $f)) | (memory 0) | (global $g)
func (m *module) declareExport(f node) error {
	if len(f.list) != 3 || !f.list[1].isStr || !f.list[2].isList {
		return fmt.Errorf("%s: (export \"name\" (kind ref))", f.pos)
	}
	ref := f.list[2]
	e := export{name: string(f.list[1].str)}
	switch ref.head() {
	case "func":
		e.kind = 0
	case "memory":
		e.kind = 2
	case "global":
		e.kind = 3
	default:
		return fmt.Errorf("%s: cannot export %q", f.pos, ref.head())
	}
	if len(ref.list) > 1 {
		e.ref = ref.list[1].atom
	}
	m.exports = append(m.exports, e)
	return nil
}

// (data (i32.const N) "bytes"…)
func (m *module) declareData(f node) error {
	rest := f.list[1:]
	if len(rest) > 0 && strings.HasPrefix(rest[0].atom, "$") {
		rest = rest[1:]
	}
	if len(rest) == 0 || !rest[0].isList {
		return fmt.Errorf("%s: data needs an offset expression", f.pos)
	}
	d := data{offset: rest[0]}
	if d.offset.head() == "offset" {
		d.offset = d.offset.list[1]
	}
	for _, s := range rest[1:] {
		if !s.isStr {
			return fmt.Errorf("%s: data contents must be strings", s.pos)
		}
		d.bytes = append(d.bytes, s.str...)
	}
	m.datas = append(m.datas, d)
	return nil
}

// ── encoding ───────────────────────────────────────────────────────────────

func (m *module) encode() ([]byte, error) {
	var out bytes.Buffer
	out.Write([]byte{0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00})

	section := func(id byte, body []byte) {
		out.WriteByte(id)
		out.Write(uleb(uint64(len(body))))
		out.Write(body)
	}

	// type
	{
		var b []byte
		b = append(b, uleb(uint64(len(m.types)))...)
		for _, t := range m.types {
			b = append(b, 0x60)
			b = append(b, uleb(uint64(len(t.params)))...)
			for _, p := range t.params {
				b = append(b, byte(p))
			}
			b = append(b, uleb(uint64(len(t.results)))...)
			for _, r := range t.results {
				b = append(b, byte(r))
			}
		}
		section(1, b)
	}

	// import
	var nImports int
	{
		var b []byte
		for _, f := range m.funcs {
			if !f.imported {
				continue
			}
			nImports++
			b = append(b, name(f.module)...)
			b = append(b, name(f.field)...)
			b = append(b, 0x00)
			b = append(b, uleb(uint64(f.typeIdx))...)
		}
		if nImports > 0 {
			section(2, append(uleb(uint64(nImports)), b...))
		}
	}

	// function
	{
		var b []byte
		n := 0
		for _, f := range m.funcs {
			if f.imported {
				continue
			}
			n++
			b = append(b, uleb(uint64(f.typeIdx))...)
		}
		section(3, append(uleb(uint64(n)), b...))
	}

	// memory
	if m.hasMem {
		b := []byte{0x01}
		if m.hasMax {
			b = append(b, 0x01)
			b = append(b, uleb(uint64(m.memMin))...)
			b = append(b, uleb(uint64(m.memMax))...)
		} else {
			b = append(b, 0x00)
			b = append(b, uleb(uint64(m.memMin))...)
		}
		section(5, b)
	}

	// global
	if len(m.globals) > 0 {
		b := uleb(uint64(len(m.globals)))
		for _, g := range m.globals {
			b = append(b, byte(g.typ))
			if g.mut {
				b = append(b, 0x01)
			} else {
				b = append(b, 0x00)
			}
			e := &emitter{m: m, fn: &function{names: map[string]uint32{}}}
			if err := e.expr(g.init); err != nil {
				return nil, err
			}
			b = append(b, e.buf.Bytes()...)
			b = append(b, 0x0B)
		}
		section(6, b)
	}

	// export
	if len(m.exports) > 0 {
		b := uleb(uint64(len(m.exports)))
		for _, e := range m.exports {
			b = append(b, name(e.name)...)
			b = append(b, e.kind)
			var idx uint32
			switch e.kind {
			case 0:
				i, ok := m.funcIdx[e.ref]
				if !ok {
					if n, err := strconv.ParseUint(e.ref, 10, 32); err == nil {
						i, ok = uint32(n), true
					}
				}
				if !ok {
					return nil, fmt.Errorf("export %q: unknown function %s", e.name, e.ref)
				}
				idx = i
			case 3:
				i, ok := m.globalIdx[e.ref]
				if !ok {
					return nil, fmt.Errorf("export %q: unknown global %s", e.name, e.ref)
				}
				idx = i
			}
			b = append(b, uleb(uint64(idx))...)
		}
		section(7, b)
	}

	// start
	if m.start != "" {
		i, ok := m.funcIdx[m.start]
		if !ok {
			return nil, fmt.Errorf("start: unknown function %s", m.start)
		}
		section(8, uleb(uint64(i)))
	}

	// code
	{
		var b []byte
		n := 0
		for _, f := range m.funcs {
			if f.imported {
				continue
			}
			n++
			body, err := m.encodeBody(f)
			if err != nil {
				return nil, err
			}
			b = append(b, uleb(uint64(len(body)))...)
			b = append(b, body...)
		}
		section(10, append(uleb(uint64(n)), b...))
	}

	// data
	if len(m.datas) > 0 {
		b := uleb(uint64(len(m.datas)))
		for _, d := range m.datas {
			b = append(b, 0x00) // active, memory 0
			e := &emitter{m: m, fn: &function{names: map[string]uint32{}}}
			if err := e.expr(d.offset); err != nil {
				return nil, err
			}
			b = append(b, e.buf.Bytes()...)
			b = append(b, 0x0B)
			b = append(b, uleb(uint64(len(d.bytes)))...)
			b = append(b, d.bytes...)
		}
		section(11, b)
	}

	// name (custom): function names and local names, so a trap reads
	// "$insert_bytes" rather than "func[57]".
	{
		var fnames []byte
		n := 0
		for i, f := range m.funcs {
			if f.name == "" {
				continue
			}
			n++
			fnames = append(fnames, uleb(uint64(i))...)
			fnames = append(fnames, name(strings.TrimPrefix(f.name, "$"))...)
		}
		var sub []byte
		sub = append(sub, 1)
		payload := append(uleb(uint64(n)), fnames...)
		sub = append(sub, uleb(uint64(len(payload)))...)
		sub = append(sub, payload...)

		var lnames []byte
		ln := 0
		for i, f := range m.funcs {
			var entries []byte
			en := 0
			for li, nm := range f.localNames {
				if nm == "" {
					continue
				}
				en++
				entries = append(entries, uleb(uint64(li))...)
				entries = append(entries, name(strings.TrimPrefix(nm, "$"))...)
			}
			if en == 0 {
				continue
			}
			ln++
			lnames = append(lnames, uleb(uint64(i))...)
			lnames = append(lnames, uleb(uint64(en))...)
			lnames = append(lnames, entries...)
		}
		if ln > 0 {
			payload := append(uleb(uint64(ln)), lnames...)
			sub = append(sub, 2)
			sub = append(sub, uleb(uint64(len(payload)))...)
			sub = append(sub, payload...)
		}
		body := append(name("name"), sub...)
		section(0, body)
	}

	return out.Bytes(), nil
}

func (m *module) encodeBody(f *function) ([]byte, error) {
	var b []byte
	// locals, run-length grouped by type
	type group struct {
		n int
		t valType
	}
	var groups []group
	for _, t := range f.locals {
		if len(groups) > 0 && groups[len(groups)-1].t == t {
			groups[len(groups)-1].n++
		} else {
			groups = append(groups, group{1, t})
		}
	}
	b = append(b, uleb(uint64(len(groups)))...)
	for _, g := range groups {
		b = append(b, uleb(uint64(g.n))...)
		b = append(b, byte(g.t))
	}
	e := &emitter{m: m, fn: f}
	if err := e.seq(f.body); err != nil {
		return nil, fmt.Errorf("in %s: %w", f.name, err)
	}
	b = append(b, e.buf.Bytes()...)
	b = append(b, 0x0B)
	return b, nil
}

// ── instructions ───────────────────────────────────────────────────────────

type emitter struct {
	m      *module
	fn     *function
	buf    bytes.Buffer
	labels []string // innermost last; "" for an unnamed block
}

// seq emits a sequence of instructions in either form: folded lists, or flat
// atoms with their immediates following as sibling atoms.
func (e *emitter) seq(nodes []node) error {
	for i := 0; i < len(nodes); i++ {
		n := nodes[i]
		if n.isList {
			if err := e.expr(n); err != nil {
				return err
			}
			continue
		}
		if n.isStr {
			return fmt.Errorf("%s: unexpected string", n.pos)
		}
		name := n.atom
		switch name {
		case "block", "loop", "if":
			// Flat structured instruction: label? blocktype? … end
			j := i + 1
			label := ""
			if j < len(nodes) && strings.HasPrefix(nodes[j].atom, "$") {
				label = nodes[j].atom
				j++
			}
			bt, j2, err := blockType(nodes, j)
			if err != nil {
				return err
			}
			j = j2
			e.buf.WriteByte(ops[name].code)
			e.buf.WriteByte(bt)
			e.labels = append(e.labels, label)
			i = j - 1
			continue
		case "else":
			e.buf.WriteByte(0x05)
			continue
		case "end":
			if len(e.labels) == 0 {
				return fmt.Errorf("%s: end without block", n.pos)
			}
			e.labels = e.labels[:len(e.labels)-1]
			e.buf.WriteByte(0x0B)
			continue
		}
		o, ok := ops[name]
		if !ok {
			return fmt.Errorf("%s: unknown instruction %q", n.pos, name)
		}
		// gather immediates: following atoms that are not instructions
		var imms []node
		for i+1 < len(nodes) && !nodes[i+1].isList && !nodes[i+1].isStr && isImmediate(nodes[i+1].atom, o.imm) {
			imms = append(imms, nodes[i+1])
			i++
			if o.imm != immBrTable && o.imm != immMem {
				break
			}
		}
		if err := e.instr(n, o, imms); err != nil {
			return err
		}
	}
	return nil
}

// isImmediate reports whether a following atom belongs to the instruction as
// an immediate rather than starting the next instruction.
func isImmediate(a string, k immKind) bool {
	if _, isOp := ops[a]; isOp {
		return false
	}
	switch k {
	case immNone, immMemIdx, immMemCopy:
		return false
	case immMem:
		return strings.HasPrefix(a, "offset=") || strings.HasPrefix(a, "align=")
	}
	return true
}

func blockType(nodes []node, j int) (byte, int, error) {
	bt := byte(0x40)
	if j < len(nodes) && nodes[j].head() == "result" {
		r := nodes[j]
		if len(r.list) != 2 {
			return 0, 0, fmt.Errorf("%s: block results: one value only", r.pos)
		}
		vt, ok := parseValType(r.list[1].atom)
		if !ok {
			return 0, 0, fmt.Errorf("%s: bad block type", r.pos)
		}
		bt = byte(vt)
		j++
	}
	return bt, j, nil
}

// expr emits one folded expression: (op imm* operand-expr*), or a structured
// (block …) / (loop …) / (if … (then …) (else …)).
func (e *emitter) expr(n node) error {
	if !n.isList || len(n.list) == 0 {
		return fmt.Errorf("%s: expected an instruction", n.pos)
	}
	name := n.head()
	args := n.list[1:]
	switch name {
	case "block", "loop":
		j := 0
		label := ""
		if j < len(args) && strings.HasPrefix(args[j].atom, "$") {
			label = args[j].atom
			j++
		}
		bt, j, err := blockType(args, j)
		if err != nil {
			return err
		}
		e.buf.WriteByte(ops[name].code)
		e.buf.WriteByte(bt)
		e.labels = append(e.labels, label)
		if err := e.seq(args[j:]); err != nil {
			return err
		}
		e.labels = e.labels[:len(e.labels)-1]
		e.buf.WriteByte(0x0B)
		return nil
	case "if":
		j := 0
		label := ""
		if j < len(args) && strings.HasPrefix(args[j].atom, "$") {
			label = args[j].atom
			j++
		}
		bt, j, err := blockType(args, j)
		if err != nil {
			return err
		}
		// condition expressions precede (then …)
		var then, els *node
		for ; j < len(args); j++ {
			switch args[j].head() {
			case "then":
				then = &args[j]
			case "else":
				els = &args[j]
			default:
				if then != nil {
					return fmt.Errorf("%s: unexpected %q after then", args[j].pos, args[j].head())
				}
				if err := e.expr(args[j]); err != nil {
					return err
				}
			}
		}
		if then == nil {
			return fmt.Errorf("%s: if without (then …)", n.pos)
		}
		e.buf.WriteByte(0x04)
		e.buf.WriteByte(bt)
		e.labels = append(e.labels, label)
		if err := e.seq(then.list[1:]); err != nil {
			return err
		}
		if els != nil {
			e.buf.WriteByte(0x05)
			if err := e.seq(els.list[1:]); err != nil {
				return err
			}
		}
		e.labels = e.labels[:len(e.labels)-1]
		e.buf.WriteByte(0x0B)
		return nil
	}
	o, ok := ops[name]
	if !ok {
		return fmt.Errorf("%s: unknown instruction %q", n.pos, name)
	}
	// immediates are the leading atoms; operands are the lists after them
	var imms []node
	k := 0
	for k < len(args) && !args[k].isList {
		imms = append(imms, args[k])
		k++
	}
	for _, operand := range args[k:] {
		if err := e.expr(operand); err != nil {
			return err
		}
	}
	return e.instr(n, o, imms)
}

func (e *emitter) instr(n node, o op, imms []node) error {
	b := &e.buf
	b.WriteByte(o.code)
	if o.prefixed {
		b.Write(uleb(uint64(o.sub)))
	}
	need := func(k int) error {
		if len(imms) != k {
			return fmt.Errorf("%s: %s takes %d immediate(s), got %d", n.pos, n.head()+n.atom, k, len(imms))
		}
		return nil
	}
	switch o.imm {
	case immNone:
		return need(0)
	case immMemIdx:
		if err := need(0); err != nil {
			return err
		}
		b.WriteByte(0x00)
	case immMemCopy:
		if err := need(0); err != nil {
			return err
		}
		b.WriteByte(0x00)
		b.WriteByte(0x00)
	case immLabel:
		if err := need(1); err != nil {
			return err
		}
		d, err := e.label(imms[0])
		if err != nil {
			return err
		}
		b.Write(uleb(uint64(d)))
	case immBrTable:
		if len(imms) < 1 {
			return fmt.Errorf("%s: br_table needs labels", n.pos)
		}
		b.Write(uleb(uint64(len(imms) - 1)))
		for _, l := range imms {
			d, err := e.label(l)
			if err != nil {
				return err
			}
			b.Write(uleb(uint64(d)))
		}
	case immFunc:
		if err := need(1); err != nil {
			return err
		}
		i, ok := e.m.funcIdx[imms[0].atom]
		if !ok {
			return fmt.Errorf("%s: unknown function %s", n.pos, imms[0].atom)
		}
		b.Write(uleb(uint64(i)))
	case immLocal:
		if err := need(1); err != nil {
			return err
		}
		i, err := e.local(imms[0])
		if err != nil {
			return err
		}
		b.Write(uleb(uint64(i)))
	case immGlobal:
		if err := need(1); err != nil {
			return err
		}
		i, ok := e.m.globalIdx[imms[0].atom]
		if !ok {
			v, err := strconv.ParseUint(imms[0].atom, 10, 32)
			if err != nil {
				return fmt.Errorf("%s: unknown global %s", n.pos, imms[0].atom)
			}
			i = uint32(v)
		}
		b.Write(uleb(uint64(i)))
	case immMem:
		align, offset := o.align, uint64(0)
		for _, im := range imms {
			k, v, _ := strings.Cut(im.atom, "=")
			x, err := strconv.ParseUint(strings.ReplaceAll(v, "_", ""), 0, 32)
			if err != nil {
				return fmt.Errorf("%s: bad %s", n.pos, im.atom)
			}
			switch k {
			case "offset":
				offset = x
			case "align":
				a := uint32(0)
				for (1 << a) < x {
					a++
				}
				align = a
			default:
				return fmt.Errorf("%s: bad memarg %s", n.pos, im.atom)
			}
		}
		b.Write(uleb(uint64(align)))
		b.Write(uleb(offset))
	case immI32:
		if err := need(1); err != nil {
			return err
		}
		v, err := parseInt(imms[0].atom, 32)
		if err != nil {
			return fmt.Errorf("%s: %v", n.pos, err)
		}
		b.Write(sleb(int64(int32(uint32(v)))))
	case immI64:
		if err := need(1); err != nil {
			return err
		}
		v, err := parseInt(imms[0].atom, 64)
		if err != nil {
			return fmt.Errorf("%s: %v", n.pos, err)
		}
		b.Write(sleb(v))
	case immF32:
		if err := need(1); err != nil {
			return err
		}
		f, err := parseFloat(imms[0].atom, 32)
		if err != nil {
			return fmt.Errorf("%s: %v", n.pos, err)
		}
		bits := math.Float32bits(float32(f))
		b.Write([]byte{byte(bits), byte(bits >> 8), byte(bits >> 16), byte(bits >> 24)})
	case immF64:
		if err := need(1); err != nil {
			return err
		}
		f, err := parseFloat(imms[0].atom, 64)
		if err != nil {
			return fmt.Errorf("%s: %v", n.pos, err)
		}
		bits := math.Float64bits(f)
		for s := 0; s < 64; s += 8 {
			b.WriteByte(byte(bits >> s))
		}
	case immCallInd:
		return fmt.Errorf("%s: call_indirect is not supported (no tables)", n.pos)
	}
	return nil
}

func (e *emitter) label(n node) (int, error) {
	if strings.HasPrefix(n.atom, "$") {
		for d := len(e.labels) - 1; d >= 0; d-- {
			if e.labels[d] == n.atom {
				return len(e.labels) - 1 - d, nil
			}
		}
		return 0, fmt.Errorf("%s: unknown label %s", n.pos, n.atom)
	}
	v, err := strconv.ParseUint(n.atom, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s: bad label %q", n.pos, n.atom)
	}
	return int(v), nil
}

func (e *emitter) local(n node) (uint32, error) {
	if strings.HasPrefix(n.atom, "$") {
		i, ok := e.fn.names[n.atom]
		if !ok {
			return 0, fmt.Errorf("%s: unknown local %s", n.pos, n.atom)
		}
		return i, nil
	}
	v, err := strconv.ParseUint(n.atom, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s: bad local %q", n.pos, n.atom)
	}
	return uint32(v), nil
}

// parseInt reads a WAT integer literal: decimal or 0x hex, optional sign,
// underscores allowed. Values up to the unsigned maximum are accepted and
// wrap, as the text format specifies.
func parseInt(s string, bits int) (int64, error) {
	s = strings.ReplaceAll(s, "_", "")
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
	u, err := strconv.ParseUint(s, 0, bits)
	if err != nil {
		return 0, fmt.Errorf("bad integer %q", s)
	}
	v := int64(u)
	if neg {
		v = -v
	}
	return v, nil
}

func parseFloat(s string, bits int) (float64, error) {
	s = strings.ReplaceAll(s, "_", "")
	switch strings.TrimLeft(s, "+-") {
	case "inf":
		if strings.HasPrefix(s, "-") {
			return math.Inf(-1), nil
		}
		return math.Inf(1), nil
	case "nan":
		return math.NaN(), nil
	}
	return strconv.ParseFloat(s, bits)
}

// ── LEB128 ─────────────────────────────────────────────────────────────────

func uleb(v uint64) []byte {
	var out []byte
	for {
		c := byte(v & 0x7F)
		v >>= 7
		if v != 0 {
			c |= 0x80
		}
		out = append(out, c)
		if v == 0 {
			return out
		}
	}
}

func sleb(v int64) []byte {
	var out []byte
	for {
		c := byte(v & 0x7F)
		v >>= 7
		done := (v == 0 && c&0x40 == 0) || (v == -1 && c&0x40 != 0)
		if !done {
			c |= 0x80
		}
		out = append(out, c)
		if done {
			return out
		}
	}
}

func name(s string) []byte {
	return append(uleb(uint64(len(s))), s...)
}
