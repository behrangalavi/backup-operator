package ftps

import (
	"errors"
	"net/textproto"
	"testing"

	"backup-operator/storage"
)

// A 550 "no such file" must surface as storage.ErrNotFound; a 550
// permission error must not — FTP overloads the code and only clear
// non-existence phrasing may be reinterpreted.
func TestWrapNotFound(t *testing.T) {
	notFound := &textproto.Error{Code: 550, Msg: "No such file or directory"}
	if err := wrapNotFound("retr", "db/x", notFound); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("550 no-such-file must wrap ErrNotFound, got %v", err)
	}
	denied := &textproto.Error{Code: 550, Msg: "Permission denied"}
	if err := wrapNotFound("retr", "db/x", denied); errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("550 permission denied must NOT be reported as not-found, got %v", err)
	}
	if err := wrapNotFound("retr", "db/x", errors.New("connection reset")); errors.Is(err, storage.ErrNotFound) {
		t.Fatal("transport errors must not be reported as not-found")
	}
}
