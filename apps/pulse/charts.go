package main

import (
	"fmt"
	"html/template"
	"strings"
)

// Inline-SVG chart builders — no JavaScript, no chart library. Every value
// interpolated is numeric or passes through template/html escaping upstream,
// and day strings are server-generated YYYY-MM-DD.

// sparkline renders a target's recent latencies as a small polyline. Failed
// checks are marked with accent-red ticks at their position. checks is
// oldest-first; an empty slice yields an em-dash placeholder.
func sparkline(checks []Check) template.HTML {
	const w, h, pad = 120.0, 24.0, 2.0
	if len(checks) == 0 {
		return template.HTML(`<span class="muted">—</span>`)
	}
	maxLat := int64(1)
	for _, c := range checks {
		if c.LatencyMS > maxLat {
			maxLat = c.LatencyMS
		}
	}
	step := (w - 2*pad) / float64(max(len(checks)-1, 1))
	var pts, fails strings.Builder
	for i, c := range checks {
		x := pad + float64(i)*step
		y := (h - pad) - (h-2*pad)*float64(c.LatencyMS)/float64(maxLat)
		fmt.Fprintf(&pts, "%.1f,%.1f ", x, y)
		if !c.OK {
			fmt.Fprintf(&fails,
				`<rect x="%.1f" y="0" width="2" height="%.0f" fill="#d93a00"/>`,
				x-1, h)
		}
	}
	return template.HTML(fmt.Sprintf(
		`<svg class="spark" viewBox="0 0 %.0f %.0f" width="%.0f" height="%.0f" role="img" aria-label="latency, last %d checks, max %d ms">`+
			`%s<polyline points="%s" fill="none" stroke="#0a0a0a" stroke-width="1.2"/></svg>`,
		w, h, w, h, len(checks), maxLat, fails.String(), strings.TrimSpace(pts.String())))
}

// barChart renders per-day counts as a bar chart with a hover <title> per
// bar. days should be a contiguous, ordered range (the handler fills gaps
// with zero days so the x axis is honest).
func barChart(days []DayCount, label string) template.HTML {
	const barW, gap, h, pad = 14.0, 3.0, 72.0, 2.0
	if len(days) == 0 {
		return template.HTML(`<p class="muted">No data in range.</p>`)
	}
	maxN := 1
	for _, d := range days {
		if d.N > maxN {
			maxN = d.N
		}
	}
	w := float64(len(days))*(barW+gap) + gap
	var bars strings.Builder
	for i, d := range days {
		x := gap + float64(i)*(barW+gap)
		bh := (h - 2*pad) * float64(d.N) / float64(maxN)
		if bh < 1 {
			bh = 1 // zero days keep a dim 1px stub, so gaps read as gaps
		}
		fmt.Fprintf(&bars,
			`<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="#0a0a0a" fill-opacity="%s"><title>%s — %d %s</title></rect>`,
			x, h-pad-bh, barW, bh, barOpacity(d.N), d.Day, d.N, label)
	}
	return template.HTML(fmt.Sprintf(
		`<svg class="bars" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" role="img" aria-label="%s per day">%s<line x1="0" y1="%.1f" x2="%.0f" y2="%.1f" stroke="#0a0a0a" stroke-width="1"/></svg>`,
		w, h, label, bars.String(), h-pad+1, w, h-pad+1))
}

// barOpacity dims the zero-day stubs so gaps read as gaps.
func barOpacity(n int) string {
	if n == 0 {
		return "0.12"
	}
	return "1"
}
