// Package pricing turns a delivery request into a price in integer pence.
// The seeder and booking-api share it so history and live quotes agree.
package pricing

import (
	"fmt"
	"math"
)

// Services GekGo sells.
const (
	TwoHour   = "two_hour"
	SameDay   = "same_day"
	Scheduled = "scheduled"
)

var basePence = map[string]int{
	TwoHour:   1450,
	SameDay:   895,
	Scheduled: 695,
}

const (
	includedKm    = 3.0
	perKmPence    = 45
	enterpriseOff = 10 // percent
)

// Quote is the breakdown of a price.
type Quote struct {
	Service        string `json:"service"`
	BasePence      int    `json:"base_pence"`
	DistancePence  int    `json:"distance_pence"`
	SurchargePence int    `json:"surcharge_pence"`
	DiscountPence  int    `json:"discount_pence"`
	TotalPence     int    `json:"total_pence"`
}

// Price computes a quote. distanceKm is the straight-line distance between
// pickup and drop-off; surchargePence comes from the drop-off zone.
func Price(service string, distanceKm float64, surchargePence int, tier string) (Quote, error) {
	base, ok := basePence[service]
	if !ok {
		return Quote{}, fmt.Errorf("unknown service %q", service)
	}
	q := Quote{Service: service, BasePence: base, SurchargePence: surchargePence}
	if extra := distanceKm - includedKm; extra > 0 {
		q.DistancePence = int(math.Round(extra * perKmPence))
	}
	sub := q.BasePence + q.DistancePence + q.SurchargePence
	if tier == "enterprise" {
		q.DiscountPence = sub * enterpriseOff / 100 // integer division rounds down in the customer's favour
	}
	q.TotalPence = sub - q.DiscountPence
	return q, nil
}
