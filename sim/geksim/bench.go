package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gekgo/internal/dbx"
)

// runBench replays the load tracking-ingest sees at one moment of a
// simulated day, against the running service, and measures it.
//
//  1. Freeze: every job that was still in progress at -at is put back into
//     the status it had then (from job_events), so the tracking page sees the
//     same parcels on the road as it would have.
//  2. Load: recipients poll GET /v1/jobs/{id}/track (open loop, fixed rate,
//     so a slow server cannot slow the load down), while every driver on
//     shift posts a GPS ping every 10 s.
//  3. Restore: job statuses go back to their final state and the pings the
//     bench sent are deleted.
//
// The traffic model is an assumption, stated in every article that uses it:
// recipients get the tracking link by SMS when their parcel goes out for
// delivery; a share of them (-watching) have the page open, and it refreshes
// every -poll.
func runBench(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	target := fs.String("target", "http://localhost:8082", "tracking-ingest base URL")
	atStr := fs.String("at", "2026-11-10T17:00", "moment to replay, Europe/London (YYYY-MM-DDTHH:MM)")
	watching := fs.Float64("watching", 0.5, "share of recipients with a parcel out for delivery who have the tracking page open")
	poll := fs.Duration("poll", 15*time.Second, "how often an open tracking page refreshes")
	dur := fs.Duration("duration", 60*time.Second, "how long to run the load")
	timeout := fs.Duration("timeout", 10*time.Second, "a request slower than this counts as failed (the page gives up)")
	label := fs.String("label", "", "name for this run in the output (default: the time of day)")
	out := fs.String("out", "", "article folder; appends to data/<run>/bench.csv and writes data/<run>/latency-<label>.csv")
	run := fs.String("run", "before", "which version of the code this measures: before, index, after")
	fs.Parse(args)

	loc, _ := time.LoadLocation("Europe/London")
	at, err := time.ParseInLocation("2006-01-02T15:04", *atStr, loc)
	if err != nil {
		return fmt.Errorf("-at: %w", err)
	}
	if *label == "" {
		*label = at.Format("15:04")
	}
	pool, err := dbx.Connect(ctx, dbx.URL())
	if err != nil {
		return err
	}
	defer pool.Close()

	// ---- freeze
	jobs, err := freezeAt(ctx, pool, at)
	if err != nil {
		return err
	}
	// Restore even if the run is interrupted.
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := restoreJobs(rctx, pool, jobs); err != nil {
			slog.Error("restore job statuses", "err", err)
		}
	}()
	var onRoad []benchJob // only these recipients have the link
	for _, j := range jobs {
		if j.status == "out_for_delivery" {
			onRoad = append(onRoad, j)
		}
	}
	drivers, err := driversOnShift(ctx, pool, at)
	if err != nil {
		return err
	}
	rate := float64(len(onRoad)) * *watching / poll.Seconds()
	slog.Info("frozen", "at", at.Format(time.DateTime), "in_progress", len(jobs), "out_for_delivery", len(onRoad),
		"drivers_on_shift", len(drivers), "track_rps", math.Round(rate*10)/10)
	if len(onRoad) == 0 {
		return fmt.Errorf("no parcels out for delivery at %s", at)
	}

	// ---- load
	client := &http.Client{
		Timeout:   *timeout,
		Transport: &http.Transport{MaxIdleConnsPerHost: 1024, MaxConnsPerHost: 0},
	}
	benchStart := time.Now()
	ctx, cancel := context.WithTimeout(ctx, *dur)
	defer cancel()

	var (
		mu      sync.Mutex
		samples []sample
		wg      sync.WaitGroup
	)
	record := func(s sample) { mu.Lock(); samples = append(samples, s); mu.Unlock() }

	// Driver pings: each driver on shift posts one every 10 s, staggered.
	var pingErrs, pingsSent int
	var pingMs []float64
	var pingMu sync.Mutex
	for i, d := range drivers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			offset := time.Duration(i) * 10 * time.Second / time.Duration(len(drivers))
			select {
			case <-ctx.Done():
				return
			case <-time.After(offset):
			}
			t := time.NewTicker(10 * time.Second)
			defer t.Stop()
			for {
				body, _ := json.Marshal(map[string]any{"pings": []map[string]any{{
					"driver_id": d.id, "at": time.Now().UTC(), "lat": d.lat, "lon": d.lon, "speed_kmh": 18,
				}}})
				t0 := time.Now()
				resp, err := client.Post(*target+"/v1/pings", "application/json", bytes.NewReader(body))
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
				pingMu.Lock()
				pingsSent++
				pingMs = append(pingMs, float64(time.Since(t0).Microseconds())/1000)
				if err != nil || resp.StatusCode != http.StatusAccepted {
					pingErrs++
				}
				pingMu.Unlock()
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}

	// Tracking page polls: open loop at a fixed rate. Latency is measured
	// from when the request was due, so queueing in the client counts too.
	interval := time.Duration(float64(time.Second) / rate)
	due := time.Now()
	for n := 0; ; n++ {
		due = due.Add(interval)
		if d := time.Until(due); d > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		}
		if ctx.Err() != nil {
			break
		}
		j := onRoad[(n*7919)%len(onRoad)] // spread over every parcel on the road
		wg.Add(1)
		go func(due time.Time) {
			defer wg.Done()
			resp, err := client.Get(fmt.Sprintf("%s/v1/jobs/%d/track", *target, j.id))
			s := sample{at: due.Sub(benchStart), ms: float64(time.Since(due).Microseconds()) / 1000, onRoad: j.status == "out_for_delivery"}
			if err != nil {
				s.failed = true
			} else {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				s.failed = resp.StatusCode != http.StatusOK
			}
			record(s)
		}(due)
	}
	wg.Wait()
	elapsed := time.Since(benchStart)

	// ---- clean up the bench's own pings (written with real "now" times,
	// after any seeded history).
	if _, err := pool.Exec(context.Background(), `DELETE FROM location_pings WHERE at >= $1`, benchStart.Add(-time.Second)); err != nil {
		slog.Error("delete bench pings", "err", err)
	}
	// After brief 1, driver_positions moved forward with the bench's pings:
	// put it back to each driver's latest remaining ping.
	if _, err := pool.Exec(context.Background(), `DO $$ BEGIN
		IF to_regclass('driver_positions') IS NOT NULL THEN
			DELETE FROM driver_positions;
			INSERT INTO driver_positions SELECT p.* FROM drivers d CROSS JOIN LATERAL (
				SELECT driver_id, at, location, speed_kmh, battery_pct
				FROM location_pings WHERE driver_id = d.id ORDER BY at DESC LIMIT 1) p;
		END IF; END $$`); err != nil {
		slog.Error("reset driver_positions", "err", err)
	}

	// ---- report
	res := summarise(*label, at, samples, elapsed)
	res.InProgress, res.OutForDelivery, res.Drivers = len(jobs), len(onRoad), len(drivers)
	res.TargetRPS = rate
	res.PingsSent, res.PingErrors = pingsSent, pingErrs
	slices.Sort(pingMs)
	res.PingP95 = pct(pingMs, 95)
	fmt.Printf("\n%-8s %6.1f req/s target, %6.1f done/s   p50 %8.0f ms   p95 %8.0f ms   p99 %8.0f ms   failed %5.1f%%   (on-road p95 %.0f ms, ping p95 %.0f ms)\n",
		res.Label, res.TargetRPS, res.DoneRPS, res.P50, res.P95, res.P99, res.FailedPct, res.OnRoadP95, res.PingP95)
	if *out != "" {
		return writeBench(filepath.Join(*out, "data", *run), res, samples)
	}
	return nil
}

