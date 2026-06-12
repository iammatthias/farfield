package main

import (
	"crypto/sha256"
	"fmt"
)

// Zone is the day's color identity — an ink palette generated fresh for every
// date from the date alone, the way Terraforms' zones give every token its
// palette while biomes give the character set. There is no preset table: the
// date hashes under its own "art-zone" domain and the hash drives a small
// procedural press — a base hue anywhere on the wheel, a bounded chroma, a
// four-ink ramp darkening monotonically from light to dark with a slight
// seed-derived hue drift, and a paper wash that is a near-surface tint of the
// base hue. The zone supplies every ink the day is printed in, across the SVG
// plate, the structure slice, and the three.js scenes.
type Zone struct {
	Name string
	Inks [4]string // elevation inks, low → high band, light → dark
	Wash string    // the day's paper tint — a pale ground under the inks
}

// ── the generator ──────────────────────────────────────────────────────────
//
// Colors are designed in OKLCH (perceptually even lightness and chroma, hue
// in degrees) and converted to sRGB. The taste guardrails live in the
// parameter bounds, not in a table:
//
//   hue      h[0..1] → a continuous 16-bit base hue, 0–360° — a year of
//            dates spreads across the whole wheel
//   chroma   h[3]    → C 0.030–0.085: instrument inks, never candy; chroma
//            rises another ≤25% toward the dark end of the ramp, the way
//            pigment concentrates
//   ramp     four inks, OKLab lightness from ~0.70 (h[4] jitters 0.680–0.720)
//            down to ~0.34 (h[5] jitters 0.320–0.360), evenly spaced —
//            monotonic darkening by construction (≈ HSL 62% → 20%)
//   drift    h[2]    → the hue walks up to ±20° across the ramp, so the dark
//            inks lean off the light ones like a real pigment series
//   wash     L 0.955, C 0.012 at the base hue — always a breath off the
//            page surface, never a color field
//
// Out-of-gamut results (deep blues at low lightness, mostly) are mapped back
// by desaturating: chroma steps down 1/16 at a time until the color fits, so
// hue and lightness — the structure of the ramp — are preserved exactly.
//
// All arithmetic is fixed-point integer math (Q16, with an embedded sRGB
// transfer table and a Taylor sine), so the palette bytes are deterministic
// on every platform forever — the same guarantee the SVG coordinates make.

// zoneFor generates the zone for one calendar day. The date hashes under its
// own "art-zone" domain prefix, so the day's palette is independent of its
// biome (a 4-D neighborhood property) and of the art seed's terrain stream —
// same date, same zone, everywhere it is drawn.
func zoneFor(dateISO string) Zone {
	return zoneFromSum(sha256.Sum256([]byte("art-zone:" + dateISO)))
}

// zoneFromSum derives the full palette from one hash. Split out so tests can
// prove the domain prefix matters.
func zoneFromSum(h [32]byte) Zone {
	hue := int64(h[0])<<8 | int64(h[1])         // base hue, Q16 turns — continuous
	drift := (2*int64(h[2]) - 255) * 3641 / 255 // ±20° across the ramp, Q16 turns
	chroma := 1966 + int64(h[3])*3604/255       // C 0.030 … 0.085
	lHi := 44564 + int64(h[4])*2621/255         // ink 0 lightness, L 0.680 … 0.720
	lLo := 20972 + int64(h[5])*2621/255         // ink 3 lightness, L 0.320 … 0.360

	var z Zone
	z.Name = zoneName(hue, chroma)
	for i := int64(0); i < 4; i++ {
		l := lHi + (lLo-lHi)*i/3
		c := chroma * (12 + i) / 12 // chroma concentrates toward the dark end
		z.Inks[i] = oklchHex(l, c, (hue+drift*i/3)&0xffff)
	}
	z.Wash = oklchHex(62587, 786, hue) // L 0.955, C 0.012 — the paper tint
	return z
}

