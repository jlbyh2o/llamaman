package auth

import (
	"io"
	"log/slog"
	"os"
)

// Small filesystem helpers used by the setup-token tests. They exist so those
// tests read as a sequence of section 2.2a's own steps rather than as a sequence
// of os package calls.

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

func statFile(path string) (os.FileInfo, error) { return os.Stat(path) }

func removeFile(path string) error { return os.Remove(path) }

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
