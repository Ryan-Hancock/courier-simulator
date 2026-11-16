package main

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gekgo/db"
	"gekgo/internal/dbx"
	"gekgo/internal/geo"
	"gekgo/internal/pricing"
)

// DemoAPIKey belongs to account 1 (GekGo's biggest customer) so the API can be
// tried locally without digging keys out of the database.
const DemoAPIKey = "gk_test_demo_account_0001"

const (
	numAddresses  = 25000
	numAccounts   = 300
	numCustomers  = 40000
	fullDailyJobs = 6000 // a normal weekday at the end of the history, scale 1.0
)

type seedConfig struct {
	seed     uint64
	until    time.Time // last simulated day (local midnight)
	months   int
	scale    float64
	pingDays int
	reset    bool
}

type address struct {
	city string
	at   geo.Point
}

type account struct {
	id     int64
	city   string
	home   int64
	tier   string
	weight float64
	joined time.Time
}

type driver struct {
	id       int64
	city     string
	vehicle  int64
	electric bool
	ebike    bool // e-cargo bikes swap batteries at the depot
	rangeKm  float64
}

type shift struct {
	driver     int // index into seeder.drivers
	start, end time.Time
}

type driverDay struct {
	driver int
	day    string
}

type waypoint struct {
	at time.Time
	p  geo.Point
}

type seeder struct {
	cfg     seedConfig
	pool    *pgxpool.Pool
	r       *rand.Rand
	loc     *time.Location
	streets map[string][]street

	addrs        []address // id = index + 1
	addrsByCity  map[string][]int64
	addrsCentral map[string][]int64 // within 4 km of the centre: offices, flats, shops
	accounts     []account
	drivers      []driver
	depotIDs     map[string]int64

	// shiftsByDay[date][city] lists the shifts working that day.
	shiftsByDay map[string]map[string][]shift
	// waypoints[{driverIndex, day}] for days that get GPS pings.
	waypoints map[driverDay][]waypoint
	pingFrom  time.Time

	jobRows   [][]any
	eventRows [][]any
	nextJobID int64
	counts    map[string]int
}