// ── the caption vocabulary ─────────────────────────────────────────────────
//
// Hue is continuous but captions stay evocative: the base hue maps into
// sixteen ink-family words, with two reserved for the near-neutral days
// (C < 0.038 — about one day in seven): "bone" when the gray leans warm
// (45–135°), "graphite" otherwise. The fourteen hue families, in OKLCH hue
// order:
//
//	 315–20°  clay        fired pink-tans, rose
//	  20–45°  oxide       iron reds
//	  45–70°  rust        orange-browns
//	  70–90°  sepia       umber browns
//	 90–110°  ochre       earth golds
//	110–130°  kelp        olive sea-greens
//	130–155°  moss        leaf greens
//	155–175°  lichen      gray-greens
//	175–195°  verdigris   weathered copper greens
//	195–215°  tarn        dark lake teals
//	215–240°  cyanotype   process blues
//	240–260°  slate       gray-blues
//	260–285°  midnight    indigo blue-blacks
//	285–315°  heath       violet-grays
func zoneName(hue, chroma int64) string {
	deg := hue * 360 >> 16
	if chroma < 2490 { // C < 0.038 — a near-neutral instrument gray
		if deg >= 45 && deg < 135 {
			return "bone"
		}
		return "graphite"
	}
	switch {
	case deg < 20:
		return "clay"
	case deg < 45:
		return "oxide"
	case deg < 70:
		return "rust"
	case deg < 90:
		return "sepia"
	case deg < 110:
		return "ochre"
	case deg < 130:
		return "kelp"
	case deg < 155:
		return "moss"
	case deg < 175:
		return "lichen"
	case deg < 195:
		return "verdigris"
	case deg < 215:
		return "tarn"
	case deg < 240:
		return "cyanotype"
	case deg < 260:
		return "slate"
	case deg < 285:
		return "midnight"
	case deg < 315:
		return "heath"
	default:
		return "clay"
	}
}

// ── OKLCH → sRGB, in integers ──────────────────────────────────────────────

const fpOne = 1 << 16 // Q16 unity

// oklchHex converts an OKLCH color (lightness and chroma in Q16, hue in Q16
// turns) to a lowercase sRGB hex string, desaturating in 1/16 steps until the
// color is inside the sRGB gamut — hue and lightness never move.
func oklchHex(l, c, hue int64) string {
	for {
		a := (c * cosQ16(hue)) >> 16
		b := (c * sinQ16(hue)) >> 16
		// OKLab → cube-root LMS (coefficients in Q16) → LMS → linear sRGB.
		lp := l + (25974*a+14143*b)>>16
		mp := l + (-6918*a-4185*b)>>16
		sp := l + (-5864*a-84639*b)>>16
		l3, m3, s3 := cube16(lp), cube16(mp), cube16(sp)
		r := (267173*l3 - 216774*m3 + 15137*s3) >> 16
		g := (-83128*l3 + 171033*m3 - 22369*s3) >> 16
		bb := (-275*l3 - 46099*m3 + 111910*s3) >> 16
		if c == 0 { // the gray axis is the fixed point — clamp and emit
			return fmt.Sprintf("#%02x%02x%02x",
				srgb8(clamp16(r)), srgb8(clamp16(g)), srgb8(clamp16(bb)))
		}
		if r >= 0 && r <= fpOne && g >= 0 && g <= fpOne && bb >= 0 && bb <= fpOne {
			return fmt.Sprintf("#%02x%02x%02x", srgb8(r), srgb8(g), srgb8(bb))
		}
		c = c * 15 / 16 // gamut-map by desaturating toward the gray axis
		if c < 64 {
			c = 0
		}
	}
}

func clamp16(v int64) int64 {
	if v < 0 {
		return 0
	}
	if v > fpOne {
		return fpOne
	}
	return v
}

// cube16 cubes a Q16 value, rescaling after each multiply.
func cube16(v int64) int64 {
	return (((v * v) >> 16) * v) >> 16
}

// sinQ16 returns sin(2π·t) in Q16 for t in Q16 turns. Range reduction by
// quadrant symmetry, then a Taylor series through x⁹ in Q30 — error well
// under one Q16 step, and bit-identical on every platform.
func sinQ16(t int64) int64 {
	t &= 0xffff
	neg := false
	if t >= 32768 {
		t -= 32768
		neg = true
	}
	if t > 16384 {
		t = 32768 - t
	}
	x := t * 102944 // radians in Q30: t/65536 turns × 2π = t × 2π·2¹⁴
	x2 := (x * x) >> 30
	s := int64(1<<30) - x2/72
	s = int64(1<<30) - ((x2/42)*s)>>30
	s = int64(1<<30) - ((x2/20)*s)>>30
	s = int64(1<<30) - ((x2/6)*s)>>30
	v := ((x * s) >> 30) >> 14
	if neg {
		return -v
	}
	return v
}

