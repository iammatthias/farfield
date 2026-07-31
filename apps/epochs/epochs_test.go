package main

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// archivedEvents are the milestone rows as the original site baked them, taken
// from the __NEXT_DATA__ payload of the 2022-12-08 Wayback capture. The epoch
// values were computed by the deployed contract, so reproducing all twenty of
// them is a direct check that Compute matches getEpochs — no network needed.
var archivedEvents = []struct {
	block  uint64
	label  string
	epochs [Count]uint64
}{
	{0, "Genesis", [Count]uint64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}},
	{46147, "First transaction", [Count]uint64{3, 5, 8, 2, 4, 1, 1, 1, 1, 1, 1, 1}},
	{3914495, "CryptoPunks contract deployed", [Count]uint64{3, 3, 1, 5, 4, 3, 3, 1, 1, 1, 1, 1}},
	{10629366, "Start of Season 7", [Count]uint64{1, 1, 1, 1, 1, 1, 7, 1, 1, 1, 1, 1}},
	{11341538, "Chromie Squiggle#0 Minted", [Count]uint64{11, 8, 1, 8, 5, 5, 7, 1, 1, 1, 1, 1}},
	{11565020, "Zora Deployed", [Count]uint64{6, 8, 11, 10, 9, 6, 7, 1, 1, 1, 1, 1}},
	{11694715, "thesarahshow.eth mints FNDv2#1", [Count]uint64{11, 6, 5, 9, 7, 7, 7, 1, 1, 1, 1, 1}},
	{11748899, "MikeDem loses souljaboy.eth", [Count]uint64{9, 4, 2, 6, 11, 7, 7, 1, 1, 1, 1, 1}},
	{12014171, "Sale of Punk#7804", [Count]uint64{5, 8, 5, 7, 7, 9, 7, 1, 1, 1, 1, 1}},
	{12027953, "Sale of Everydays", [Count]uint64{4, 7, 9, 6, 8, 9, 7, 1, 1, 1, 1, 1}},
	{12061284, "FWB Pro Deployed", [Count]uint64{5, 1, 10, 9, 10, 9, 7, 1, 1, 1, 1, 1}},
	{12108534, "x*y=k Minted", [Count]uint64{10, 6, 4, 1, 3, 10, 7, 1, 1, 1, 1, 1}},
	{12272493, "Solvency Deployed", [Count]uint64{3, 7, 6, 3, 3, 11, 7, 1, 1, 1, 1, 1}},
	{12372205, "Zora Auction House Deployed", [Count]uint64{11, 7, 5, 1, 10, 11, 7, 1, 1, 1, 1, 1}},
	{12376091, "PartyDAO Crowdfund Deployed", [Count]uint64{3, 9, 4, 4, 10, 11, 7, 1, 1, 1, 1, 1}},
	{12400927, "Start of Season 8", [Count]uint64{1, 1, 1, 1, 1, 1, 8, 1, 1, 1, 1, 1}},
	{12995606, "Punk 3269 bought by @houseofhalle", [Count]uint64{9, 8, 9, 7, 8, 4, 8, 1, 1, 1, 1, 1}},
	{13291730, "10.3 ETH bid for Latashá's glo.up remix", [Count]uint64{2, 1, 4, 10, 6, 6, 8, 1, 1, 1, 1, 1}},
	{14172488, "Start of Season 9", [Count]uint64{1, 1, 1, 1, 1, 1, 9, 1, 1, 1, 1, 1}},
	{19487171, "Start of Revolution 2", [Count]uint64{1, 1, 1, 1, 1, 1, 1, 2, 1, 1, 1, 1}},
}

func TestComputeMatchesArchivedContractOutput(t *testing.T) {
	for _, e := range archivedEvents {
		if got := Compute(e.block); got != e.epochs {
			t.Errorf("Compute(%d) [%s]\n got %v\nwant %v", e.block, e.label, got, e.epochs)
		}
	}
}

// TestComputeMatchesLiveCall pins Compute against a return value captured from
// the deployed contract — getEpochs(15537394), the Merge block.
func TestComputeMatchesLiveCall(t *testing.T) {
	ret := loadFixture(t, "testdata/getEpochs_15537394.hex")
	onchain, err := decodeEpochs(ret)
	if err != nil {
		t.Fatalf("decodeEpochs: %v", err)
	}
	if local := Compute(15537394); local != onchain {
		t.Errorf("local %v != on-chain %v", local, onchain)
	}
}

func TestDecodeLabelsFromLiveReturn(t *testing.T) {
	ret := loadFixture(t, "testdata/getEpochLabels.hex")
	got, err := decodeLabels(ret)
	if err != nil {
		t.Fatalf("decodeLabels: %v", err)
	}
	if got != DefaultLabels {
		t.Errorf("labels\n got %v\nwant %v", got, DefaultLabels)
	}
}