type benchJob struct {
	id     int64
	status string // status at the frozen moment
}

type benchDriver struct {
	id       int64
	lat, lon float64
}

type sample struct {
	at     time.Duration // since the run started
	ms     float64
	failed bool
	onRoad bool
}

type benchResult struct {
	Label          string
	At             time.Time
	InProgress     int
	OutForDelivery int
	Drivers        int
	TargetRPS      float64
	DoneRPS        float64
	Requests       int
	P50, P95, P99  float64
	OnRoadP95      float64
	FailedPct      float64
	PingsSent      int
	PingErrors     int
	PingP95        float64
}

// freezeAt sets every job that was in progress at `at` back to its status at
// that moment. It returns those jobs.
func freezeAt(ctx context.Context, pool *pgxpool.Pool, at time.Time) ([]benchJob, error) {
	rows, err := pool.Query(ctx, `
		SELECT j.id,
		       CASE e.type WHEN 'reattempt_scheduled' THEN 'at_depot' ELSE e.type END
		FROM jobs j
		CROSS JOIN LATERAL (SELECT type FROM job_events
		                    WHERE job_id = j.id AND at <= $1 ORDER BY at DESC LIMIT 1) e
		WHERE j.created_at <= $1 AND j.created_at > $1 - interval '7 days'
		  AND e.type IN ('created', 'collected', 'at_depot', 'out_for_delivery', 'reattempt_scheduled')
		ORDER BY j.id`, at)
	if err != nil {
		return nil, err
	}
	var jobs []benchJob
	var ids []int64
	var sts []string
	for rows.Next() {
		var j benchJob
		if err := rows.Scan(&j.id, &j.status); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
		ids, sts = append(ids, j.id), append(sts, j.status)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_, err = pool.Exec(ctx, `UPDATE jobs SET status = t.s FROM unnest($1::bigint[], $2::text[]) AS t(id, s) WHERE jobs.id = t.id`, ids, sts)
	return jobs, err
}

// restoreJobs puts the frozen jobs back to the status of their last event.
func restoreJobs(ctx context.Context, pool *pgxpool.Pool, jobs []benchJob) error {
	ids := make([]int64, len(jobs))
	for i, j := range jobs {
		ids[i] = j.id
	}
	_, err := pool.Exec(ctx, `
		UPDATE jobs j SET status = CASE e.type WHEN 'reattempt_scheduled' THEN 'at_depot' ELSE e.type END
		FROM (SELECT DISTINCT ON (job_id) job_id, type FROM job_events
		      WHERE job_id = ANY($1) ORDER BY job_id, at DESC) e
		WHERE j.id = e.job_id`, ids)
	return err
}

// driversOnShift returns the drivers working at `at`, placed at their depot
// (where they are doesn't change the cost of storing a ping).
func driversOnShift(ctx context.Context, pool *pgxpool.Pool, at time.Time) ([]benchDriver, error) {
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT d.id, ST_Y(dp.location::geometry), ST_X(dp.location::geometry)
		FROM shifts s JOIN drivers d ON d.id = s.driver_id JOIN depots dp ON dp.id = d.home_depot_id
		WHERE s.starts_at <= $1 AND s.ends_at > $1 ORDER BY d.id`, at)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ds []benchDriver
	for rows.Next() {
		var d benchDriver
		if err := rows.Scan(&d.id, &d.lat, &d.lon); err != nil {
			return nil, err
		}
		ds = append(ds, d)
	}
	return ds, rows.Err()
}

func summarise(label string, at time.Time, samples []sample, elapsed time.Duration) benchResult {
	r := benchResult{Label: label, At: at, Requests: len(samples)}
	var all, road []float64
	failed := 0
	for _, s := range samples {
		if s.failed {
			failed++
		}
		all = append(all, s.ms)
		if s.onRoad {
			road = append(road, s.ms)
		}
	}
	slices.Sort(all)
	slices.Sort(road)
	r.P50, r.P95, r.P99 = pct(all, 50), pct(all, 95), pct(all, 99)
	r.OnRoadP95 = pct(road, 95)
	if len(samples) > 0 {
		r.FailedPct = 100 * float64(failed) / float64(len(samples))
	}
	r.DoneRPS = float64(len(samples)-failed) / elapsed.Seconds()
	return r
}

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	return sorted[max(i, 0)]
}

func writeBench(dir string, r benchResult, samples []sample) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "bench.csv")
	_, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if os.IsNotExist(statErr) {
		fmt.Fprintln(f, "label,at,in_progress,out_for_delivery,drivers,target_rps,done_rps,requests,p50_ms,p95_ms,p99_ms,on_road_p95_ms,failed_pct,pings_sent,ping_errors,ping_p95_ms")
	}
	fmt.Fprintf(f, "%s,%s,%d,%d,%d,%.2f,%.2f,%d,%.1f,%.1f,%.1f,%.1f,%.2f,%d,%d,%.1f\n",
		r.Label, r.At.Format("2006-01-02T15:04"), r.InProgress, r.OutForDelivery, r.Drivers, r.TargetRPS, r.DoneRPS,
		r.Requests, r.P50, r.P95, r.P99, r.OnRoadP95, r.FailedPct, r.PingsSent, r.PingErrors, r.PingP95)

	lf, err := os.Create(filepath.Join(dir, "latency-"+fileSafe(r.Label)+".csv"))
	if err != nil {
		return err
	}
	defer lf.Close()
	fmt.Fprintln(lf, "t_ms,latency_ms,failed,on_road")
	slices.SortFunc(samples, func(a, b sample) int { return int(a.at - b.at) })
	for _, s := range samples {
		fmt.Fprintf(lf, "%d,%.1f,%t,%t\n", s.at.Milliseconds(), s.ms, s.failed, s.onRoad)
	}
	return nil
}

func fileSafe(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			b[i] = '-'
		}
	}
	return string(b)
}
