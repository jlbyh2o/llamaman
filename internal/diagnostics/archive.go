package diagnostics

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"time"
)

// WriteTarGz writes files as a gzip-compressed tar to w, sorted by name so the
// archive is deterministic across two runs against the same input — which is
// what lets a test compare one bundle's bytes to another's, and what makes two
// bundles taken moments apart diffable by hand.
//
// Every entry gets the same fixed mtime rather than each file's own: nothing
// in a redacted support bundle should depend on wall-clock skew between the
// sections that built it, and a stable mtime keeps the archive byte-identical
// for byte-identical content.
func WriteTarGz(w io.Writer, files []File, at time.Time) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	for _, f := range files {
		hdr := &tar.Header{
			Name:    f.Name,
			Mode:    0o640,
			Size:    int64(len(f.Content)),
			ModTime: at,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("diagnostics: write header for %s: %w", f.Name, err)
		}
		if _, err := tw.Write(f.Content); err != nil {
			return fmt.Errorf("diagnostics: write %s: %w", f.Name, err)
		}
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("diagnostics: close tar writer: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("diagnostics: close gzip writer: %w", err)
	}
	return nil
}
