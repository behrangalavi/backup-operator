package dumper

import (
	"bufio"
	"io"
	"strings"
	"sync"
)

// RowCounter wraps an io.Writer and counts rows per table by parsing
// the dump stream. It supports PostgreSQL (COPY ... FROM stdin),
// MySQL (INSERT INTO), and passes through all bytes unmodified.
//
// For MongoDB (mongodump --archive), row counting from the binary
// stream is not feasible — use pre/post stats comparison instead.
type RowCounter struct {
	w       io.Writer
	dbType  string
	mu      sync.Mutex
	counts  map[string]int64
	pr      *io.PipeReader
	pw      *io.PipeWriter
	done    chan struct{}
	scanErr error
}

// NewRowCounter creates a RowCounter that writes all bytes to w while
// counting rows per table based on the dump format for the given dbType.
// Pass nil for w and call SetWriter later if the writer is not yet available.
func NewRowCounter(w io.Writer, dbType string) *RowCounter {
	rc := &RowCounter{
		w:      w,
		dbType: dbType,
		counts: make(map[string]int64),
		done:   make(chan struct{}),
	}
	if dbType == "postgres" || dbType == "mysql" {
		rc.pr, rc.pw = io.Pipe()
		go rc.scan()
	}
	return rc
}

// SetWriter sets the underlying writer. Must be called before Write if
// the RowCounter was created with a nil writer.
func (rc *RowCounter) SetWriter(w io.Writer) {
	rc.w = w
}

func (rc *RowCounter) Write(p []byte) (int, error) {
	n, err := rc.w.Write(p)
	if rc.pw != nil && n > 0 {
		_, _ = rc.pw.Write(p[:n])
	}
	return n, err
}

// Close signals end of input to the scanner goroutine. Must be called
// after the dump completes.
func (rc *RowCounter) Close() error {
	if rc.pw != nil {
		_ = rc.pw.Close()
		<-rc.done
	}
	return nil
}

// Counts returns the per-table row counts observed in the dump stream.
func (rc *RowCounter) Counts() map[string]int64 {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	out := make(map[string]int64, len(rc.counts))
	for k, v := range rc.counts {
		out[k] = v
	}
	return out
}

// TotalRows returns the total number of rows counted across all tables.
func (rc *RowCounter) TotalRows() int64 {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var total int64
	for _, v := range rc.counts {
		total += v
	}
	return total
}

func (rc *RowCounter) scan() {
	defer close(rc.done)
	defer rc.pr.Close() // unblock pending pw.Write if scanner stops early (e.g. token too long)
	scanner := bufio.NewScanner(rc.pr)
	scanner.Buffer(make([]byte, 256*1024), 10*1024*1024) // 10 MB max line for extended-insert

	switch rc.dbType {
	case "postgres":
		rc.scanPostgres(scanner)
	case "mysql":
		rc.scanMySQL(scanner)
	}
	rc.scanErr = scanner.Err()
}

// scanPostgres parses pg_dump output looking for COPY blocks:
//
//	COPY schema.table (columns) FROM stdin;
//	data\tdata\tdata
//	\.
func (rc *RowCounter) scanPostgres(scanner *bufio.Scanner) {
	var currentTable string
	inCopy := false

	for scanner.Scan() {
		line := scanner.Text()
		if !inCopy {
			if strings.HasPrefix(line, "COPY ") && strings.Contains(line, " FROM stdin;") {
				// Extract table name: COPY schema.table (cols) FROM stdin;
				rest := line[5:]
				idx := strings.IndexByte(rest, ' ')
				if idx > 0 {
					currentTable = rest[:idx]
				}
				inCopy = true
			}
			continue
		}
		// Inside COPY block
		if line == "\\." {
			inCopy = false
			currentTable = ""
			continue
		}
		if currentTable != "" {
			rc.mu.Lock()
			rc.counts[currentTable]++
			rc.mu.Unlock()
		}
	}
}

// scanMySQL parses mysqldump output looking for INSERT statements:
//
//	INSERT INTO `table` VALUES (row1),(row2),(row3);
//
// Each VALUES tuple is one row.
func (rc *RowCounter) scanMySQL(scanner *bufio.Scanner) {
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "INSERT INTO ") {
			continue
		}
		// Extract table name: INSERT INTO `table` VALUES ...
		rest := line[12:]
		tableName := extractMySQLTable(rest)
		if tableName == "" {
			continue
		}
		// Count rows by counting top-level "(" in VALUES section
		valIdx := strings.Index(line, " VALUES ")
		if valIdx < 0 {
			continue
		}
		valPart := line[valIdx+8:]
		rows := countMySQLRows(valPart)
		rc.mu.Lock()
		rc.counts[tableName] += rows
		rc.mu.Unlock()
	}
}

// extractMySQLTable extracts table name from after "INSERT INTO ".
// Handles both backtick-quoted (`table`) and unquoted names.
func extractMySQLTable(s string) string {
	if len(s) == 0 {
		return ""
	}
	if s[0] == '`' {
		end := strings.IndexByte(s[1:], '`')
		if end < 0 {
			return ""
		}
		return s[1 : end+1]
	}
	end := strings.IndexByte(s, ' ')
	if end < 0 {
		return ""
	}
	return s[:end]
}

// countMySQLRows counts top-level parenthesized groups in a VALUES clause.
// E.g. "(1,'a'),(2,'b')" → 2
func countMySQLRows(s string) int64 {
	var count int64
	depth := 0
	inStr := false
	escape := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escape {
			escape = false
			continue
		}
		if c == '\\' && inStr {
			escape = true
			continue
		}
		if c == '\'' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		if c == '(' {
			if depth == 0 {
				count++
			}
			depth++
		} else if c == ')' {
			depth--
		}
	}
	return count
}
