// Package gguf is a pure-Go reader for GGUF headers over an io.ReaderAt, so the
// same parser serves a local file and an HTTP Range reader against a remote
// repository. It imports nothing outside the standard library (DESIGN section 1,
// invariant 5; DESIGN section 8.5).
//
// # What it reads, and what it deliberately does not
//
// A GGUF file is a header — magic, version, two counts, a metadata key/value
// table, a tensor index — followed by the tensor data itself. Everything this
// package answers is in the header: the model geometry section 8.1 lists, the
// per-tensor names, shapes, types and byte sizes section 8.2 buckets by layer,
// and the totals the models service records. The tensor data is never read, and
// on the remote path never fetched, which is the entire point: a 20 GB quant can
// be measured from its first megabyte.
//
// That also means a file whose data is absent parses fine. The section 15
// fixtures are truncated headers, and the section 8.5 remote peek reads only a
// prefix, so Parse validates the header against itself — the tensor index must
// be internally consistent — and never against the file's length. File.Complete
// reports whether the data a valid header describes is actually present.
//
// # Layering
//
//	Parse(io.ReaderAt, size)        the parser; everything else is a way to get a ReaderAt
//	ParseFile(path)                 the local implementation: *os.File
//	RangeReader + NewRemoteReaderAt the remote one: an interface internal/hf implements with an
//	                                HTTP Range request, wrapped in a read-ahead ReaderAt so the
//	                                parser's many small sequential reads become a few requests
//
// The remote half is an interface here and nothing more. No HTTP client, no URL
// and no token appears in this package; that wiring lands with the downloader.
//
// # Endianness
//
// GGUF is little-endian by default and big-endian on big-endian hosts, in which
// case every field except the magic is byte-swapped. The magic is written as a
// byte sequence and reads "GGUF" either way; the version field is what discloses
// the order, because a small integer read in the wrong order has a zero low
// half. See Parse.
package gguf
