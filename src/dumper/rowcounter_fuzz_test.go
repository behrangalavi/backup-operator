package dumper

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// FuzzRowCounter feeds arbitrary bytes through the pass-through writer for
// both SQL dialects and checks the invariants the pipeline relies on: every
// byte reaches the underlying writer unmodified, Close never hangs or
// panics, counts are never negative, and an inactive counter (mongo) stays
// inactive.
func FuzzRowCounter(f *testing.F) {
	f.Add([]byte("COPY public.users (id) FROM stdin;\n1\n2\n\\.\n"), "postgres")
	f.Add([]byte("INSERT INTO `users` VALUES (1),(2),(3);\n"), "mysql")
	f.Add([]byte("INSERT INTO `t` VALUES ('a,(b)'),('c');\nINSERT INTO `t` VALUES (1);\n"), "mariadb")
	f.Add([]byte("COPY a FROM stdin;\nno terminator"), "postgres")
	f.Add([]byte{0x81, 0x99, 0xe2, 0x6d, 0, 1, 2}, "mongo")
	f.Fuzz(func(t *testing.T, data []byte, dbType string) {
		var sink bytes.Buffer
		rc := NewRowCounter(&sink, dbType)
		// Write in two chunks so a line boundary can land mid-chunk.
		mid := len(data) / 2
		for _, chunk := range [][]byte{data[:mid], data[mid:]} {
			n, err := rc.Write(chunk)
			if err != nil || n != len(chunk) {
				t.Fatalf("Write returned (%d, %v) for %d bytes", n, err, len(chunk))
			}
		}
		_ = rc.Close()
		if !bytes.Equal(sink.Bytes(), data) {
			t.Fatal("bytes were not passed through unmodified")
		}
		active := dbType == "postgres" || dbType == "mysql" || dbType == "mariadb"
		if rc.Active() != active {
			t.Fatalf("Active() = %v for dbType %q", rc.Active(), dbType)
		}
		for table, n := range rc.Counts() {
			if n < 0 {
				t.Fatalf("negative count %d for %q", n, table)
			}
		}
		if rc.TotalRows() < 0 {
			t.Fatal("negative total")
		}
	})
}

// BenchmarkRowCounter_MySQLExtendedInsert measures the per-byte cost of the
// SQL stream parser on the hot path: a large extended-insert dump streamed
// through the counter into io.Discard. The dump itself streams through this
// writer, so any regression here is a regression on every backup run.
func BenchmarkRowCounter_MySQLExtendedInsert(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < 2000; i++ {
		sb.WriteString("INSERT INTO `orders` VALUES ")
		for j := 0; j < 50; j++ {
			if j > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString("(1,'some text with, commas (and parens)',3.14,NULL)")
		}
		sb.WriteString(";\n")
	}
	dump := []byte(sb.String())
	b.SetBytes(int64(len(dump)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rc := NewRowCounter(io.Discard, "mysql")
		if _, err := rc.Write(dump); err != nil {
			b.Fatal(err)
		}
		_ = rc.Close()
	}
}

// BenchmarkRowCounter_PostgresCopy is the COPY-block counterpart.
func BenchmarkRowCounter_PostgresCopy(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		sb.WriteString("COPY public.events (id, payload) FROM stdin;\n")
		for j := 0; j < 500; j++ {
			sb.WriteString("1\t{\"k\":\"v\"}\n")
		}
		sb.WriteString("\\.\n")
	}
	dump := []byte(sb.String())
	b.SetBytes(int64(len(dump)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rc := NewRowCounter(io.Discard, "postgres")
		if _, err := rc.Write(dump); err != nil {
			b.Fatal(err)
		}
		_ = rc.Close()
	}
}