func runSeed(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	seed := fs.Uint64("seed", 42, "random seed; the same seed and -until give the same data")
	until := fs.String("until", "", "last simulated day, YYYY-MM-DD (default: yesterday, Europe/London)")
	months := fs.Int("months", 18, "months of history")
	scale := fs.Float64("scale", 0.1, "job volume as a fraction of real size (1.0 = ~6,000 a weekday, ~3M jobs)")
	pingDays := fs.Int("ping-days", 7, "days of GPS pings to generate, ending on -until")
	reset := fs.Bool("reset", false, "wipe existing data first")
	fs.Parse(args)

	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		return err
	}
	var last time.Time
	if *until == "" {
		y, m, d := time.Now().In(loc).AddDate(0, 0, -1).Date()
		last = time.Date(y, m, d, 0, 0, 0, 0, loc)
	} else if last, err = time.ParseInLocation("2006-01-02", *until, loc); err != nil {
		return fmt.Errorf("-until: %w", err)
	}

	streets, err := loadStreets()
	if err != nil {
		return err
	}
	pool, err := dbx.Connect(ctx, dbx.URL())
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := dbx.Migrate(ctx, pool, db.Migrations, "migrations"); err != nil {
		return err
	}

	var existing int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM depots`).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 && !*reset {
		return fmt.Errorf("database already has data; pass -reset to wipe it")
	}
	if *reset {
		if _, err := pool.Exec(ctx, `TRUNCATE depots, zones, addresses, accounts, customers, drivers, vehicles,
			shifts, jobs, job_events, routes, route_stops, location_pings, driver_positions, proofs_of_delivery,
			invoices, invoice_lines RESTART IDENTITY CASCADE`); err != nil {
			return err
		}
	}

	s := &seeder{
		cfg:          seedConfig{*seed, last, *months, *scale, *pingDays, *reset},
		pool:         pool,
		r:            rand.New(rand.NewPCG(*seed, *seed^0x9E3779B97F4A7C15)),
		loc:          loc,
		streets:      streets,
		addrsByCity:  map[string][]int64{},
		addrsCentral: map[string][]int64{},
		depotIDs:     map[string]int64{},
		shiftsByDay:  map[string]map[string][]shift{},
		waypoints:    map[driverDay][]waypoint{},
		pingFrom:     last.AddDate(0, 0, -(*pingDays - 1)),
		nextJobID:    1,
		counts:       map[string]int{},
	}
	start := time.Now()
	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"places", s.places},
		{"addresses", s.seedAddresses},
		{"accounts", s.seedAccounts},
		{"customers", s.seedCustomers},
		{"fleet", s.seedFleet},
		{"shifts", s.seedShifts},
		{"jobs", s.seedJobs},
		{"pings", s.seedPings},
		{"finish", s.finish},
	}
	for _, st := range steps {
		t := time.Now()
		if err := st.fn(ctx); err != nil {
			return fmt.Errorf("%s: %w", st.name, err)
		}
		slog.Info("seeded", "step", st.name, "took", time.Since(t).Round(time.Millisecond))
	}
	slog.Info("seed complete", "from", last.AddDate(0, -*months, 0).Format("2006-01-02"), "until", last.Format("2006-01-02"),
		"jobs", s.counts["jobs"], "events", s.counts["events"], "pings", s.counts["pings"], "took", time.Since(start).Round(time.Second))
	fmt.Printf("\nDemo API key for account 1: %s\n", DemoAPIKey)
	return nil
}

// ---------------------------------------------------------------- places

func (s *seeder) places(ctx context.Context) error {
	for _, c := range geo.Cities {
		var id int64
		err := s.pool.QueryRow(ctx, `INSERT INTO depots (name, city, location)
			VALUES ($1, $2, ST_SetSRID(ST_MakePoint($3, $4), 4326)::geography) RETURNING id`,
			c.Depot.Name, c.Key, c.Depot.Location.Lon, c.Depot.Location.Lat).Scan(&id)
		if err != nil {
			return err
		}
		s.depotIDs[c.Key] = id
		for _, z := range geo.Zones {
			// A ring: buffer(outer) minus buffer(inner), built in metres on geography.
			_, err := s.pool.Exec(ctx, `
				WITH c AS (SELECT ST_SetSRID(ST_MakePoint($2, $3), 4326)::geography AS g)
				INSERT INTO zones (depot_id, name, area, surcharge_pence)
				SELECT $1, $4,
				  CASE WHEN $5::float8 = 0 THEN ST_Buffer(c.g, $6 * 1000)
				       ELSE ST_Difference(ST_Buffer(c.g, $6 * 1000)::geometry, ST_Buffer(c.g, $5 * 1000)::geometry)::geography END,
				  $7
				FROM c`,
				id, c.Centre.Lon, c.Centre.Lat, c.Key+"-"+z.Name, z.InnerKm, z.OuterKm, z.SurchargePence)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *seeder) seedAddresses(ctx context.Context) error {
	ids := make([]int64, 0, numAddresses)
	line1s, cities, postcodes := make([]string, 0, numAddresses), make([]string, 0, numAddresses), make([]string, 0, numAddresses)
	lats, lons := make([]float64, 0, numAddresses), make([]float64, 0, numAddresses)
	for i := range numAddresses {
		c := geo.Glasgow
		if s.r.Float64() < 0.4 {
			c = geo.Edinburgh
		}
		st := s.streets[c.Key][s.r.IntN(len(s.streets[c.Key]))]
		p := geo.Point{
			Lat: st.at.Lat + (s.r.Float64()-0.5)*0.0008,
			Lon: st.at.Lon + (s.r.Float64()-0.5)*0.0014,
		}
		num := 1 + s.r.IntN(180)
		line1 := fmt.Sprintf("%d %s", num, st.name)
		if s.r.Float64() < 0.4 { // tenement flats, written the local way
			if c.Key == "glasgow" {
				line1 = fmt.Sprintf("Flat %d/%d, %s", s.r.IntN(4), 1+s.r.IntN(2), line1)
			} else {
				line1 = fmt.Sprintf("%dF%d, %s", 1+s.r.IntN(4), 1+s.r.IntN(3), line1)
			}
		}
		id := int64(i + 1)
		ids = append(ids, id)
		line1s = append(line1s, line1)
		cities = append(cities, c.Name)
		postcodes = append(postcodes, postcode(s.r, c.Prefix))
		lats = append(lats, p.Lat)
		lons = append(lons, p.Lon)
		s.addrs = append(s.addrs, address{c.Key, p})
		s.addrsByCity[c.Key] = append(s.addrsByCity[c.Key], id)
		if geo.HaversineKm(c.Centre, p) < 4 {
			s.addrsCentral[c.Key] = append(s.addrsCentral[c.Key], id)
		}
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO addresses (id, line1, city, postcode, location)
		SELECT id, line1, city, postcode, ST_SetSRID(ST_MakePoint(lon, lat), 4326)::geography
		FROM unnest($1::bigint[], $2::text[], $3::text[], $4::text[], $5::float8[], $6::float8[])
		  AS t(id, line1, city, postcode, lat, lon)`, ids, line1s, cities, postcodes, lats, lons)
	s.counts["addresses"] = numAddresses
	return err
}

