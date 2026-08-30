package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
)

// hasher is the two-line wrapper the copy paths share, so that "hash the bytes
// as they go past" never becomes "read the file a second time and hope it is the
// same file". Section 12.2 step 1 depends on exactly that distinction: the
// retained copy's digest is compared against the digest of THE BYTES JUST READ,
// not against a second read of a file another process could have replaced in
// between.
type hasher struct{ h hash.Hash }

func newHasher() *hasher { return &hasher{h: sha256.New()} }

func (w *hasher) Write(p []byte) (int, error) { return w.h.Write(p) }

func (w *hasher) hex() string { return hex.EncodeToString(w.h.Sum(nil)) }
