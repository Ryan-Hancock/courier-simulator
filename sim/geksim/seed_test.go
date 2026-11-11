package main

import (
	"testing"
	"time"
)

func TestBlackFriday(t *testing.T) {
	for year, want := range map[int]int{2025: 28, 2026: 27} {
		if got := blackFriday(year, time.UTC).Day(); got != want {
			t.Errorf("Black Friday %d = %d Nov, want %d", year, got, want)
		}
	}
}

func TestClockSurvivesDST(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/London")
	day := time.Date(2026, time.October, 25, 0, 0, 0, 0, loc) // clocks go back
	if got := clock(day, 9, 0).Hour(); got != 9 {
		t.Errorf("clock() hour = %d, want 9", got)
	}
}
