# api/

`openapi.json` is **generated, committed and drift-checked** — it is produced
from the route registry by `internal/api/openapi` via `make openapi`, and CI
fails if regenerating it changes the committed file (DESIGN section 3, D43).
The same file feeds `openapi-typescript`, which emits `ui/src/api/schema.d.ts`
so the frontend types can never lie about the API (DESIGN section 4).

Do not hand-edit `openapi.json`. Change a route or a DTO in `internal/api` and
run `make openapi`.

The committed file is currently an empty JSON object: no routes exist yet, and
JSON has no comments, so this README is where that is recorded.
