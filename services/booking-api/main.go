// Command booking-api lets business accounts quote and book deliveries.
//
//	POST /v1/quotes          price a delivery
//	POST /v1/bookings        book it
//	GET  /v1/bookings/{id}   a booking and its event history
//	GET  /healthz
//
// Authenticate with "Authorization: Bearer <api key>".
//
// Known gaps, kept on purpose for later briefs:
//   - no idempotency keys: a retried POST /v1/bookings books twice (brief 4)
//   - booking.created is not published yet; dispatch arrives in weeks 3–4
package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gekgo/internal/dbx"
	"gekgo/internal/geo"
	"gekgo/internal/httpx"
	"gekgo/internal/pricing"
)

func main() {
	pool, err := dbx.Connect(context.Background(), dbx.URL())
	if err != nil {
		slog.Error("database", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	api := &api{db: pool, now: time.Now}
	srv := &http.Server{
		Addr:              httpx.Env("ADDR", ":8081"),
		Handler:           httpx.Logged(api.routes()),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := httpx.Serve(srv); err != nil {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
}

type api struct {
	db  *pgxpool.Pool
	now func() time.Time
}

func (a *api) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("POST /v1/quotes", a.auth(a.quote))
	mux.Handle("POST /v1/bookings", a.auth(a.book))
	mux.Handle("GET /v1/bookings/{id}", a.auth(a.getBooking))
	return mux
}

// ---------------------------------------------------------------- auth

type account struct {
	ID   int64
	Name string
	Tier string
}

func (a *api) auth(next func(http.ResponseWriter, *http.Request, account)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || key == "" {
			httpx.Error(w, http.StatusUnauthorized, "missing API key")
			return
		}
		hash := sha256.Sum256([]byte(key))
		var acc account
		err := a.db.QueryRow(r.Context(), `SELECT id, name, tier FROM accounts WHERE api_key_hash = $1`, hash[:]).
			Scan(&acc.ID, &acc.Name, &acc.Tier)
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusUnauthorized, "unknown API key")
			return
		}
		if err != nil {
			serverError(w, err)
			return
		}
		next(w, r, acc)
	})
}

// ---------------------------------------------------------------- quotes

type place struct {
	Line1    string  `json:"line1"`
	Postcode string  `json:"postcode"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
}

type quoteRequest struct {
	Service string `json:"service"`
	Pickup  place  `json:"pickup"`
	Dropoff place  `json:"dropoff"`
}

type quoteResult struct {
	pricing.Quote
	City       string  `json:"city"`
	Zone       string  `json:"zone"`
	DistanceKm float64 `json:"distance_km"`
}

func (q quoteRequest) validate() error {
	switch q.Service {
	case pricing.TwoHour, pricing.SameDay, pricing.Scheduled:
	default:
		return errors.New("service must be two_hour, same_day or scheduled")
	}
	for name, p := range map[string]place{"pickup": q.Pickup, "dropoff": q.Dropoff} {
		if p.Line1 == "" || p.Postcode == "" {
			return errors.New(name + ": line1 and postcode are required")
		}
		if p.Lat == 0 || p.Lon == 0 {
			return errors.New(name + ": lat and lon are required (geocoding is not offered yet)")
		}
	}
	return nil
}

// price looks up the drop-off zone in PostGIS and prices the job. Pickup and
// drop-off must be in the same city's service area.
func (a *api) price(ctx context.Context, req quoteRequest, tier string) (quoteResult, error) {
	pu := geo.Point{Lat: req.Pickup.Lat, Lon: req.Pickup.Lon}
	dr := geo.Point{Lat: req.Dropoff.Lat, Lon: req.Dropoff.Lon}
	c1, ok1 := geo.CityFor(pu)
	c2, ok2 := geo.CityFor(dr)
	if !ok1 || !ok2 {
		return quoteResult{}, errOutsideArea
	}
	if c1.Key != c2.Key {
		return quoteResult{}, errDifferentCity
	}
	var zone string
	var surcharge int
	err := a.db.QueryRow(ctx, `SELECT name, surcharge_pence FROM zones
		WHERE ST_Covers(area, ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography)
		ORDER BY surcharge_pence DESC LIMIT 1`, dr.Lon, dr.Lat).Scan(&zone, &surcharge)
	if errors.Is(err, pgx.ErrNoRows) {
		return quoteResult{}, errOutsideArea
	}
	if err != nil {
		return quoteResult{}, err
	}
	dist := geo.HaversineKm(pu, dr) * 1.3 // straight line to rough road distance
	q, err := pricing.Price(req.Service, dist, surcharge, tier)
	if err != nil {
		return quoteResult{}, err
	}
	return quoteResult{Quote: q, City: c1.Key, Zone: zone, DistanceKm: float64(int(dist*10)) / 10}, nil
}

var (
	errOutsideArea   = errors.New("address is outside the GekGo service area")
	errDifferentCity = errors.New("pickup and drop-off must be in the same city")
)

func (a *api) quote(w http.ResponseWriter, r *http.Request, acc account) {
	var req quoteRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := req.validate(); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	q, err := a.price(r.Context(), req, acc.Tier)
	if err != nil {
		badOrServer(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, q)
}

// ---------------------------------------------------------------- bookings

type recipient struct {
	Name  string `json:"name"`
	Phone string `json:"phone"`
	Email string `json:"email"`
}

type bookingRequest struct {
	quoteRequest
	Recipient recipient  `json:"recipient"`
	SlotStart *time.Time `json:"slot_start,omitempty"` // scheduled service only
}

type booking struct {
	ID          int64     `json:"id"`
	Status      string    `json:"status"`
	Service     string    `json:"service"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	PricePence  int       `json:"price_pence"`
	CreatedAt   time.Time `json:"created_at"`
	Events      []event   `json:"events,omitempty"`
}

