// A separate module on purpose.
//
// `go build ./...` and `go test ./...` at the repository root must not see this
// binary: it is a TEST FIXTURE that pretends to be llama-server, and compiling
// it into every CI run would make a deliberately misbehaving HTTP server part
// of the product's own build graph. Its own go.mod makes the root module's
// package walk stop at this directory, and the supervision tests build it on
// demand with `go build` run inside it.
//
// It has no dependencies and never will: everything it needs is in the standard
// library, and a fixture that could pull a module would be a fixture that could
// break a build for a reason having nothing to do with the product.
module llamaman.test/stubllama

go 1.27.0
