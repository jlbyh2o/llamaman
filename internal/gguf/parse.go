package gguf

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/bits"
	"os"
)

// Defaults for the parser's two bounds and the remote reader's window.
const (
	// DefaultArrayLimit is how many elements of a metadata array are retained.
	// Everything DESIGN section 8.1 reads as an array — head_count_kv,
	// feed_forward_length — is one entry per layer, so a few hundred at the
	// outside; the arrays that are orders of magnitude larger are the tokenizer's
	// vocabulary, merges and scores, which section 8.5 says to parse but not
	// keep. Their LENGTH survives elision, and that is the only thing read off
	// them (n_vocab).
	DefaultArrayLimit = 1024

	// DefaultMaxHeaderBytes bounds the whole header. Section 8.5 puts a typical
	// header well under 2 MiB and a large token array at ~8 MiB, so 64 MiB is
	// far past anything legitimate while still refusing to allocate a gigabyte
	// because a corrupt length prefix said to. Without it the file's own size is
	// the only bound, which in a 20 GB quant is no bound at all.
	DefaultMaxHeaderBytes = 64 << 20

	// DefaultAlignment is the tensor-data alignment assumed when
	// `general.alignment` is absent, per the specification.
	DefaultAlignment = 32

	// maxArrayDepth bounds nested arrays. The format allows an array of arrays;
	// nothing llama.cpp reads nests more than two deep, and the bound is what
	// keeps a crafted file from recursing the decoder off its stack.
	maxArrayDepth = 16

	// maxTensorDims is ggml's GGML_MAX_DIMS. A tensor with more dimensions
	// cannot be represented by the runtime that will load this file, so the
	// header is rejected rather than described.
	maxTensorDims = 4

	// minKVBytes and minTensorInfoBytes are the smallest encodings of one
	// metadata pair (8-byte key length + 4-byte type + a 1-byte value) and one
	// tensor descriptor (8-byte name length + 4-byte dimension count + 4-byte
	// type + 8-byte offset). They turn the two counts in the header into a bound
	// that is checked before anything is allocated.
	minKVBytes         = 13
	minTensorInfoBytes = 24
)

type options struct {
	arrayLimit     int
	maxHeaderBytes int64
}

// An Option adjusts the parser's bounds.
type Option func(*options)

// WithArrayLimit sets how many elements of a metadata array are retained. A
// negative limit retains every element; zero retains none. Arrays past the limit
// are still fully decoded — their bytes are walked, so a truncation inside one
// is still caught — but their elements are dropped and Array.Elided is set.
func WithArrayLimit(n int) Option { return func(o *options) { o.arrayLimit = n } }

// WithMaxHeaderBytes bounds the header. A header that runs past the bound fails
// with ErrHeaderTooLarge rather than being read into memory. Zero or negative
// means the file's own length is the only bound, which is safe only for input
// whose size is already trusted.
func WithMaxHeaderBytes(n int64) Option { return func(o *options) { o.maxHeaderBytes = n } }

func defaults() options {
	return options{arrayLimit: DefaultArrayLimit, maxHeaderBytes: DefaultMaxHeaderBytes}
}

// File is a parsed GGUF header: the metadata table, the tensor index, and the
// geometry of the tensor-data region those two imply.
//
// Nothing here is read from the tensor data itself, and DataOffset/DataSize
// describe where that data WOULD be. See File.Complete.
type File struct {
	// Version is 2 or 3.
	Version uint32
	// BigEndian records the byte order the file was written in. It is reporting
	// only — every number in this struct is already in host terms.
	BigEndian bool
	// TensorCount is the count the header declares, which equals len(Tensors)
	// for a header that parsed.
	TensorCount uint64
	// KV is the metadata table, in file order.
	KV KV
	// Tensors is the tensor index, in file order.
	Tensors []TensorInfo
	// Alignment is `general.alignment`, or DefaultAlignment when absent.
	Alignment uint64
	// HeaderBytes is how many bytes the magic, the metadata table and the tensor
	// index occupied — the number a remote peek needed to fetch.
	HeaderBytes int64
	// DataOffset is HeaderBytes rounded up to Alignment: the absolute offset of
	// the tensor data.
	DataOffset int64
	// DataSize is the length of the tensor-data region, as the tensor index
	// describes it: each tensor's byte size padded up to Alignment, summed.
	DataSize int64
	// FileSize is the size handed to Parse — for a remote peek, the object's
	// full Content-Length, not the number of bytes fetched.
	FileSize int64
}

