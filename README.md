# GekGo

> GekGo is a fictional same-day courier in Glasgow and Edinburgh, run on simulated data to show real engineering work. The people, accounts and parcels are made up.

## Run it

Needs Docker and Go 1.26.

```sh
make up      # Postgres + PostGIS, migrations, booking-api, tracking-ingest, ops dashboard
make seed    # 18 months of history at 10% volume (~30 s); make seed-full for ~2.5M jobs
open http://localhost:8080
```

Book something:

```sh
curl -s -X POST localhost:8081/v1/bookings \
  -H 'Authorization: Bearer gk_test_demo_account_0001' \
  -d '{"service":"two_hour",
       "pickup":{"line1":"12 Byres Road","postcode":"G11 5JY","lat":55.8745,"lon":-4.2930},
       "dropoff":{"line1":"200 Duke Street","postcode":"G31 1JD","lat":55.8580,"lon":-4.2300},
       "recipient":{"name":"Isla Reid","phone":"07700 900123"}}'
```

## Layout

```text
services/        booking-api, tracking-ingest (Go)
internal/        shared Go packages: pricing, geo, dbx, httpx
sim/geksim/      the simulator: migrate, osm, seed, stats
db/migrations/   SQL, applied in order by `geksim migrate`
web/             ops-dashboard (Angular + Leaflet)
articles/        one folder per article: outline, design notes, visuals, measured data
compose.yaml     make up = the whole company on a laptop
```

The full plan is in [Fictional Company Portfolio — Design.md](Fictional%20Company%20Portfolio%20—%20Design.md).

## Data

Street names and positions © [OpenStreetMap](https://www.openstreetmap.org/copyright) contributors, available under the Open Database Licence. `sim/geksim/data/streets.tsv` is derived from OpenStreetMap and is therefore also under the ODbL. Map tiles © OpenStreetMap contributors.
