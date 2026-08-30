package diagnostics

import (
	"bytes"
	"regexp"
)

// Redact scrubs every occurrence of every secret in values from every file's
// content, then runs one shape-based pass over the result as a second line of
// defense — so a token this bundle never intentionally wrote, but that
// happened to appear verbatim in a journal line or a build log, is still
// caught.
//
// It is deliberately a POST-process over the finished bundle rather than a
// rule each section must remember to apply: a bundle's sections are added to
// over releases, and "every new section must remember to redact" is exactly
// the kind of rule that gets forgotten once. This function is the one place
// that promise is kept, and it is kept for content this package did not
// necessarily author itself (a journal line, a build log).
func Redact(files []File, values []string) []File {
	out := make([]File, len(files))
	for i, f := range files {
		content := f.Content
		for _, v := range values {
			if v == "" {
				continue
			}
			content = bytes.ReplaceAll(content, []byte(v), []byte(redactedMarker))
		}
		content = shapeRedact(content)
		out[i] = File{Name: f.Name, Content: content}
	}
	return out
}

const redactedMarker = "[REDACTED]"

// shapePatterns matches the SHAPES of the credentials this project handles — a
// Hugging Face or GitHub token — independently of whether this process
// currently has that value loaded. It is a safety net, not the primary
// mechanism: Redact's value-based pass above is what a real stored secret goes
// through.
var shapePatterns = []*regexp.Regexp{
	regexp.MustCompile(`hf_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`ghp_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
}

func shapeRedact(content []byte) []byte {
	for _, re := range shapePatterns {
		content = re.ReplaceAll(content, []byte(redactedMarker))
	}
	return content
}