// Complete reports whether the file is long enough to hold the tensor data its
// own index describes. It is false for the truncated headers this package is
// tested against and for a download that was interrupted, and true for a whole
// model — so a caller that needs the weights, rather than just their sizes,
// has one thing to check.
func (f *File) Complete() bool { return f.FileSize >= f.DataOffset+f.DataSize }

// ParseFile reads the header of a local GGUF file. This is section 8.5's local
// implementation: an *os.File is an io.ReaderAt, so the parser is the same one
// the remote path uses.
func ParseFile(path string, opts ...Option) (*File, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return nil, err
	}
	f, err := Parse(fh, st.Size(), opts...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// Parse reads a GGUF header from r, whose object is size bytes long.
//
// size is the length of the whole object, not of the bytes available: a remote
// peek passes the Content-Length and the reader fetches what the parse asks
// for. It is used as a bound — no field may claim to run past the end of the
// file — and never as a requirement, because a header is complete long before
// the tensor data it points at.
//
// # Endianness
//
// The four magic bytes are written as a byte sequence and read "GGUF" on a
// big-endian file too, so they cannot disclose the order. The version does: it
// is a small number, so a big-endian 3 read as little-endian is 0x03000000,
// whose low 16 bits are zero. That is the reference implementation's own test
// and it is what this uses. Every subsequent field is then read in the
// disclosed order.
func Parse(r io.ReaderAt, size int64, opts ...Option) (*File, error) {
	o := defaults()
	for _, opt := range opts {
		opt(&o)
	}
	if r == nil {
		return nil, fmt.Errorf("gguf: nil reader: %w", ErrTruncated)
	}
	if size < 0 {
		return nil, fmt.Errorf("gguf: negative size %d: %w", size, ErrTruncated)
	}

	limit := size
	if o.maxHeaderBytes > 0 && o.maxHeaderBytes < limit {
		limit = o.maxHeaderBytes
	}
	c := &cursor{
		br:         bufio.NewReaderSize(io.NewSectionReader(r, 0, limit), 64<<10),
		order:      binary.LittleEndian,
		size:       size,
		limit:      limit,
		arrayLimit: o.arrayLimit,
	}

	f := &File{FileSize: size}

	var magic [4]byte
	if err := c.read(magic[:]); err != nil {
		return nil, err
	}
	if string(magic[:]) != "GGUF" {
		return nil, fmt.Errorf("gguf: magic %q, want \"GGUF\": %w", magic[:], ErrBadMagic)
	}

	version, err := c.u32()
	if err != nil {
		return nil, err
	}
	if version&0xFFFF == 0 {
		// The low half is zero, so this number was written in the other order.
		c.order = binary.BigEndian
		version = bits.ReverseBytes32(version)
		f.BigEndian = true
	}
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("gguf: version %d: %w", version, ErrUnsupportedVersion)
	}
	f.Version = version

	tensorCount, err := c.u64()
	if err != nil {
		return nil, err
	}
	kvCount, err := c.u64()
	if err != nil {
		return nil, err
	}
	// Both counts are bounded by what could physically follow them, before
	// either is used to size anything. A count the file cannot hold is reported
	// as truncation and not as corruption, because that is what it says: the
	// header describes more than the file contains.
	if room := uint64(c.remaining()); tensorCount > room/minTensorInfoBytes {
		return nil, fmt.Errorf("gguf: tensor count %d exceeds the %d bytes that follow: %w", tensorCount, room, ErrTruncated)
	} else if kvCount > room/minKVBytes {
		return nil, fmt.Errorf("gguf: metadata count %d exceeds the %d bytes that follow: %w", kvCount, room, ErrTruncated)
	}
	f.TensorCount = tensorCount

	for i := uint64(0); i < kvCount; i++ {
		key, err := c.str()
		if err != nil {
			return nil, fmt.Errorf("gguf: metadata pair %d: %w", i, err)
		}
		vt, err := c.valueType()
		if err != nil {
			return nil, fmt.Errorf("gguf: metadata key %q: %w", key, err)
		}
		v, err := c.value(vt, 0)
		if err != nil {
			return nil, fmt.Errorf("gguf: metadata key %q: %w", key, err)
		}
		if !f.KV.add(key, v) {
			return nil, fmt.Errorf("gguf: duplicate metadata key %q: %w", key, ErrCorrupt)
		}
	}

	f.Tensors = make([]TensorInfo, 0, min(tensorCount, 4096))
	seen := make(map[string]struct{}, min(tensorCount, 4096))
	for i := uint64(0); i < tensorCount; i++ {
		ti, err := c.tensorInfo()
		if err != nil {
			return nil, fmt.Errorf("gguf: tensor %d: %w", i, err)
		}
		if _, dup := seen[ti.Name]; dup {
			return nil, fmt.Errorf("gguf: duplicate tensor name %q: %w", ti.Name, ErrCorrupt)
		}
		seen[ti.Name] = struct{}{}
		f.Tensors = append(f.Tensors, ti)
	}

	f.HeaderBytes = c.off

	f.Alignment = DefaultAlignment
	if raw, ok := f.KV.Get(KeyAlignment); ok {
		a, ok := raw.AsUint()
		if !ok || a == 0 || a&(a-1) != 0 {
			return nil, fmt.Errorf("gguf: %s is not a positive power of two: %w", KeyAlignment, ErrCorrupt)
		}
		f.Alignment = a
	}

	dataOffset, err := padUp(uint64(c.off), f.Alignment)
	if err != nil || dataOffset > math.MaxInt64 {
		return nil, fmt.Errorf("gguf: tensor data offset overflows: %w", ErrCorrupt)
	}
	f.DataOffset = int64(dataOffset)

	// The tensor index must lay the data out consecutively, each tensor padded
	// up to the alignment, which is the rule llama.cpp enforces when it loads a
	// file. Checking it here is what turns a garbled index into an error instead
	// of into per-layer byte totals that look plausible and are wrong.
	var running uint64
	for i, ti := range f.Tensors {
		if ti.Offset != running {
			return nil, fmt.Errorf("gguf: tensor %d %q has offset %d, expected %d: %w", i, ti.Name, ti.Offset, running, ErrCorrupt)
		}
		padded, err := padUp(ti.Bytes(), f.Alignment)
		if err != nil {
			return nil, fmt.Errorf("gguf: tensor %d %q size overflows: %w", i, ti.Name, ErrCorrupt)
		}
		next, carry := bits.Add64(running, padded, 0)
		if carry != 0 || next > math.MaxInt64 {
			return nil, fmt.Errorf("gguf: tensor data size overflows at tensor %d %q: %w", i, ti.Name, ErrCorrupt)
		}
		running = next
	}
	f.DataSize = int64(running)

	return f, nil
}

