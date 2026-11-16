// Command tracking-ingest takes GPS pings and parcel scans from the driver
// app, and serves positions to the tracking page and the ops dashboard.
//
//	POST /v1/pings                  a batch of GPS pings
//	POST /v1/jobs/{id}/scans        a driver scan (collected, delivered, failed…)
//	GET  /v1/jobs/{id}/track        what the customer tracking page shows
//	GET  /v1/positions?at=          latest position of every active driver
//	GET  /v1/pings/range            first and last ping time, for replay
//	GET  /v1/fleet                  drivers and their vehicles
//	GET  /v1/history/deliveries     delivery counts on a grid, for the map
//	GET  /healthz
//
// location_pings is partitioned by day; driver_positions holds each
// driver's latest ping (brief 1). Known gaps, kept on purpose for later briefs:
//   - pings go straight to Postgres; Pub/Sub sits in front later (brief 7)
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"
	_ "time/tzdata"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gekgo/internal/dbx"
	"gekgo/internal/httpx"
)

func main() {
	pool, err := dbx.Connect(context.Background(), dbx.URL())
	if err != nil {
		slog.Error("database", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	s := &service{db: pool, now: time.Now}
	go s.keepPartitions(context.Background())
	srv := &http.Server{
		Addr:              httpx.Env("ADDR", ":8082"),
		Handler:           httpx.Logged(s.routes()),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := httpx.Serve(srv); err != nil {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
}

type service struct {
	db  *pgxpool.Pool
	now func() time.Time
}

// keepPartitions makes sure the next week of daily ping partitions exists,
// at startup and then every hour.
func (s *service) keepPartitions(ctx context.Context) {
	for {
		var n int
		err := s.db.QueryRow(ctx, `SELECT ensure_ping_partitions((now() AT TIME ZONE 'Europe/London')::date, 7)`).Scan(&n)
		if err != nil {
			slog.Error("ping partitions", "err", err)
		} else if n > 0 {
			slog.Info("ping partitions created", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

func (s *service) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /v1/pings", s.pings)
	mux.HandleFunc("POST /v1/jobs/{id}/scans", s.scan)
	mux.HandleFunc("GET /v1/jobs/{id}/track", s.track)
	mux.HandleFunc("GET /v1/positions", s.positions)
	mux.HandleFunc("GET /v1/pings/range", s.pingRange)
	mux.HandleFunc("GET /v1/fleet", s.fleet)
	mux.HandleFunc("GET /v1/history/deliveries", s.deliveryHeat)
	return mux
}

// ---------------------------------------------------------------- pings

type ping struct {
	DriverID   int64     `json:"driver_id"`
	At         time.Time `json:"at"`
	Lat        float64   `json:"lat"`
	Lon        float64   `json:"lon"`
	SpeedKmh   *float32  `json:"speed_kmh,omitempty"`
	BatteryPct *float32  `json:"battery_pct,omitempty"`
}

const maxBatch = 500

func (s *service) pings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Pings []ping `json:"pings"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if n := len(body.Pings); n == 0 || n > maxBatch {
		httpx.Error(w, http.StatusBadRequest, "send between 1 and 500 pings")
		return
	}
	n := len(body.Pings)
	drivers, ats := make([]int64, n), make([]time.Time, n)
	lats, lons := make([]float64, n), make([]float64, n)
	speeds, bats := make([]*float32, n), make([]*float32, n)
	now := s.now()
	for i, p := range body.Pings {
		if p.DriverID <= 0 || p.At.IsZero() || p.Lat < -90 || p.Lat > 90 || p.Lon < -180 || p.Lon > 180 {
			httpx.Error(w, http.StatusBadRequest, "ping "+strconv.Itoa(i)+": driver_id, at, lat and lon are required")
			return
		}
		if p.At.After(now.Add(time.Minute)) {
			httpx.Error(w, http.StatusBadRequest, "ping "+strconv.Itoa(i)+": timestamp is in the future")
			return
		}
		drivers[i], ats[i], lats[i], lons[i], speeds[i], bats[i] = p.DriverID, p.At, p.Lat, p.Lon, p.SpeedKmh, p.BatteryPct
	}
	// One statement stores the pings and moves each driver's latest position
	// forward (never backwards: a late batch from a phone that lost signal
	// doesn't overwrite a newer position).
	_, err := s.db.Exec(r.Context(), `WITH p AS (
			SELECT d, a, ST_SetSRID(ST_MakePoint(lon, lat), 4326)::geography AS g, sp, b
			FROM unnest($1::bigint[], $2::timestamptz[], $3::float8[], $4::float8[], $5::real[], $6::real[]) AS t(d, a, lat, lon, sp, b)
		), stored AS (
			INSERT INTO location_pings (driver_id, at, location, speed_kmh, battery_pct)
			SELECT d, a, g, sp, b FROM p
		)
		INSERT INTO driver_positions (driver_id, at, location, speed_kmh, battery_pct)
		SELECT DISTINCT ON (d) d, a, g, sp, b FROM p ORDER BY d, a DESC
		ON CONFLICT (driver_id) DO UPDATE
			SET at = EXCLUDED.at, location = EXCLUDED.location,
			    speed_kmh = EXCLUDED.speed_kmh, battery_pct = EXCLUDED.battery_pct
			WHERE driver_positions.at < EXCLUDED.at`,
		drivers, ats, lats, lons, speeds, bats)
	if err != nil {
		serverError(w, err)
		return
	}
	httpx.JSON(w, http.StatusAccepted, map[string]int{"accepted": n})
}

// ---------------------------------------------------------------- scans

// Allowed status changes. A scan moves a job from one of the "from" states.
var transitions = map[string][]string{
	"collected":        {"created"},
	"at_depot":         {"collected"},
	"out_for_delivery": {"collected", "at_depot"},
	"delivered":        {"out_for_delivery"},
	"failed":           {"out_for_delivery"},
	"returned":         {"failed", "at_depot"},
}

type scanRequest struct {
	Type     string     `json:"type"`
	DriverID int64      `json:"driver_id"`
	At       *time.Time `json:"at,omitempty"`
	Reason   string     `json:"reason,omitempty"` // for failed
	Proof    *struct {
		Kind      string `json:"kind"` // photo, signature, safe_place
		ObjectURL string `json:"object_url,omitempty"`
		Recipient string `json:"recipient,omitempty"`
	} `json:"proof,omitempty"`
}

func (s *service) scan(w http.ResponseWriter, r *http.Request) {
	jobID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad job id")
		return
	}
	var req scanRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	from, ok := transitions[req.Type]
	if !ok || req.DriverID <= 0 {
		httpx.Error(w, http.StatusBadRequest, "type must be a known scan and driver_id is required")
		return
	}
	if req.Type == "delivered" && req.Proof == nil {
		httpx.Error(w, http.StatusBadRequest, "delivered scans need proof")
		return
	}
	if req.Type == "failed" && req.Reason == "" {
		httpx.Error(w, http.StatusBadRequest, "failed scans need a reason")
		return
	}
	at := s.now()
	if req.At != nil {
		at = *req.At // offline scans arrive late with their real time
	}

	ctx := r.Context()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1 FOR UPDATE`, jobID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	allowed := false
	for _, f := range from {
		allowed = allowed || f == status
	}
	if !allowed {
		httpx.Error(w, http.StatusConflict, "cannot go from "+status+" to "+req.Type)
		return
	}
	actor := "driver:" + strconv.FormatInt(req.DriverID, 10)
	var payload map[string]string
	if req.Reason != "" {
		payload = map[string]string{"reason": req.Reason}
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET status = $2, driver_id = $3 WHERE id = $1`, jobID, req.Type, req.DriverID); err != nil {
		serverError(w, err)
		return
	}
	if _, err := tx.Exec(ctx, `INSERT INTO job_events (job_id, type, at, actor, payload) VALUES ($1, $2, $3, $4, $5)`,
		jobID, req.Type, at, actor, payload); err != nil {
		serverError(w, err)
		return
	}
	if req.Proof != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO proofs_of_delivery (job_id, driver_id, kind, object_url, recipient, captured_at)
			VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6)`,
			jobID, req.DriverID, req.Proof.Kind, req.Proof.ObjectURL, req.Proof.Recipient, at); err != nil {
			serverError(w, err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"job_id": jobID, "status": req.Type, "at": at})
}

// ---------------------------------------------------------------- reads

type position struct {
	DriverID   int64     `json:"driver_id"`
	At         time.Time `json:"at"`
	Lat        float64   `json:"lat"`
	Lon        float64   `json:"lon"`
	SpeedKmh   *float32  `json:"speed_kmh"`
	BatteryPct *float32  `json:"battery_pct"`
}

// track is what the customer tracking page polls. It finds the job's driver
// and reads their latest position: one row by primary key (brief 1).
func (s *service) track(w http.ResponseWriter, r *http.Request) {
	jobID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad job id")
		return
	}
	ctx := r.Context()
	var out struct {
		JobID       int64     `json:"job_id"`
		Status      string    `json:"status"`
		WindowStart time.Time `json:"window_start"`
		WindowEnd   time.Time `json:"window_end"`
		DropoffLat  float64   `json:"dropoff_lat"`
		DropoffLon  float64   `json:"dropoff_lon"`
		Driver      *position `json:"driver,omitempty"`
	}
	var driverID *int64
	err = s.db.QueryRow(ctx, `SELECT j.id, j.status, j.window_start, j.window_end,
			ST_Y(a.location::geometry), ST_X(a.location::geometry), j.driver_id
		FROM jobs j JOIN addresses a ON a.id = j.dropoff_id WHERE j.id = $1`, jobID).
		Scan(&out.JobID, &out.Status, &out.WindowStart, &out.WindowEnd, &out.DropoffLat, &out.DropoffLon, &driverID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if driverID != nil && out.Status == "out_for_delivery" {
		var p position
		err := s.db.QueryRow(ctx, `SELECT driver_id, at, ST_Y(location::geometry), ST_X(location::geometry), speed_kmh, battery_pct
			FROM driver_positions WHERE driver_id = $1`, *driverID).
			Scan(&p.DriverID, &p.At, &p.Lat, &p.Lon, &p.SpeedKmh, &p.BatteryPct)
		if err == nil {
			out.Driver = &p
		} else if !errors.Is(err, pgx.ErrNoRows) {
			serverError(w, err)
			return
		}
	}
	httpx.JSON(w, http.StatusOK, out)
}

// positions returns each driver's latest ping in the 5 minutes before ?at
// (default now). The ops map polls this; ?at lets it replay history.
func (s *service) positions(w http.ResponseWriter, r *http.Request) {
	at := s.now()
	if v := r.URL.Query().Get("at"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "at must be RFC 3339")
			return
		}
		at = t
	}
	rows, err := s.db.Query(r.Context(), `SELECT DISTINCT ON (driver_id)
			driver_id, at, ST_Y(location::geometry), ST_X(location::geometry), speed_kmh, battery_pct
		FROM location_pings
		WHERE at > $1 AND at <= $2
		ORDER BY driver_id, at DESC`, at.Add(-5*time.Minute), at)
	if err != nil {
		serverError(w, err)
		return
	}
	ps, err := pgx.CollectRows(rows, pgx.RowToStructByPos[position])
	if err != nil {
		serverError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"at": at, "positions": ps})
}

// pingRange tells the ops map which times it can replay.
func (s *service) pingRange(w http.ResponseWriter, r *http.Request) {
	var from, to *time.Time
	// min/max per driver walks the (driver_id, at DESC) index from each end:
	// ~200 index lookups instead of reading 120 million rows.
	if err := s.db.QueryRow(r.Context(), `SELECT min(f.at), max(l.at) FROM drivers d
		CROSS JOIN LATERAL (SELECT at FROM location_pings WHERE driver_id = d.id ORDER BY at LIMIT 1) f
		CROSS JOIN LATERAL (SELECT at FROM location_pings WHERE driver_id = d.id ORDER BY at DESC LIMIT 1) l`).Scan(&from, &to); err != nil {
		serverError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"from": from, "to": to})
}

func (s *service) fleet(w http.ResponseWriter, r *http.Request) {
	type member struct {
		DriverID    int64   `json:"driver_id"`
		Name        string  `json:"name"`
		City        string  `json:"city"`
		Vehicle     *string `json:"vehicle"`
		VehicleType *string `json:"vehicle_type"`
		IsElectric  *bool   `json:"is_electric"`
	}
	rows, err := s.db.Query(r.Context(), `SELECT d.id, d.name, dep.city, v.registration, v.type, v.is_electric
		FROM drivers d
		JOIN depots dep ON dep.id = d.home_depot_id
		LEFT JOIN LATERAL (SELECT vehicle_id FROM shifts WHERE driver_id = d.id ORDER BY starts_at DESC LIMIT 1) sh ON true
		LEFT JOIN vehicles v ON v.id = sh.vehicle_id
		ORDER BY d.id`)
	if err != nil {
		serverError(w, err)
		return
	}
	ms, err := pgx.CollectRows(rows, pgx.RowToStructByPos[member])
	if err != nil {
		serverError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"drivers": ms})
}

// deliveryHeat counts delivered jobs on a ~250 m grid between ?from and ?to
// (dates, default the whole history), for the map's history layer.
func (s *service) deliveryHeat(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), s.now()
	for name, dst := range map[string]*time.Time{"from": &from, "to": &to} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse("2006-01-02", v)
			if err != nil {
				httpx.Error(w, http.StatusBadRequest, name+" must be YYYY-MM-DD")
				return
			}
			*dst = t
		}
	}
	type cell struct {
		Lat   float64 `json:"lat"`
		Lon   float64 `json:"lon"`
		Count int     `json:"count"`
	}
	rows, err := s.db.Query(r.Context(), `
		SELECT ST_Y(c), ST_X(c), n::int FROM (
			SELECT ST_SnapToGrid(a.location::geometry, 0.004, 0.0025) AS c, count(*) AS n
			FROM jobs j JOIN addresses a ON a.id = j.dropoff_id
			WHERE j.status = 'delivered' AND j.created_at >= $1 AND j.created_at < $2
			GROUP BY 1) g
		ORDER BY n DESC`, from, to)
	if err != nil {
		serverError(w, err)
		return
	}
	cells, err := pgx.CollectRows(rows, pgx.RowToStructByPos[cell])
	if err != nil {
		serverError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "cells": cells})
}

func serverError(w http.ResponseWriter, err error) {
	slog.Error("internal error", "err", err)
	httpx.Error(w, http.StatusInternalServerError, "internal error")
}
