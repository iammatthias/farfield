package main

import (
	"fmt"
	"strings"
	"time"
)

// Count is the number of concentric epochs the contract tracks: 11^0 through
// 11^11 blocks.
const Count = 12

// BlockTime is the ballpark seconds-per-block the original site used to turn
// epoch lengths into earth-time. Pre-Merge Ethereum averaged ~13.5s; the Merge
// fixed slots at 12s, but this stays 13.5 because the printed table is part of
// what is being reproduced, not a live measurement.
const BlockTime = 13.5

// DefaultLabels are the twelve epoch names as set on mainnet. The contract
// exposes getEpochLabels() and the owner can change them, so these are only
// the fallback for a cold start with no RPC — the live names win.
var DefaultLabels = [Count]string{
	"Block", "Form", "Structure", "Bloom", "Episode", "Phase",
	"Season", "Revolution", "Aepoch", "Aera", "Arche", "Aeon",
}

// Pow11 is 11^i for i in [0,12). 11^11 fits comfortably in a uint64
// (285,311,670,611), so the whole ladder is exact integer arithmetic.
var Pow11 = func() [Count]uint64 {
	var p [Count]uint64
	p[0] = 1
	for i := 1; i < Count; i++ {
		p[i] = p[i-1] * 11
	}
	return p
}()

// Compute returns the twelve epoch values for a block, mirroring the
// contract's getEpochs(uint256) exactly:
//
//	epochs[i] = ((blockNumber / 11**i) % 11) + 1
//
// Every value is 1-indexed, so each epoch cycles 1..11. The app calls the
// chain for the live reading and uses this for the milestone table and as the
// fallback when the RPC is unreachable; a test pins the two together.
func Compute(block uint64) [Count]uint64 {
	var out [Count]uint64
	for i := 0; i < Count; i++ {
		out[i] = (block/Pow11[i])%11 + 1
	}
	return out
}

// Reading is one rendered moment: a block height and its epochs, carrying the
// labels so a template can zip them without knowing the order.
type Reading struct {
	Block  uint64          `json:"block"`
	Epochs [Count]uint64   `json:"epochs"`
	Labels [Count]string   `json:"labels"`
	Live   bool            `json:"live"` // false when served from the last-known snapshot
	Rows   []LabelledEpoch `json:"-"`
}

// LabelledEpoch pairs one epoch name with its current value, top-down (Aeon
// first) to match the diagram's reversed row order.
type LabelledEpoch struct {
	Label string
	Value uint64
	Index int
}

// NewReading builds a Reading with its display rows filled in.
func NewReading(block uint64, epochs [Count]uint64, labels [Count]string, live bool) Reading {
	r := Reading{Block: block, Epochs: epochs, Labels: labels, Live: live}
	// The diagram draws Object.values(epochs).reverse() — Aeon at the top,
	// Block at the bottom.
	for i := Count - 1; i >= 0; i-- {
		r.Rows = append(r.Rows, LabelledEpoch{Label: labels[i], Value: epochs[i], Index: i})
	}
	return r
}

// At returns the epoch value for one index — the templates ask for specific
// rungs of the ladder (Revolution through Bloom) by position.
func (r Reading) At(i int) uint64 {
	if i < 0 || i >= Count {
		return 0
	}
	return r.Epochs[i]
}

// Label returns the epoch name at one index.
func (r Reading) Label(i int) string {
	if i < 0 || i >= Count {
		return ""
	}
	return r.Labels[i]
}

// SystemRow is one line of the "The System" table: the epoch, its length
// expressed in the previous epoch, the exponent, the block count, and the
// earth-time approximation.
type SystemRow struct {
	Label     string
	PrevLabel string
	Exp       int
	Blocks    uint64
	Time      string
}

// SystemTable reproduces the original's derivation: each epoch is eleven of
// the one below it, and its earth-time is 11^n blocks at BlockTime seconds.
func SystemTable(labels [Count]string) []SystemRow {
	rows := make([]SystemRow, 0, Count)
	for i := 0; i < Count; i++ {
		row := SystemRow{Label: labels[i], Exp: i, Blocks: Pow11[i]}
		if i == 0 {
			// The base unit is stated, not derived.
			row.Time = fmt.Sprintf("%v seconds", BlockTime)
		} else {
			row.PrevLabel = fmt.Sprintf("11 %ss", labels[i-1])
			row.Time = HumanDuration(Pow11[i])
		}
		rows = append(rows, row)
	}
	return rows
}

// HumanDuration renders how long n blocks take in earth-time, reproducing the
// original's date-fns pipeline:
//
//	formatDuration(intervalToDuration({start: 0, end: 1000 * n * 13.5}),
//	               {format: ["years","months","weeks","days","hours","minutes"]})
//
// intervalToDuration is calendar-aware and measures from the Unix epoch, so
// "1 year" here means the real 1970→1971, not 365 days. It never emits a weeks
// field, so listing weeks in the format was a no-op — years, months, days,
// hours, minutes is the effective set. Zero components are dropped.
func HumanDuration(blocks uint64) string {
	// n * 13.5 seconds in milliseconds — exact, because 13.5s is 13500ms.
	ms := int64(blocks) * 13500

	start := time.Unix(0, 0).UTC()
	end := time.UnixMilli(ms).UTC()

	years := fullYears(start, end)
	start = start.AddDate(years, 0, 0)
	months := fullMonths(start, end)
	start = start.AddDate(0, months, 0)

	rest := end.Sub(start)
	days := int(rest / (24 * time.Hour))
	rest -= time.Duration(days) * 24 * time.Hour
	hours := int(rest / time.Hour)
	rest -= time.Duration(hours) * time.Hour
	minutes := int(rest / time.Minute)

	var parts []string
	for _, u := range []struct {
		n    int
		name string
	}{
		{years, "year"}, {months, "month"},
		{days, "day"}, {hours, "hour"}, {minutes, "minute"},
	} {
		if u.n == 0 {
			continue // date-fns formatDuration omits zero components
		}
		if u.n == 1 {
			parts = append(parts, fmt.Sprintf("1 %s", u.name))
			continue
		}
		parts = append(parts, fmt.Sprintf("%d %ss", u.n, u.name))
	}
	if len(parts) == 0 {
		return "0 minutes"
	}
	return strings.Join(parts, " ")
}

// fullYears counts whole calendar years from a to b (date-fns
// differenceInYears): the count of times a's anniversary has passed.
func fullYears(a, b time.Time) int {
	y := b.Year() - a.Year()
	if y > 0 && a.AddDate(y, 0, 0).After(b) {
		y--
	}
	return y
}

// fullMonths counts whole calendar months from a to b (differenceInMonths).
func fullMonths(a, b time.Time) int {
	m := (b.Year()-a.Year())*12 + int(b.Month()) - int(a.Month())
	if m > 0 && a.AddDate(0, m, 0).After(b) {
		m--
	}
	return m
}