// padUp rounds n up to a multiple of align, which the caller has already checked
// is a power of two.
func padUp(n, align uint64) (uint64, error) {
	sum, carry := bits.Add64(n, align-1, 0)
	if carry != 0 {
		return 0, ErrCorrupt
	}
	return sum &^ (align - 1), nil
}

// cursor is a sequential decoder over an io.ReaderAt. It buffers, because a
// header is thousands of small reads and the remote reader behind it charges an
// HTTP request per miss, and it tracks its own offset, because the tensor-data
// offset is "wherever the header ended, rounded up".
type cursor struct {
	br         *bufio.Reader
	order      binary.ByteOrder
	off        int64 // bytes consumed
	size       int64 // the whole object
	limit      int64 // min(size, maxHeaderBytes)
	arrayLimit int
	buf        [8]byte
}

// remaining is how many bytes of the FILE are still ahead. It bounds the
// declared counts and lengths — an array of n elements cannot be believed when
// n elements could not fit in the file — and is deliberately measured against
// the file rather than against the header limit, so that a header which merely
// exceeds the limit fails as ErrHeaderTooLarge on the read that crosses it
// instead of being mislabeled truncation.
func (c *cursor) remaining() int64 { return c.size - c.off }

// bound checks that n more bytes may be read, distinguishing the two ways they
// may not: past the end of the file, or past the header bound.
func (c *cursor) bound(n uint64) error {
	if n > math.MaxInt64 {
		return fmt.Errorf("gguf: length %d at offset %d: %w", n, c.off, ErrTruncated)
	}
	end, carry := bits.Add64(uint64(c.off), n, 0)
	if carry != 0 || end > uint64(c.size) {
		return fmt.Errorf("gguf: %d bytes at offset %d run past the end of the %d-byte file: %w", n, c.off, c.size, ErrTruncated)
	}
	if end > uint64(c.limit) {
		return fmt.Errorf("gguf: %d bytes at offset %d run past the %d-byte header limit: %w", n, c.off, c.limit, ErrHeaderTooLarge)
	}
	return nil
}

