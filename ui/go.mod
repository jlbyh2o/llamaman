// This module boundary exists for one reason: it stops the root module's
// `./...` pattern from descending into ui/ at all, so `go build/vet/test ./...`
// run from the repo root never sees ui/node_modules — which, being an npm
// tree, can and does contain vendored Go packages (e.g. flatted's) that are no
// part of this project and must never be built, vetted or gofmt-checked by it.
// The ui/ directory has no Go code of its own; this file is deliberately inert.
module github.com/jlbyh2o/llamaman/ui

go 1.27.0
