# Fictional Company Portfolio — Design

Nov 10, 2026 · @Ryan Hancock

## The company

GekGo is a made-up same-day courier serving Glasgow and Edinburgh. It grew fast, and its software never caught up. That gap is your source of client problems.

| Fact | Value (fictional) |
| --- | --- |
| Founded | 2019, Glasgow |
| Staff | 140 (95 drivers and riders, 20 dispatch and ops, 25 office) |
| Fleet | 85 vans (60 diesel, 25 electric) and 30 e-cargo bikes |
| Volume | About 6,000 parcels a day, 3x that at Christmas |
| Customers | 300 business accounts (florists, print shops, law firms, restaurants) plus a consumer app |
| Services | 2-hour, same-day and scheduled delivery |
| Tech team | One overstretched developer, so they hire freelancers (you) |

**Cast for the articles**

- **Morag Reid, Head of Operations:** writes most of the briefs and cares about drivers and late deliveries.
- **Dev Patel, Finance Director:** cares about invoices, the cloud bill and refunds.
- **Aisha Bello, Commercial Lead:** brings in big clients who want APIs and carbon reports.

Before you publish anything, check the name doesn't clash with a real UK courier (Companies House and a web search). Rename it if it does.

## Why a courier

A courier uses your whole stack, and every client understands it at once.

- **Your stack, end to end:** Go services, Postgres with PostGIS, real-time tracking, queues, and GKE on GCP.
- **Hard problems with clear numbers:** late deliveries, slow queries and cloud costs are all easy to measure before and after a fix.
- **Easy for any client to read:** every business has orders, customers, invoices and peak days, so a fix for GekGo reads as a fix for them.
- **A net-zero thread:** EV routing and emissions reporting link the portfolio to your RAINZ and IDCORE research interests.

## System landscape

GekGo runs a set of Go services on GCP. Most of them were written in a hurry as the company grew, and that's where the realistic problems live.

| Component | Built with | Owns | Talks via |
| --- | --- | --- | --- |
| booking-api | Go | Business accounts, quotes, bookings, partner API keys | REST and gRPC; publishes `booking.created` |
| dispatch | Go, PostGIS | Assigning jobs to drivers, batching stops into routes | Consumes bookings; publishes `route.assigned` |
| tracking-ingest | Go | GPS pings from drivers (one every 10 s) and proof of delivery | HTTP in, Pub/Sub, partitioned Postgres tables |
| notifications | Go | SMS and email ETAs, delivery photos | Consumes job events |
| billing | Go | Monthly invoices, refunds, carbon per parcel | Reads job events; writes invoices |
| Customer web and app | React (Next.js) | Booking, account management, live tracking page | booking-api, Server-Sent Events |
| Driver app | React PWA, offline-first | Today's route, scans, photos, signatures | tracking-ingest, sync queue |
| Ops dashboard | Angular | Live map, exceptions, driver chat, admin | Every service, through a gateway |

**Platform:** GKE Autopilot, Cloud SQL (Postgres 16 with PostGIS), Pub/Sub, Cloud Storage for photos, Terraform, and OpenTelemetry into Grafana. Locally everything runs in Docker Compose.

## Core data model

Sixteen Postgres tables are enough to carry every brief. A job's history lives in an append-only events table, which makes auditing, billing and replays straightforward.

| Table | Key columns | Why it matters |
| --- | --- | --- |
| `accounts` | id, name, tier, billing\_terms, api\_key\_hash | Business customers and their contracts |
| `addresses` | id, line1, postcode, location `geography(Point)` | Geocoded once, reused everywhere |
| `depots` | id, name, location | Hillington (Glasgow) and Sighthill (Edinburgh) |
| `zones` | id, depot\_id, area `geography(Polygon)`, surcharge\_pence | Pricing and dispatch boundaries |
| `jobs` | id, account\_id, pickup\_id, dropoff\_id, service, window\_start, window\_end, status, price\_pence | One parcel movement |
| `job_events` | id, job\_id, type, at, actor, payload `jsonb` | Append-only status history |
| `drivers` | id, name, employment\_type, home\_depot\_id | Employed or self-employed |
| `vehicles` | id, type, is\_electric, range\_km, battery\_kwh | Vans and e-cargo bikes |
| `shifts` | id, driver\_id, vehicle\_id, starts\_at, ends\_at | Who drives what, when |
| `routes` | id, shift\_id, planned\_km, planned\_at | A driver's batch of stops |
| `route_stops` | route\_id, job\_id, seq, eta, arrived\_at | Order and ETAs |
| `location_pings` | driver\_id, at, location, speed, battery\_pct | About 400,000 rows a day (115 vehicles, one ping every 10 s), 10x in load tests, partitioned by day |
| `invoices` / `invoice_lines` | account\_id, period, total\_pence / job\_id, amount\_pence | Money as integer pence, never floats |
| `outbox` | id, topic, payload, published\_at | Transactional outbox for reliable events |
| `idempotency_keys` | key, account\_id, response, created\_at | Safe retries from partners |

