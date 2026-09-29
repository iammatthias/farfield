package wat

// immKind says what immediates an instruction carries after its opcode.
type immKind int

const (
	immNone    immKind = iota
	immBlock           // block type (label + optional result)
	immLabel           // one label index
	immBrTable         // vector of labels + default
	immFunc            // function index
	immLocal           // local index
	immGlobal          // global index
	immMem             // memarg: align, offset
	immI32             // i32 const
	immI64             // i64 const
	immF32             // f32 const
	immF64             // f64 const
	immMemIdx          // a single 0x00 memory index (memory.size / grow)
	immMemCopy         // two memory indexes (memory.copy)
	immCallInd         // type index + table index
)

// op is one instruction: its encoding and immediates. Prefixed instructions
// (0xFC) carry their sub-opcode in sub.
type op struct {
	code     byte
	prefixed bool
	sub      uint32
	imm      immKind
	// natural alignment (log2 bytes) for memory instructions.
	align uint32
}

// ops maps every instruction name this assembler knows to its encoding: the
// full WebAssembly 1.0 (MVP) instruction set, plus sign-extension, the
// saturating truncations, and bulk memory (memory.copy / memory.fill), which
// every engine the editor targets supports.
var ops = map[string]op{
	// control
	"unreachable":   {code: 0x00},
	"nop":           {code: 0x01},
	"block":         {code: 0x02, imm: immBlock},
	"loop":          {code: 0x03, imm: immBlock},
	"if":            {code: 0x04, imm: immBlock},
	"else":          {code: 0x05},
	"end":           {code: 0x0B},
	"br":            {code: 0x0C, imm: immLabel},
	"br_if":         {code: 0x0D, imm: immLabel},
	"br_table":      {code: 0x0E, imm: immBrTable},
	"return":        {code: 0x0F},
	"call":          {code: 0x10, imm: immFunc},
	"call_indirect": {code: 0x11, imm: immCallInd},

	// parametric
	"drop":   {code: 0x1A},
	"select": {code: 0x1B},

	// variables
	"local.get":  {code: 0x20, imm: immLocal},
	"local.set":  {code: 0x21, imm: immLocal},
	"local.tee":  {code: 0x22, imm: immLocal},
	"global.get": {code: 0x23, imm: immGlobal},
	"global.set": {code: 0x24, imm: immGlobal},

	// memory
	"i32.load":     {code: 0x28, imm: immMem, align: 2},
	"i64.load":     {code: 0x29, imm: immMem, align: 3},
	"f32.load":     {code: 0x2A, imm: immMem, align: 2},
	"f64.load":     {code: 0x2B, imm: immMem, align: 3},
	"i32.load8_s":  {code: 0x2C, imm: immMem, align: 0},
	"i32.load8_u":  {code: 0x2D, imm: immMem, align: 0},
	"i32.load16_s": {code: 0x2E, imm: immMem, align: 1},
	"i32.load16_u": {code: 0x2F, imm: immMem, align: 1},
	"i64.load8_s":  {code: 0x30, imm: immMem, align: 0},
	"i64.load8_u":  {code: 0x31, imm: immMem, align: 0},
	"i64.load16_s": {code: 0x32, imm: immMem, align: 1},
	"i64.load16_u": {code: 0x33, imm: immMem, align: 1},
	"i64.load32_s": {code: 0x34, imm: immMem, align: 2},
	"i64.load32_u": {code: 0x35, imm: immMem, align: 2},
	"i32.store":    {code: 0x36, imm: immMem, align: 2},
	"i64.store":    {code: 0x37, imm: immMem, align: 3},
	"f32.store":    {code: 0x38, imm: immMem, align: 2},
	"f64.store":    {code: 0x39, imm: immMem, align: 3},
	"i32.store8":   {code: 0x3A, imm: immMem, align: 0},
	"i32.store16":  {code: 0x3B, imm: immMem, align: 1},
	"i64.store8":   {code: 0x3C, imm: immMem, align: 0},
	"i64.store16":  {code: 0x3D, imm: immMem, align: 1},
	"i64.store32":  {code: 0x3E, imm: immMem, align: 2},
	"memory.size":  {code: 0x3F, imm: immMemIdx},
	"memory.grow":  {code: 0x40, imm: immMemIdx},
	"memory.copy":  {code: 0xFC, prefixed: true, sub: 10, imm: immMemCopy},
	"memory.fill":  {code: 0xFC, prefixed: true, sub: 11, imm: immMemIdx},

	// constants
	"i32.const": {code: 0x41, imm: immI32},
	"i64.const": {code: 0x42, imm: immI64},
	"f32.const": {code: 0x43, imm: immF32},
	"f64.const": {code: 0x44, imm: immF64},

	// i32 comparison
	"i32.eqz": {code: 0x45}, "i32.eq": {code: 0x46}, "i32.ne": {code: 0x47},
	"i32.lt_s": {code: 0x48}, "i32.lt_u": {code: 0x49}, "i32.gt_s": {code: 0x4A},
	"i32.gt_u": {code: 0x4B}, "i32.le_s": {code: 0x4C}, "i32.le_u": {code: 0x4D},
	"i32.ge_s": {code: 0x4E}, "i32.ge_u": {code: 0x4F},
	// i64 comparison
	"i64.eqz": {code: 0x50}, "i64.eq": {code: 0x51}, "i64.ne": {code: 0x52},
	"i64.lt_s": {code: 0x53}, "i64.lt_u": {code: 0x54}, "i64.gt_s": {code: 0x55},
	"i64.gt_u": {code: 0x56}, "i64.le_s": {code: 0x57}, "i64.le_u": {code: 0x58},
	"i64.ge_s": {code: 0x59}, "i64.ge_u": {code: 0x5A},
	// f32 comparison
	"f32.eq": {code: 0x5B}, "f32.ne": {code: 0x5C}, "f32.lt": {code: 0x5D},
	"f32.gt": {code: 0x5E}, "f32.le": {code: 0x5F}, "f32.ge": {code: 0x60},
	// f64 comparison
	"f64.eq": {code: 0x61}, "f64.ne": {code: 0x62}, "f64.lt": {code: 0x63},
	"f64.gt": {code: 0x64}, "f64.le": {code: 0x65}, "f64.ge": {code: 0x66},

	// i32 arithmetic
	"i32.clz": {code: 0x67}, "i32.ctz": {code: 0x68}, "i32.popcnt": {code: 0x69},
	"i32.add": {code: 0x6A}, "i32.sub": {code: 0x6B}, "i32.mul": {code: 0x6C},
	"i32.div_s": {code: 0x6D}, "i32.div_u": {code: 0x6E}, "i32.rem_s": {code: 0x6F},
	"i32.rem_u": {code: 0x70}, "i32.and": {code: 0x71}, "i32.or": {code: 0x72},
	"i32.xor": {code: 0x73}, "i32.shl": {code: 0x74}, "i32.shr_s": {code: 0x75},
	"i32.shr_u": {code: 0x76}, "i32.rotl": {code: 0x77}, "i32.rotr": {code: 0x78},
	// i64 arithmetic
	"i64.clz": {code: 0x79}, "i64.ctz": {code: 0x7A}, "i64.popcnt": {code: 0x7B},
	"i64.add": {code: 0x7C}, "i64.sub": {code: 0x7D}, "i64.mul": {code: 0x7E},
	"i64.div_s": {code: 0x7F}, "i64.div_u": {code: 0x80}, "i64.rem_s": {code: 0x81},
	"i64.rem_u": {code: 0x82}, "i64.and": {code: 0x83}, "i64.or": {code: 0x84},
	"i64.xor": {code: 0x85}, "i64.shl": {code: 0x86}, "i64.shr_s": {code: 0x87},
	"i64.shr_u": {code: 0x88}, "i64.rotl": {code: 0x89}, "i64.rotr": {code: 0x8A},
	// f32 arithmetic
	"f32.abs": {code: 0x8B}, "f32.neg": {code: 0x8C}, "f32.ceil": {code: 0x8D},
	"f32.floor": {code: 0x8E}, "f32.trunc": {code: 0x8F}, "f32.nearest": {code: 0x90},
	"f32.sqrt": {code: 0x91}, "f32.add": {code: 0x92}, "f32.sub": {code: 0x93},
	"f32.mul": {code: 0x94}, "f32.div": {code: 0x95}, "f32.min": {code: 0x96},
	"f32.max": {code: 0x97}, "f32.copysign": {code: 0x98},
	// f64 arithmetic
	"f64.abs": {code: 0x99}, "f64.neg": {code: 0x9A}, "f64.ceil": {code: 0x9B},
	"f64.floor": {code: 0x9C}, "f64.trunc": {code: 0x9D}, "f64.nearest": {code: 0x9E},
	"f64.sqrt": {code: 0x9F}, "f64.add": {code: 0xA0}, "f64.sub": {code: 0xA1},
	"f64.mul": {code: 0xA2}, "f64.div": {code: 0xA3}, "f64.min": {code: 0xA4},
	"f64.max": {code: 0xA5}, "f64.copysign": {code: 0xA6},

	// conversions
	"i32.wrap_i64":        {code: 0xA7},
	"i32.trunc_f32_s":     {code: 0xA8},
	"i32.trunc_f32_u":     {code: 0xA9},
	"i32.trunc_f64_s":     {code: 0xAA},
	"i32.trunc_f64_u":     {code: 0xAB},
	"i64.extend_i32_s":    {code: 0xAC},
	"i64.extend_i32_u":    {code: 0xAD},
	"i64.trunc_f32_s":     {code: 0xAE},
	"i64.trunc_f32_u":     {code: 0xAF},
	"i64.trunc_f64_s":     {code: 0xB0},
	"i64.trunc_f64_u":     {code: 0xB1},
	"f32.convert_i32_s":   {code: 0xB2},
	"f32.convert_i32_u":   {code: 0xB3},
	"f32.convert_i64_s":   {code: 0xB4},
	"f32.convert_i64_u":   {code: 0xB5},
	"f32.demote_f64":      {code: 0xB6},
	"f64.convert_i32_s":   {code: 0xB7},
	"f64.convert_i32_u":   {code: 0xB8},
	"f64.convert_i64_s":   {code: 0xB9},
	"f64.convert_i64_u":   {code: 0xBA},
	"f64.promote_f32":     {code: 0xBB},
	"i32.reinterpret_f32": {code: 0xBC},
	"i64.reinterpret_f64": {code: 0xBD},
	"f32.reinterpret_i32": {code: 0xBE},
	"f64.reinterpret_i64": {code: 0xBF},

	// sign extension
	"i32.extend8_s":  {code: 0xC0},
	"i32.extend16_s": {code: 0xC1},
	"i64.extend8_s":  {code: 0xC2},
	"i64.extend16_s": {code: 0xC3},
	"i64.extend32_s": {code: 0xC4},

	// saturating truncation
	"i32.trunc_sat_f32_s": {code: 0xFC, prefixed: true, sub: 0},
	"i32.trunc_sat_f32_u": {code: 0xFC, prefixed: true, sub: 1},
	"i32.trunc_sat_f64_s": {code: 0xFC, prefixed: true, sub: 2},
	"i32.trunc_sat_f64_u": {code: 0xFC, prefixed: true, sub: 3},
	"i64.trunc_sat_f32_s": {code: 0xFC, prefixed: true, sub: 4},
	"i64.trunc_sat_f32_u": {code: 0xFC, prefixed: true, sub: 5},
	"i64.trunc_sat_f64_s": {code: 0xFC, prefixed: true, sub: 6},
	"i64.trunc_sat_f64_u": {code: 0xFC, prefixed: true, sub: 7},
}
