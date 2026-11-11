package main

import (
	"bufio"
	"context"
	_ "embed"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"gekgo/internal/geo"
)

// streetsTSV is a snapshot of named street segments from OpenStreetMap:
// city <TAB> street name <TAB> lat <TAB> lon (one row per OSM way centre).
// © OpenStreetMap contributors, ODbL. Refresh with `geksim osm`.
//
//go:embed data/streets.tsv
var streetsTSV string

const overpassURL = "https://overpass-api.de/api/interpreter"

// Bounding boxes cover each city's 12 km service radius.
var osmBoxes = map[string]string{
	"glasgow":   "55.75,-4.45,55.97,-4.06",
	"edinburgh": "55.84,-3.38,56.06,-3.00",
}

func runOSM(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("osm", flag.ExitOnError)
	out := fs.String("out", "sim/geksim/data/streets.tsv", "file to write")
	fs.Parse(args)

	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, c := range geo.Cities {
		q := fmt.Sprintf(`[out:csv(name,::lat,::lon;false;"\t")][timeout:120];`+
			`way["highway"~"^(residential|primary|secondary|tertiary|unclassified|living_street)$"]["name"](%s);out center;`,
			osmBoxes[c.Key])
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, overpassURL, strings.NewReader("data="+url.QueryEscape(q)))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", "gekgo-geksim/0.1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("overpass %s: %s", c.Key, resp.Status)
		}
		n := 0
		for _, line := range strings.Split(string(body), "\n") {
			parts := strings.Split(line, "\t")
			if len(parts) != 3 || parts[0] == "" {
				continue
			}
			lat, err1 := strconv.ParseFloat(parts[1], 64)
			lon, err2 := strconv.ParseFloat(parts[2], 64)
			if err1 != nil || err2 != nil {
				continue
			}
			if geo.HaversineKm(c.Centre, geo.Point{Lat: lat, Lon: lon}) >= geo.ServiceRadiusKm {
				continue
			}
			fmt.Fprintf(w, "%s\t%s\t%.6f\t%.6f\n", c.Key, parts[0], lat, lon)
			n++
		}
		slog.Info("fetched streets", "city", c.Key, "segments", n)
	}
	return w.Flush()
}

type street struct {
	city string
	name string
	at   geo.Point
}

func loadStreets() (map[string][]street, error) {
	out := map[string][]street{}
	for _, line := range strings.Split(streetsTSV, "\n") {
		p := strings.Split(line, "\t")
		if len(p) != 4 {
			continue
		}
		lat, _ := strconv.ParseFloat(p[2], 64)
		lon, _ := strconv.ParseFloat(p[3], 64)
		out[p[0]] = append(out[p[0]], street{p[0], p[1], geo.Point{Lat: lat, Lon: lon}})
	}
	for _, c := range geo.Cities {
		if len(out[c.Key]) == 0 {
			return nil, fmt.Errorf("no streets for %s in data/streets.tsv; run `geksim osm`", c.Key)
		}
	}
	return out, nil
}