func (s *seeder) seedAccounts(ctx context.Context) error {
	historyStart := s.cfg.until.AddDate(0, -s.cfg.months, 0)
	span := s.cfg.until.Sub(historyStart)
	rows := make([][]any, 0, numAccounts)
	for i := range numAccounts {
		id := int64(i + 1)
		sec := sectors[s.r.IntN(len(sectors))]
		if id == 1 {
			sec = sectors[0] // the biggest customer is a florist chain
		}
		c := geo.Glasgow
		if s.r.Float64() < 0.4 {
			c = geo.Edinburgh
		}
		tier := "standard"
		if id <= 3 {
			tier = "enterprise"
		} else if s.r.Float64() < sec.tierMix {
			tier = "plus"
		}
		terms := map[string]string{"standard": "prepay", "plus": "net14", "enterprise": "net30"}[tier]
		if tier == "standard" && s.r.Float64() < 0.3 {
			terms = "net14"
		}
		// Most accounts were trading before the history starts; the rest join
		// during it, which is where GekGo's growth comes from.
		joined := historyStart.Add(-time.Duration(s.r.Float64() * float64(2*365*24*time.Hour)))
		if id > 10 && s.r.Float64() < 0.35 {
			joined = historyStart.Add(time.Duration(s.r.Float64() * float64(span)))
		}
		home := s.randomAddress(c.Key)
		key := fmt.Sprintf("gk_live_%016x%016x", s.r.Uint64(), s.r.Uint64())
		if id == 1 {
			key = DemoAPIKey
		}
		hash := sha256.Sum256([]byte(key))
		a := account{
			id: id, city: c.Key, home: home, tier: tier, joined: joined,
			// Zipf-like: a few accounts book far more than the long tail.
			weight: sec.weight / math.Pow(float64(id), 0.75),
		}
		s.accounts = append(s.accounts, a)
		rows = append(rows, []any{id, businessName(s.r, sec), sec.key, tier, terms, home, hash[:], joined})
	}
	_, err := s.pool.CopyFrom(ctx, pgx.Identifier{"accounts"},
		[]string{"id", "name", "sector", "tier", "billing_terms", "home_address_id", "api_key_hash", "created_at"},
		pgx.CopyFromRows(rows))
	s.counts["accounts"] = numAccounts
	return err
}

func (s *seeder) seedCustomers(ctx context.Context) error {
	rows := make([][]any, 0, numCustomers)
	base := s.cfg.until.AddDate(-3, 0, 0)
	for i := range numCustomers {
		first, last := personName(s.r)
		created := base.Add(time.Duration(s.r.Float64() * float64(3*365*24*time.Hour)))
		rows = append(rows, []any{int64(i + 1), first + " " + last, phone(s.r), email(first, last, i+1), created})
	}
	_, err := s.pool.CopyFrom(ctx, pgx.Identifier{"customers"},
		[]string{"id", "name", "phone", "email", "created_at"}, pgx.CopyFromRows(rows))
	s.counts["customers"] = numCustomers
	return err
}

