package main

import "testing"

func TestNextOccurrence(t *testing.T) {
	tests := []struct {
		name, due, rule, today, want string
	}{
		{"daily heute fällig", "2026-07-20", "daily", "2026-07-20", "2026-07-21"},
		{"daily überfällig springt in die Zukunft", "2026-07-01", "daily", "2026-07-20", "2026-07-21"},
		{"weekly heute fällig", "2026-07-20", "weekly", "2026-07-20", "2026-07-27"},
		{"weekly 3 Wochen überfällig behält Wochentag", "2026-06-29", "weekly", "2026-07-20", "2026-07-27"},
		{"weekly Zukunftstask wird trotzdem gesteppt", "2026-07-25", "weekly", "2026-07-20", "2026-08-01"},
		{"monthly normal", "2026-07-15", "monthly", "2026-07-15", "2026-08-15"},
		{"monthly 31. Jan clampt auf 28. Feb", "2026-01-31", "monthly", "2026-01-31", "2026-02-28"},
		{"monthly 31. Jan Schaltjahr clampt auf 29. Feb", "2028-01-31", "monthly", "2028-01-31", "2028-02-29"},
		{"monthly 31. Dez über Jahresgrenze", "2026-12-31", "monthly", "2026-12-31", "2027-01-31"},
		// Documented drift: after the February clamp, the 28th becomes
		// the new anchor
		{"monthly 30. über Feb hinweg driftet auf 28.", "2026-01-30", "monthly", "2026-03-01", "2026-03-28"},
		{"monthly 31. Aug auf 30. Sep", "2026-08-31", "monthly", "2026-08-31", "2026-09-30"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nextOccurrence(tc.due, tc.rule, tc.today)
			if err != nil {
				t.Fatalf("nextOccurrence(%q, %q, %q): %v", tc.due, tc.rule, tc.today, err)
			}
			if got != tc.want {
				t.Errorf("nextOccurrence(%q, %q, %q) = %q, want %q", tc.due, tc.rule, tc.today, got, tc.want)
			}
			if got <= tc.today {
				t.Errorf("Ergebnis %q liegt nicht in der Zukunft (today %q)", got, tc.today)
			}
		})
	}
}

func TestNextOccurrenceInvalidRule(t *testing.T) {
	if _, err := nextOccurrence("2026-07-20", "yearly", "2026-07-20"); err == nil {
		t.Error("erwartete Fehler für unbekannte Wiederholung")
	}
	if _, err := nextOccurrence("kaputt", "daily", "2026-07-20"); err == nil {
		t.Error("erwartete Fehler für kaputtes Datum")
	}
}
