package crypto

import (
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
)

// Encryptor wraps an age recipient — public-key only — so the running
// service has no way to decrypt past or future backups.
type Encryptor interface {
	// Wrap returns a WriteCloser that encrypts to the underlying writer.
	// The caller MUST Close the returned writer to flush age's trailer.
	Wrap(w io.Writer) (io.WriteCloser, error)
}

type ageEncryptor struct {
	recipients []age.Recipient
}

// NewFromPublicKeys parses one or more age recipient lines. The input may be
// newline-separated to support recipient rotation (multiple keys can decrypt).
func NewFromPublicKeys(keys string) (Encryptor, error) {
	keys = strings.TrimSpace(keys)
	if keys == "" {
		return nil, fmt.Errorf("no age recipients configured")
	}
	var rs []age.Recipient
	for _, line := range strings.Split(keys, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r, err := age.ParseX25519Recipient(line)
		if err != nil {
			return nil, fmt.Errorf("parse recipient %q: %w", line, err)
		}
		rs = append(rs, r)
	}
	if len(rs) == 0 {
		return nil, fmt.Errorf("no valid age recipients parsed")
	}
	return &ageEncryptor{recipients: rs}, nil
}

func (e *ageEncryptor) Wrap(w io.Writer) (io.WriteCloser, error) {
	return age.Encrypt(w, e.recipients...)
}

// Decryptor wraps one or more age identities — used by the restore CLI.
// Identities are loaded only from local files at restore time; the running
// service in the cluster never sees them.
type Decryptor interface {
	// Wrap reads the encrypted bytes from r and returns a Reader that yields
	// the plaintext. The caller does not need to close the returned Reader.
	Wrap(r io.Reader) (io.Reader, error)
}

type ageDecryptor struct {
	identities []age.Identity
}

// NewDecryptorFromKeys parses one or more age identity lines (private keys).
// Lines starting with `#` and blank lines are ignored so the file format
// matches what `age-keygen -o` writes (header + key).
func NewDecryptorFromKeys(keys string) (Decryptor, error) {
	keys = strings.TrimSpace(keys)
	if keys == "" {
		return nil, fmt.Errorf("no age identities provided")
	}
	var ids []age.Identity
	for _, line := range strings.Split(keys, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		id, err := age.ParseX25519Identity(line)
		if err != nil {
			return nil, fmt.Errorf("parse identity: %w", err)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no valid age identities parsed")
	}
	return &ageDecryptor{identities: ids}, nil
}

func (d *ageDecryptor) Wrap(r io.Reader) (io.Reader, error) {
	return age.Decrypt(r, d.identities...)
}