func (c *cursor) read(p []byte) error {
	if err := c.bound(uint64(len(p))); err != nil {
		return err
	}
	if _, err := io.ReadFull(c.br, p); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return fmt.Errorf("gguf: short read at offset %d: %w", c.off, ErrTruncated)
		}
		return err
	}
	c.off += int64(len(p))
	return nil
}

func (c *cursor) skip(n uint64) error {
	if err := c.bound(n); err != nil {
		return err
	}
	for n > 0 {
		chunk := n
		if chunk > math.MaxInt32 {
			chunk = math.MaxInt32
		}
		got, err := c.br.Discard(int(chunk))
		c.off += int64(got)
		if err != nil {
			if err == io.EOF {
				return fmt.Errorf("gguf: short read at offset %d: %w", c.off, ErrTruncated)
			}
			return err
		}
		n -= uint64(got)
	}
	return nil
}

func (c *cursor) u8() (uint8, error) {
	if err := c.read(c.buf[:1]); err != nil {
		return 0, err
	}
	return c.buf[0], nil
}

func (c *cursor) u16() (uint16, error) {
	if err := c.read(c.buf[:2]); err != nil {
		return 0, err
	}
	return c.order.Uint16(c.buf[:2]), nil
}

func (c *cursor) u32() (uint32, error) {
	if err := c.read(c.buf[:4]); err != nil {
		return 0, err
	}
	return c.order.Uint32(c.buf[:4]), nil
}

func (c *cursor) u64() (uint64, error) {
	if err := c.read(c.buf[:8]); err != nil {
		return 0, err
	}
	return c.order.Uint64(c.buf[:8]), nil
}

// str reads a GGUF string: a uint64 length followed by that many bytes, with no
// terminator. The length is bounded before the slice is allocated.
func (c *cursor) str() (string, error) {
	n, err := c.u64()
	if err != nil {
		return "", err
	}
	if err := c.bound(n); err != nil {
		return "", err
	}
	if n == 0 {
		return "", nil
	}
	b := make([]byte, n)
	if err := c.read(b); err != nil {
		return "", err
	}
	return string(b), nil
}

