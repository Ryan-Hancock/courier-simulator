// Command geksim keeps GekGo "in business": it builds the database, seeds
// 18 months of trading history and (later) runs live simulated days.
//
//	geksim migrate              apply db/migrations
//	geksim osm                  refresh data/streets.tsv from OpenStreetMap
//	geksim seed [flags]         build history (see geksim seed -h)
//	geksim stats -out DIR       export measured numbers and charts for articles
//	geksim bench [flags]        replay one moment's tracking load and measure it
//	geksim bench-charts -out DIR  draw brief 1's charts from the bench data
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	_ "time/tzdata" // Europe/London in minimal containers

	"gekgo/db"
	"gekgo/internal/dbx"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "migrate":
		err = runMigrate(ctx)
	case "osm":
		err = runOSM(ctx, args)
	case "seed":
		err = runSeed(ctx, args)
	case "stats":
		err = runStats(ctx, args)
	case "bench":
		err = runBench(ctx, args)
	case "bench-charts":
		err = runBenchCharts(ctx, args)
	default:
		usage()
	}
	if err != nil {
		slog.Error(os.Args[1]+" failed", "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: geksim migrate | osm | seed [flags] | stats [flags] | bench [flags] | bench-charts [flags]")
	os.Exit(2)
}

func runMigrate(ctx context.Context) error {
	pool, err := dbx.Connect(ctx, dbx.URL())
	if err != nil {
		return err
	}
	defer pool.Close()
	return dbx.Migrate(ctx, pool, db.Migrations, "migrations")
}
