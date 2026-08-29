// Package settings is the typed settings registry: every setting is declared
// once with a key, a type, a default and a validator, and reads go through a
// read-through cache so a hot path never hits the database for a value that has
// not changed (DESIGN section 1).
package settings
