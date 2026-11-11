package geo

import (
	"math"
	"testing"
)

func TestHaversineGlasgowToEdinburgh(t *testing.T) {
	d := HaversineKm(Glasgow.Centre, Edinburgh.Centre)
	if math.Abs(d-67) > 2 {
		t.Errorf("Glasgow–Edinburgh = %.1f km, want about 67", d)
	}
}

func TestZoneFor(t *testing.T) {
	if z, ok := ZoneFor(Glasgow, Glasgow.Centre); !ok || z.Name != "central" {
		t.Errorf("centre should be central, got %q %v", z.Name, ok)
	}
	if z, ok := ZoneFor(Glasgow, Glasgow.Depot.Location); !ok || z.Name != "outer" {
		t.Errorf("Hillington should be outer, got %q %v", z.Name, ok)
	}
	if _, ok := ZoneFor(Glasgow, Edinburgh.Centre); ok {
		t.Error("Edinburgh is outside Glasgow's service area")
	}
}
