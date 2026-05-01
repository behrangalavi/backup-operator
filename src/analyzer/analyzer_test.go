package analyzer

import (
	"testing"

	"backup-operator/dumper"
)

func TestCompare_FirstRun(t *testing.T) {
	a := NewAnalyzer()
	r := a.Compare(nil, &dumper.Stats{}, 0, 100)

	if len(r.Anomalies) != 0 {
		t.Errorf("first run should have no anomalies, got %d", len(r.Anomalies))
	}
	if r.SchemaChanged {
		t.Error("first run should not flag schema change")
	}
}

func TestCompare_SizeCollapse(t *testing.T) {
	a := NewAnalyzer()
	prev := &dumper.Stats{SchemaHash: "h"}
	curr := &dumper.Stats{SchemaHash: "h"}

	r := a.Compare(prev, curr, 1000, 100) // 10x smaller

	if !hasAnomaly(r.Anomalies, "size-collapse") {
		t.Errorf("expected size-collapse anomaly, got %+v", r.Anomalies)
	}
}

func TestCompare_NoSizeCollapseWhenStable(t *testing.T) {
	a := NewAnalyzer()
	prev := &dumper.Stats{SchemaHash: "h"}
	curr := &dumper.Stats{SchemaHash: "h"}

	r := a.Compare(prev, curr, 1000, 950) // 5% shrink

	if hasAnomaly(r.Anomalies, "size-collapse") {
		t.Errorf("did not expect size-collapse, got %+v", r.Anomalies)
	}
}

func TestCompare_TableDisappeared(t *testing.T) {
	a := NewAnalyzer()
	prev := &dumper.Stats{
		SchemaHash: "h",
		Tables: []dumper.TableStats{
			{Name: "users", RowCount: 100},
			{Name: "orders", RowCount: 50},
		},
	}
	curr := &dumper.Stats{
		SchemaHash: "h",
		Tables: []dumper.TableStats{
			{Name: "users", RowCount: 100},
		},
	}

	r := a.Compare(prev, curr, 1000, 1000)

	if !hasAnomalyForSubject(r.Anomalies, "table-disappeared", "orders") {
		t.Errorf("expected table-disappeared for 'orders', got %+v", r.Anomalies)
	}
}

func TestCompare_RowCountDrop(t *testing.T) {
	a := NewAnalyzer()
	prev := &dumper.Stats{
		SchemaHash: "h",
		Tables:     []dumper.TableStats{{Name: "users", RowCount: 1000}},
	}
	curr := &dumper.Stats{
		SchemaHash: "h",
		Tables:     []dumper.TableStats{{Name: "users", RowCount: 100}}, // 90% gone
	}

	r := a.Compare(prev, curr, 1000, 1000)

	if !hasAnomalyForSubject(r.Anomalies, "row-count-drop", "users") {
		t.Errorf("expected row-count-drop for 'users', got %+v", r.Anomalies)
	}
}

func TestCompare_SchemaChange(t *testing.T) {
	a := NewAnalyzer()
	prev := &dumper.Stats{SchemaHash: "old-hash"}
	curr := &dumper.Stats{SchemaHash: "new-hash"}

	r := a.Compare(prev, curr, 1000, 1000)

	if !r.SchemaChanged {
		t.Error("expected SchemaChanged=true when hashes differ")
	}
}

func TestCompare_NoSchemaChangeOnEmptyHash(t *testing.T) {
	a := NewAnalyzer()
	// CollectStats may not be implemented for some DB types — empty hash
	// must NOT be misread as a schema change.
	prev := &dumper.Stats{SchemaHash: ""}
	curr := &dumper.Stats{SchemaHash: ""}

	r := a.Compare(prev, curr, 1000, 1000)

	if r.SchemaChanged {
		t.Error("empty hashes should not flag schema change")
	}
}

func TestNewAnalyzerWithThresholds_Custom(t *testing.T) {
	// With a 0.2 threshold, a 40% shrink (ratio=0.4) should NOT trigger.
	a := NewAnalyzerWithThresholds(0.2, 0.2)
	prev := &dumper.Stats{
		SchemaHash: "h",
		Tables:     []dumper.TableStats{{Name: "users", RowCount: 100}},
	}
	curr := &dumper.Stats{
		SchemaHash: "h",
		Tables:     []dumper.TableStats{{Name: "users", RowCount: 40}},
	}
	r := a.Compare(prev, curr, 1000, 400)

	if hasAnomaly(r.Anomalies, "size-collapse") {
		t.Error("0.2 threshold should not flag a 0.4 ratio as size-collapse")
	}
	if hasAnomalyForSubject(r.Anomalies, "row-count-drop", "users") {
		t.Error("0.2 threshold should not flag a 0.4 ratio as row-count-drop")
	}
}

func TestNewAnalyzerWithThresholds_NegativeUsesDefault(t *testing.T) {
	a := NewAnalyzerWithThresholds(-1, -1)
	prev := &dumper.Stats{SchemaHash: "h"}
	curr := &dumper.Stats{SchemaHash: "h"}

	// With default 0.5 threshold, a 0.1 ratio should trigger.
	r := a.Compare(prev, curr, 1000, 100)
	if !hasAnomaly(r.Anomalies, "size-collapse") {
		t.Error("negative threshold should use default 0.5; 0.1 ratio should trigger")
	}
}

func hasAnomaly(as []Anomaly, kind string) bool {
	for _, a := range as {
		if a.Kind == kind {
			return true
		}
	}
	return false
}

func hasAnomalyForSubject(as []Anomaly, kind, subject string) bool {
	for _, a := range as {
		if a.Kind == kind && a.Subject == subject {
			return true
		}
	}
	return false
}
