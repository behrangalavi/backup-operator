package analyzer

import (
	"strconv"

	"backup-operator/dumper"
)

// Report is the result of comparing the current run's stats with the previous
// run's stats. It is consumed by metricStore to expose semantic Prometheus
// signals — Alertmanager (or anything scraping us) decides what to do.
type Report struct {
	Current  *dumper.Stats `json:"current"`
	Previous *dumper.Stats `json:"previous"` // nil on first run

	SizeChangeRatio float64       `json:"sizeChangeRatio"` // current/previous total dump bytes; 1.0 = unchanged
	SchemaChanged   bool          `json:"schemaChanged"`
	Anomalies       []Anomaly     `json:"anomalies"`
	TableDiffs      []TableDiff   `json:"tableDiffs"`
}

type Anomaly struct {
	Kind    string `json:"kind"`    // e.g. "table-disappeared", "row-count-drop", "size-collapse"
	Subject string `json:"subject"` // table name or "<dump>"
	Detail  string `json:"detail"`
}

type TableDiff struct {
	Name           string  `json:"name"`
	PrevRows       int64   `json:"prevRows"`
	CurrRows       int64   `json:"currRows"`
	RowChangeRatio float64 `json:"rowChangeRatio"`
}

// Default thresholds — used when the source annotation is absent (-1).
const (
	DefaultRowDropThreshold  = 0.5 // table shrunk to less than half its rows
	DefaultSizeDropThreshold = 0.5 // dump shrunk to less than half its bytes
)

// Analyzer compares two consecutive runs.
type Analyzer interface {
	Compare(prev, curr *dumper.Stats, prevDumpSize, currDumpSize int64) *Report
}

type analyzer struct {
	rowDropThreshold  float64 // anomaly if curr/prev < this
	sizeDropThreshold float64
}

// NewAnalyzer returns an Analyzer with sensible defaults.
func NewAnalyzer() Analyzer {
	return &analyzer{
		rowDropThreshold:  DefaultRowDropThreshold,
		sizeDropThreshold: DefaultSizeDropThreshold,
	}
}

// NewAnalyzerWithThresholds returns an Analyzer whose row-drop and size-drop
// thresholds can be tuned per source. Pass -1 for either value to use the
// default.
func NewAnalyzerWithThresholds(rowDrop, sizeDrop float64) Analyzer {
	rd := DefaultRowDropThreshold
	sd := DefaultSizeDropThreshold
	if rowDrop >= 0 {
		rd = rowDrop
	}
	if sizeDrop >= 0 {
		sd = sizeDrop
	}
	return &analyzer{
		rowDropThreshold:  rd,
		sizeDropThreshold: sd,
	}
}

func (a *analyzer) Compare(prev, curr *dumper.Stats, prevSize, currSize int64) *Report {
	r := &Report{Current: curr, Previous: prev}

	if prev == nil || curr == nil {
		return r
	}

	if prevSize > 0 {
		r.SizeChangeRatio = float64(currSize) / float64(prevSize)
		if r.SizeChangeRatio < a.sizeDropThreshold {
			r.Anomalies = append(r.Anomalies, Anomaly{
				Kind:    "size-collapse",
				Subject: "<dump>",
				Detail:  formatSizeAnomaly(prevSize, currSize, r.SizeChangeRatio),
			})
		}
	}

	if prev.SchemaHash != "" && curr.SchemaHash != "" && prev.SchemaHash != curr.SchemaHash {
		r.SchemaChanged = true
	}

	prevTables := indexTables(prev.Tables)
	currTables := indexTables(curr.Tables)

	for name, p := range prevTables {
		c, ok := currTables[name]
		if !ok {
			r.Anomalies = append(r.Anomalies, Anomaly{
				Kind:    "table-disappeared",
				Subject: name,
				Detail:  "table existed in previous run but is missing now",
			})
			continue
		}
		ratio := 1.0
		if p.RowCount > 0 {
			ratio = float64(c.RowCount) / float64(p.RowCount)
		}
		r.TableDiffs = append(r.TableDiffs, TableDiff{
			Name:           name,
			PrevRows:       p.RowCount,
			CurrRows:       c.RowCount,
			RowChangeRatio: ratio,
		})
		if p.RowCount > 0 && ratio < a.rowDropThreshold {
			r.Anomalies = append(r.Anomalies, Anomaly{
				Kind:    "row-count-drop",
				Subject: name,
				Detail:  formatRowAnomaly(p.RowCount, c.RowCount, ratio),
			})
		}
	}

	return r
}

func indexTables(ts []dumper.TableStats) map[string]dumper.TableStats {
	m := make(map[string]dumper.TableStats, len(ts))
	for _, t := range ts {
		m[t.Name] = t
	}
	return m
}

func formatSizeAnomaly(prev, curr int64, ratio float64) string {
	return fmtRatio("dump shrunk", prev, curr, ratio)
}

func formatRowAnomaly(prev, curr int64, ratio float64) string {
	return fmtRatio("rows dropped", prev, curr, ratio)
}

func fmtRatio(label string, prev, curr int64, ratio float64) string {
	return label + ": " + i64(prev) + " -> " + i64(curr) + " (ratio " + f2(ratio) + ")"
}

func i64(v int64) string { return strconv.FormatInt(v, 10) }
func f2(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }
