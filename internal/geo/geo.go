// Package geo holds GekGo's fixed places and small geometry helpers.
package geo

import "math"

// Point is a WGS84 coordinate.
type Point struct {
	Lat, Lon float64
}

// City describes one of the two cities GekGo serves.
type City struct {
	Key    string // "glasgow" or "edinburgh"
	Name   string
	Centre Point // pricing zones are rings around this point
	Depot  Depot
	Prefix string // postcode area
}

// Depot is a GekGo hub.
type Depot struct {
	Name     string
	Location Point
}

// Zone is a ring around a city centre, from InnerKm to OuterKm.
type Zone struct {
	Name           string
	InnerKm        float64
	OuterKm        float64
	SurchargePence int
}

var (
	Glasgow = City{
		Key: "glasgow", Name: "Glasgow", Prefix: "G",
		Centre: Point{55.8611, -4.2502}, // George Square
		Depot:  Depot{"Hillington", Point{55.8566, -4.3580}},
	}
	Edinburgh = City{
		Key: "edinburgh", Name: "Edinburgh", Prefix: "EH",
		Centre: Point{55.9520, -3.1895}, // Waverley
		Depot:  Depot{"Sighthill", Point{55.9237, -3.2905}},
	}
	Cities = []City{Glasgow, Edinburgh}

	// Zones are the same shape in both cities. Beyond the last ring is
	// outside the service area.
	Zones = []Zone{
		{"central", 0, 2.5, 150}, // city-centre traffic and low emission zone
		{"inner", 2.5, 6, 0},
		{"outer", 6, 12, 250},
	}
)

// ServiceRadiusKm is the edge of the outermost zone.
const ServiceRadiusKm = 12

// HaversineKm returns the great-circle distance between two points.
func HaversineKm(a, b Point) float64 {
	const r = 6371.0
	la1, la2 := a.Lat*math.Pi/180, b.Lat*math.Pi/180
	dLat := la2 - la1
	dLon := (b.Lon - a.Lon) * math.Pi / 180
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(la1)*math.Cos(la2)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Sqrt(h))
}

// ZoneFor returns the zone a point falls in for a city, and false if it is
// outside the service area.
func ZoneFor(c City, p Point) (Zone, bool) {
	d := HaversineKm(c.Centre, p)
	for _, z := range Zones {
		if d >= z.InnerKm && d < z.OuterKm {
			return z, true
		}
	}
	return Zone{}, false
}

// CityFor returns the city whose service area contains p.
func CityFor(p Point) (City, bool) {
	for _, c := range Cities {
		if HaversineKm(c.Centre, p) < ServiceRadiusKm {
			return c, true
		}
	}
	return City{}, false
}

// Lerp moves a fraction t of the way from a to b (fine at city scale).
func Lerp(a, b Point, t float64) Point {
	return Point{a.Lat + (b.Lat-a.Lat)*t, a.Lon + (b.Lon-a.Lon)*t}
}