// seedFleet creates 95 drivers and 115 vehicles: 60 diesel vans, 25 electric
// vans and 30 e-cargo bikes, split between the two depots.
func (s *seeder) seedFleet(ctx context.Context) error {
	type mix struct{ diesel, ev, bikes, drivers int }
	fleet := map[string]mix{"glasgow": {38, 16, 19, 60}, "edinburgh": {22, 9, 11, 35}}
	var vRows, dRows [][]any
	var vid, did int64
	for _, c := range geo.Cities {
		m := fleet[c.Key]
		type veh struct {
			id       int64
			electric bool
			ebike    bool
			rangeKm  float64
		}
		var vs []veh
		add := func(n int, typ string, electric bool) {
			for range n {
				vid++
				var rangeKm, kwh any
				r := 0.0
				switch {
				case typ == "ebike":
					r = 55 + float64(s.r.IntN(25))
					rangeKm, kwh = int(r), 1.0
				case electric:
					r = 180 + float64(s.r.IntN(80))
					rangeKm, kwh = int(r), []float64{50, 68, 75}[s.r.IntN(3)]
				}
				vRows = append(vRows, []any{vid, registration(s.r, 19+s.r.IntN(7)), typ, electric, rangeKm, kwh, s.depotIDs[c.Key]})
				vs = append(vs, veh{vid, electric, typ == "ebike", r})
			}
		}
		add(m.diesel, "van", false)
		add(m.ev, "van", true)
		add(m.bikes, "ebike", true)
		s.r.Shuffle(len(vs), func(i, j int) { vs[i], vs[j] = vs[j], vs[i] })
		for i := range m.drivers {
			did++
			first, last := personName(s.r)
			emp := "employed"
			if s.r.Float64() < 0.35 {
				emp = "self_employed"
			}
			dRows = append(dRows, []any{did, first + " " + last, emp, s.depotIDs[c.Key]})
			v := vs[i%len(vs)]
			s.drivers = append(s.drivers, driver{did, c.Key, v.id, v.electric, v.ebike, v.rangeKm})
		}
	}
	if _, err := s.pool.CopyFrom(ctx, pgx.Identifier{"vehicles"},
		[]string{"id", "registration", "type", "is_electric", "range_km", "battery_kwh", "depot_id"}, pgx.CopyFromRows(vRows)); err != nil {
		return err
	}
	_, err := s.pool.CopyFrom(ctx, pgx.Identifier{"drivers"},
		[]string{"id", "name", "employment_type", "home_depot_id"}, pgx.CopyFromRows(dRows))
	return err
}

// ---------------------------------------------------------------- calendar

func dayKey(t time.Time) string { return t.Format("2006-01-02") }

func closed(d time.Time) bool {
	m, day := d.Month(), d.Day()
	return (m == time.December && (day == 25 || day == 26)) || (m == time.January && day == 1)
}

// blackFriday returns the fourth Friday of November in year y.
func blackFriday(y int, loc *time.Location) time.Time {
	d := time.Date(y, time.November, 1, 0, 0, 0, 0, loc)
	for d.Weekday() != time.Friday {
		d = d.AddDate(0, 0, 1)
	}
	return d.AddDate(0, 0, 21)
}

var weekdayFactor = map[time.Weekday]float64{
	time.Monday: 1.22, time.Tuesday: 1.05, time.Wednesday: 1.0, time.Thursday: 1.02,
	time.Friday: 1.08, time.Saturday: 0.45, time.Sunday: 0.15,
}

// season is the demand multiplier for peaks and quiet spells.
func (s *seeder) season(d time.Time) float64 {
	if closed(d) {
		return 0
	}
	bf := blackFriday(d.Year(), s.loc)
	switch diff := int(math.Round(d.Sub(bf).Hours() / 24)); {
	case diff == 0:
		return 2.4
	case diff == 1:
		return 1.6
	case diff == 3: // Cyber Monday
		return 1.9
	case diff >= -4 && diff < 0:
		return 1.3
	}
	m, day := d.Month(), d.Day()
	switch {
	case m == time.December && day <= 23:
		return 1 + 2.0*math.Min(1, float64(day-1)/16) // builds to 3x by mid-December
	case m == time.December && day == 24:
		return 1.8
	case m == time.December:
		return 0.6
	case m == time.January && day == 2:
		return 0.7
	case m == time.January:
		return 0.85
	case m == time.July || m == time.August:
		return 0.92
	}
	return 1
}

// growth is how big the business is on day d relative to the end of history.
func (s *seeder) growth(d time.Time) float64 {
	start := s.cfg.until.AddDate(0, -s.cfg.months, 0)
	frac := d.Sub(start).Hours() / s.cfg.until.Sub(start).Hours()
	return 0.72 + 0.28*frac
}

// load is demand relative to a normal day; peaks make lateness and failures worse.
func (s *seeder) load(d time.Time) float64 {
	return weekdayFactor[d.Weekday()] * s.season(d)
}

// ---------------------------------------------------------------- shifts