// TestDecodersRejectTruncatedReturns makes sure a short or hostile response
// produces an error rather than walking off the end of the buffer.
func TestDecodersRejectTruncatedReturns(t *testing.T) {
	full := loadFixture(t, "testdata/getEpochLabels.hex")
	// Cuts that remove real structure. Lopping only the last byte or two is
	// not included: those bytes are the final string's zero padding, and a
	// return that is short by less than a word still carries every label.
	for _, n := range []int{0, 16, 32, 64, len(full) / 2} {
		if _, err := decodeLabels(full[:n]); err == nil {
			t.Errorf("decodeLabels accepted a %d-byte return", n)
		}
	}
	epochs := loadFixture(t, "testdata/getEpochs_15537394.hex")
	for _, n := range []int{0, 32, Count*32 - 1} {
		if _, err := decodeEpochs(epochs[:n]); err == nil {
			t.Errorf("decodeEpochs accepted a %d-byte return", n)
		}
	}
	// A word claiming a value beyond uint64 must be refused, not truncated.
	oversized := make([]byte, Count*32)
	oversized[0] = 0xff
	if _, err := decodeEpochs(oversized); err == nil {
		t.Error("decodeEpochs accepted an oversized word")
	}
}

// TestEpochsAreOneIndexed guards the +1 in the contract's formula: every value
// must land in 1..11 for any block, including boundaries.
func TestEpochsAreOneIndexed(t *testing.T) {
	blocks := []uint64{0, 1, 10, 11, 12, 120, 121, 1771560, 1771561,
		19487170, 19487171, 25647505, 1 << 40}
	for _, b := range blocks {
		for i, v := range Compute(b) {
			if v < 1 || v > 11 {
				t.Errorf("Compute(%d)[%d] = %d, want 1..11", b, i, v)
			}
		}
	}
}

// TestEpochRollover checks the ladder ticks where it should: an exact multiple
// of 11^n advances epoch n and resets everything below it.
func TestEpochRollover(t *testing.T) {
	season := Pow11[6] // 1,771,561 blocks
	at := Compute(season * 6)
	if at[6] != 7 {
		t.Errorf("Season at 6*11^6 = %d, want 7", at[6])
	}
	for i := 0; i < 6; i++ {
		if at[i] != 1 {
			t.Errorf("epoch %d at a Season boundary = %d, want 1", i, at[i])
		}
	}
	if before := Compute(season*6 - 1); before[6] != 6 {
		t.Errorf("Season one block earlier = %d, want 6", before[6])
	}
}

func TestHumanDuration(t *testing.T) {
	// Expected strings from the original's date-fns pipeline: calendar-aware
	// durations measured from the Unix epoch, zero components dropped.
	cases := []struct {
		blocks uint64
		want   string
	}{
		{11, "2 minutes"},
		{121, "27 minutes"},
		{1331, "4 hours 59 minutes"},
		{14641, "2 days 6 hours 54 minutes"},
	}
	for _, c := range cases {
		if got := HumanDuration(c.blocks); got != c.want {
			t.Errorf("HumanDuration(%d) = %q, want %q", c.blocks, got, c.want)
		}
	}
}

// TestHumanDurationSingularises checks the "1 year" / "2 years" split rather
// than emitting "1 years".
func TestHumanDurationSingularises(t *testing.T) {
	// Walk the output as (count, unit) pairs rather than substring-matching:
	// "21 minutes" trivially contains "1 minutes".
	for _, blocks := range []uint64{11, 1331, 161051, 1771561, Pow11[9], Pow11[11]} {
		got := HumanDuration(blocks)
		fields := strings.Fields(got)
		if len(fields)%2 != 0 {
			t.Fatalf("HumanDuration(%d) = %q: not count/unit pairs", blocks, got)
		}
		for i := 0; i < len(fields); i += 2 {
			count, unit := fields[i], fields[i+1]
			if count == "1" && strings.HasSuffix(unit, "s") {
				t.Errorf("HumanDuration(%d) = %q: pluralised a 1", blocks, got)
			}
			if count != "1" && !strings.HasSuffix(unit, "s") {
				t.Errorf("HumanDuration(%d) = %q: singularised a %s", blocks, got, count)
			}
		}
	}
}

func TestSystemTable(t *testing.T) {
	rows := SystemTable(DefaultLabels)
	if len(rows) != Count {
		t.Fatalf("got %d rows, want %d", len(rows), Count)
	}
	if rows[0].Time != "13.5 seconds" {
		t.Errorf("Block row time = %q, want %q", rows[0].Time, "13.5 seconds")
	}
	if rows[0].PrevLabel != "" {
		t.Errorf("Block row should have no derivation, got %q", rows[0].PrevLabel)
	}
	if rows[1].PrevLabel != "11 Blocks" {
		t.Errorf("Form derivation = %q, want %q", rows[1].PrevLabel, "11 Blocks")
	}
	if rows[11].Blocks != 285311670611 {
		t.Errorf("Aeon blocks = %d, want 285311670611", rows[11].Blocks)
	}
}

