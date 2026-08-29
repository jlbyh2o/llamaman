package store

// Blank import: IDs are TEXT ULIDs that double as SSE cursors (DESIGN section
// 2). This keeps the section-14 module in the build graph until the id minter
// lands. Delete when the real import appears.
import (
	_ "github.com/oklog/ulid/v2"
)
