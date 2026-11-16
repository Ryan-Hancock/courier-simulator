# Brief 1: "The tracking page crawls at 5pm."

**From:** Morag Reid, Head of Operations
**To:** the freelancer
**Shows off:** Postgres performance

> Every evening around five the tracking page goes to treacle. Customers get a
> spinner, give up, and phone us instead. The office line is jammed from half
> four to six with "where's my parcel?" calls that the page should be
> answering. It used to be fine. It's been getting worse for months and nobody
> here has had time to look at it. The drivers' app seems to get slower too
> when it's bad. Can you find out what's wrong and fix it, without us losing
> the GPS history? Finance want it for the EV work next year.

## What "done" means

- The tracking page answers in well under a second at the 5pm peak.
- It stays that way as the history grows. The fix must not get worse every day.
- No GPS history is lost.
- Before and after are measured the same way, from the same frozen moment.

## How it is measured

```sh
make seed-brief-01     # full history with 18 months of GPS pings (the "before" database)
make up
make bench-01          # replays 09:00–19:00 on Tue 10 Nov 2026, writes the article data
```

`geksim bench` freezes the company at a moment (every parcel back in the status it had
then), then replays tracking-page polls and driver pings against `tracking-ingest`.

Traffic model (an assumption, stated in the article): recipients get the tracking link by
SMS when their parcel goes out for delivery; half of them have the page open, and it
refreshes every 15 s.

## Tags

- `brief-01-before`: the naive `location_pings` table and queries.
- `brief-01-after`: the fix.

Results: [results.md](results.md) (after the fix).
