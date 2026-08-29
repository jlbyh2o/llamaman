// Package logx configures logging: a log/slog handler tuned for journald, so
// levels, structured fields and multi-line values survive the journal rather
// than being flattened into prose (DESIGN section 1).
package logx
