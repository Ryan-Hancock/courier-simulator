package pricing

import "testing"

func TestPrice(t *testing.T) {
	tests := []struct {
		name      string
		service   string
		km        float64
		surcharge int
		tier      string
		want      int
	}{
		{"short same-day", SameDay, 2, 0, "standard", 895},
		{"distance beyond 3 km", SameDay, 13, 0, "standard", 895 + 450},
		{"central surcharge", TwoHour, 1, 150, "standard", 1600},
		{"enterprise 10% off, rounded for the customer", Scheduled, 3, 0, "enterprise", 695 - 69},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := Price(tt.service, tt.km, tt.surcharge, tt.tier)
			if err != nil {
				t.Fatal(err)
			}
			if q.TotalPence != tt.want {
				t.Errorf("total = %d, want %d", q.TotalPence, tt.want)
			}
			if q.BasePence+q.DistancePence+q.SurchargePence-q.DiscountPence != q.TotalPence {
				t.Errorf("breakdown does not add up: %+v", q)
			}
		})
	}
	if _, err := Price("teleport", 1, 0, "standard"); err == nil {
		t.Error("unknown service should fail")
	}
}