func TestCommas(t *testing.T) {
	cases := map[uint64]string{
		0: "0", 7: "7", 999: "999", 1000: "1,000", 46147: "46,147",
		15537394: "15,537,394", 285311670611: "285,311,670,611",
	}
	for in, want := range cases {
		if got := commas(in); got != want {
			t.Errorf("commas(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestDiagramGeometry reproduces the original SVG: twelve rows of eleven
// circles, Aeon first, with exactly one filled circle per row at value-1.
func TestDiagramGeometry(t *testing.T) {
	block := uint64(15537394)
	epochs := Compute(block)
	rows := Diagram(NewReading(block, epochs, DefaultLabels, true))

	if len(rows) != Count {
		t.Fatalf("got %d rows, want %d", len(rows), Count)
	}
	if rows[0].Label != "Aeon" {
		t.Errorf("first row = %q, want Aeon (rows run reversed)", rows[0].Label)
	}
	if rows[Count-1].Label != "Block" {
		t.Errorf("last row = %q, want Block", rows[Count-1].Label)
	}
	for i, row := range rows {
		if len(row.Circles) != 11 {
			t.Fatalf("row %d has %d circles, want 11", i, len(row.Circles))
		}
		if row.Y != i*diagramRowStep {
			t.Errorf("row %d y = %d, want %d", i, row.Y, i*diagramRowStep)
		}
		// Row i shows epoch index Count-1-i.
		want := epochs[Count-1-i]
		filled := 0
		for n, c := range row.Circles {
			if c.CX != 8+diagramRowStep*n+2 {
				t.Errorf("row %d circle %d cx = %d", i, n, c.CX)
			}
			if c.Filled {
				filled++
				if uint64(n) != want-1 {
					t.Errorf("row %d filled at %d, want %d", i, n, want-1)
				}
			}
		}
		if filled != 1 {
			t.Errorf("row %d has %d filled circles, want 1", i, filled)
		}
	}
}

func TestMilestoneRowsSortedWithCurrentBlock(t *testing.T) {
	current := NewReading(25647505, Compute(25647505), DefaultLabels, true)
	rows := MilestoneRows(current)

	if len(rows) != len(milestones)+1 {
		t.Fatalf("got %d rows, want %d", len(rows), len(milestones)+1)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].Block > rows[i].Block {
			t.Fatalf("rows not sorted at %d: %d > %d", i, rows[i-1].Block, rows[i].Block)
		}
	}
	var found bool
	for _, r := range rows {
		if r.Label == "Current Block" {
			found = true
			if r.Block != current.Block {
				t.Errorf("Current Block at %d, want %d", r.Block, current.Block)
			}
			if r.Hash != "" {
				t.Error("Current Block should have no transaction hash")
			}
		}
	}
	if !found {
		t.Error("Current Block row missing")
	}
	// Every recovered event's epochs must be recomputed, not left zeroed.
	for _, r := range rows {
		if r.Epochs[0] == 0 {
			t.Errorf("%q has unpopulated epochs", r.Label)
		}
	}
}

// TestArchivedMilestonesPreserved guards the recovered dataset against being
// silently trimmed, and keeps the two deliberate quirks from being "fixed".
func TestArchivedMilestonesPreserved(t *testing.T) {
	if len(milestones) != 20 {
		t.Errorf("got %d milestones, want the 20 recovered from the capture", len(milestones))
	}
	byLabel := map[string]Milestone{}
	for _, m := range milestones {
		byLabel[m.Label] = m
	}
	// A contract address stood in for a transaction hash in the original data.
	if got := byLabel["CryptoPunks contract deployed"].Hash; got != "0xb47e3cd837dDF8e4c57F05d70Ab865de6e193BBB" {
		t.Errorf("CryptoPunks hash = %q; the original's address-in-hash quirk is intentional", got)
	}
	if byLabel["Genesis"].Hash != "" {
		t.Error("Genesis should have no hash, so it renders italic")
	}
}

func TestReadingAccessorsAreBoundsSafe(t *testing.T) {
	r := NewReading(100, Compute(100), DefaultLabels, true)
	if r.At(-1) != 0 || r.At(Count) != 0 {
		t.Error("At should return 0 out of range")
	}
	if r.Label(-1) != "" || r.Label(Count) != "" {
		t.Error("Label should return empty out of range")
	}
	if r.Label(6) != "Season" {
		t.Errorf("Label(6) = %q, want Season", r.Label(6))
	}
}

func loadFixture(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(string(raw)), "0x"))
	if err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	return b
}
