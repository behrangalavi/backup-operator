package ftps

import (
	"errors"
	"testing"
)

// TestIsNotExistFTPError regresses the root-550 swallow: only clear
// non-existence phrasing is treated as "empty target dir"; a permission-denied
// 550 (which used to be swallowed, letting retention silently no-op while
// storage filled) must surface.
func TestIsNotExistFTPError(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"550 /backups/t: No such file or directory", true},
		{"550 Directory not found", true},
		{"550 does not exist", true},
		{"550 Permission denied", false},
		{"550 Access is denied", false},
		{"550", false}, // bare/ambiguous -> surface
		{"connection reset by peer", false},
	}
	for _, c := range cases {
		if got := isNotExistFTPError(errors.New(c.msg)); got != c.want {
			t.Errorf("isNotExistFTPError(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
	if isNotExistFTPError(nil) {
		t.Error("nil error must not be treated as not-exist")
	}
}
