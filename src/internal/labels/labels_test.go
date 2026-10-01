package labels

import "testing"

func TestDumpSuffixes_SingleSourceOfTruth(t *testing.T) {
	for _, sfx := range DumpSuffixes() {
		if !IsDumpSuffix("target/2026/01/01/dump-20260101T020000Z" + sfx) {
			t.Errorf("IsDumpSuffix must accept every entry of DumpSuffixes, failed for %q", sfx)
		}
	}
	if IsDumpSuffix("dump-20260101T020000Z.meta.json") {
		t.Error("meta sidecar must not count as a dump")
	}
}

func TestCompressionForPath(t *testing.T) {
	cases := map[string]struct {
		want string
		ok   bool
	}{
		"a/dump-x.sql.gz.age":  {CompressionGzip, true},
		"a/dump-x.sql.zst.age": {CompressionZstd, true},
		"a/dump-x.meta.json":   {"", false},
	}
	for p, c := range cases {
		got, ok := CompressionForPath(p)
		if got != c.want || ok != c.ok {
			t.Errorf("CompressionForPath(%q) = %q,%v want %q,%v", p, got, ok, c.want, c.ok)
		}
	}
	if DumpSuffix(CompressionZstd) != "sql.zst.age" || DumpSuffix("") != "sql.gz.age" {
		t.Error("DumpSuffix mapping changed")
	}
}
