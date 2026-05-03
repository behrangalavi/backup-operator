package dumper

import (
	"io"
	"testing"
)

func TestRowCounterPostgres(t *testing.T) {
	dump := `--
-- PostgreSQL database dump
--
SET statement_timeout = 0;
COPY public.users (id, name, email) FROM stdin;
1	Alice	alice@example.com
2	Bob	bob@example.com
3	Charlie	charlie@example.com
\.

COPY public.orders (id, user_id, total) FROM stdin;
1	1	99.99
2	2	149.50
\.

`
	rc := NewRowCounter(io.Discard, "postgres")
	_, err := rc.Write([]byte(dump))
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}

	counts := rc.Counts()
	if counts["public.users"] != 3 {
		t.Errorf("users: want 3, got %d", counts["public.users"])
	}
	if counts["public.orders"] != 2 {
		t.Errorf("orders: want 2, got %d", counts["public.orders"])
	}
	if rc.TotalRows() != 5 {
		t.Errorf("total: want 5, got %d", rc.TotalRows())
	}
}

func TestRowCounterMySQL(t *testing.T) {
	dump := `-- MySQL dump
/*!40101 SET @OLD_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT */;
INSERT INTO ` + "`users`" + ` VALUES (1,'Alice','alice@example.com'),(2,'Bob','bob@example.com');
INSERT INTO ` + "`orders`" + ` VALUES (1,1,99.99);
`
	rc := NewRowCounter(io.Discard, "mysql")
	_, err := rc.Write([]byte(dump))
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}

	counts := rc.Counts()
	if counts["users"] != 2 {
		t.Errorf("users: want 2, got %d", counts["users"])
	}
	if counts["orders"] != 1 {
		t.Errorf("orders: want 1, got %d", counts["orders"])
	}
	if rc.TotalRows() != 3 {
		t.Errorf("total: want 3, got %d", rc.TotalRows())
	}
}

func TestRowCounterMySQLEscapedStrings(t *testing.T) {
	dump := `INSERT INTO ` + "`logs`" + ` VALUES (1,'it''s a test','ok'),(2,'hello\\nworld','ok');
`
	rc := NewRowCounter(io.Discard, "mysql")
	_, _ = rc.Write([]byte(dump))
	_ = rc.Close()

	if rc.Counts()["logs"] != 2 {
		t.Errorf("logs: want 2, got %d", rc.Counts()["logs"])
	}
}

func TestRowCounterMongo(t *testing.T) {
	// Mongo uses binary archive format — no row counting possible from stream
	rc := NewRowCounter(io.Discard, "mongo")
	_, _ = rc.Write([]byte("binary data"))
	_ = rc.Close()
	if len(rc.Counts()) != 0 {
		t.Errorf("mongo should have no counts, got %v", rc.Counts())
	}
}

func TestRowCounterPassthrough(t *testing.T) {
	var buf []byte
	w := &byteSliceWriter{buf: &buf}
	rc := NewRowCounter(w, "postgres")
	input := []byte("COPY t (a) FROM stdin;\n1\n\\.\n")
	n, err := rc.Write(input)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(input) {
		t.Errorf("want %d bytes written, got %d", len(input), n)
	}
	_ = rc.Close()
	if string(*w.buf) != string(input) {
		t.Errorf("passthrough mismatch")
	}
}

func TestCountMySQLRows(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"(1,'a'),(2,'b'),(3,'c');", 3},
		{"(1,'it''s');", 1},
		{"(1,'(nested)'),(2,'ok');", 2},
		{"", 0},
	}
	for _, tt := range tests {
		got := countMySQLRows(tt.input)
		if got != tt.want {
			t.Errorf("countMySQLRows(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestExtractMySQLTable(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"`users` VALUES", "users"},
		{"orders VALUES", "orders"},
		{"`weird``name` VALUES", "weird"},
		{"", ""},
	}
	for _, tt := range tests {
		got := extractMySQLTable(tt.input)
		if got != tt.want {
			t.Errorf("extractMySQLTable(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

type byteSliceWriter struct {
	buf *[]byte
}

func (w *byteSliceWriter) Write(p []byte) (int, error) {
	*w.buf = append(*w.buf, p...)
	return len(p), nil
}
