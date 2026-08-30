package gguf

import "errors"

// The sentinel errors every failure in this package wraps. They exist so a
// caller can tell "this is not a GGUF file" from "this is a GGUF file that has
// been cut short" from "this is a GGUF file whose header contradicts itself",
// which are three different things to say to a user: the first is a mislabeled
// download, the second is an interrupted one, the third is corruption.
//
// Every returned error wraps exactly one of these with %w and adds the byte
// offset and the field that failed, so errors.Is answers the category and the
// message answers where.
var (
	// ErrBadMagic is returned when the first four bytes are not "GGUF". The
	// magic is a byte sequence, not an integer, so it reads the same on a
	// big-endian file (see Parse) and this check is never an endianness
	// question.
	ErrBadMagic = errors.New("gguf: not a GGUF file")

	// ErrUnsupportedVersion is returned for any version other than 2 or 3
	// (DESIGN section 8.5). Version 1 sized its counts and tensor dimensions
	// differently and no longer exists in the wild; a future version 4 must be
	// read before it is trusted, so it is refused rather than guessed at.
	ErrUnsupportedVersion = errors.New("gguf: unsupported version")

	// ErrTruncated is returned when a field runs past the end of the file: the
	// header claims more bytes than exist. This is the interrupted-download
	// case, and it is distinct from a header that merely lacks its tensor DATA,
	// which is normal and parses (see File.Complete).
	ErrTruncated = errors.New("gguf: truncated")

	// ErrCorrupt is returned when the header is self-contradictory: a duplicate
	// key or tensor name, a tensor whose offset is not where the running layout
	// puts it, a dimension count ggml cannot represent, an alignment that is not
	// a power of two, an element count that overflows.
	ErrCorrupt = errors.New("gguf: corrupt header")

	// ErrHeaderTooLarge is returned when the header exceeds the configured
	// bound (WithMaxHeaderBytes). A header claiming a gigabyte-long string is
	// not truncated — the bytes may genuinely be there in a 20 GB file — so it
	// is refused on its size rather than read into memory.
	ErrHeaderTooLarge = errors.New("gguf: header exceeds limit")

	// ErrUnknownValueType is returned for a metadata value type outside the
	// thirteen the format defines.
	ErrUnknownValueType = errors.New("gguf: unknown metadata value type")

	// ErrUnknownTensorType is returned for a ggml tensor type this build has no
	// block size for. Sizing such a tensor is impossible, and a guess would
	// silently corrupt every number in DESIGN section 8.2, so the whole parse
	// fails instead.
	ErrUnknownTensorType = errors.New("gguf: unknown tensor type")
)