func (c *cursor) valueType() (ValueType, error) {
	raw, err := c.u32()
	if err != nil {
		return 0, err
	}
	vt := ValueType(raw)
	if !vt.Valid() {
		return 0, fmt.Errorf("gguf: value type %d at offset %d: %w", raw, c.off-4, ErrUnknownValueType)
	}
	return vt, nil
}

func (c *cursor) value(vt ValueType, depth int) (Value, error) {
	v := Value{Type: vt}
	switch vt {
	case ValueUint8:
		n, err := c.u8()
		v.Uint = uint64(n)
		return v, err
	case ValueInt8:
		n, err := c.u8()
		v.Int = int64(int8(n))
		return v, err
	case ValueUint16:
		n, err := c.u16()
		v.Uint = uint64(n)
		return v, err
	case ValueInt16:
		n, err := c.u16()
		v.Int = int64(int16(n))
		return v, err
	case ValueUint32:
		n, err := c.u32()
		v.Uint = uint64(n)
		return v, err
	case ValueInt32:
		n, err := c.u32()
		v.Int = int64(int32(n))
		return v, err
	case ValueFloat32:
		n, err := c.u32()
		v.Float = float64(math.Float32frombits(n))
		return v, err
	case ValueBool:
		n, err := c.u8()
		// The format writes a bool as one byte. Upstream writes 0 and 1; any
		// non-zero byte is read as true rather than refused, matching C.
		v.Bool = n != 0
		return v, err
	case ValueUint64:
		n, err := c.u64()
		v.Uint = n
		return v, err
	case ValueInt64:
		n, err := c.u64()
		v.Int = int64(n)
		return v, err
	case ValueFloat64:
		n, err := c.u64()
		v.Float = math.Float64frombits(n)
		return v, err
	case ValueString:
		s, err := c.str()
		v.String = s
		return v, err
	case ValueArray:
		a, err := c.array(depth)
		v.Array = a
		return v, err
	default:
		return v, fmt.Errorf("gguf: value type %d at offset %d: %w", vt, c.off, ErrUnknownValueType)
	}
}

func (c *cursor) arrayHeader(depth int) (ValueType, uint64, error) {
	if depth >= maxArrayDepth {
		return 0, 0, fmt.Errorf("gguf: arrays nested more than %d deep at offset %d: %w", maxArrayDepth, c.off, ErrCorrupt)
	}
	et, err := c.valueType()
	if err != nil {
		return 0, 0, err
	}
	n, err := c.u64()
	if err != nil {
		return 0, 0, err
	}
	// An array of n elements occupies at least n×minWireSize bytes, so an n
	// bigger than the file cannot be believed — and is refused here, before it
	// is used as a loop bound or a capacity.
	if room := uint64(c.remaining()); n > room/et.minWireSize() {
		return 0, 0, fmt.Errorf("gguf: array of %d %s exceeds the %d bytes that follow: %w", n, et, room, ErrTruncated)
	}
	return et, n, nil
}

func (c *cursor) array(depth int) (*Array, error) {
	et, n, err := c.arrayHeader(depth)
	if err != nil {
		return nil, err
	}
	a := &Array{Type: et, Len: n}

	retain := c.arrayLimit < 0 || n <= uint64(c.arrayLimit)
	if !retain {
		// Parsed but not retained (DESIGN section 8.5). The bytes are still
		// walked — a truncation inside a token array is still an error — but
		// nothing is kept except the length.
		a.Elided = true
		return a, c.skipElements(et, n, depth+1)
	}

	a.Values = make([]Value, 0, n)
	for i := uint64(0); i < n; i++ {
		v, err := c.value(et, depth+1)
		if err != nil {
			return nil, fmt.Errorf("gguf: array element %d: %w", i, err)
		}
		a.Values = append(a.Values, v)
	}
	return a, nil
}

