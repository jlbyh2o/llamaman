package diagnostics

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// journalSection captures the last N lines of every relevant unit's journal
// (D50, D77). A host where journalctl is absent, or where this identity
// cannot read it, gets one file saying so rather than an empty, unexplained
// directory — the same distinction §3.3's `GET /system/journal` draws between
// "empty" and "denied".
func journalSection(ctx context.Context, opt Options) []File {
	if opt.JournalTail == nil {
		return []File{{
			Name:    "journal/README.txt",
			Content: []byte("no journal reader was available on this host (journalctl absent, or this identity cannot read it — see doctor.json's \"journal\" row)\n"),
		}}
	}

	units := opt.JournalUnits
	if len(units) == 0 {
		scope := opt.Scope
		if scope == "" {
			scope = model.ScopeSystem
		}
		units = systemd.UnitNames(scope)
	}

	var out []File
	for _, unit := range units {
		entries, err := opt.JournalTail(ctx, systemd.JournalOptions{
			Scope: opt.Scope, Units: []string{unit}, Lines: opt.JournalLines,
		})
		name := "journal/" + unit + ".log"
		if err != nil {
			out = append(out, File{Name: name, Content: []byte("could not read this unit's journal: " + err.Error() + "\n")})
			continue
		}
		out = append(out, File{Name: name, Content: renderEntries(entries)})
	}
	return out
}

func renderEntries(entries []systemd.Entry) []byte {
	if len(entries) == 0 {
		return []byte("(no entries)\n")
	}
	var b strings.Builder
	for _, e := range entries {
		ts := "?"
		if !e.Realtime.IsZero() {
			ts = e.Realtime.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&b, "%s [%d] %s[%d]: %s\n", ts, e.Priority, e.Identifier, e.PID, e.Message)
	}
	return []byte(b.String())
}
