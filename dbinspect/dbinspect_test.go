package dbinspect

import (
	"context"
	"strings"
	"testing"
)

func TestFlavorOf(t *testing.T) {
	tests := []struct {
		driver string
		want   string
	}{
		{"mysql", "mysql"},
		{"mysql:isutools", "mysql"},
		{"pgx", "postgres"},
		{"postgres", "postgres"},
		{"sqlite3", "unknown"},
	}
	for _, tt := range tests {
		if got := flavorOf(tt.driver); got != tt.want {
			t.Errorf("flavorOf(%q) = %q, want %q", tt.driver, got, tt.want)
		}
	}
}

func TestCollectUnsupportedFlavor(t *testing.T) {
	s := Collect(context.Background(), "sqlite3", "file.db")
	if s.Error == "" {
		t.Error("unsupported flavor must set Error")
	}
	if !strings.Contains(s.Error, "MySQL") {
		t.Errorf("Error = %q, want mention of supported flavors", s.Error)
	}
}

func TestCollectUnknownDriverFailsOpen(t *testing.T) {
	s := Collect(context.Background(), "mysql-not-registered-here", "dsn")
	if s == nil {
		t.Fatal("Collect must never return nil")
	}
	if s.Error == "" {
		t.Error("unknown driver must set Error, not panic")
	}
}
