# api/

`openapi.json` is **generated, committed and drift-checked** — it is produced
from the route registry by `internal/api/openapi` via `make openapi`, and CI
fails if regenerating it changes the committed file (DESIGN section 3, D43).
The same file feeds `openapi-typescript`, which emits `ui/src/api/schema.d.ts`
so the frontend types can never lie about the API (DESIGN section 4).

Do not hand-edit `openapi.json`. Change a route or a DTO in `internal/api` and
run `make openapi`, which is `go test ./internal/api/openapi -update`.

The generator lives behind a test flag rather than a `main` package on purpose:
the routes are declared by `api.New`, which needs a `Config`, and building one
from a separate command would mean a second description of the daemon's wiring
— exactly the drift D43 exists to prevent. `go test ./...` runs the check;
`make openapi` writes the file.

It documents whatever the route registry currently mounts, which is a subset of
DESIGN section 3 while the endpoints are still being built. That is the point:
the file is a projection of the code, never a plan for it.