// cosQ16 returns cos(2π·t) in Q16 for t in Q16 turns.
func cosQ16(t int64) int64 {
	return sinQ16(t + 16384)
}

// srgb8 applies the sRGB transfer function to a linear Q16 channel and
// returns the 8-bit value, by nearest match against the exact linearization
// table — no floating point anywhere.
func srgb8(lin int64) int {
	if lin <= 0 {
		return 0
	}
	if lin >= fpOne {
		return 255
	}
	x := uint32(lin << 8) // Q24, like the table
	lo, hi := 0, 255
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if srgbLinTab[mid] <= x {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	if lo < 255 && srgbLinTab[lo+1]-x < x-srgbLinTab[lo] {
		lo++
	}
	return lo
}

// srgbLinTab[v] is the linear value of sRGB byte v in Q24 — the standard
// piecewise transfer (v/255 ≤ 0.04045 → /12.92, else ((v/255+0.055)/1.055)^2.4),
// precomputed exactly so the conversion is pure integer comparison.
var srgbLinTab = [256]uint32{
	0, 5092, 10185, 15277, 20369, 25462, 30554, 35646,
	40739, 45831, 50923, 56146, 61682, 67524, 73676, 80144,
	86931, 94043, 101483, 109255, 117364, 125813, 134607, 143749,
	153244, 163095, 173306, 183880, 194821, 206133, 217819, 229883,
	242327, 255157, 268373, 281981, 295983, 310382, 325182, 340386,
	355996, 372016, 388449, 405298, 422565, 440255, 458369, 476910,
	495881, 515286, 535127, 555406, 576126, 597291, 618902, 640963,
	663476, 686443, 709868, 733752, 758099, 782910, 808189, 833938,
	860159, 886854, 914027, 941680, 969814, 998433, 1027538, 1057133,
	1087218, 1117798, 1148873, 1180447, 1212520, 1245097, 1278179, 1311767,
	1345865, 1380475, 1415598, 1451237, 1487394, 1524071, 1561270, 1598994,
	1637244, 1676023, 1715332, 1755173, 1795550, 1836463, 1877915, 1919907,
	1962442, 2005522, 2049149, 2093324, 2138049, 2183328, 2229161, 2275550,
	2322497, 2370005, 2418074, 2466708, 2515908, 2565675, 2616012, 2666920,
	2718402, 2770458, 2823092, 2876304, 2930097, 2984472, 3039432, 3094977,
	3151110, 3207832, 3265145, 3323052, 3381553, 3440650, 3500346, 3560641,
	3621538, 3683038, 3745144, 3807855, 3871176, 3935106, 3999648, 4064803,
	4130573, 4196960, 4263965, 4331589, 4399836, 4468706, 4538200, 4608321,
	4679069, 4750448, 4822457, 4895099, 4968376, 5042288, 5116838, 5192027,
	5267856, 5344328, 5421443, 5499204, 5577611, 5656667, 5736372, 5816729,
	5897738, 5979402, 6061722, 6144699, 6228335, 6312631, 6397589, 6483210,
	6569496, 6656448, 6744068, 6832357, 6921317, 7010948, 7101253, 7192233,
	7283889, 7376223, 7469237, 7562930, 7657306, 7752366, 7848110, 7944540,
	8041658, 8139465, 8237963, 8337152, 8437035, 8537612, 8638885, 8740855,
	8843524, 8946893, 9050964, 9155737, 9261215, 9367397, 9474287, 9581885,
	9690192, 9799210, 9908940, 10019383, 10130542, 10242416, 10355008, 10468318,
	10582349, 10697100, 10812575, 10928773, 11045697, 11163346, 11281724, 11400831,
	11520668, 11641236, 11762538, 11884573, 12007344, 12130852, 12255098, 12380082,
	12505807, 12632274, 12759484, 12887438, 13016137, 13145583, 13275776, 13406719,
	13538412, 13670857, 13804054, 13938006, 14072712, 14208175, 14344396, 14481375,
	14619114, 14757615, 14896878, 15036905, 15177696, 15319253, 15461578, 15604671,
	15748533, 15893166, 16038571, 16184750, 16331702, 16479430, 16627934, 16777216,
}