type event struct {
	Type  string    `json:"type"`
	At    time.Time `json:"at"`
	Actor string    `json:"actor"`
}

var london, _ = time.LoadLocation("Europe/London")

// window returns the delivery window promised for a service booked at now.
func window(service string, now time.Time, slot *time.Time) (time.Time, time.Time, error) {
	local := now.In(london)
	at := func(d time.Time, h int) time.Time {
		y, m, dd := d.Date()
		return time.Date(y, m, dd, h, 0, 0, 0, london)
	}
	switch service {
	case pricing.TwoHour:
		start := now.Add(15 * time.Minute)
		return start, start.Add(2 * time.Hour), nil
	case pricing.SameDay:
		if local.Hour() < 14 {
			return now, at(local, 18), nil
		}
		next := local.AddDate(0, 0, 1)
		return at(next, 9), at(next, 18), nil
	case pricing.Scheduled:
		if slot == nil {
			return time.Time{}, time.Time{}, errors.New("slot_start is required for scheduled service")
		}
		if slot.Before(now.Add(3 * time.Hour)) {
			return time.Time{}, time.Time{}, errors.New("slot_start must be at least 3 hours ahead")
		}
		return *slot, slot.Add(2 * time.Hour), nil
	}
	return time.Time{}, time.Time{}, errors.New("unknown service")
}

func (a *api) book(w http.ResponseWriter, r *http.Request, acc account) {
	var req bookingRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := req.validate(); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Recipient.Name == "" || req.Recipient.Phone == "" {
		httpx.Error(w, http.StatusBadRequest, "recipient name and phone are required")
		return
	}
	now := a.now()
	ws, we, err := window(req.Service, now, req.SlotStart)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	q, err := a.price(r.Context(), req.quoteRequest, acc.Tier)
	if err != nil {
		badOrServer(w, err)
		return
	}

	ctx := r.Context()
	tx, err := a.db.Begin(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(ctx)

	insertAddr := func(p place) (int64, error) {
		var id int64
		err := tx.QueryRow(ctx, `INSERT INTO addresses (line1, city, postcode, location)
			VALUES ($1, $2, $3, ST_SetSRID(ST_MakePoint($4, $5), 4326)::geography) RETURNING id`,
			p.Line1, q.City, strings.ToUpper(p.Postcode), p.Lon, p.Lat).Scan(&id)
		return id, err
	}
	pickupID, err := insertAddr(req.Pickup)
	if err != nil {
		serverError(w, err)
		return
	}
	dropoffID, err := insertAddr(req.Dropoff)
	if err != nil {
		serverError(w, err)
		return
	}
	var customerID int64
	if err := tx.QueryRow(ctx, `INSERT INTO customers (name, phone, email) VALUES ($1, $2, $3) RETURNING id`,
		req.Recipient.Name, req.Recipient.Phone, req.Recipient.Email).Scan(&customerID); err != nil {
		serverError(w, err)
		return
	}
	b := booking{Status: "created", Service: req.Service, WindowStart: ws, WindowEnd: we, PricePence: q.TotalPence, CreatedAt: now}
	if err := tx.QueryRow(ctx, `INSERT INTO jobs (account_id, customer_id, pickup_id, dropoff_id, service,
			window_start, window_end, status, price_pence, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'created', $8, $9) RETURNING id`,
		acc.ID, customerID, pickupID, dropoffID, req.Service, ws, we, q.TotalPence, now).Scan(&b.ID); err != nil {
		serverError(w, err)
		return
	}
	actor := "account:" + strconv.FormatInt(acc.ID, 10)
	if _, err := tx.Exec(ctx, `INSERT INTO job_events (job_id, type, at, actor, payload)
		VALUES ($1, 'created', $2, $3, jsonb_build_object('quote', $4::int))`, b.ID, now, actor, q.TotalPence); err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, err)
		return
	}
	b.Events = []event{{"created", now, actor}}
	w.Header().Set("Location", "/v1/bookings/"+strconv.FormatInt(b.ID, 10))
	httpx.JSON(w, http.StatusCreated, b)
}

func (a *api) getBooking(w http.ResponseWriter, r *http.Request, acc account) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad booking id")
		return
	}
	var b booking
	err = a.db.QueryRow(r.Context(), `SELECT id, status, service, window_start, window_end, price_pence, created_at
		FROM jobs WHERE id = $1 AND account_id = $2`, id, acc.ID).
		Scan(&b.ID, &b.Status, &b.Service, &b.WindowStart, &b.WindowEnd, &b.PricePence, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusNotFound, "booking not found")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	rows, err := a.db.Query(r.Context(), `SELECT type, at, actor FROM job_events WHERE job_id = $1 ORDER BY at, id`, id)
	if err != nil {
		serverError(w, err)
		return
	}
	b.Events, err = pgx.CollectRows(rows, pgx.RowToStructByPos[event])
	if err != nil {
		serverError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, b)
}

// ---------------------------------------------------------------- errors

func badOrServer(w http.ResponseWriter, err error) {
	if errors.Is(err, errOutsideArea) || errors.Is(err, errDifferentCity) {
		httpx.Error(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	serverError(w, err)
}

func serverError(w http.ResponseWriter, err error) {
	slog.Error("internal error", "err", err)
	httpx.Error(w, http.StatusInternalServerError, "internal error")
}
