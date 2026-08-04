package dumper

import "testing"

func TestHashSchema_StableOrder(t *testing.T) {
	a := HashSchema([]string{"db0", "db1"})
	b := HashSchema([]string{"db1", "db0"})
	if a != b {
		t.Errorf("hash must be order-independent: %s vs %s", a, b)
	}
	if HashSchema([]string{"db0"}) == a {
		t.Errorf("different sets must hash differently")
	}
	// Newline termination prevents "ab"+"c" colliding with "a"+"bc".
	if HashSchema([]string{"ab", "c"}) == HashSchema([]string{"a", "bc"}) {
		t.Errorf("boundary collision: elements must be newline-delimited")
	}
}