**Job lifecycle:** created → collected → at\_depot → out\_for\_delivery → delivered, or failed → reattempt or returned.

## The simulator

A Go program, `geksim`, keeps GekGo "in business" around the clock. It's the simulator you were after: every brief starts from a scenario it can replay, and every article quotes before and after numbers it measured.

**1. Seeder (history).** Builds 18 months of past trading so the database feels lived in.

- 300 accounts, 40,000 customers and 25,000 addresses. Names come from a faker library; addresses sit on real Glasgow and Edinburgh streets from OpenStreetMap, with house numbers made up.
- About 3 million past jobs, with real-looking patterns: Monday surges, a lunchtime dip, Black Friday and Christmas peaks, and a 3–5% failed-delivery rate.

**2. Live mode (agents).** Runs a simulated day against the real services, through their public APIs only.

- Customers book on a time-of-day curve, using a Poisson process.
- Drivers follow real roads (routes from an OSRM server on OpenStreetMap data), send GPS pings and scan parcels.
- Electric vans drain their batteries by distance and weather; vans break down now and then.

**3. Controls.**

- Clock speed from 1x up to 60x, so a full day runs in 15 minutes.
- A load multiplier for peak days.
- Fault injection: slow database, Pub/Sub delays, a partner sending the same webhook twice, a bad deploy, a lost phone signal.

**4. Scenarios as files.** Each brief has a `scenarios/<brief>.yaml` with a fixed random seed, so readers can rerun the exact incident.

**5. Public showcase.** A read-only ops dashboard shows the live map of vans moving around Glasgow. Link it from every article and from your CV.

## Business layer

A thin model of the economy sits on top of geksim, so problems cost money and customers react to the service they get. Give it one week, and build it after briefs 1–3 are published.

| Loop | Input | Effect | Starting setting (made up) |
| --- | --- | --- | --- |
| Service → retention | Each account's 30-day on-time rate and failed-delivery rate | Chance the account leaves this month; happy accounts book more | 1% monthly churn at 97% on time, plus 0.5 points for every point below |
| Price → demand | Price per zone and service | Number of bookings | Elasticity of −0.6 |
| Costs → daily profit and loss | Driver hours, pence per km (diesel or electric), van leases, the cloud bill | A daily profit and loss for the business | The cloud line uses the demo project's real GCP billing export |
| Events → demand shocks | Seasons, storms, a rival closing, a big contract won | A booking multiplier for a set period | Defined in each scenario file |

**How it plugs in**

- A daily tick in geksim reads `job_events`, scores each account, and adjusts its booking rate for the next day.
- All settings live in `sim/economy.yaml` and are labelled as assumptions.
- Results go to a `ledger_days` table. The ops dashboard gets a profit-and-loss panel and an "accounts at risk" list.
- Fast-forward mode runs a simulated quarter in about an hour, which shows the long-term effect of a fix.

**How articles use it:** each article's "After" section adds one line in pounds. That figure comes from re-running the same simulated quarter with and without the fix.

**Guardrails:** keep to the one week, and stop tuning once the results look plausible. State the assumptions in every article, and never let this layer hold up a brief. Building it can be an article in its own right: "Simulating a business in Go".

## Client problems backlog

Twelve briefs, in the order to write them. Each becomes one Medium article and one tagged branch. Start with 1–3: they need only the core services and give strong before and after numbers.

