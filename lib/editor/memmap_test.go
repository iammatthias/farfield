package editor

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestMemoryMapHasNoOverlaps reads every fixed region's address from the
// .wat sources and checks, against its size, that no two regions share
// memory. Two once did — image placements and the emphasis parser's stack —
// and an image vanished whenever emphasis followed it.
func TestMemoryMapHasNoOverlaps(t *testing.T) {
	srcs, err := Sources()
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, s := range srcs {
		all.WriteString(s.Text)
	}
	addr := map[string]int64{}
	re := regexp.MustCompile(`\(global \$([A-Z_]+)\s+i32\s+\(i32\.const\s+(0x[0-9A-Fa-f]+|\d+)\)\)`)
	for _, m := range re.FindAllStringSubmatch(all.String(), -1) {
		v, err := strconv.ParseInt(m[2], 0, 64)
		if err != nil {
			t.Fatal(err)
		}
		addr[m[1]] = v
	}
	const KiB, MiB = 1 << 10, 1 << 20
	// region → size. A region missing from the sources fails the test, so a
	// rename cannot quietly drop out of the check.
	size := map[string]int64{
		"PALETTE": 16 * 4, "CARET_OUT": 32, "GAMMA": 256, "FONTREC": 8 * 64,
		"IMGTAB": 64 * 32, "OPENERS": 64 * 12, "PLACEHOLDER": KiB, "SCRATCH": 256,
		"PLACE": 128 * 20,
		"IO":    4 * MiB, "FONTS": 2 * MiB, "GMAP": 256 * KiB, "GIDX": 512 * KiB,
		"GBMP": 6 * MiB, "RASTER": 0x00DA0000 - 0x00CD0000, "PTS": 0x00DC0000 - 0x00DA0000,
		"PTSF": 0x00DD0000 - 0x00DC0000, "STYLE": 4 * MiB, "LINES": 2 * MiB,
		"UNDO": 4 * MiB, "TEXT": 4 * MiB, "DICT": 1 * MiB, "FB": 64 * MiB,
	}
	type region struct {
		name   string
		lo, hi int64
	}
	var rs []region
	for name, n := range size {
		a, ok := addr[name]
		if !ok {
			t.Fatalf("region %s is in the map but not in the sources", name)
		}
		rs = append(rs, region{name, a, a + n})
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].lo < rs[j].lo })
	for i := 1; i < len(rs); i++ {
		if rs[i].lo < rs[i-1].hi {
			t.Errorf("%s (%#x–%#x) overlaps %s (%#x–%#x)",
				rs[i].name, rs[i].lo, rs[i].hi, rs[i-1].name, rs[i-1].lo, rs[i-1].hi)
		}
	}
	if addr["IMG_HEAP"] < addr["FB"]+64*MiB {
		t.Errorf("the image heap (%#x) starts inside the framebuffer's 64 MiB", addr["IMG_HEAP"])
	}
	if addr["DICT_SLOTS"]*4 != size["DICT"] {
		t.Errorf("DICT_SLOTS × 4 = %d, but the region is %d bytes", addr["DICT_SLOTS"]*4, size["DICT"])
	}
	pages := regexp.MustCompile(`\(memory \(export "memory"\) (\d+)\)`).FindStringSubmatch(all.String())
	if pages == nil {
		t.Fatal("no memory declaration")
	}
	n, _ := strconv.ParseInt(pages[1], 10, 64)
	if n*64*KiB <= addr["FB"] {
		t.Errorf("initial memory (%d pages) does not reach the framebuffer at %#x", n, addr["FB"])
	}
}