func (s *seeder) seedShifts(ctx context.Context) error {
	from := s.cfg.until.AddDate(0, -s.cfg.months, 0)
	to := s.cfg.until.AddDate(0, 0, 7) // scheduled jobs can be booked a few days ahead
	var rows [][]any
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		day := map[string][]shift{}
		s.shiftsByDay[dayKey(d)] = day
		if closed(d) {
			continue
		}
		p := map[time.Weekday]float64{time.Saturday: 0.5, time.Sunday: 0.2}[d.Weekday()]
		if p == 0 {
			p = 0.85
		}
		if s.load(d) > 1.5 {
			p = 0.97 // all hands at peak
		}
		for i, dr := range s.drivers {
			if s.r.Float64() >= p {
				continue
			}
			start := clock(d, 7, s.r.IntN(90))
			end := start.Add(8*time.Hour + time.Duration(s.r.IntN(150))*time.Minute)
			day[dr.city] = append(day[dr.city], shift{i, start, end})
			if !d.After(s.cfg.until) {
				rows = append(rows, []any{dr.id, dr.vehicle, start, end})
			}
		}
	}
	_, err := s.pool.CopyFrom(ctx, pgx.Identifier{"shifts"},
		[]string{"driver_id", "vehicle_id", "starts_at", "ends_at"}, pgx.CopyFromRows(rows))
	s.counts["shifts"] = len(rows)
	return err
}

// ---------------------------------------------------------------- jobs

// Booking times across the day: a morning rush, a lunchtime dip and an
// afternoon bump (hours 7–19, local time).
var hourWeights = []float64{2, 6, 10, 11, 9, 5, 6, 9, 8, 5, 3, 1.5, 0.5}

var failReasons = []string{"no_one_home", "no_one_home", "no_one_home", "access_denied", "wrong_address", "refused", "damaged"}

func (s *seeder) seedJobs(ctx context.Context) error {
	from := s.cfg.until.AddDate(0, -s.cfg.months, 0)
	cutoff := s.cfg.until.AddDate(0, 0, 1) // nothing happens after the end of the last day
	hourCum := cumulative(hourWeights)

	for d := from; !d.After(s.cfg.until); d = d.AddDate(0, 0, 1) {
		mu := fullDailyJobs * s.cfg.scale * s.growth(d) * s.load(d)
		n := int(math.Max(0, math.Round(mu+math.Sqrt(mu)*s.r.NormFloat64())))

		var active []account
		for _, a := range s.accounts {
			if !a.joined.After(d) {
				active = append(active, a)
			}
		}
		weights := make([]float64, len(active))
		for i, a := range active {
			weights[i] = a.weight
		}
		accCum := cumulative(weights)
		// Day-to-day noise in the failed-delivery rate: weather, roadworks, bad luck.
		dayFail := 0.03 + 0.012*s.r.Float64() + 0.006*math.Max(0, s.load(d)-1)

		for range n {
			hour := 7 + pick(s.r, hourCum)
			created := clock(d, hour, 0).Add(time.Duration(s.r.IntN(3600)) * time.Second)
			var acc *account
			if s.r.Float64() < 0.8 && len(active) > 0 {
				acc = &active[pick(s.r, accCum)]
			}
			s.makeJob(created, acc, math.Min(dayFail, 0.05), cutoff)
		}
		if len(s.jobRows) >= 20000 {
			if err := s.flushJobs(ctx); err != nil {
				return err
			}
		}
	}
	return s.flushJobs(ctx)
}

