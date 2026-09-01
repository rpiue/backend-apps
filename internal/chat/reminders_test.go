package chat

import (
	"reflect"
	"testing"
)

func TestParseHHMM(t *testing.T) {
	cases := map[string]struct {
		min int
		ok  bool
	}{
		"08:00": {480, true},
		"12:30": {750, true},
		"23:59": {1439, true},
		"00:00": {0, true},
		"24:00": {0, false},
		"08:60": {0, false},
		"8":     {0, false},
		"abc":   {0, false},
	}
	for in, want := range cases {
		got, ok := parseHHMM(in)
		if ok != want.ok || (ok && got != want.min) {
			t.Errorf("parseHHMM(%q) = (%d,%v), want (%d,%v)", in, got, ok, want.min, want.ok)
		}
	}
}

func TestNormalizeTimes(t *testing.T) {
	got := normalizeTimes([]string{"8:0", "08:00", "12:5", "bad", "23:59", "12:05"})
	want := []string{"08:00", "12:05", "23:59"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeTimes = %v, want %v (dedupe + formato HH:MM)", got, want)
	}
}

func TestReminderMode(t *testing.T) {
	if reminderMode("once") != "once" {
		t.Fatal("once")
	}
	if reminderMode("daily") != "daily" || reminderMode("") != "daily" || reminderMode("x") != "daily" {
		t.Fatal("daily default")
	}
}