| # | The brief (who asks) | What you build | Shows off |
| --- | --- | --- | --- |
| 1 | "The tracking page crawls at 5pm." (Morag) | Daily partitions on `location_pings`, the right indexes, a latest-position table, EXPLAIN before and after | Postgres performance |
| 2 | "Two drivers turned up for the same parcel." (Morag) | A fix for the dispatch race using `SELECT … FOR UPDATE SKIP LOCKED`, plus a concurrency test that fails first | Go concurrency, correctness |
| 3 | "Customers keep phoning to ask where their parcel is." (Morag) | Live ETAs over Server-Sent Events, recalculated from pings | Real-time full stack |
| 4 | "Our biggest shop's orders arrive twice." (Aisha) | Idempotency keys and a transactional outbox for webhooks | API reliability |
| 5 | "Drivers lose scans in tunnels and tower blocks." (Morag) | Offline-first sync queue in the driver PWA, with conflict rules | Offline web apps |
| 6 | "Every release breaks something for the drivers." (Morag) | Canary deploys on GKE, feature flags, and automatic rollback when the simulator's error rate jumps | Safe deployment |
| 7 | "Black Friday took us down last year." (Aisha) | A 10x load test with the simulator and k6, autoscaling, backpressure on Pub/Sub | Scaling on GKE |
| 8 | "Why do deliveries fail? We only find out next day." (Morag) | OpenTelemetry tracing and a failure dashboard | Observability |
| 9 | "The cloud bill doubled." (Dev) | A cost breakdown, right-sizing, moving quiet services to Cloud Run | Cost reduction |
| 10 | "Month-end invoices are wrong again." (Dev) | Integer-pence money, the BST changeover fix, property-based tests | Correctness with money |
| 11 | "Which diesel vans can we swap for electric?" (Aisha) | Route-range analysis from pings, a charging plan, carbon per parcel | Data analysis, net zero |
| 12 | "Could robots do the last 200 metres in the city centre?" (Aisha) | A simulated kerbside robot leg with a tamper-evident teleoperation log | Robotics, research link |

Briefs 11 and 12 also give you material to point to in research applications.

## Repo and infrastructure layout

Use one public monorepo, so a reader can clone it and run GekGo with one command. Each brief gets a `before` and an `after` tag.

```text
gekgo/
  services/        booking-api, dispatch, tracking-ingest, notifications, billing (Go)
  web/             customer (Next.js), ops-dashboard (Angular), driver-app (PWA)
  sim/             geksim: seeder, agents, scenarios/*.yaml
  db/              migrations, seed snapshots
  deploy/          terraform/ (GCP), k8s/ (local and demo overlays)
  briefs/          one folder per brief: brief.md, results.md
  articles/        one folder per article: outline, design notes, visuals, measured data
  compose.yaml     make up = the whole company on a laptop
```

- **Local:** Docker Compose, with OSRM and Postgres in containers.
- **Demo:** GKE Autopilot and a small Cloud SQL instance, in its own GCP project with a budget alert. Scale it to zero overnight; live mode runs in office hours only.
- **Tags:** `brief-01-before` and `brief-01-after`, so each article links to the exact code.

## Build roadmap

The first article can go out in about four weeks. After that, aim for one brief every one to two weeks. Each phase ends with a gate to meet before moving on.

1. **Weeks 1–2: the skeleton.** Schema and migrations, the seeder, booking-api, tracking-ingest, and the ops dashboard map. Deploy the demo.
   - Gate: the public map shows 18 months of history and today's simulated vans.
2. **Weeks 3–4: live mode.** geksim agents, dispatch, and the driver PWA.
   - Gate: a simulated day runs end to end in 15 minutes at 60x.
3. **Week 5 onward: briefs 1–3.** Write each one as the brief, then the fix, then the numbers, then the article.
   - Gate: three articles live, each linking to its tags and the demo.
4. **One week: the business layer.** Feedback loops, the daily profit and loss, and the dashboard panel (see Business layer).
   - Gate: a simulated quarter with and without brief 1's fix shows a believable difference in pounds.
5. **Then: briefs 4–10,** in whatever order freelance leads make most relevant.
6. **Later: briefs 11–12** and a one-page case-study site that links everything.

## Article template and ground rules

Every article follows the same shape. A reader who runs a real business should see their own problem in it and know you can fix it.

```markdown
# How I [result, with a number] for a [type of business]

> GekGo is a fictional company I run on simulated data to show real engineering work.

## The brief
Who asked, in their words, and why it hurt the business.

## Before
The measured numbers: latency, cost, error rate. Include a chart.

## Options I considered
Two or three options and the trade-offs between them.

## What I built
The key code and diagrams, kept short. Link to the before and after tags.

## After
The same measurements, re-run from the same scenario.

## Try it yourself
The repo, the scenario file and the live demo.

## Could this help your business?
One paragraph and a contact link.
```

**Ground rules**

- Say clearly near the top of every article that GekGo is fictional. Never suggest it's a real client.
- Only quote numbers the simulator actually measured, and keep the scenario file so anyone can reproduce them.
- Check the name against Companies House before you publish.
- Use made-up people, and never real customer data, even as a sample.
- OpenStreetMap data requires attribution: credit it on the dashboard and in the README.
