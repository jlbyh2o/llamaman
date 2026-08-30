# `testdata/update/` — the frozen `update/pending` shapes

DESIGN section 18 item 7 reduces the cross-version contracts of this design to
**one file with one parser**: `update/pending`. Section 12.1 states the freeze
rules for it — **fields may be added, never removed and never retyped; a reader
ignores fields it does not know** — and section 15 requires "a fixture of every
historical `pending` shape in `testdata/update/`, replayed against the current
gate".

These files are that fixture set. `TestMarkerHistoricalShapes` in
`internal/selfupdate` replays every one of them.

| file | is | must resolve as |
|---|---|---|
| `v1.0.0-floor.json` | the six fields the v1.0.0 floor shipped, and nothing else | parses; every field readable |
| `v1.2.0-added-field.json` | a marker a NEWER binary wrote, carrying a field this binary has never heard of | parses; the unknown field is ignored, not rejected — that is the add-only half of the rule, and rejecting it would make every field a future release adds fatal to the release before it |
| `user-scope-prefix.json` | the D2 topology's `binary_path`, under `~<user>/.local/bin` | parses; nothing about the parser is prefix-aware |
| `unknown-format.json` | `"format": 99` — a marker from a release that changed the format | **swept**, not deferred to: section 12.3's branch 3, naming the file rather than a version. This is the property that stops a file no reader understands from outliving every process that does (D91) |
| `truncated.json` | a marker cut off mid-write — the shape a power loss would leave if the write were not atomic | swept, exactly as above |

Two of these are worth reading twice, because they are the whole reason the
fixture set exists rather than a single round-trip test:

- **The reader that matters is not always the newer one.** The gate reads this
  file in both directions across versions: a newer binary confirming an update,
  and — after a downgrade — an OLDER binary reading a marker a newer one wrote.
  `v1.2.0-added-field.json` is that second case.
- **An unreadable marker is not a "wait and see".** Sweeping it is safe
  *precisely because* the sweep's precondition is a fact about processes, not
  about file contents: no actor is running, so no actor is waiting for it.

`truncated.json` deliberately has no trailing newline: it is the raw bytes a
partial write leaves, not a valid document with a field missing.