func (s *seeder) makeJob(created time.Time, acc *account, pFail float64, cutoff time.Time) {
	var city string
	var pickup int64
	tier := "standard"
	actor := "customer"
	var accountID any
	if acc != nil {
		city, tier, accountID = acc.city, acc.tier, acc.id
		actor = fmt.Sprintf("account:%d", acc.id)
		pickup = acc.home
		if s.r.Float64() < 0.15 {
			pickup = s.randomAddress(city)
		}
	} else {
		city = "glasgow"
		if s.r.Float64() < 0.4 {
			city = "edinburgh"
		}
		pickup = s.randomAddress(city)
	}
	dropoff := s.randomDropoff(city)
	for dropoff == pickup {
		dropoff = s.randomDropoff(city)
	}

	service := pricing.SameDay
	switch x := s.r.Float64(); {
	case x < 0.25:
		service = pricing.TwoHour
	case x >= 0.85:
		service = pricing.Scheduled
	}
	day := midnight(created, s.loc)
	var ws, we time.Time
	switch service {
	case pricing.TwoHour:
		ws = created.Add(15 * time.Minute)
		we = ws.Add(2 * time.Hour)
	case pricing.SameDay:
		if created.Hour() < 14 {
			ws, we = created, clock(day, 18, 0)
		} else {
			next := s.nextOpenDay(day.AddDate(0, 0, 1))
			ws, we = clock(next, 9, 0), clock(next, 18, 0)
		}
	case pricing.Scheduled:
		next := s.nextOpenDay(day.AddDate(0, 0, 1+s.r.IntN(3)))
		ws = clock(next, 9+2*s.r.IntN(4), 0)
		we = ws.Add(2 * time.Hour)
	}
	if closed(ws) {
		next := s.nextOpenDay(midnight(ws, s.loc))
		ws = clock(next, 9, 0)
		we = ws.Add(2 * time.Hour)
	}

	pu, dr := s.addrs[pickup-1].at, s.addrs[dropoff-1].at
	c := geo.Glasgow
	if city == "edinburgh" {
		c = geo.Edinburgh
	}
	zone, _ := geo.ZoneFor(c, dr)
	q, _ := pricing.Price(service, geo.HaversineKm(pu, dr)*1.3, zone.SurchargePence, tier)

	// Pick a driver on shift on the delivery day.
	drvIdx := -1
	if shifts := s.shiftsByDay[dayKey(midnight(ws, s.loc))][city]; len(shifts) > 0 {
		drvIdx = shifts[s.r.IntN(len(shifts))].driver
	}

	id := s.nextJobID
	s.nextJobID++
	type ev struct {
		typ     string
		at      time.Time
		actor   string
		payload any
		where   *geo.Point
	}
	evs := []ev{{typ: "created", at: created, actor: actor}}
	depot := c.Depot.Location
	mins := func(lo, hi int) time.Duration { return time.Duration(lo+s.r.IntN(hi-lo+1)) * time.Minute }

	if s.r.Float64() < 0.008 || drvIdx < 0 {
		evs = append(evs, ev{typ: "cancelled", at: created.Add(mins(5, 60)), actor: actor})
	} else {
		da := fmt.Sprintf("driver:%d", s.drivers[drvIdx].id)
		collected := maxTime(ws, created).Add(mins(5, 45))
		if service == pricing.Scheduled {
			// Collected ahead of time so the parcel leaves the depot as the slot opens.
			collected = maxTime(created.Add(mins(20, 60)), ws.Add(-mins(180, 300)))
		}
		evs = append(evs, ev{typ: "collected", at: collected, actor: da, where: &pu})
		ofd := collected.Add(time.Minute)
		if service != pricing.TwoHour {
			atDepot := collected.Add(mins(25, 80))
			evs = append(evs, ev{typ: "at_depot", at: atDepot, actor: "system", where: &depot})
			ofd = atDepot.Add(mins(20, 80))
			if service == pricing.Scheduled {
				ofd = maxTime(ofd, ws.Add(-mins(0, 20)))
			}
		}
		evs = append(evs, ev{typ: "out_for_delivery", at: ofd, actor: da, where: &depot})
		arrive := ofd.Add(mins(15, 180))
		if limit := we.Add(-5 * time.Minute); arrive.After(limit) {
			arrive = maxTime(ofd.Add(10*time.Minute), we.Add(-mins(5, 60)))
		}
		pLate := 0.035 + 0.06*math.Max(0, s.load(midnight(ws, s.loc))-1)
		if s.r.Float64() < pLate {
			arrive = we.Add(mins(5, 120))
		}
		if s.r.Float64() >= pFail {
			evs = append(evs, ev{typ: "delivered", at: arrive, actor: da, payload: map[string]string{"proof": "photo"}, where: &dr})
		} else {
			reason := failReasons[s.r.IntN(len(failReasons))]
			evs = append(evs, ev{typ: "failed", at: arrive, actor: da, payload: map[string]string{"reason": reason}, where: &dr})
			next := s.nextOpenDay(midnight(arrive, s.loc).AddDate(0, 0, 1))
			if reason != "wrong_address" && reason != "damaged" && s.r.Float64() < 0.8 {
				evs = append(evs,
					ev{typ: "reattempt_scheduled", at: arrive.Add(mins(30, 180)), actor: "system"},
					ev{typ: "out_for_delivery", at: clock(next, 9, 0).Add(mins(0, 120)), actor: da, where: &depot})
				final := evs[len(evs)-1].at.Add(mins(15, 240))
				if s.r.Float64() < 0.9 {
					evs = append(evs, ev{typ: "delivered", at: final, actor: da, payload: map[string]string{"proof": "photo"}, where: &dr})
				} else {
					evs = append(evs, ev{typ: "returned", at: final.Add(mins(60, 180)), actor: da, where: &pu})
				}
			} else {
				evs = append(evs, ev{typ: "returned", at: clock(next, 10, 0).Add(mins(0, 240)), actor: da, where: &pu})
			}
		}
	}

	status := "created"
	statusFor := map[string]string{
		"created": "created", "collected": "collected", "at_depot": "at_depot", "out_for_delivery": "out_for_delivery",
		"delivered": "delivered", "failed": "failed", "reattempt_scheduled": "at_depot", "returned": "returned", "cancelled": "cancelled",
	}
	for _, e := range evs {
		if !e.at.Before(cutoff) {
			break // the future hasn't happened yet
		}
		status = statusFor[e.typ]
		s.eventRows = append(s.eventRows, []any{id, e.typ, e.at, e.actor, e.payload})
		if e.where != nil && drvIdx >= 0 && !e.at.Before(s.pingFrom) {
			k := driverDay{drvIdx, dayKey(e.at.In(s.loc))}
			s.waypoints[k] = append(s.waypoints[k], waypoint{e.at, *e.where})
		}
	}
	var driverID any
	if drvIdx >= 0 && status != "created" && status != "cancelled" {
		driverID = s.drivers[drvIdx].id
	}
	s.jobRows = append(s.jobRows, []any{id, accountID, int64(1 + s.r.IntN(numCustomers)), pickup, dropoff,
		service, ws, we, status, driverID, q.TotalPence, created})
}

