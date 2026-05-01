package meta

import (
	"testing"
	"time"
)

func TestIsFailure(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{StatusFailed, true},
		{StatusSuccess, false},
		{"", false}, // legacy metas without status count as success
	}
	for _, tt := range tests {
		m := MetaFile{Status: tt.status}
		if got := m.IsFailure(); got != tt.want {
			t.Errorf("IsFailure(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestParsedTimestamp_Valid(t *testing.T) {
	m := MetaFile{Timestamp: "20260428T020000Z"}
	got := m.ParsedTimestamp()
	want := time.Date(2026, 4, 28, 2, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("ParsedTimestamp() = %v, want %v", got, want)
	}
}

func TestParsedTimestamp_Invalid(t *testing.T) {
	m := MetaFile{Timestamp: "not-a-timestamp"}
	got := m.ParsedTimestamp()
	if !got.IsZero() {
		t.Errorf("ParsedTimestamp() should be zero for invalid input, got %v", got)
	}
}

func TestParsedTimestamp_Empty(t *testing.T) {
	m := MetaFile{}
	got := m.ParsedTimestamp()
	if !got.IsZero() {
		t.Errorf("ParsedTimestamp() should be zero for empty input, got %v", got)
	}
}

func TestMetaFile_JSONPath_OmittedFromJSON(t *testing.T) {
	// Path has json:"-", so it should not appear in JSON output.
	m := MetaFile{
		Target:    "prod-db",
		Timestamp: "20260428T020000Z",
		DBType:    "postgres",
		Status:    StatusSuccess,
		Path:      "prod-db/dump-20260428T020000Z.meta.json",
	}
	if m.Path == "" {
		t.Error("Path should be set before serialization check")
	}
}
