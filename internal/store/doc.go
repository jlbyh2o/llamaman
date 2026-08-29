// Package store is the only package in the project that contains SQL (DESIGN
// section 1, invariant 1). It owns the two connection pools over the single
// SQLite file, the pragmas applied to every connection, the migration runner
// over the embedded numbered .sql files, and the boot integrity check. Query
// methods take a context and a transaction and hold no business logic; every
// other package sees a repository interface it declares itself (DESIGN section
// 2).
package store