func (s *seeder) flushJobs(ctx context.Context) error {
	if len(s.jobRows) == 0 {
		return nil
	}
	if _, err := s.pool.CopyFrom(ctx, pgx.Identifier{"jobs"},
		[]string{"id", "account_id", "customer_id", "pickup_id", "dropoff_id", "service",
			"window_start", "window_end", "status", "driver_id", "price_pence", "created_at"},
		pgx.CopyFromRows(s.jobRows)); err != nil {
		return err
	}
	if _, err := s.pool.CopyFrom(ctx, pgx.Identifier{"job_events"},
		[]string{"job_id", "type", "at", "actor", "payload"}, pgx.CopyFromRows(s.eventRows)); err != nil {
		return err
	}
	s.counts["jobs"] += len(s.jobRows)
	s.counts["events"] += len(s.eventRows)
	s.jobRows, s.eventRows = s.jobRows[:0], s.eventRows[:0]
	return nil
}

// ---------------------------------------------------------------- pings

// seedPings writes a GPS ping every 10 s for every shift in the ping window.
// Drivers move in straight lines between the places their job events put
// them (live mode will use real roads via OSRM).
func (s *seeder) seedPings(ctx context.Context) error {
	if s.cfg.pingDays <= 0 {
		return nil
	}
	if _, err := s.pool.Exec(ctx, `CREATE UNLOGGED TABLE IF NOT EXISTS seed_pings
		(driver_id bigint, at timestamptz, lon float8, lat float8, speed real, battery real)`); err != nil {
		return err
	}
	// Daily partitions for the ping window (days the historical partition
	// covers are skipped).
	if _, err := s.pool.Exec(ctx, `SELECT ensure_ping_partitions(($1::timestamptz AT TIME ZONE 'Europe/London')::date, 7)`, s.pingFrom); err != nil {
		return err
	}
	const step = 10 * time.Second
	const maxKmh = 40.0 // city average; waypoints that would need more are skipped
	for d := s.pingFrom; !d.After(s.cfg.until); d = d.AddDate(0, 0, 1) {
		var rows [][]any
		for _, city := range []string{"glasgow", "edinburgh"} {
			depot := geo.Glasgow.Depot.Location
			if city == "edinburgh" {
				depot = geo.Edinburgh.Depot.Location
			}
			for _, sh := range s.shiftsByDay[dayKey(d)][city] {
				dr := s.drivers[sh.driver]
				path := []waypoint{{sh.start, depot}}
				var todays []waypoint
				for _, w := range s.waypoints[driverDay{sh.driver, dayKey(d)}] {
					if !w.at.Before(sh.start) && w.at.Before(sh.end) {
						todays = append(todays, w)
					}
				}
				sort.Slice(todays, func(i, j int) bool { return todays[i].at.Before(todays[j].at) })
				for _, w := range todays {
					prev := path[len(path)-1]
					if hrs := w.at.Sub(prev.at).Hours(); hrs > 0 && geo.HaversineKm(prev.p, w.p)/hrs <= maxKmh {
						path = append(path, w)
					}
				}
				lastWP := path[len(path)-1]
				home := sh.end
				if need := lastWP.at.Add(time.Duration(geo.HaversineKm(lastWP.p, depot) / 25 * float64(time.Hour))); need.After(home) {
					home = need // still driving back when the shift ends
				}
				path = append(path, waypoint{home, depot})

				battery := 100 - 8*s.r.Float64()
				seg := 0
				var last geo.Point
				for t := sh.start; t.Before(sh.end); t = t.Add(step) {
					for seg < len(path)-2 && !t.Before(path[seg+1].at) {
						seg++
						if dr.ebike && path[seg].p == depot {
							battery = 100 // fresh battery from the depot rack
						}
					}
					a, b := path[seg], path[seg+1]
					dist := geo.HaversineKm(a.p, b.p)
					travel := time.Duration(dist / 25 * float64(time.Hour)) // ~25 km/h when moving
					gap := b.at.Sub(a.at)
					if travel > gap {
						travel = gap
					}
					leave := b.at.Add(-travel) // wait at a, then drive to arrive at b on time
					p, speed := a.p, 0.0
					if t.After(leave) && travel > 0 {
						p = geo.Lerp(a.p, b.p, float64(t.Sub(leave))/float64(travel))
						speed = dist / travel.Hours()
					}
					p.Lat += (s.r.Float64() - 0.5) * 0.00008 // ~5 m of GPS noise
					p.Lon += (s.r.Float64() - 0.5) * 0.00014
					var bat any
					if dr.electric {
						if last != (geo.Point{}) {
							battery -= geo.HaversineKm(last, p) / dr.rangeKm * 100
						}
						bat = float32(math.Max(battery, 3))
					}
					last = p
					rows = append(rows, []any{dr.id, t, p.Lon, p.Lat, float32(speed), bat})
				}
			}
		}
		// Arrive in time order, like a live ingest would write them.
		sort.Slice(rows, func(i, j int) bool { return rows[i][1].(time.Time).Before(rows[j][1].(time.Time)) })
		if _, err := s.pool.CopyFrom(ctx, pgx.Identifier{"seed_pings"},
			[]string{"driver_id", "at", "lon", "lat", "speed", "battery"}, pgx.CopyFromRows(rows)); err != nil {
			return err
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO location_pings (driver_id, at, location, speed_kmh, battery_pct)
			SELECT driver_id, at, ST_SetSRID(ST_MakePoint(lon, lat), 4326)::geography, speed, battery
			FROM seed_pings ORDER BY at; TRUNCATE seed_pings`); err != nil {
			return err
		}
		s.counts["pings"] += len(rows)
		slog.Info("pings", "day", dayKey(d), "rows", len(rows))
	}
	if _, err := s.pool.Exec(ctx, `DROP TABLE seed_pings`); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO driver_positions
		SELECT p.* FROM drivers d CROSS JOIN LATERAL (
			SELECT driver_id, at, location, speed_kmh, battery_pct
			FROM location_pings WHERE driver_id = d.id ORDER BY at DESC LIMIT 1) p`)
	return err
}

func (s *seeder) finish(ctx context.Context) error {
	for _, t := range []string{"depots", "zones", "addresses", "accounts", "customers", "drivers", "vehicles", "shifts", "jobs", "job_events"} {
		if _, err := s.pool.Exec(ctx, fmt.Sprintf(
			`SELECT setval(pg_get_serial_sequence('%[1]s', 'id'), GREATEST((SELECT max(id) FROM %[1]s), 1))`, t)); err != nil {
			return err
		}
	}
	_, err := s.pool.Exec(ctx, `ANALYZE`)
	return err
}

// ---------------------------------------------------------------- helpers

func (s *seeder) randomAddress(city string) int64 {
	ids := s.addrsByCity[city]
	return ids[s.r.IntN(len(ids))]
}

// randomDropoff leans towards the centre, where most business deliveries go.
func (s *seeder) randomDropoff(city string) int64 {
	if s.r.Float64() < 0.45 {
		ids := s.addrsCentral[city]
		return ids[s.r.IntN(len(ids))]
	}
	return s.randomAddress(city)
}

func (s *seeder) nextOpenDay(d time.Time) time.Time {
	for closed(d) || d.Weekday() == time.Sunday {
		d = d.AddDate(0, 0, 1)
	}
	return d
}

// clock returns hour:minute local time on day d (safe across clock changes).
func clock(d time.Time, hour, minute int) time.Time {
	y, m, day := d.Date()
	return time.Date(y, m, day, hour, minute, 0, 0, d.Location())
}

func midnight(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func cumulative(w []float64) []float64 {
	out := make([]float64, len(w))
	sum := 0.0
	for i, x := range w {
		sum += x
		out[i] = sum
	}
	return out
}

// pick returns an index chosen with probability proportional to its weight.
func pick(r *rand.Rand, cum []float64) int {
	x := r.Float64() * cum[len(cum)-1]
	return sort.SearchFloat64s(cum, x)
}