// skipElements advances over n consecutive values of one type without
// allocating them. A fixed-width type is one arithmetic jump; the two
// variable-length types have to be walked, which is what makes a truncation
// inside an elided token array still an error rather than a silent short read.
func (c *cursor) skipElements(et ValueType, n uint64, depth int) error {
	if w := et.wireSize(); w > 0 {
		return c.skip(n * w)
	}
	for i := uint64(0); i < n; i++ {
		if err := c.skipValue(et, depth); err != nil {
			return fmt.Errorf("gguf: array element %d: %w", i, err)
		}
	}
	return nil
}

// skipValue advances over one value without allocating it. It is only ever
// called for the two variable-length types, because skipElements jumps over
// every fixed-width one arithmetically.
func (c *cursor) skipValue(vt ValueType, depth int) error {
	switch vt {
	case ValueString:
		n, err := c.u64()
		if err != nil {
			return err
		}
		if err := c.bound(n); err != nil {
			return err
		}
		return c.skip(n)
	case ValueArray:
		et, n, err := c.arrayHeader(depth)
		if err != nil {
			return err
		}
		return c.skipElements(et, n, depth+1)
	}
	return fmt.Errorf("gguf: value type %s at offset %d: %w", vt, c.off, ErrUnknownValueType)
}

func (c *cursor) tensorInfo() (TensorInfo, error) {
	var ti TensorInfo
	name, err := c.str()
	if err != nil {
		return ti, err
	}
	ti.Name = name

	nd, err := c.u32()
	if err != nil {
		return ti, err
	}
	if nd > maxTensorDims {
		return ti, fmt.Errorf("gguf: tensor %q has %d dimensions, ggml allows %d: %w", name, nd, maxTensorDims, ErrCorrupt)
	}
	ti.Dims = make([]uint64, 0, nd)
	elements := uint64(1)
	for i := uint32(0); i < nd; i++ {
		d, err := c.u64()
		if err != nil {
			return ti, err
		}
		// ggml carries dimensions as int64, so a value above MaxInt64 is one the
		// runtime could not represent even if the bytes were there.
		if d > math.MaxInt64 {
			return ti, fmt.Errorf("gguf: tensor %q dimension %d is %d: %w", name, i, d, ErrCorrupt)
		}
		hi, lo := bits.Mul64(elements, d)
		if hi != 0 || lo > math.MaxInt64 {
			return ti, fmt.Errorf("gguf: tensor %q element count overflows: %w", name, ErrCorrupt)
		}
		elements = lo
		ti.Dims = append(ti.Dims, d)
	}

	raw, err := c.u32()
	if err != nil {
		return ti, err
	}
	ti.Type = GGMLType(raw)
	if !ti.Type.Valid() {
		return ti, fmt.Errorf("gguf: tensor %q has type %d: %w", name, raw, ErrUnknownTensorType)
	}
	// The first dimension is the one the quantization blocks run along, so a
	// tensor whose row is not a whole number of blocks has no size at all —
	// which is why this is a parse error and not something Bytes rounds away.
	if len(ti.Dims) > 0 && ti.Dims[0]%ti.Type.BlockSize() != 0 {
		return ti, fmt.Errorf("gguf: tensor %q has first dimension %d, not a multiple of the %s block size %d: %w",
			name, ti.Dims[0], ti.Type, ti.Type.BlockSize(), ErrCorrupt)
	}
	// The element count fits an int64, but the BYTE count is elements scaled by
	// bytes-per-block, which for f64 is eight times larger. Checking it here is
	// what lets TensorInfo.Bytes be a plain multiplication everywhere else: a
	// header that survives this cannot make it wrap.
	if hi, lo := bits.Mul64(elements/ti.Type.BlockSize(), ti.Type.TypeSize()); hi != 0 || lo > math.MaxInt64 {
		return ti, fmt.Errorf("gguf: tensor %q byte size overflows: %w", name, ErrCorrupt)
	}

	off, err := c.u64()
	if err != nil {
		return ti, err
	}
	ti.Offset = off
	return ti, nil
}
