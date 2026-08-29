// Package bench runs benchmarks: it expands a sweep definition into its points,
// invokes llama-bench, parses its JSON output, compares runs against one
// another, and exports the results (DESIGN sections 1 and 10).
package bench
