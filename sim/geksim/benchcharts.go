package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"html"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// runBenchCharts draws brief 1's charts from data/<run>/bench.csv in an
// article folder. Runs found: before, index (option A only), after.
func runBenchCharts(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("bench-charts", flag.ExitOnError)
	out := fs.String("out", "articles/obsidian/01-tracking-page", "article folder")
	fs.Parse(args)

	var runs []benchRun
	for _, r := range benchRuns {
		rows, err := readCSV(filepath.Join(*out, "data", r.name, "bench.csv"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		r.rows = rows
		runs = append(runs, r)
	}
	if len(runs) == 0 {
		return fmt.Errorf("no bench data in %s/data/*/bench.csv", *out)
	}
	if err := os.MkdirAll(filepath.Join(*out, "visuals"), 0o755); err != nil {
		return err
	}
	charts := map[string]string{
		"parcels-out-by-hour.svg":     parcelsOutChart(runs[0]),
		"answered-by-hour.svg":        answeredChart(runs),
		"answered-by-hour-before.svg": answeredChart(runs[:1]), // for the "Before" section
	}
	for name, svg := range charts {
		if err := os.WriteFile(filepath.Join(*out, "visuals", name), []byte(svg), 0o644); err != nil {
			return err
		}
		fmt.Println("wrote", filepath.Join(*out, "visuals", name))
	}
	return nil
}

type benchRun struct {
	name, legend, color string
	rows                []map[string]string
}

// Colour follows the run, whichever runs are present.
var benchRuns = []benchRun{
	{name: "before", legend: "Before", color: "#a3a29c"},
	{name: "index", legend: "Index only", color: "#8fb8ea"},
	{name: "after", legend: "After the fix", color: series1},
}

const demandCol = "#6b6a65"

func readCSV(path string) ([]map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil || len(recs) < 2 {
		return nil, err
	}
	var rows []map[string]string
	for _, rec := range recs[1:] {
		m := map[string]string{}
		for i, h := range recs[0] {
			m[h] = rec[i]
		}
		rows = append(rows, m)
	}
	return rows, nil
}

// panel is one plot area inside a chart.
type panel struct {
	b              *strings.Builder
	x0, x1, y0, y1 float64 // plot area (y0 top, y1 baseline)
	top            float64 // value at y0
	n              int     // categories
}

func newPanel(b *strings.Builder, x0, x1, y0, y1, maxV float64, n int, tick func(float64) string) panel {
	step := niceStep(maxV / 4)
	p := panel{b: b, x0: x0, x1: x1, y0: y0, y1: y1, top: math.Ceil(maxV/step) * step, n: n}
	for v := 0.0; v <= p.top+step/2; v += step {
		fmt.Fprintf(b, `<line x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f" stroke="%s" stroke-width="1"/>`, x0, x1, p.y(v), p.y(v), gridCol)
		fmt.Fprintf(b, `<text x="%.0f" y="%.1f" font-size="12" fill="%s" text-anchor="end" dominant-baseline="middle">%s</text>`, x0-8, p.y(v), inkMuted, tick(v))
	}
	return p
}

func (p panel) y(v float64) float64 { return p.y0 + (p.y1-p.y0)*(1-v/p.top) }
func (p panel) slot() float64       { return (p.x1 - p.x0) / float64(p.n) }
func (p panel) cx(i int) float64    { return p.x0 + p.slot()*(float64(i)+0.5) }

// bar draws one bar with a 4px rounded data end, square at the baseline.
func (p panel) bar(cx, w, v float64, color, tip string) {
	top, base := p.y(v), p.y1
	if base-top < 0.5 {
		top = base - 0.5 // keep a hairline so a zero still shows as measured
	}
	rad := math.Min(4, (base-top)/2)
	fmt.Fprintf(p.b, `<path d="M%.1f,%.1f V%.1f Q%.1f,%.1f %.1f,%.1f H%.1f Q%.1f,%.1f %.1f,%.1f V%.1f Z" fill="%s"><title>%s</title></path>`,
		cx-w/2, base, top+rad, cx-w/2, top, cx-w/2+rad, top, cx+w/2-rad, cx+w/2, top, cx+w/2, top+rad, base, color, html.EscapeString(tip))
}

func (p panel) xLabels(labels []string) {
	for i, l := range labels {
		fmt.Fprintf(p.b, `<text x="%.1f" y="%.0f" font-size="12" fill="%s" text-anchor="middle">%s</text>`, p.cx(i), p.y1+20, inkMuted, html.EscapeString(l))
	}
}

func svgStart(b *strings.Builder, w, h int, title, subtitle string) {
	fmt.Fprintf(b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" font-family="%s" role="img" aria-label="%s">`,
		w, h, w, h, html.EscapeString(font), html.EscapeString(title))
	fmt.Fprintf(b, `<title>%s</title><rect width="100%%" height="100%%" fill="%s"/>`, html.EscapeString(title), surface)
	fmt.Fprintf(b, `<text x="%d" y="40" font-size="22" font-weight="600" fill="%s">%s</text>`, padL, ink, html.EscapeString(title))
	fmt.Fprintf(b, `<text x="%d" y="66" font-size="14" fill="%s">%s</text>`, padL, inkMuted, html.EscapeString(subtitle))
	fmt.Fprintf(b, `<text x="%d" y="%d" font-size="11" fill="%s" text-anchor="end">GekGo is fictional. Data: geksim seed 42, geksim bench.</text>`, w-padR, h-10, inkMuted)
}

func annotate(b *strings.Builder, x, yMark, yText float64, anchor, text string) {
	fmt.Fprintf(b, `<line x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f" stroke="%s" stroke-width="1"/>`, x, x, yText+6, yMark-6, inkMuted)
	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" font-size="13" fill="%s" text-anchor="%s">%s</text>`, x, yText, ink, anchor, html.EscapeString(text))
}

func hourLabels(rows []map[string]string) []string {
	var ls []string
	for _, r := range rows {
		ls = append(ls, r["label"])
	}
	return ls
}

// parcelsOutChart: where the load really is during the day.
func parcelsOutChart(r benchRun) string {
	var b strings.Builder
	svgStart(&b, chartW, chartH, "The road is busiest at noon, not 5pm",
		"Parcels out for delivery on Tue 10 Nov 2026, on the hour")
	maxV := 0.0
	for _, row := range r.rows {
		maxV = math.Max(maxV, atof(row["out_for_delivery"]))
	}
	p := newPanel(&b, padL, chartW-padR, padT+24, chartH-padB, maxV, len(r.rows), func(v float64) string { return thousands(int(v)) })
	w := math.Min(p.slot()-2, p.slot()*0.62)
	peak := 0
	for i, row := range r.rows {
		v := atof(row["out_for_delivery"])
		if v > atof(r.rows[peak]["out_for_delivery"]) {
			peak = i
		}
		p.bar(p.cx(i), w, v, series1, fmt.Sprintf("%s: %s parcels", row["label"], thousands(int(v))))
	}
	p.xLabels(hourLabels(r.rows))
	pv := atof(r.rows[peak]["out_for_delivery"])
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="13" fill="%s" text-anchor="middle">%s</text>`, p.cx(peak), p.y(pv)-10, ink, thousands(int(pv)))
	for i, row := range r.rows {
		if row["label"] == "17:00" {
			v := atof(row["out_for_delivery"])
			annotate(&b, p.cx(i), p.y(v), p.y(pv)+40, "middle", fmt.Sprintf("17:00: %s, when the phones ring", thousands(int(v))))
		}
	}
	b.WriteString(`</svg>`)
	return b.String()
}

// answeredChart: demand on top, share answered within 10 s below, one bar
// per run in each hour.
func answeredChart(runs []benchRun) string {
	const w, h = 1000, 700
	var b strings.Builder
	title := "Before the fix, the tracking page answered almost nobody"
	if len(runs) > 1 {
		title = "The tracking page, before and after"
	}
	svgStart(&b, w, h, title,
		"Tue 10 Nov 2026. Half of recipients with a parcel out for delivery have the page open; it refreshes every 15 s.")
	base := runs[0].rows
	labels := hourLabels(base)

	// Panel 1: demand.
	fmt.Fprintf(&b, `<text x="%d" y="112" font-size="14" font-weight="600" fill="%s">Tracking requests per second</text>`, padL, ink)
	maxV := 0.0
	for _, r := range base {
		maxV = math.Max(maxV, atof(r["target_rps"]))
	}
	p1 := newPanel(&b, padL, w-padR, 132, 300, maxV, len(base), func(v float64) string { return thousands(int(v)) })
	bw := math.Min(p1.slot()-2, p1.slot()*0.62)
	for i, r := range base {
		p1.bar(p1.cx(i), bw, atof(r["target_rps"]), demandCol, fmt.Sprintf("%s: %.1f requests/s", r["label"], atof(r["target_rps"])))
	}
	p1.xLabels(labels)

	// Panel 2: answered within 10 s.
	fmt.Fprintf(&b, `<text x="%d" y="376" font-size="14" font-weight="600" fill="%s">Answered within 10 seconds</text>`, padL, ink)
	p2 := newPanel(&b, padL, w-padR, 396, 640, 100, len(base), func(v float64) string { return fmt.Sprintf("%.0f%%", v) })
	group := math.Min(p2.slot()-4, p2.slot()*0.72)
	each := group / float64(len(runs))
	worst := 100.0
	for k, run := range runs {
		for i, r := range run.rows {
			v := 100 - atof(r["failed_pct"])
			if run.name == "before" {
				worst = math.Min(worst, v)
			}
			cx := p2.cx(i) - group/2 + each*(float64(k)+0.5)
			p2.bar(cx, each-2, v, run.color, fmt.Sprintf("%s, %s: %.1f%% answered", run.legend, r["label"], v))
		}
	}
	p2.xLabels(labels)
	if len(runs) == 1 {
		best := 0.0
		for _, r := range base {
			best = math.Max(best, 100-atof(r["failed_pct"]))
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="15" fill="%s" text-anchor="middle">Between %.1f%% and %.1f%% of requests answered, at every hour</text>`,
			(p2.x0+p2.x1)/2, p2.y(50), ink, worst, best)
	}
	// Legend for the runs (always, so identity never rests on colour alone).
	lx := float64(w - padR)
	for k := len(runs) - 1; k >= 0; k-- {
		tw := float64(len(runs[k].legend))*7 + 22
		lx -= tw
		fmt.Fprintf(&b, `<rect x="%.1f" y="366" width="12" height="12" rx="2" fill="%s"/><text x="%.1f" y="376" font-size="12" fill="%s">%s</text>`,
			lx, runs[k].color, lx+17, inkMuted, runs[k].legend)
		lx -= 12
	}
	b.WriteString(`</svg>`)
	return b.String()
}
