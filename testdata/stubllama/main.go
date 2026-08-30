// Command stubllama is a stand-in for `llama-server` in supervision tests.
//
// It speaks the part of llama-server's surface DESIGN section 5.8 actually
// supervises, and nothing else:
//
//   - it binds `--host`/`--port` and HOLDS the port, so a port conflict is a
//     real conflict rather than a mocked one;
//   - `/health` answers 503 with llama.cpp's own "Loading model" body for a
//     configurable delay, then 200 — which is the whole `starting → loading →
//     ready` sequence the supervisor's state machine is built around;
//   - `/props` answers the two fields the supervisor records on the first ready;
//   - it exits cleanly or dirtily, immediately or after a delay, on demand.
//
// Everything is driven by STUBLLAMA_* environment variables rather than by
// flags, because the argv it receives is rendered by internal/instances from a
// real instance row and must stay exactly what llama-server would get. A stub
// that needed its own flags would either force the renderer to know about it or
// force the test to bypass the renderer — and bypassing the renderer is
// bypassing the thing under test.
//
// Unrecognized flags are IGNORED rather than rejected, deliberately: the point
// of the fixture is that the supervisor's behavior does not depend on which
// llama.cpp flags a build accepts.
package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

func main() {
	host, port := listener(os.Args[1:])

	// STUBLLAMA_EXIT_AFTER and STUBLLAMA_EXIT_CODE together are the crash-loop
	// fixture: a process that binds, answers for a while (or not at all) and
	// then dies with a status the supervisor reads back through
	// ExecMainStatus.
	exitAfter := durationEnv("STUBLLAMA_EXIT_AFTER", 0)
	exitCode := intEnv("STUBLLAMA_EXIT_CODE", 0)

	// STUBLLAMA_READY_AFTER is the model-load delay. Zero means ready on the
	// first probe; a positive value makes /health answer 503 until it elapses,
	// which is the only way to exercise `loading` without loading a model.
	readyAfter := durationEnv("STUBLLAMA_READY_AFTER", 0)

	// STUBLLAMA_NO_LISTEN makes the process refuse to bind at all — a
	// llama-server that died during model load. It is distinct from an exit,
	// because the unit stays active while the port never answers, which is
	// exactly the state `start_timeout_sec` exists to bound.
	if os.Getenv("STUBLLAMA_NO_LISTEN") != "" {
		fmt.Fprintln(os.Stderr, "stubllama: not listening (STUBLLAMA_NO_LISTEN)")
		sleepThenExit(exitAfter, exitCode)
		return
	}

	// STUBLLAMA_EXIT_BEFORE_LISTEN exits before the socket exists at all: the
	// crash-on-load case, where the ledger row is open because the launcher
	// wrote it BEFORE the exec (D54) and nothing else ever observes the run.
	if d := durationEnv("STUBLLAMA_EXIT_BEFORE_LISTEN", -1); d >= 0 {
		time.Sleep(d)
		fmt.Fprintf(os.Stderr, "stubllama: exiting %d before listening\n", exitCode)
		os.Exit(exitCode)
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		// llama-server's own answer to an occupied port is a non-zero exit, and
		// the supervisor reads that status. The launcher's preflight bind
		// (§5.6 step 8) is what normally catches this first.
		fmt.Fprintf(os.Stderr, "stubllama: listen %s:%d: %v\n", host, port, err)
		os.Exit(1)
	}
	defer ln.Close()

	start := time.Now()
	var served atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		if time.Since(start) < readyAfter {
			// llama.cpp's own shape for "still loading", which the supervisor
			// distinguishes from an unreachable port: one is progress, the
			// other is not yet anything.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"code":503,"message":"Loading model","type":"unavailable_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"ok"}`)
	})

	mux.HandleFunc("/props", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"total_slots":%d,"default_generation_settings":{"n_ctx":%d}}`,
			intEnv("STUBLLAMA_SLOTS", 4), intEnv("STUBLLAMA_CTX", 8192))
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "llamacpp:requests_processing %d\n", served.Load())
	})

	// One line on stderr so a test reading the journal (or a captured pipe) can
	// tell a process that reached its listener from one that never did.
	fmt.Fprintf(os.Stderr, "stubllama: listening on %s:%d, ready after %s\n", host, port, readyAfter)

	if exitAfter > 0 {
		go func() {
			time.Sleep(exitAfter)
			fmt.Fprintf(os.Stderr, "stubllama: exiting %d after %s\n", exitCode, exitAfter)
			os.Exit(exitCode)
		}()
	}

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "stubllama: serve: %v\n", err)
		os.Exit(1)
	}
}

// listener recovers `--host` and `--port` from an argv rendered for the real
// llama-server. Both are always present: the renderer appends them
// unconditionally, because the gateway is the front door and an instance never
// listens on a routable address.
func listener(args []string) (string, int) {
	host, port := "127.0.0.1", 0
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--host":
			if i+1 < len(args) {
				host = args[i+1]
				i++
			}
		case "--port":
			if i+1 < len(args) {
				port, _ = strconv.Atoi(args[i+1])
				i++
			}
		default:
			// `--port=N` is not a spelling this renderer produces, but
			// accepting it costs two lines and removes a way for the fixture to
			// silently bind port 0.
			if v, ok := strings.CutPrefix(args[i], "--port="); ok {
				port, _ = strconv.Atoi(v)
			}
			if v, ok := strings.CutPrefix(args[i], "--host="); ok {
				host = v
			}
		}
	}
	if port == 0 {
		fmt.Fprintln(os.Stderr, "stubllama: no --port in argv")
		os.Exit(64)
	}
	return host, port
}

func sleepThenExit(after time.Duration, code int) {
	if after > 0 {
		time.Sleep(after)
	}
	os.Exit(code)
}

func durationEnv(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stubllama: bad %s=%q: %v\n", key, v, err)
		os.Exit(64)
	}
	return d
}

func intEnv(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stubllama: bad %s=%q: %v\n", key, v, err)
		os.Exit(64)
	}
	return n
}
