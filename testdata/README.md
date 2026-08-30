# testdata/

Fixtures shared across packages: GGUF headers, Hugging Face API responses, and
`llama-bench` JSON output (DESIGN sections 1 and 15). Package-local fixtures
live in that package's own `testdata/` directory instead.

## `fit/`

DESIGN section 8.7's golden corpus, and section 15 names it again: the fit suite
runs against the recorded loads here and asserts predictions within ±10% and,
non-negotiably, that a verdict never says "fits" for a load that actually OOM'd.

- `corpus.json` — twenty loads. Each names one of the four synthetic GGUF
  fixtures, the flags it was launched with, the card it was launched on, the
  three buffer figures `llama.cpp` prints at startup, and whether it died
  allocating. `internal/fit/corpus_test.go` reads it.
- `gencorpus.py` — how the current rows were derived: a second implementation of
  sections 8.2–8.4 written from the design text, so the Go calculator is
  compared against the specification rather than against itself.

A row recorded from a real host has the same shape and drops straight in: a
`fit_observations` row carries exactly these figures
(`actual_weights_bytes`, `actual_kv_bytes`, `actual_compute_bytes`, `oom`).
Real rows are worth most for the compute buffer, which section 8.7 calls the
only genuinely empirical term in the model.

## `stubllama/`

A stand-in for `llama-server` and `llama-bench`, so the supervisor and the bench
runner can be exercised without a GPU.
