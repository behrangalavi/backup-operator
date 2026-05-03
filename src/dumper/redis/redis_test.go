package redis

import "testing"

func TestParseKeyspace_Multiple(t *testing.T) {
	in := `# Keyspace
db0:keys=42,expires=2,avg_ttl=0
db3:keys=7,expires=0,avg_ttl=0
`
	got := parseKeyspace(in)
	if len(got) != 2 {
		t.Fatalf("want 2 dbs, got %d", len(got))
	}
	if got[0].name != "db0" || got[0].keys != 42 {
		t.Errorf("db0 mismatch: %+v", got[0])
	}
	if got[1].name != "db3" || got[1].keys != 7 {
		t.Errorf("db3 mismatch: %+v", got[1])
	}
}

func TestParseKeyspace_Empty(t *testing.T) {
	if got := parseKeyspace("# Keyspace\n"); len(got) != 0 {
		t.Errorf("want empty, got %+v", got)
	}
}

func TestParseKeyspace_Sorted(t *testing.T) {
	in := "db5:keys=1,expires=0,avg_ttl=0\ndb0:keys=2,expires=0,avg_ttl=0\n"
	got := parseKeyspace(in)
	if len(got) != 2 || got[0].name != "db0" || got[1].name != "db5" {
		t.Errorf("expected sorted db0,db5: %+v", got)
	}
}

func TestHashSchema_StableOrder(t *testing.T) {
	a := hashSchema([]string{"db0", "db1"})
	b := hashSchema([]string{"db1", "db0"})
	if a != b {
		t.Errorf("hash should be order-independent: %s vs %s", a, b)
	}
	c := hashSchema([]string{"db0"})
	if a == c {
		t.Errorf("different sets must hash differently")
	}
}
