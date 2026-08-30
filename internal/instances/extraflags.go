package instances

import (
	"fmt"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// `extra_flags`, SPEC section 3.3's escape hatch (DESIGN section 5.7).
//
// The string is split with POSIX shell WORD RULES — no shell invocation, no
// globbing, no variable expansion, no command substitution. That distinction is
// the whole security posture of the field: a user may add any llama.cpp flag
// this design has not modeled, and cannot make the daemon run a second program.

// forbiddenServerFlags are the five overrides section 5.7 rejects. Each one
// contradicts something the renderer owns:
//
//   - `--host`/`--port` are the listener identity the gateway proxies to, and
//     the supervisor may reassign the port after an exit 78 (F5).
//   - `-m`/`--model` is the resolved model path, which is a `config_hash` input
//     the models service maintains (D69).
//   - `--api-key` would put a credential in argv, where `GET /instances/{id}/command`
//     shows it and the journal records it; instance auth is the gateway's
//     (section 9.2), not llama-server's.
var forbiddenServerFlags = []string{"--host", "--port", "-m", "--model", "--api-key"}

// forbiddenBenchFlags are section 10.1's: llama-bench's model, output format
// and repetition count all come from the sweep, and a run whose `-o` is not
// `json` cannot be parsed at all.
var forbiddenBenchFlags = []string{"-m", "-o", "-r"}

// ParseExtraFlags splits an `instances.extra_flags` string into argv words and
// rejects the five overrides section 5.7 forbids.
//
// The refusal is `422 extra_flag_forbidden` at save time; the launcher runs the
// same check because it renders from the stored row, and a row saved before a
// rule existed must not start with a flag the rule forbids.
func ParseExtraFlags(s string) ([]string, error) {
	return parseWords(s, forbiddenServerFlags,
		"extra_flags may not override %s: it is rendered by Llama Man and changing it "+
			"would break the gateway, the model resolution or the start ledger")
}

// parseWords splits s and refuses any word whose flag name is in forbidden.
func parseWords(s string, forbidden []string, message string) ([]string, error) {
	words, err := SplitWords(s)
	if err != nil {
		return nil, model.Error{
			Code:    model.CodeExtraFlagForbidden,
			Message: err.Error(),
		}
	}
	for _, w := range words {
		name, ok := flagName(w)
		if !ok {
			continue
		}
		for _, bad := range forbidden {
			if name != bad {
				continue
			}
			return nil, model.Error{
				Code:    model.CodeExtraFlagForbidden,
				Message: fmt.Sprintf(message, bad),
				Details: map[string]any{"flag": bad},
			}
		}
	}
	return words, nil
}

// SplitWords splits a string into words the way a POSIX shell would, and does
// nothing else a shell would do.
//
// Handled: unquoted whitespace separates words; single quotes take everything
// literally; double quotes take everything literally except a backslash before
// `"`, `\` or a newline; a bare backslash escapes the next character. NOT
// handled, deliberately: globbing, `$` expansion, backticks, `$(…)`, `~`,
// redirection and `;`/`&&`/`|`. Those characters are ordinary text here, which
// is what makes the field safe to hand to `syscall.Exec` without a shell.
//
// An unterminated quote is an error rather than an implicit close: it is
// invariably a typo, and guessing which word the user meant would put an
// argument on the command line that they never wrote.
func SplitWords(s string) ([]string, error) {
	var (
		out     []string
		cur     strings.Builder
		started bool
	)
	flush := func() {
		if started {
			out = append(out, cur.String())
			cur.Reset()
			started = false
		}
	}

	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()

		case c == '\'':
			started = true
			j := i + 1
			for j < len(runes) && runes[j] != '\'' {
				cur.WriteRune(runes[j])
				j++
			}
			if j >= len(runes) {
				return nil, fmt.Errorf("unterminated single quote")
			}
			i = j

		case c == '"':
			started = true
			j := i + 1
			for j < len(runes) && runes[j] != '"' {
				if runes[j] == '\\' && j+1 < len(runes) {
					if n := runes[j+1]; n == '"' || n == '\\' || n == '\n' {
						cur.WriteRune(n)
						j += 2
						continue
					}
				}
				cur.WriteRune(runes[j])
				j++
			}
			if j >= len(runes) {
				return nil, fmt.Errorf("unterminated double quote")
			}
			i = j

		case c == '\\':
			if i+1 >= len(runes) {
				return nil, fmt.Errorf("trailing backslash escapes nothing")
			}
			started = true
			i++
			cur.WriteRune(runes[i])

		default:
			started = true
			cur.WriteRune(c)
		}
	}
	flush()
	return out, nil
}
