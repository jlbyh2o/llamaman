// Package settings is the typed settings registry: every setting is declared
// once with a key, a type, a default and a validator, and reads go through a
// read-through cache so a hot path never hits the database for a value that has
// not changed (DESIGN section 1).
//
// Registry (registry.go) is the closed, immutable set of every key DESIGN
// section 2.1 names, and does no I/O — construct one with NewRegistry. Cache
// (settings.go) layers a read-through cache and store-backed persistence over
// a Registry: Get resolves and caches a key's override row or its registry
// default, Set validates and persists an override and invalidates the cached
// entry, and Reset deletes an override to restore the default. It takes a
// Store — the repository interface this package declares for itself per
// DESIGN section 1's invariant 1 — which *internal/store.Store satisfies.
package settings
