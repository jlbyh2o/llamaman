// Package models is the local model library service: grouping shards into one
// logical model, pairing an mmproj projector with the model it belongs to,
// reconciling a disk scan against the database, accounting for disk usage, and
// the guards that refuse to delete a model an instance is still using (DESIGN
// section 1).
package models
