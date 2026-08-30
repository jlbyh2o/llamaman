package app

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/api/openapi"
)

// D43's other half, and the reason it exists: "a response-conformance
// middleware runs in integration tests — an undocumented endpoint, a missing
// documented field, or an extra field fails the suite".
//
// The committed `api/openapi.json` is the document under test, not a document
// generated beside the handlers, and that is what closes the loop: the drift
// test in internal/api/openapi asserts the committed file equals what the route
// registry produces, and this test asserts the running daemon answers what the
// committed file promises. Either half alone can pass while the binary and its
// contract disagree — a route registered with a 200 body the handler never
// writes passes the drift check perfectly.
//
// It runs against the REAL composition root — the real store, the real auth
// service, the real setup wizard, the real instance service — because the class
// of bug it is here to catch is a wiring bug: a documented operation whose
// subsystem is nil answers `503 internal_error`, which no route in this
// document lists, and the checker says so by name.

// TestAPIConformance drives every operation this binary serves and fails on any
// answer the committed document does not describe.
func TestAPIConformance(t *testing.T) {
	dir := t.TempDir()
	seedLoopback(t, dir)

	doc := loadCommittedSpec(t)
	checker := openapi.NewChecker(doc)
	// The middleware runs on the server's goroutine, so violations are reported
	// with t.Errorf rather than t.Fatalf: only the test's own goroutine may
	// stop the test, and a violation is a finding rather than a reason to stop
	// exercising the rest of the surface.
	checker.Violation = func(err error) { t.Errorf("D43: %v", err) }

	addr := startDaemonWith(t, dir, checker.Middleware)
	c := newTestClient(t, addr)

	// --- public, before the claim.
	expectStatus(t, c.do(http.MethodGet, "/healthz", "", nil), http.StatusOK)
	expectStatus(t, c.do(http.MethodGet, "/api/v1/meta", "", nil), http.StatusOK)
	expectStatus(t, c.do(http.MethodGet, "/api/v1/setup/state", "", nil), http.StatusOK)
	expectStatus(t, c.do(http.MethodGet, "/api/v1/auth/session", "", nil), http.StatusOK)

	// The setup gate: a session route on an unclaimed host answers 409
	// `setup_required`, which every session operation documents.
	expectStatus(t, c.do(http.MethodGet, "/api/v1/instances", "", nil), http.StatusConflict)

	// A wrong password before the account exists is `bad_credentials`, not an
	// oracle for the claim state.
	expectStatus(t, c.do(http.MethodPost, "/api/v1/auth/login",
		`{"password":"not the password"}`, nil), http.StatusUnauthorized)

	// The cross-origin guard on the one `setup` route, exercised while the
	// window is still open — which is the only moment the attack it refuses is
	// possible at all. A page on another origin cannot claim this host, and the
	// refusal is a documented answer.
	expectStatus(t, c.do(http.MethodPost, "/api/v1/setup/password",
		`{"password":"a hostile password"}`,
		map[string]string{"Sec-Fetch-Site": "cross-site"}), http.StatusForbidden)

	// The claim itself, from loopback, with no token (D38).
	expectStatus(t, c.do(http.MethodPost, "/api/v1/setup/password",
		`{"password":"a good first password"}`, nil), http.StatusNoContent)

	// --- session, after the claim.
	expectStatus(t, c.do(http.MethodGet, "/api/v1/auth/sessions", "", nil), http.StatusOK)
	expectStatus(t, c.do(http.MethodGet, "/api/v1/setup/state", "", nil), http.StatusOK)

	// The five instance operations of section 3.10 that this binary serves. A
	// daemon whose instance service was never constructed answers all of them
	// `503 internal_error`, and every one of those is a violation the checker
	// reports — which is exactly the bug this test exists to catch.
	expectStatus(t, c.do(http.MethodGet, "/api/v1/instances", "", nil), http.StatusOK)

	// A create with no model is refused `422 model_missing`, which is one of the
	// codes that operation documents for 422 — so the refusal is itself a
	// conformance case, and it reaches the instance SERVICE rather than the
	// 503 a daemon built without one would answer. A create that succeeds needs
	// a `models` row, and the only writer of that table is the models service
	// (section 7.2a); inventing one here with raw SQL would put the second SQL
	// statement in this repository outside internal/store, which D49's first
	// invariant forbids.
	expectStatus(t, c.do(http.MethodPost, "/api/v1/instances", `{"name":"conformance"}`, nil),
		http.StatusUnprocessableEntity)

	// The three id-addressed operations, against an id that names no row: the
	// 404 they document, and the proof that each one reached the service.
	const noSuchID = "01JZZZZZZZZZZZZZZZZZZZZZZZ"
	expectStatus(t, c.do(http.MethodGet, "/api/v1/instances/"+noSuchID, "", nil),
		http.StatusNotFound)
	expectStatus(t, c.do(http.MethodPatch, "/api/v1/instances/"+noSuchID,
		`{"generation":1,"display_name":"renamed"}`, nil), http.StatusNotFound)
	expectStatus(t, c.do(http.MethodDelete, "/api/v1/instances/"+noSuchID, "", nil),
		http.StatusNotFound)

	// --- the two answers the fallback owns, which the checker treats as
	// documented by construction: a path no route matches, and a method no
	// route serves on a path that exists.
	expectStatus(t, c.do(http.MethodGet, "/api/v1/no-such-endpoint", "", nil), http.StatusNotFound)
	expectStatus(t, c.do(http.MethodDelete, "/api/v1/meta", "", nil), http.StatusMethodNotAllowed)

	// --- logout last: it revokes the session every request above used.
	expectStatus(t, c.do(http.MethodPost, "/api/v1/auth/logout", "", nil), http.StatusNoContent)
}

// startDaemonWith is startDaemon plus the D43 middleware.
func startDaemonWith(t *testing.T, dir string, conformance func(http.Handler) http.Handler) string {
	t.Helper()

	ready := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Logger:           quiet(),
			Notifier:         &recordingNotifier{},
			StateDirOverride: dir,
			Getenv:           func(string) string { return "" },
			ReadyHook:        func(addr string) { ready <- addr },
			Conformance:      conformance,
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("the daemon did not shut down")
		}
	})

	select {
	case addr := <-ready:
		return addr
	case err := <-done:
		t.Fatalf("Run returned before it was listening: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the daemon never became ready")
	}
	return ""
}

// loadCommittedSpec reads api/openapi.json — the generated, committed,
// drift-checked document, and the one a client actually codes against.
func loadCommittedSpec(t *testing.T) map[string]any {
	t.Helper()
	path := filepath.Join("..", "..", "api", "openapi.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc
}

func expectStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Errorf("%s %s = %d, want %d", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, want)
	}
}
