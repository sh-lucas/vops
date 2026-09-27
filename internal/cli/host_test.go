package cli

import (
	"testing"
	"time"
)

func TestParseLogTime(t *testing.T) {
	if got, err := parseLogTime(""); err != nil || got != 0 {
		t.Fatalf("empty: %d, %v", got, err)
	}
	if got, err := parseLogTime("2h"); err != nil || got > time.Now().Add(-2*time.Hour).Unix()+1 || got < time.Now().Add(-2*time.Hour).Unix()-1 {
		t.Fatalf("2h: %d, %v", got, err)
	}
	want := time.Date(2024, 3, 15, 0, 0, 0, 0, time.Local).Unix()
	if got, err := parseLogTime("2024-03-15"); err != nil || got != want {
		t.Fatalf("date: %d, %v (want %d)", got, err, want)
	}
	want = time.Date(2024, 3, 15, 14, 30, 0, 0, time.Local).Unix()
	if got, err := parseLogTime("2024-03-15 14:30"); err != nil || got != want {
		t.Fatalf("datetime: %d, %v (want %d)", got, err, want)
	}
	want = time.Date(2024, 3, 15, 14, 30, 0, 0, time.UTC).Unix()
	if got, err := parseLogTime("2024-03-15T14:30:00Z"); err != nil || got != want {
		t.Fatalf("rfc3339: %d, %v (want %d)", got, err, want)
	}
	if _, err := parseLogTime("not a time"); err == nil {
		t.Fatal("expected an error for garbage input")
	}
}
