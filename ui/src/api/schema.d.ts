/**
 * GENERATED FILE — DO NOT EDIT.
 *
 * Llama Man v1, from api/openapi.json.
 * Regenerate with `npm run gen:api`; CI runs `npm run gen:api:check` and fails on drift
 * (DESIGN section 3, D43 — the types can never lie about the API).
 */

/* eslint-disable */

export interface paths {
    "/api/v1/auth/login": {
        get?: never;
        put?: never;
        /** Exchange the admin password for a session cookie and a CSRF cookie. */
        post: operations["login"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/auth/logout": {
        get?: never;
        put?: never;
        /** Revoke this session and clear its cookies. */
        post: operations["logout"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/auth/password": {
        get?: never;
        put?: never;
        /** Change the admin password; every other session is revoked. */
        post: operations["changePassword"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/auth/session": {
        /** Whether this request carries a session, whether the host has been claimed, whether the wizard has finished, and when the session expires. */
        get: operations["getAuthSession"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/auth/sessions": {
        /** The active admin sessions, with address, user agent and last-seen time. */
        get: operations["listSessions"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/auth/sessions/{id}": {
        get?: never;
        put?: never;
        post?: never;
        /** Revoke one session — a device the operator no longer uses, or their own. */
        delete: operations["revokeSession"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/bench/compare": {
        get?: never;
        put?: never;
        /** Chart-ready series across runs: one grouped query over `bench_points ⋈ bench_results` with the sweep axes as columns. */
        post: operations["compareBenchRuns"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/bench/preflight": {
        /** What this sweep would do before it is committed: GPU conflicts, free VRAM, the point count, a duration estimate, and every FlagSet field llama-bench has no equivalent for. */
        get: operations["benchPreflight"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/bench/runs": {
        /** Every benchmark run, newest first, with its summary counters. */
        get: operations["listBenchRuns"];
        put?: never;
        /** Expand a sweep into its points and queue it. The cross-product becomes `bench_points` rows BEFORE anything executes, which is what makes progress and resume exact. */
        post: operations["createBenchRun"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/bench/runs/{id}": {
        /** One run: its points, the captured environment, and the live job's `{points_done, points_total, current}` progress. */
        get: operations["getBenchRun"];
        put?: never;
        post?: never;
        /** Delete a run and its points and results. Refused while its job is live or while it still owes stopped instances a restart. */
        delete: operations["deleteBenchRun"];
        options?: never;
        head?: never;
        /** Rename or annotate a run. Its inputs and results are immutable. */
        patch: operations["patchBenchRun"];
        trace?: never;
    };
    "/api/v1/bench/runs/{id}/cancel": {
        get?: never;
        put?: never;
        /** Stop a running sweep. The process group is signaled, the remaining points are marked `skipped`, and the stop-and-restore finalizer restarts every instance the run stopped. */
        post: operations["cancelBenchRun"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/bench/runs/{id}/export": {
        /** Export a run: `json` (run + points + results, self-describing), `csv` (one row per result), or `md` (a provenance header plus a table, ready to paste into an issue). Each carries a `Content-Disposition` filename. */
        get: operations["exportBenchRun"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/bench/runs/{id}/results": {
        /** The run's results, flattened one row per measurement with all axes as columns. */
        get: operations["getBenchRunResults"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/bench/runs/{id}/start": {
        get?: never;
        put?: never;
        /** Queue a draft run. It waits for the bench lease, one sweep at a time. */
        post: operations["startBenchRun"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/bench/series": {
        /** History for one model across llama.cpp versions, oldest first. */
        get: operations["benchSeries"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/cache/roots": {
        /** Every known hub directory, primary first. */
        get: operations["listCacheRoots"];
        put?: never;
        /** Register an existing hub directory as scan-and-serve. A new root is never primary; promote it to make downloads land there. */
        post: operations["addCacheRoot"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/cache/roots/{id}": {
        get?: never;
        put?: never;
        post?: never;
        /** Detach a root: its catalog rows are removed and no file is touched. Refused while ANY instance references one of its models, soft-deleted ones included. */
        delete: operations["detachCacheRoot"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/cache/roots/{id}/promote": {
        get?: never;
        put?: never;
        /** Make this root primary — the single write path shared with PATCH /settings {hf.hub_dir}. Nothing is moved or copied. */
        post: operations["promoteCacheRoot"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/cache/scan": {
        get?: never;
        put?: never;
        /** Walk a cache root and reconcile the catalog against it. Makes no network calls. */
        post: operations["scanCache"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/cache/scans/{id}": {
        /** One scan's progress and results. */
        get: operations["getCacheScan"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/cache/strays": {
        /** Files in a cache root that belong to no model, largest first. */
        get: operations["listStrays"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/cache/strays/{id}": {
        get?: never;
        put?: never;
        post?: never;
        /** Forget a stray, and optionally remove the file it names. */
        delete: operations["deleteStray"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/cache/strays/{id}/dismiss": {
        get?: never;
        put?: never;
        /** Hide a stray from the list without removing anything. */
        post: operations["dismissStray"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/downloads": {
        /** Downloads with their per-file progress. */
        get: operations["listDownloads"];
        put?: never;
        /** Queue a model download. The jobs, downloads, models and model_files rows are written in one transaction, so the job is the receipt. */
        post: operations["createDownload"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/downloads/{id}": {
        /** One download with its per-file progress. */
        get: operations["getDownload"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        /** Change a download's queue priority. Moves the jobs row and the downloads row together, because the pool leases on the former. */
        patch: operations["reorderDownload"];
        trace?: never;
    };
    "/api/v1/downloads/{id}/cancel": {
        get?: never;
        put?: never;
        /** Cancel a download. The partial files are kept by default, so a retry resumes rather than starting over. */
        post: operations["cancelDownload"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/downloads/{id}/pause": {
        get?: never;
        put?: never;
        /** Pause a download. The job releases its lease, the running transfers unwind, and every `.incomplete` file stands where it is for the resume to continue from. */
        post: operations["pauseDownload"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/downloads/{id}/resume": {
        get?: never;
        put?: never;
        /** Resume a paused download. It continues from the byte each file reached. */
        post: operations["resumeDownload"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/downloads/{id}/retry": {
        get?: never;
        put?: never;
        /** Run a failed or canceled download again, resuming from whatever is on disk. */
        post: operations["retryDownload"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/events": {
        /** Server-Sent Events. `?topics=` filters the stream; `Last-Event-ID` replays the `events` topic from the durable log. */
        get: operations["streamEvents"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/events/log": {
        /** The durable event log, newest first, paged backwards on the ULID cursor. */
        get: operations["listEvents"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/fit/estimate": {
        get?: never;
        put?: never;
        /** Will this model and these flags run on these GPUs, and where does the memory go: per-GPU placement, the KV and compute breakdown, the `-ngl auto` advisory, and a recommendation (section 8). */
        post: operations["estimateFit"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/fit/estimate-batch": {
        get?: never;
        put?: never;
        /** One report per quantization of a repository, plus the largest one that fits — this is what drives the quant picker (section 3.9). */
        post: operations["estimateFitBatch"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/gateway/denials": {
        /** Denial counters per instance and reason (section 2.9). */
        get: operations["listGatewayDenials"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/github/token": {
        /** Whether a GitHub token is stored, its masked hint, and what the last validation said. Never the token. */
        get: operations["getGitHubToken"];
        /** Validate a GitHub token against `GET https://api.github.com/user` and store it sealed only if it is accepted. */
        put: operations["putGitHubToken"];
        post?: never;
        /** Forget the stored GitHub token. The client that used it reverts to anonymous. */
        delete: operations["deleteGitHubToken"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/hf/card/{repo}": {
        /** The model card, RENDERED AND SANITIZED SERVER-SIDE (D35), plus the raw markdown for a "view source" toggle. */
        get: operations["getHFCard"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/hf/model/{repo}": {
        /** One repository's metadata, its `gguf` summary, its gated flag and this host's local-availability annotations. */
        get: operations["getHFModel"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/hf/peek/{repo}": {
        /** The GGUF header read over HTTP Range BEFORE downloading: architecture, layers, KV heads, trained context, vocabulary, SWA window and the tensor summary (sections 3.6, 8.5). */
        get: operations["peekHFFile"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/hf/search": {
        /** Search the Hub's GGUF repositories. `?cursor=` is the Hub's own opaque cursor, passed through unmodified. */
        get: operations["searchHF"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/hf/token": {
        /** Whether a Hugging Face token is stored, its masked hint, and what the last validation said. Never the token. */
        get: operations["getHFToken"];
        /** Validate a Hugging Face token against `GET /api/whoami-v2` and store it sealed only if it is accepted. */
        put: operations["putHFToken"];
        post?: never;
        /** Forget the stored Hugging Face token. The client that used it reverts to anonymous. */
        delete: operations["deleteHFToken"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/hf/tree/{repo}": {
        /** The file tree grouped by quantization, with TRUE `lfs.size` totals, shard groups, mmproj candidates and `local_model_id`. */
        get: operations["getHFTree"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/instances": {
        /** Config joined with status, plus the four derived flags. Soft-deleted instances are excluded unless ?include_deleted=true. */
        get: operations["listInstances"];
        put?: never;
        /** Create an instance. Ports are auto-allocated when omitted and validated against the section 2.8 rules. */
        post: operations["createInstance"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/instances/validate": {
        get?: never;
        put?: never;
        /** Dry-run a FlagSet: render argv, check conflicts, return the three-valued draft verdict and a fit estimate. Never a 422 — it reports rather than refuses. */
        post: operations["validateInstance"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/instances/{id}": {
        /** One instance, with its rendered argv and its last five starts. */
        get: operations["getInstance"];
        put?: never;
        post?: never;
        /** Soft delete: stop and disable the unit, close the listener, stamp deleted_at, keep every row. ?purge=true is the explicit hard delete. */
        delete: operations["deleteInstance"];
        options?: never;
        head?: never;
        /** Edit an instance. The body must carry the `generation` the client read. */
        patch: operations["patchInstance"];
        trace?: never;
    };
    "/api/v1/instances/{id}/autostart": {
        get?: never;
        /** Enable or disable the unit file, and nothing else — it never starts or stops. Answers hint=start_now when enabling on a stopped instance. */
        put: operations["setInstanceAutostart"];
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/instances/{id}/command": {
        /** The argv, environment and unit name — copyable and auditable. */
        get: operations["getInstanceCommand"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/instances/{id}/logs": {
        /** The unit's journal. SSE when Accept: text/event-stream. */
        get: operations["getInstanceLogs"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/instances/{id}/metrics": {
        /** llama-server's /metrics, proxied. */
        get: operations["getInstanceMetrics"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/instances/{id}/pin-ngl": {
        get?: never;
        put?: never;
        /** Rewrite n_gpu_layers from auto to a count using the current fit estimate. An explicit config edit: it bumps generation and config_hash. */
        post: operations["pinInstanceNGL"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/instances/{id}/props": {
        /** llama-server's /props, proxied. */
        get: operations["getInstanceProps"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/instances/{id}/reset-failed": {
        get?: never;
        put?: never;
        /** Clear the crash-loop latch and the backoff, start the crash-loop window over, and call systemd ResetFailed. */
        post: operations["resetFailedInstance"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/instances/{id}/restart": {
        get?: never;
        put?: never;
        /** Stop and start in one request. */
        post: operations["restartInstance"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/instances/{id}/safe-start": {
        get?: never;
        put?: never;
        /** One-shot start with -ngl 0 -c 2048 to isolate GPU from model problems. Never persisted. */
        post: operations["safeStartInstance"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/instances/{id}/slots": {
        /** llama-server's /slots, proxied. */
        get: operations["getInstanceSlots"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/instances/{id}/start": {
        get?: never;
        put?: never;
        /** Set desired_state=running and stamp pending_trigger='user'. The supervisor starts it. */
        post: operations["startInstance"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/instances/{id}/starts": {
        /** The start ledger: trigger, outcome, exit code, detail and argv — including preflight failures that never reached execve. */
        get: operations["listInstanceStarts"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/instances/{id}/status": {
        /** The instance_status row plus a live read of the unit and the health probe. */
        get: operations["getInstanceStatus"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/instances/{id}/stop": {
        get?: never;
        put?: never;
        /** Set desired_state=stopped. Answers hint=will_start_at_boot when autostart is on. */
        post: operations["stopInstance"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/instances/{id}/usage": {
        /** instance_usage_daily rows for both auth modes, plus llama-server's own counter, each labeled with its source. */
        get: operations["getInstanceUsage"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/jobs": {
        /** The job queue, newest first. `?state=active` is the live set. */
        get: operations["listJobs"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/jobs/{id}": {
        /** One job: state, progress, error. */
        get: operations["getJob"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/jobs/{id}/cancel": {
        get?: never;
        put?: never;
        /** Request cancellation. Two kinds carry a cut-off rather than a blanket accept: `llamacpp_activate` before its step-3 commit, and `self_update` before the `staged` commit. */
        post: operations["cancelJob"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/llamacpp/active": {
        /** The active build, with its build options and the devices it can see. */
        get: operations["getActiveLlamacpp"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/llamacpp/plan": {
        /** What installing this would do, before committing: the acquisition decision with its reason, the detected CUDA architectures, the missing toolchain items and a free-space check (section 6.3). */
        get: operations["planLlamacppInstall"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/llamacpp/releases": {
        /** The releases of a channel, with the changelog rendered and sanitized server-side (D35), the resolved `nightly_tag`, and the api.github.com rate-limit headroom. */
        get: operations["listLlamacppReleases"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/llamacpp/rollback": {
        get?: never;
        put?: never;
        /** Activate the retained previous build. This is the activation routine with `previous_active` as the target, revert path included. */
        post: operations["rollbackLlamacpp"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/llamacpp/versions": {
        /** Every llama.cpp build this host has, with its state, size, dates, `is_active`, `previous_active` and `in_use_by`. */
        get: operations["listLlamacppVersions"];
        put?: never;
        /** Install a llama.cpp build. The request resolves to a three-part id before anything is inserted, and then takes exactly one of D71's five branches. */
        post: operations["installLlamacppVersion"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/llamacpp/versions/{id}": {
        /** One version, including its build options, failing step and device list. */
        get: operations["getLlamacppVersion"];
        put?: never;
        post?: never;
        /** Remove a build's directory. Refused for the active build, for the rollback target, and for any directory a live process is executing out of (D25). */
        delete: operations["deleteLlamacppVersion"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/llamacpp/versions/{id}/activate": {
        get?: never;
        put?: never;
        /** Make this build active: flip `is_active`, recompute every instance's `config_hash`, move the symlink, and optionally run the canary-gated rolling restart. */
        post: operations["activateLlamacppVersion"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/llamacpp/versions/{id}/cancel": {
        get?: never;
        put?: never;
        /** Stop a build that is running. Section 2.5's cancel edge: the process group is signaled and the partial directories are removed by the worker. */
        post: operations["cancelLlamacppVersion"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/llamacpp/versions/{id}/log": {
        /** The build log: `?offset=&limit=` plain text by default, the JSON envelope with `Accept: application/json`, and a live SSE tail with `Accept: text/event-stream`. */
        get: operations["getLlamacppVersionLog"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/llamacpp/versions/{id}/retry": {
        get?: never;
        put?: never;
        /** Run a stopped build again. An `interrupted` build resumes against the warm worktree and cmake cache (D4); a failed or canceled one takes section 2.5's reuse-and-reset. */
        post: operations["retryLlamacppVersion"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/meta": {
        /** Version, claim state and the port the management listener landed on — what install.sh polls. */
        get: operations["getMeta"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/models": {
        /** The local model catalog, with the instances using each row. */
        get: operations["listModels"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/models/{id}": {
        /** One model with its files, its paired projector and the projector picker. */
        get: operations["getModel"];
        put?: never;
        post?: never;
        /** Execute the delete preview. The row moves deleting → deleted and is kept; no SQL DELETE is ever issued against a model row. */
        delete: operations["deleteModel"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/models/{id}/delete-preview": {
        /** What deleting this model would free, with blobs refcounted across every snapshot in the repository (D28). */
        get: operations["previewModelDelete"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/models/{id}/metadata": {
        /** The full GGUF key/value map, re-read from the file so a scan never has to retain a tokenizer table it will be asked about once. */
        get: operations["getModelMetadata"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/models/{id}/pair-mmproj": {
        get?: never;
        put?: never;
        /** Attach or detach a multimodal projector. Sets mmproj_auto = 0, so no later scan overrules the choice. */
        post: operations["pairModelMmproj"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/models/{id}/verify": {
        get?: never;
        put?: never;
        /** Re-stat every file and, when hf.verify_checksums is on, re-hash the ones whose blob name is a sha256. */
        post: operations["verifyModel"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/ports/suggest": {
        /** The next free port not in the database and not bound. */
        get: operations["suggestPort"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/presets": {
        /** Every flag preset, built-ins first. */
        get: operations["listPresets"];
        put?: never;
        /** Save a FlagSet and extra_flags under a name. */
        post: operations["createPreset"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/presets/from-instance": {
        get?: never;
        put?: never;
        /** Capture an instance's FlagSet and extra_flags as a new preset. The instance is named in the body rather than in the path. */
        post: operations["createPresetFromInstance"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/presets/{id}": {
        /** One preset. */
        get: operations["getPreset"];
        put?: never;
        post?: never;
        /** Remove a preset. Built-ins refuse. */
        delete: operations["deletePreset"];
        options?: never;
        head?: never;
        /** Rename or re-tune a preset. Built-ins refuse. */
        patch: operations["patchPreset"];
        trace?: never;
    };
    "/api/v1/presets/{id}/apply": {
        get?: never;
        put?: never;
        /** Apply a preset to instances, key by key, and report the per-instance diff with each one's restart_required. */
        post: operations["applyPreset"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/settings": {
        /** Every registered key's current value, plus the schema the form is generated from. Secrets are never here — they have their own validating endpoints. */
        get: operations["getSettings"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        /** Partial update. A value that fails its definition refuses the WHOLE patch — a half-applied settings form is worse than a refused one. */
        patch: operations["patchSettings"];
        trace?: never;
    };
    "/api/v1/settings/reset": {
        get?: never;
        put?: never;
        /** Delete the named override rows; the built-in defaults resume. */
        post: operations["resetSettings"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/setup/complete": {
        get?: never;
        put?: never;
        /** Finish the wizard. Refused while a non-skippable step is unfinished. */
        post: operations["completeSetup"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/setup/password": {
        get?: never;
        put?: never;
        /** Claim this host: create the admin account, burn the one-time token and log the browser in. Loopback callers need no `X-Setup-Token` (D38). */
        post: operations["setupPassword"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/setup/skip": {
        get?: never;
        put?: never;
        /** Skip a wizard step that section 11.2 marks skippable. */
        post: operations["skipSetupStep"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/setup/state": {
        /** Claim state, whether this caller needs a setup token, and where the wizard stands. */
        get: operations["getSetupState"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/system/capabilities": {
        /** The single object the UI reads to decide which controls to disable and which explanatory copy to show, so it never has to guess. */
        get: operations["getSystemCapabilities"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/system/diagnostics": {
        /** The D50 support bundle: configuration, unit files, the journal tail and the toolchain report, redacted of every secret. */
        get: operations["downloadDiagnostics"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/system/disk": {
        /** Total, free and used per cache root and for the state directory. */
        get: operations["getSystemDisk"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/system/gpus": {
        /** The GPU inventory with live VRAM, utilization and temperature. */
        get: operations["listSystemGPUs"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/system/info": {
        /** Version, uptime, service identity, systemd facts, ports, paths and host hardware. */
        get: operations["getSystemInfo"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/system/journal": {
        /** The journal tail for a unit. SSE when Accept: text/event-stream. */
        get: operations["getSystemJournal"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/system/notifications": {
        /** Undismissed notifications with their remediation actions. */
        get: operations["listSystemNotifications"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/system/notifications/{id}/dismiss": {
        get?: never;
        put?: never;
        /** Clear one card. The row is stamped, not deleted. */
        post: operations["dismissSystemNotification"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/system/restart": {
        get?: never;
        put?: never;
        /** Restart llamaman.service: commit, flush this 202, drain, hand the listeners to the fd store, then a non-blocking RestartNoWait. Instances are untouched. */
        post: operations["restartSystem"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/system/toolchain": {
        /** The latest probe: per-tool found/version/verdict with fix guidance. */
        get: operations["getSystemToolchain"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/system/toolchain/probe": {
        get?: never;
        put?: never;
        /** Re-run the toolchain probe. */
        post: operations["probeSystemToolchain"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/system/units": {
        /** Installed unit files versus what this binary would render, with the exact repair command. Read-only — the daemon cannot write /etc. */
        get: operations["listSystemUnits"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/tokens": {
        /** Every API token: prefix, scope, state, counts and last use. Never a secret — none is stored. */
        get: operations["listAPITokens"];
        put?: never;
        /** Mint a token. The 201 is the only response in this API that ever contains the secret; it is stored as a sha256 and cannot be shown again. */
        post: operations["createAPIToken"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/tokens/{id}": {
        /** One token, with its per-instance usage summary. */
        get: operations["getAPIToken"];
        put?: never;
        post?: never;
        /** Revoke. Soft and terminal: the row and its hash are retained so the secret can never become valid again. */
        delete: operations["deleteAPIToken"];
        options?: never;
        head?: never;
        /** Rename, disable, re-enable, rescope or rate-limit a token. Revoked is terminal and cannot be edited. */
        patch: operations["patchAPIToken"];
        trace?: never;
    };
    "/api/v1/tokens/{id}/usage": {
        /** This token's daily requests, errors, bytes and reported token counts, per instance. */
        get: operations["getAPITokenUsage"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/update/apply": {
        get?: never;
        put?: never;
        /** Stage an update to any listed tag, newer or older. Four guard clauses, all evaluated inside the transaction that inserts the row and its job. */
        post: operations["applyUpdate"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/update/check": {
        get?: never;
        put?: never;
        /** Refresh the release feed. */
        post: operations["checkUpdates"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/update/releases": {
        /** Every release, with the changelog rendered server-side. */
        get: operations["listUpdateReleases"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/api/v1/update/status": {
        /** Current version, latest known, the in-flight row, and the one self-update fact the confirmation gate last computed. */
        get: operations["getUpdateStatus"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/healthz": {
        /** Liveness only, no state. */
        get: operations["getHealth"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
}

export interface components {
    schemas: {
        APITokenDTO: {
            created_at: string;
            expires_at?: string | null;
            hint: string;
            id: string;
            instance_ids: string[];
            last_used_at?: string | null;
            last_used_ip?: string | null;
            name: string;
            prefix: string;
            rate_limit_rpm?: number | null;
            request_count: number;
            revoked_at?: string | null;
            scope: string;
            state: string;
            updated_at: string;
        };
        ActivateLlamacppRequest: {
            canary_instance_id?: string;
            restart_instances?: string;
        };
        AddCacheRootRequest: {
            path: string;
        };
        AddCacheRootResponse: {
            root: components["schemas"]["CacheRootDTO"];
            scan?: components["schemas"]["JobReceiptDTO"] | null;
        };
        ApplyPresetRequest: {
            instance_ids: string[];
            overwrite: string[];
        };
        ApplyUpdateRequest: {
            tag: string;
        };
        ApplyUpdateResponse: {
            job_id: string;
            procedure?: string[];
            schema_warning: boolean;
            self_update_id: string;
        };
        AutostartDTO: {
            enabled: boolean;
            hint?: string | null;
            hints: string[];
        };
        BenchCompareDTO: {
            points: components["schemas"]["BenchComparePointDTO"][];
            series: string;
            x: string;
            y: string;
        };
        BenchComparePointDTO: {
            samples: number;
            series: string;
            value: number;
            x: string;
        };
        BenchConflictDTO: {
            assumed: boolean;
            attribution: string;
            gpu_uuids: string[];
            instance_id: string;
            name: string;
            state: string;
        };
        BenchIgnoredFlagDTO: {
            field: string;
            reason: string;
        };
        BenchPointDTO: {
            argv: string[];
            error_message?: string | null;
            finished_at?: string | null;
            flash_attn?: boolean | null;
            id: string;
            label: string;
            n_batch?: number | null;
            n_depth?: number | null;
            n_gpu_layers?: number | null;
            n_threads?: number | null;
            n_ubatch?: number | null;
            ordinal: number;
            split_mode?: string | null;
            started_at?: string | null;
            state: string;
            tensor_split?: string | null;
            type_k?: string | null;
            type_v?: string | null;
        };
        BenchPreflightDTO: {
            conflicts: components["schemas"]["BenchConflictDTO"][];
            estimate_from_history: boolean;
            estimated_sec: number;
            exclusive_gpu: boolean;
            free_vram_bytes: Record<string, number>;
            gpu_identity_known: boolean;
            ignored_flags: components["schemas"]["BenchIgnoredFlagDTO"][];
            llamacpp_tag: string;
            llamacpp_version_id: string;
            model_id: string;
            model_label: string;
            model_path: string;
            notes: string[];
            points_total: number;
            repetitions: number;
            runtime_ready: boolean;
            target_gpus: string[];
        };
        BenchProgressDTO: {
            current: string;
            points_done: number;
            points_total: number;
        };
        BenchResultRowDTO: {
            avg_ns: number;
            avg_ts: number;
            flash_attn?: boolean | null;
            label: string;
            n_batch?: number | null;
            n_depth: number;
            n_gen: number;
            n_gpu_layers?: number | null;
            n_prompt: number;
            n_threads?: number | null;
            n_ubatch?: number | null;
            ordinal: number;
            point_id: string;
            samples: number;
            split_mode?: string | null;
            stddev_ns: number;
            stddev_ts: number;
            tensor_split?: string | null;
            test_kind: string;
            type_k?: string | null;
            type_v?: string | null;
        };
        BenchRunDTO: {
            created_at: string;
            error_message?: string | null;
            finished_at?: string | null;
            id: string;
            llamacpp_backend: string;
            llamacpp_commit?: string | null;
            llamacpp_tag: string;
            llamacpp_version_id?: string | null;
            model_id?: string | null;
            model_label: string;
            model_path: string;
            name: string;
            notes?: string | null;
            points_done: number;
            points_failed: number;
            points_total: number;
            quant_label?: string | null;
            repetitions: number;
            restore_done: boolean;
            started_at?: string | null;
            state: string;
            stopped_instances: string[];
        };
        BenchRunDetailDTO: {
            gpu: (Record<string, unknown>)[];
            host: Record<string, unknown>;
            job_id?: string | null;
            job_state?: string | null;
            points: components["schemas"]["BenchPointDTO"][];
            progress?: components["schemas"]["BenchProgressDTO"] | null;
            run: components["schemas"]["BenchRunDTO"];
            sweep: Record<string, unknown>;
        };
        BenchSeriesDTO: {
            group: string;
            metric: string;
            points: components["schemas"]["BenchSeriesPointDTO"][];
        };
        BenchSeriesPointDTO: {
            at: string;
            group: string;
            run_id: string;
            run_name: string;
            samples: number;
            value: number;
        };
        CacheRootDTO: {
            bytes_on_disk: number;
            created_at: string;
            detected_from?: string | null;
            free_bytes?: number | null;
            fs_type?: string | null;
            hf_home: string;
            id: string;
            is_primary: boolean;
            last_scan_at?: string | null;
            models: number;
            path: string;
            symlinks_ok: boolean;
            total_bytes?: number | null;
            writable: boolean;
        };
        CacheScanDTO: {
            bytes_total: number;
            created_at: string;
            dirs_seen: number;
            error_message?: string | null;
            files_seen: number;
            finished_at?: string | null;
            id: string;
            models_added: number;
            models_found: number;
            models_missing: number;
            root_id: string;
            started_at?: string | null;
            state: string;
            strays_found: number;
            trigger: string;
        };
        CapabilitiesDTO: {
            autostart_control: boolean;
            degraded: components["schemas"]["DegradedModeDTO"][];
            instance_control: boolean;
            journal_read: string;
            listener_continuity: string;
            polkit_ok?: boolean | null;
            polkit_unit_files?: boolean | null;
            self_update: boolean;
            self_update_revert: boolean;
            systemd_control: string;
            systemd_scope: string;
        };
        CompareBenchRequest: {
            filters?: Record<string, string>;
            run_ids?: string[];
            series?: string;
            x: string;
            y: string;
        };
        CreateAPITokenRequest: {
            expires_at?: string | null;
            instance_ids?: string[];
            name: string;
            rate_limit_rpm?: number | null;
            scope?: string | null;
        };
        CreateAPITokenResponse: {
            hint: string;
            secret: string;
            token: components["schemas"]["APITokenDTO"];
        };
        CreateBenchRunRequest: {
            draft?: boolean;
            model_id: string;
            name?: string;
            on_conflict?: string;
            repetitions?: number;
            sweep?: string;
        };
        CreateBenchRunResponse: {
            job_id?: string | null;
            points: components["schemas"]["BenchPointDTO"][];
            run: components["schemas"]["BenchRunDTO"];
            subject: components["schemas"]["SubjectDTO"];
        };
        CreateDownloadRequest: {
            files: string[];
            include_mmproj?: boolean | null;
            kind?: string;
            mmproj_file?: string;
            priority?: number;
            repo_id: string;
            revision?: string;
        };
        CreateDownloadResponse: {
            bytes_total: number;
            download_id: string;
            job_id?: string | null;
            mmproj_model_id?: string | null;
            model_id: string;
            subject: components["schemas"]["SubjectDTO"];
        };
        CreateInstanceRequest: {
            auth_mode?: string | null;
            autostart?: boolean | null;
            description?: string | null;
            display_name?: string | null;
            draft_model_id?: string | null;
            extra_flags?: string | null;
            flags?: components["schemas"]["FlagSet"] | null;
            internal_port?: number | null;
            mmproj_model_id?: string | null;
            model_id: string;
            name: string;
            public_port?: number | null;
            restart_max?: number | null;
            restart_policy?: string | null;
            restart_window_sec?: number | null;
        };
        CreateInstanceResponse: {
            instance: components["schemas"]["InstanceDTO"];
            warnings: components["schemas"]["WarningDTO"][];
        };
        DegradedModeDTO: {
            hints: string[];
            id: string;
            summary: string;
        };
        DeleteInstanceResponse: {
            hints: string[];
            purged: boolean;
        };
        DeleteModelResponse: {
            job_id?: string | null;
            plan: components["schemas"]["DeletePreviewDTO"];
            subject: components["schemas"]["SubjectDTO"];
        };
        DeletePreviewDTO: {
            blobs_shared_kept: number;
            bytes: number;
            files: number;
            in_use_by: components["schemas"]["InstanceRefDTO"][];
            removes_repo_dir: boolean;
            shared_bytes: number;
        };
        DiagnosticsDTO: {
            filename: string;
        };
        DiskUsageDTO: {
            free_bytes: number;
            kind: string;
            model_bytes?: number | null;
            path: string;
            primary?: boolean | null;
            total_bytes: number;
            used_bytes: number;
            version_bytes?: number | null;
        };
        DownloadDTO: {
            attempts: number;
            bytes_at_start: number;
            bytes_done: number;
            bytes_total: number;
            created_at: string;
            error_code?: string | null;
            error_message?: string | null;
            eta_sec?: number | null;
            files: components["schemas"]["DownloadFileDTO"][];
            finished_at?: string | null;
            id: string;
            include_mmproj: boolean;
            model_id: string;
            primary_file: string;
            priority: number;
            repo_id: string;
            revision: string;
            speed_bps: number;
            started_at?: string | null;
            state: string;
        };
        DownloadFileDTO: {
            attempts: number;
            bytes_done: number;
            bytes_total: number;
            etag?: string | null;
            filename: string;
            id: string;
            last_error?: string | null;
            model_file_id: string;
            model_id: string;
            shard_index: number;
            shard_total: number;
            state: string;
        };
        DraftFlags: {
            ctx_size?: number | null;
            n_gpu_layers?: number | null;
            n_max?: number | null;
            n_min?: number | null;
            p_min?: number | null;
        };
        Error: {
            code: string;
            details?: Record<string, unknown>;
            message: string;
        };
        ErrorDetailDTO: {
            code: string;
            message: string;
        };
        ErrorEnvelope: {
            error: components["schemas"]["Error"];
        };
        EventDTO: {
            action: string;
            actor: string;
            at: string;
            category: string;
            detail: Record<string, unknown>;
            from_state?: string | null;
            id: string;
            level: string;
            message: string;
            subject_id?: string | null;
            subject_type?: string | null;
            to_state?: string | null;
        };
        FitBatchReportDTO: {
            recommended_file: string;
            reports: components["schemas"]["FitReportDTO"][];
        };
        FitBatchRequest: {
            files: string[];
            flags: components["schemas"]["FlagSet"];
            gpus?: string[];
            repo_id: string;
            reserve_bytes_per_gpu?: number;
            revision?: string;
        };
        FitDeviceDTO: {
            assigned_bytes: number;
            backend_overhead_bytes: number;
            extra_bytes: number;
            free_bytes?: number | null;
            index: number;
            kv_bytes: number;
            margin_bytes: number;
            name: string;
            ok: boolean;
            reserve_bytes: number;
            short_by_bytes: number;
            total_bytes?: number | null;
            uuid: string;
            weights_bytes: number;
        };
        FitEstimateRequest: {
            flags: components["schemas"]["FlagSet"];
            gpus?: string[];
            reserve_bytes_per_gpu?: number;
            source: components["schemas"]["FitSourceDTO"];
        };
        FitInputsDTO: {
            arch: string;
            flash_attn: boolean;
            head_dim_k: number;
            head_dim_v: number;
            kv_ctx: number;
            n_batch: number;
            n_ctx: number;
            n_embd: number;
            n_expert: number;
            n_expert_used: number;
            n_ff: number;
            n_head: number;
            n_head_kv: number[];
            n_layer: number;
            n_layer_swa: number;
            n_parallel: number;
            n_ubatch: number;
            n_vocab: number;
            type_k: string;
            type_v: string;
        };
        FitRecommendationDTO: {
            flash_attn: boolean;
            n_ctx?: number;
            n_gpu_layers: number;
            reason: string;
            type_k: string;
            type_v: string;
        };
        FitReportDTO: {
            backend_overhead_bytes: number;
            calibration_clamped: boolean;
            calibration_samples: number;
            compute_act_bytes: number;
            compute_attn_bytes: number;
            compute_bytes: number;
            compute_logits_bytes: number;
            compute_moe_bytes: number;
            confidence: string;
            file?: string;
            inputs: components["schemas"]["FitInputsDTO"];
            kv_bytes: number;
            kv_offloaded_bytes: number;
            kv_swa_bytes: number;
            margin_bytes: number;
            margin_bytes_per_gpu: number;
            max_ctx_at_full_offload: number;
            max_n_gpu_layers: number;
            model_id?: string;
            n_gpu_layers: number;
            notes: string[];
            per_gpu: components["schemas"]["FitDeviceDTO"][];
            per_slot_ctx: number;
            recommendation: components["schemas"]["FitRecommendationDTO"];
            required_vram_bytes: number;
            reserve_bytes: number;
            reserve_bytes_per_gpu: number;
            spill_to_ram_bytes: number;
            system_ram_free_bytes: number;
            system_ram_known: boolean;
            verdict: string;
            vram_unknown: boolean;
            weights_bytes: number;
            weights_offloaded_bytes: number;
        };
        FitSourceDTO: {
            file?: string;
            model_id?: string;
            repo_id?: string;
            revision?: string;
        };
        FlagSet: {
            alias?: string | null;
            batch_size?: number | null;
            cache_reuse?: number | null;
            cache_type_k?: string | null;
            cache_type_v?: string | null;
            chat_template?: string | null;
            chat_template_file?: string | null;
            cont_batching?: boolean | null;
            cpu_mask?: string | null;
            ctx_size?: number | null;
            defrag_thold?: number | null;
            device_filter?: string | null;
            device_uuids?: string[];
            draft?: components["schemas"]["DraftFlags"] | null;
            embedding?: boolean | null;
            flash_attn?: string | null;
            jinja?: boolean | null;
            log_verbosity?: number | null;
            main_gpu?: number | null;
            metrics_endpoint?: boolean | null;
            mlock?: boolean | null;
            n_gpu_layers?: components["schemas"]["NGpuLayers"] | null;
            n_keep?: number | null;
            n_predict?: number | null;
            no_mmap?: boolean | null;
            numa?: string | null;
            parallel?: number | null;
            pooling?: string | null;
            prio?: number | null;
            props_endpoint?: boolean | null;
            rerank?: boolean | null;
            rope_freq_base?: number | null;
            rope_freq_scale?: number | null;
            rope_scaling?: string | null;
            slot_save_path?: string | null;
            slots_endpoint?: boolean | null;
            split_mode?: string | null;
            tensor_split?: number[];
            threads?: number | null;
            threads_batch?: number | null;
            ubatch_size?: number | null;
            yarn_attn_factor?: number | null;
            yarn_ext_factor?: number | null;
        };
        GPUDTO: {
            compute_cap?: string | null;
            cuda?: string | null;
            driver: string;
            id: string;
            index: number;
            name: string;
            state: string;
            temp_c?: number | null;
            util_pct?: number | null;
            uuid: string;
            vram_free?: number | null;
            vram_total?: number | null;
            vram_used?: number | null;
        };
        GatewayDenialDTO: {
            count: number;
            day: string;
            instance_id: string;
            reason: string;
        };
        HFCardDTO: {
            html: string;
            markdown: string;
            repo_id: string;
            revision: string;
        };
        HFGGUFSummaryDTO: {
            architecture: string;
            context_length: number;
            total: number;
        };
        HFModelDTO: {
            author: string;
            disabled: boolean;
            downloads: number;
            gated: boolean;
            gguf?: components["schemas"]["HFGGUFSummaryDTO"] | null;
            id: string;
            last_modified?: string | null;
            likes: number;
            local_model_ids: Record<string, string>;
            private: boolean;
            sha: string;
            tags: string[];
        };
        HFPeekDTO: {
            arch: string;
            file: string;
            head_dim_k: number;
            head_dim_v: number;
            n_ctx_train: number;
            n_embd: number;
            n_expert: number;
            n_expert_used: number;
            n_head: number;
            n_head_kv: number[];
            n_layer: number;
            n_vocab: number;
            notes: string[];
            quantization: string;
            repo_id: string;
            size_bytes: number;
            swa_pattern?: number | null;
            swa_window?: number | null;
            tensor_summary: components["schemas"]["Sizes"];
            tokenizer_model: string;
        };
        HFSearchResultDTO: {
            author: string;
            downloads: number;
            gated: boolean;
            gguf?: components["schemas"]["HFGGUFSummaryDTO"] | null;
            id: string;
            likes: number;
            private: boolean;
            tags: string[];
            updated_at?: string | null;
        };
        HFTreeDTO: {
            groups: components["schemas"]["HFTreeGroupDTO"][];
            mmproj: components["schemas"]["HFTreeGroupDTO"][];
            repo_id: string;
            revision: string;
        };
        HFTreeEntryDTO: {
            lfs: boolean;
            oid: string;
            path: string;
            size_bytes: number;
        };
        HFTreeGroupDTO: {
            complete: boolean;
            files: components["schemas"]["HFTreeEntryDTO"][];
            key: string;
            local_model_id?: string | null;
            mmproj: boolean;
            quant_label: string;
            shard_total: number;
            total_bytes: number;
        };
        Health: {
            status: string;
            version: string;
        };
        InstallLlamacppRequest: {
            backend?: string;
            channel: string;
            cmake_extra?: string[];
            force_rebuild?: boolean;
            force_source?: boolean;
            git_ref?: string;
            git_url?: string;
            tag?: string;
        };
        InstallLlamacppResponse: {
            job_id?: string | null;
            reused: boolean;
            subject: components["schemas"]["SubjectDTO"];
            version: components["schemas"]["LlamacppVersionDTO"];
        };
        InstanceCommandDTO: {
            argv: string[];
            env: Record<string, string>;
            unit: string;
            unknown_flags: string[];
        };
        InstanceControlDTO: {
            desired_state: string;
            hint?: string | null;
            hints: string[];
            job_id?: string | null;
        };
        InstanceDTO: {
            auth_mode: string;
            autostart: boolean;
            config_hash: string;
            created_at: string;
            deleted_at?: string | null;
            description?: string | null;
            desired_state: string;
            display_name?: string | null;
            draft_model_id?: string | null;
            draft_unverified: boolean;
            draft_validation: string;
            extra_flags: string;
            flags: components["schemas"]["FlagSet"];
            generation: number;
            id: string;
            inhibit_reason?: string | null;
            inhibited: boolean;
            internal_port: number;
            mmproj_model_id?: string | null;
            model_id?: string | null;
            name: string;
            public_port: number;
            restart_max: number;
            restart_policy: string;
            restart_required: boolean;
            restart_window_sec: number;
            stale_version: boolean;
            status: components["schemas"]["InstanceStatusDTO"];
            unit_name: string;
            updated_at: string;
        };
        InstanceDetailDTO: {
            active_version_id: string;
            argv: string[];
            instance: components["schemas"]["InstanceDTO"];
            starts: components["schemas"]["InstanceStartDTO"][];
            unknown_flags: string[];
            warnings: components["schemas"]["WarningDTO"][];
        };
        InstanceLiveStatusDTO: {
            health_url: string;
            status: components["schemas"]["InstanceStatusDTO"];
            unit?: components["schemas"]["UnitLiveDTO"] | null;
        };
        InstanceRefDTO: {
            deleted: boolean;
            id: string;
            name: string;
            role: string;
        };
        InstanceStartDTO: {
            argv: string[];
            at: string;
            config_hash: string;
            detail: Record<string, unknown>;
            effective_config_hash?: string | null;
            ended_at?: string | null;
            error_code?: string | null;
            error_message?: string | null;
            exit_code?: number | null;
            id: string;
            llamacpp_version_id?: string | null;
            outcome?: string | null;
            override: Record<string, unknown>;
            ready_at?: string | null;
            trigger: string;
        };
        InstanceStatusDTO: {
            applied_config_hash?: string | null;
            ctx_size?: number | null;
            exe_version_id?: string | null;
            gpu_attribution: string;
            health_code?: number | null;
            last_change_at: string;
            last_error?: string | null;
            last_exit_code?: number | null;
            last_health_at?: string | null;
            main_pid?: number | null;
            ready_at?: string | null;
            requests_served?: number | null;
            rss_bytes?: number | null;
            slots_busy?: number | null;
            slots_total?: number | null;
            state: string;
            systemd_active?: string | null;
            systemd_result?: string | null;
            systemd_sub?: string | null;
            vram_bytes?: number | null;
        };
        InstanceUsageDTO: {
            items: components["schemas"]["InstanceUsageDayDTO"][];
            requests_served?: number | null;
            total: number;
        };
        InstanceUsageDayDTO: {
            auth_mode: string;
            bytes_in: number;
            bytes_out: number;
            day: string;
            duration_ms: number;
            errors: number;
            requests: number;
            source: string;
        };
        JobDTO: {
            attempts: number;
            cancel_requested: boolean;
            created_at: string;
            error_code?: string | null;
            error_message?: string | null;
            finished_at?: string | null;
            id: string;
            kind: string;
            lease_expires_at?: string | null;
            max_attempts: number;
            priority: number;
            progress: Record<string, unknown>;
            run_after: string;
            started_at?: string | null;
            state: string;
            subject: components["schemas"]["SubjectDTO"];
        };
        JobReceiptDTO: {
            job_id?: string | null;
            subject: components["schemas"]["SubjectDTO"];
        };
        JournalLineDTO: {
            at: string;
            message: string;
            priority?: number | null;
            unit?: string | null;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.APITokenDTO]": {
            items: components["schemas"]["APITokenDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.BenchResultRowDTO]": {
            items: components["schemas"]["BenchResultRowDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.BenchRunDTO]": {
            items: components["schemas"]["BenchRunDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.CacheRootDTO]": {
            items: components["schemas"]["CacheRootDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.DiskUsageDTO]": {
            items: components["schemas"]["DiskUsageDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.DownloadDTO]": {
            items: components["schemas"]["DownloadDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.EventDTO]": {
            items: components["schemas"]["EventDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.GPUDTO]": {
            items: components["schemas"]["GPUDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.GatewayDenialDTO]": {
            items: components["schemas"]["GatewayDenialDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.HFSearchResultDTO]": {
            items: components["schemas"]["HFSearchResultDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.InstanceDTO]": {
            items: components["schemas"]["InstanceDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.InstanceStartDTO]": {
            items: components["schemas"]["InstanceStartDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.JobDTO]": {
            items: components["schemas"]["JobDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.JournalLineDTO]": {
            items: components["schemas"]["JournalLineDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.LlamacppVersionDTO]": {
            items: components["schemas"]["LlamacppVersionDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.ModelDTO]": {
            items: components["schemas"]["ModelDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.NotificationDTO]": {
            items: components["schemas"]["NotificationDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.PresetDTO]": {
            items: components["schemas"]["PresetDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.SessionDTO]": {
            items: components["schemas"]["SessionDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.StrayFileDTO]": {
            items: components["schemas"]["StrayFileDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.TokenUsageDTO]": {
            items: components["schemas"]["TokenUsageDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.ToolchainCheckDTO]": {
            items: components["schemas"]["ToolchainCheckDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.UnitStatusDTO]": {
            items: components["schemas"]["UnitStatusDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        "List[github.com/jlbyh2o/llamaman/internal/api.UpdateReleaseDTO]": {
            items: components["schemas"]["UpdateReleaseDTO"][];
            next_cursor?: string | null;
            total: number;
        };
        LlamacppVersionDTO: {
            acquisition: string;
            activated_at?: string | null;
            backend: string;
            build_tag?: string | null;
            channel: string;
            created_at: string;
            error_code?: string | null;
            error_message?: string | null;
            failing_step?: string | null;
            finished_at?: string | null;
            git_ref?: string | null;
            git_url: string;
            id: string;
            in_use_by: string[];
            is_active: boolean;
            previous_active: boolean;
            resolved_commit?: string | null;
            size_bytes?: number | null;
            started_at?: string | null;
            state: string;
            superseded_by?: string | null;
            supports_fit: boolean;
            tag: string;
        };
        LlamacppVersionDetailDTO: {
            binaries?: string | null;
            build_options?: string | null;
            cuda_arch_list?: string | null;
            devices_output?: string | null;
            host_cpu_flags?: string | null;
            log_path?: string | null;
            version: components["schemas"]["LlamacppVersionDTO"];
        };
        LogPageDTO: {
            live: boolean;
            next_offset: number;
            offset: number;
            size: number;
            text: string;
            version_id: string;
        };
        LoginRequest: {
            password: string;
        };
        Meta: {
            claimed: boolean;
            commit: string;
            setup_complete: boolean;
            ui_port: number;
            version: string;
        };
        MetadataDTO: {
            kv: Record<string, unknown>;
            model_id: string;
        };
        ModelDTO: {
            arch?: string | null;
            bytes_on_disk: number;
            created_at: string;
            file_type?: string | null;
            gguf_parsed_at?: string | null;
            has_vision: boolean;
            head_dim_k?: number | null;
            head_dim_v?: number | null;
            id: string;
            in_use_by: components["schemas"]["InstanceRefDTO"][];
            kind: string;
            last_verified_at?: string | null;
            mmproj_auto: boolean;
            mmproj_model_id?: string | null;
            n_ctx_train?: number | null;
            n_embd?: number | null;
            n_expert?: number | null;
            n_expert_used?: number | null;
            n_ff?: number | null;
            n_head?: number | null;
            n_head_kv: unknown;
            n_layer?: number | null;
            n_vocab?: number | null;
            origin: string;
            path: string;
            primary_file: string;
            quant_label?: string | null;
            ref_name?: string | null;
            repo_id: string;
            revision: string;
            root_id: string;
            root_path: string;
            shard_count: number;
            snapshot_dir: string;
            state: string;
            swa_pattern?: number | null;
            swa_window?: number | null;
            tensor_summary: unknown;
            tokenizer_model?: string | null;
            total_bytes: number;
            updated_at: string;
        };
        ModelDetailDTO: {
            files: components["schemas"]["ModelFileDTO"][];
            mmproj?: components["schemas"]["ModelDTO"] | null;
            mmproj_candidates: components["schemas"]["ModelDTO"][];
            model: components["schemas"]["ModelDTO"];
        };
        ModelFileDTO: {
            blob_path?: string | null;
            bytes_on_disk: number;
            checksum_verified: boolean;
            etag?: string | null;
            filename: string;
            id: string;
            link_path?: string | null;
            shard_index: number;
            shard_total: number;
            size_bytes: number;
            state: string;
        };
        NGpuLayers: {
            count?: number | null;
            mode: string;
        };
        NotificationDTO: {
            code: string;
            created_at: string;
            dismissed_at?: string | null;
            hints: string[];
            id: string;
            message: string;
            severity: string;
            subject?: components["schemas"]["SubjectDTO"] | null;
            title: string;
        };
        PairMmprojRequest: {
            mmproj_model_id: string;
        };
        PasswordChangeRequest: {
            current: string;
            next: string;
        };
        PatchAPITokenRequest: {
            expires_at?: string | null;
            instance_ids?: string[];
            name?: string | null;
            rate_limit_rpm?: number | null;
            state?: string | null;
        };
        PatchBenchRunRequest: {
            name?: string;
            notes?: string | null;
        };
        PatchDownloadRequest: {
            priority: number;
        };
        PatchInstanceRequest: {
            auth_mode?: string | null;
            description?: string | null;
            display_name?: string | null;
            draft_model_id?: string | null;
            extra_flags?: string | null;
            flags?: components["schemas"]["FlagSet"] | null;
            generation: number;
            internal_port?: number | null;
            mmproj_model_id?: string | null;
            model_id?: string | null;
            name?: string | null;
            public_port?: number | null;
            restart_max?: number | null;
            restart_policy?: string | null;
            restart_window_sec?: number | null;
        };
        PatchInstanceResponse: {
            instance: components["schemas"]["InstanceDTO"];
            warnings: components["schemas"]["WarningDTO"][];
        };
        PatchSettingsDTO: {
            restart_keys: string[];
            restart_required: boolean;
            values: Record<string, unknown>;
        };
        PinNGLDTO: {
            instance: components["schemas"]["InstanceDTO"];
            pinned_layers: number;
            warnings: components["schemas"]["WarningDTO"][];
        };
        PlanDTO: {
            acquisition: string;
            asset_name?: string | null;
            backend: string;
            build_tag: string;
            can_proceed: boolean;
            channel: string;
            cuda_arch: string[];
            estimated_minutes: number;
            free_bytes: number;
            free_space_known: boolean;
            free_space_ok: boolean;
            missing_tools: string[];
            reason: string;
            required_bytes: number;
            tag: string;
            version_id: string;
        };
        PortSuggestionDTO: {
            kind: string;
            port: number;
        };
        PresetApplyDTO: {
            items: components["schemas"]["PresetApplyEntryDTO"][];
            total: number;
        };
        PresetApplyEntryDTO: {
            changed: string[];
            error?: components["schemas"]["ErrorDetailDTO"] | null;
            instance_id: string;
            name: string;
            restart_required: boolean;
        };
        PresetDTO: {
            builtin: boolean;
            created_at: string;
            description?: string | null;
            extra_flags: string;
            flags: components["schemas"]["FlagSet"];
            id: string;
            name: string;
            updated_at: string;
        };
        PresetFromInstanceRequest: {
            description?: string | null;
            extra_flags?: string | null;
            flags?: components["schemas"]["FlagSet"] | null;
            instance_id: string;
            name?: string | null;
        };
        PresetInput: {
            description?: string | null;
            extra_flags?: string | null;
            flags?: components["schemas"]["FlagSet"] | null;
            name?: string | null;
        };
        PromoteCacheRootResponse: {
            restart_required: boolean;
            root: components["schemas"]["CacheRootDTO"];
            scan?: components["schemas"]["JobReceiptDTO"] | null;
        };
        PutTokenRequest: {
            token: string;
        };
        RateLimitDTO: {
            authenticated: boolean;
            known: boolean;
            limit: number;
            remaining: number;
            reset_at: string;
        };
        ReleaseDTO: {
            asset_name?: string | null;
            asset_size?: number | null;
            body_html: string;
            body_markdown: string;
            installed: boolean;
            name: string;
            nightly_tag: string;
            prerelease: boolean;
            published_at: string;
            tag: string;
        };
        ReleaseListDTO: {
            channel: string;
            fetched_at?: string | null;
            rate_limit: components["schemas"]["RateLimitDTO"];
            releases: components["schemas"]["ReleaseDTO"][];
            stale: boolean;
        };
        ResetSettingsRequest: {
            keys: string[];
        };
        RestartDTO: {
            drain_sec: number;
            job_id?: string | null;
            listener_continuity: string;
            unit: string;
        };
        ScanRequest: {
            root_id?: string;
        };
        SelfUpdateDTO: {
            created_at: string;
            db_backup_path?: string | null;
            error_message?: string | null;
            finished_at?: string | null;
            from_version: string;
            id: string;
            signature_ok?: boolean | null;
            state: string;
            to_version: string;
        };
        SessionDTO: {
            created_at: string;
            current: boolean;
            expires_at: string;
            id: string;
            ip?: string | null;
            last_seen_at: string;
            user_agent?: string | null;
        };
        SessionStateDTO: {
            authenticated: boolean;
            claimed: boolean;
            expires_at?: string | null;
            setup_complete: boolean;
        };
        SettingDefDTO: {
            default: unknown;
            enum: string[];
            group: string;
            key: string;
            label: string;
            max?: number | null;
            min?: number | null;
            restart_required: boolean;
            type: string;
            unit_change_required: boolean;
        };
        SettingsDTO: {
            schema: components["schemas"]["SettingDefDTO"][];
            values: Record<string, unknown>;
        };
        SetupPasswordRequest: {
            password: string;
        };
        SetupSkipRequest: {
            step: string;
        };
        SetupStateDTO: {
            active_step?: string | null;
            claimed: boolean;
            complete: boolean;
            steps: components["schemas"]["WizardStepDTO"][];
            token_required: boolean;
        };
        Sizes: {
            by_type: components["schemas"]["TypeUsage"][];
            layer_bytes: number[];
            other_bytes: number;
            tensor_count: number;
            total_bytes: number;
        };
        StrayFileDTO: {
            dismissed_at?: string | null;
            first_seen_at: string;
            id: string;
            last_seen_at: string;
            path: string;
            reason: string;
            root_id: string;
            size_bytes: number;
        };
        SubjectDTO: {
            id: string;
            type: string;
        };
        SystemInfoDTO: {
            commit?: string | null;
            cpu_count: number;
            cpu_model: string;
            hf_home: string;
            identity: string;
            kernel: string;
            polkit_ok?: boolean | null;
            ram_free_bytes: number;
            ram_total_bytes: number;
            state_dir: string;
            systemd_control: string;
            systemd_scope: string;
            ui_port: number;
            ui_url: string;
            uptime_sec: number;
            version: string;
        };
        TokenDetailDTO: {
            token: components["schemas"]["APITokenDTO"];
            usage: components["schemas"]["TokenUsageDTO"][];
        };
        TokenStatusDTO: {
            hint: string;
            present: boolean;
            rate_limit?: components["schemas"]["RateLimitDTO"] | null;
            scopes: string[];
            user: string;
            valid?: boolean | null;
        };
        TokenUsageDTO: {
            bytes_in: number;
            bytes_out: number;
            completion_tokens?: number | null;
            day: string;
            duration_ms: number;
            errors: number;
            instance_id: string;
            prompt_tokens?: number | null;
            requests: number;
            token_id: string;
        };
        ToolchainCheckDTO: {
            docs_url?: string | null;
            found: boolean;
            min_version?: string | null;
            name: string;
            note?: string | null;
            ok: boolean;
            path?: string | null;
            version?: string | null;
        };
        TypeUsage: {
            bytes: number;
            elements: number;
            name: string;
            tensors: number;
            type: number;
        };
        UnitLiveDTO: {
            active_state: string;
            main_pid?: number | null;
            memory_bytes?: number | null;
            n_restarts: number;
            result: string;
            since_at?: string | null;
            sub_state: string;
        };
        UnitStatusDTO: {
            drift: string;
            installed_stamp?: number | null;
            masked_diff: string[];
            repair_command?: string | null;
            requires_diff: string[];
            template_stamp: number;
            unit: string;
            wants_diff: string[];
        };
        UpdatePendingDTO: {
            actor_active: boolean;
            from_version: string;
            self_update_id: string;
            staged_at: string;
            target_version: string;
        };
        UpdateReleaseDTO: {
            body_html: string;
            has_asset: boolean;
            name: string;
            newer: boolean;
            older: boolean;
            published_at?: string | null;
            same: boolean;
            tag: string;
        };
        UpdateStatusDTO: {
            current_version: string;
            in_flight?: components["schemas"]["SelfUpdateDTO"] | null;
            last_checked_at?: string | null;
            latest_version: string;
            pending?: components["schemas"]["UpdatePendingDTO"] | null;
            update_available: boolean;
        };
        UpstreamBodyDTO: {
            json: unknown;
            text?: string | null;
        };
        ValidateInstanceDTO: {
            argv: string[];
            draft_validation: string;
            fit?: components["schemas"]["FitReportDTO"] | null;
            unknown_flags: string[];
            warnings: components["schemas"]["WarningDTO"][];
        };
        ValidateInstanceRequest: {
            draft_model_id?: string | null;
            extra_flags?: string | null;
            flags?: components["schemas"]["FlagSet"] | null;
            instance_id?: string;
            mmproj_model_id?: string | null;
            model_id: string;
        };
        WarningDTO: {
            code: string;
            details?: Record<string, unknown>;
            message: string;
        };
        WizardStepDTO: {
            blocked: boolean;
            skippable: boolean;
            state: string;
            step: string;
        };
        body: {
            enabled: boolean;
        };
    };
    responses: never;
    parameters: never;
    requestBodies: never;
    headers: never;
    pathItems: never;
}

export interface operations {
    /** POST /api/v1/llamacpp/versions/{id}/activate — Make this build active: flip `is_active`, recompute every instance's `config_hash`, move the symlink, and optionally run the canary-gated rolling restart. */
    activateLlamacppVersion: {
        parameters: {
            query?: never;
            header?: {
                /** Optional replay key. A repeat within 10 minutes returns the original job with 200 instead of creating a second one; the same key with a different body inside the window is 422 idempotency_key_reused (D39/D65). */
                "Idempotency-Key"?: string;
            };
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["ActivateLlamacppRequest"];
        };
        };
        responses: {
            /** The activation was queued. Watch `job_id` for the canary roll. */
            "202": {
                content: {
                    "application/json": components["schemas"]["JobReceiptDTO"];
                };
            };
            /**
             * The Idempotency-Key header is not a short printable ASCII token.
             *
             * Error codes: idempotency_key_invalid
             */
            "400": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No version has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The version is not ready, another activation is already running, or a benchmark is live (section 6.6 step 1).
             *
             * Error codes: activation_in_flight, bench_in_flight, job_in_flight, version_not_ready
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * `restart_instances` is neither "none" nor "rolling".
             *
             * Error codes: bad_flags
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/cache/roots — Register an existing hub directory as scan-and-serve. A new root is never primary; promote it to make downloads land there. */
    addCacheRoot: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["AddCacheRootRequest"];
        };
        };
        responses: {
            /** The root was registered and a scan of it was queued. */
            "201": {
                content: {
                    "application/json": components["schemas"]["AddCacheRootResponse"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The path is not usable as a cache root: it is under a prefix the unit mounts read-only, it is not an absolute directory, or it is already registered.
             *
             * Error codes: root_path_protected, setting_invalid
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/presets/{id}/apply — Apply a preset to instances, key by key, and report the per-instance diff with each one's restart_required. */
    applyPreset: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["ApplyPresetRequest"];
        };
        };
        responses: {
            /** One entry per instance. A per-row refusal is reported inside the 200 rather than failing the whole apply. */
            "200": {
                content: {
                    "application/json": components["schemas"]["PresetApplyDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No preset has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/update/apply — Stage an update to any listed tag, newer or older. Four guard clauses, all evaluated inside the transaction that inserts the row and its job. */
    applyUpdate: {
        parameters: {
            query?: never;
            header?: {
                /** Optional replay key. A repeat within 10 minutes returns the original job with 200 instead of creating a second one; the same key with a different body inside the window is 422 idempotency_key_reused (D39/D65). */
                "Idempotency-Key"?: string;
            };
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["ApplyUpdateRequest"];
        };
        };
        responses: {
            /** An Idempotency-Key replay: the same job, not a second one. */
            "200": {
                content: {
                    "application/json": components["schemas"]["ApplyUpdateResponse"];
                };
            };
            /** The update was staged and its job created. */
            "202": {
                content: {
                    "application/json": components["schemas"]["ApplyUpdateResponse"];
                };
            };
            /**
             * The Idempotency-Key header is not a short printable ASCII token.
             *
             * Error codes: idempotency_key_invalid
             */
            "400": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * One of the four guard clauses refused: a build or another self-update is live, this host has no service manager, the swap actor cannot be summoned, or there is no working revert.
             *
             * Error codes: job_in_flight, revert_unavailable, selfupdate_unavailable, selfupdate_unsupported
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The Idempotency-Key was reused with a different body.
             *
             * Error codes: idempotency_key_reused
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/bench/preflight — What this sweep would do before it is committed: GPU conflicts, free VRAM, the point count, a duration estimate, and every FlagSet field llama-bench has no equivalent for. */
    benchPreflight: {
        parameters: {
            query: {
                /** The model to benchmark. */
                model_id: string;
                /** llama-bench's `-r`. */
                repetitions?: number;
                /** The sweep document as JSON, url-encoded. Absent means a single-point sweep, which is what the estimate for "just run it once" is. */
                sweep?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** What would happen, and what would be ignored. */
            "200": {
                content: {
                    "application/json": components["schemas"]["BenchPreflightDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No `model_id`, a model that does not exist, or a sweep that is malformed or expands past the point limit.
             *
             * Error codes: bad_flags, model_missing, sweep_too_large
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/bench/series — History for one model across llama.cpp versions, oldest first. */
    benchSeries: {
        parameters: {
            query?: {
                /** What each line is labeled by. Default llamacpp_tag. */
                group?: "flash_attn" | "llamacpp_backend" | "llamacpp_tag" | "model_label" | "n_batch" | "n_depth" | "n_gen" | "n_gpu_layers" | "n_prompt" | "n_threads" | "n_ubatch" | "quant_label" | "run_id" | "run_name" | "split_mode" | "tensor_split" | "test_kind" | "type_k" | "type_v";
                /** Maximum points to return. Default 200. */
                limit?: number;
                /** The measured value. Default avg_ts. */
                metric?: "avg_ns" | "avg_ts" | "stddev_ns" | "stddev_ts";
                /** Restrict to one model. */
                model_id?: string;
                /** The test shape to plot. */
                test?: "pp" | "tg" | "pp+tg";
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The history, oldest first. */
            "200": {
                content: {
                    "application/json": components["schemas"]["BenchSeriesDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * `metric`, `group` or `test` is not one this API can plot.
             *
             * Error codes: bad_flags
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/bench/runs/{id}/cancel — Stop a running sweep. The process group is signaled, the remaining points are marked `skipped`, and the stop-and-restore finalizer restarts every instance the run stopped. */
    cancelBenchRun: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The cancel was requested. The sweep stops after the current point and the instances it stopped are restarted. */
            "202": {
                content: {
                    "application/json": components["schemas"]["JobReceiptDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No run has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No job for this run is live, so there is nothing to cancel.
             *
             * Error codes: bench_not_cancelable
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/downloads/{id}/cancel — Cancel a download. The partial files are kept by default, so a retry resumes rather than starting over. */
    cancelDownload: {
        parameters: {
            query?: {
                /** Default true. False removes each `.incomplete` file as the transfer that owns it releases its handle. */
                keep_partial?: boolean;
            };
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The canceled download. */
            "202": {
                content: {
                    "application/json": components["schemas"]["DownloadDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No download has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The download has already finished.
             *
             * Error codes: download_not_pausable
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/jobs/{id}/cancel — Request cancellation. Two kinds carry a cut-off rather than a blanket accept: `llamacpp_activate` before its step-3 commit, and `self_update` before the `staged` commit. */
    cancelJob: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The cancel was requested. A job that is not running moves to `canceled` immediately; a running one stops at its next checkpoint. */
            "202": {
                content: {
                    "application/json": components["schemas"]["JobDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No job has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The job is past its cut-off. A `self_update` at or after the `staged` commit answers `selfupdate_not_cancelable`: the marker is on disk and the swap belongs to the service manager (D96).
             *
             * Error codes: job_not_cancelable, selfupdate_not_cancelable
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/llamacpp/versions/{id}/cancel — Stop a build that is running. Section 2.5's cancel edge: the process group is signaled and the partial directories are removed by the worker. */
    cancelLlamacppVersion: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The cancel was requested. The build stops at its next checkpoint and the job reports the outcome. */
            "202": {
                content: {
                    "application/json": components["schemas"]["JobReceiptDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No version has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No job for this version is running, so there is nothing to cancel.
             *
             * Error codes: build_not_cancelable
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/auth/password — Change the admin password; every other session is revoked. */
    changePassword: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["PasswordChangeRequest"];
        };
        };
        responses: {
            /** The password was changed and every other session was revoked. */
            "204": {
                content?: never;
            };
            /**
             * The new password does not meet the minimum length.
             *
             * Error codes: password_invalid
             */
            "400": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The current password is wrong.
             *
             * Error codes: bad_credentials
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/update/check — Refresh the release feed. */
    checkUpdates: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The refresh was accepted; the result lands in the release listing. */
            "202": {
                content?: never;
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/bench/compare — Chart-ready series across runs: one grouped query over `bench_points ⋈ bench_results` with the sweep axes as columns. */
    compareBenchRuns: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["CompareBenchRequest"];
        };
        };
        responses: {
            /** The aggregated series. */
            "200": {
                content: {
                    "application/json": components["schemas"]["BenchCompareDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * `x`, `series` or a filter key is not a comparable axis, or `y` is not a measured metric.
             *
             * Error codes: bad_flags
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/setup/complete — Finish the wizard. Refused while a non-skippable step is unfinished. */
    completeSetup: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The wizard is complete. */
            "204": {
                content?: never;
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * A non-skippable step is still unfinished.
             *
             * Error codes: wizard_step_locked
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/tokens — Mint a token. The 201 is the only response in this API that ever contains the secret; it is stored as a sha256 and cannot be shown again. */
    createAPIToken: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["CreateAPITokenRequest"];
        };
        };
        responses: {
            /** The token, with its secret. Shown once. */
            "201": {
                content: {
                    "application/json": components["schemas"]["CreateAPITokenResponse"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The token was refused: no name, a scope that is not one of global/instances, an `instances` scope naming no instance, or a negative rate limit.
             *
             * Error codes: bad_request, token_name_required, token_rate_limit_invalid, token_scope_invalid
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/bench/runs — Expand a sweep into its points and queue it. The cross-product becomes `bench_points` rows BEFORE anything executes, which is what makes progress and resume exact. */
    createBenchRun: {
        parameters: {
            query?: never;
            header?: {
                /** Optional replay key. A repeat within 10 minutes returns the original job with 200 instead of creating a second one; the same key with a different body inside the window is 422 idempotency_key_reused (D39/D65). */
                "Idempotency-Key"?: string;
            };
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["CreateBenchRunRequest"];
        };
        };
        responses: {
            /** An `Idempotency-Key` replay inside its window. */
            "200": {
                content: {
                    "application/json": components["schemas"]["CreateBenchRunResponse"];
                };
            };
            /** `draft: true` — the run and its points exist, and nothing was queued. */
            "201": {
                content: {
                    "application/json": components["schemas"]["CreateBenchRunResponse"];
                };
            };
            /** The sweep was expanded and queued. Watch `job_id`. */
            "202": {
                content: {
                    "application/json": components["schemas"]["CreateBenchRunResponse"];
                };
            };
            /**
             * The Idempotency-Key header is not a short printable ASCII token.
             *
             * Error codes: idempotency_key_invalid
             */
            "400": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * Instances are loaded on the GPUs this benchmark would use and `on_conflict` is `abort`; `details.instances` names them, including any included because per-GPU attribution was unavailable. Or no llama.cpp build is active, so there is no llama-bench to run.
             *
             * Error codes: bench_gpu_conflict, bench_no_runtime, job_in_flight
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The sweep expands past the point limit, an axis is malformed, `on_conflict` is not one of the two policies, or the model has no file on disk to benchmark.
             *
             * Error codes: bad_flags, extra_flag_forbidden, idempotency_key_reused, model_missing, sweep_too_large
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/downloads — Queue a model download. The jobs, downloads, models and model_files rows are written in one transaction, so the job is the receipt. */
    createDownload: {
        parameters: {
            query?: never;
            header?: {
                /** Optional replay key. A repeat within 10 minutes returns the original job with 200 instead of creating a second one; the same key with a different body inside the window is 422 idempotency_key_reused (D39/D65). */
                "Idempotency-Key"?: string;
            };
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["CreateDownloadRequest"];
        };
        };
        responses: {
            /** An `Idempotency-Key` replay inside its window: the original download. */
            "200": {
                content: {
                    "application/json": components["schemas"]["CreateDownloadResponse"];
                };
            };
            /** The download was queued. Watch `job_id`. */
            "202": {
                content: {
                    "application/json": components["schemas"]["CreateDownloadResponse"];
                };
            };
            /**
             * The Idempotency-Key header is not a short printable ASCII token.
             *
             * Error codes: idempotency_key_invalid
             */
            "400": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The repository is gated. `details` carries `repo` and `request_url`; access grants are browser-only on the Hub's side, so the UI links out.
             *
             * Error codes: hf_gated, hf_private
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This model is already being downloaded (`details.download_id` names it), or the target filesystem cannot hold it (`details` carries the numbers).
             *
             * Error codes: download_exists, insufficient_disk, job_in_flight
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The request names files this repository does not hold at this revision, a shard set the repository holds only part of, more than one quantization, or a projector choice that is ambiguous.
             *
             * Error codes: file_not_in_repo, mmproj_ambiguous, multiple_quants, no_files_selected, shard_set_incomplete
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The Hugging Face Hub could not be reached.
             *
             * Error codes: hf_unreachable
             */
            "502": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This daemon has no primary cache root yet.
             *
             * Error codes: no_cache_root
             */
            "503": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/instances — Create an instance. Ports are auto-allocated when omitted and validated against the section 2.8 rules. */
    createInstance: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["CreateInstanceRequest"];
        };
        };
        responses: {
            /** The instance was created. `warnings` may carry a deferred draft check. */
            "201": {
                content: {
                    "application/json": components["schemas"]["CreateInstanceResponse"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * Another live instance already has this name.
             *
             * Error codes: instance_name_taken
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The configuration was refused: a name that is not a legal unit id, a port that breaks one of the section 2.8 rules, a draft model whose vocabulary differs, `-ngl auto` with an explicit tensor split, an `extra_flags` override of a flag the renderer owns, or a model that does not exist.
             *
             * Error codes: bad_flags, draft_vocab_mismatch, extra_flag_forbidden, instance_name_invalid, model_missing, ngl_auto_conflict, port_unavailable
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/presets — Save a FlagSet and extra_flags under a name. */
    createPreset: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["PresetInput"];
        };
        };
        responses: {
            /** The saved preset. */
            "201": {
                content: {
                    "application/json": components["schemas"]["PresetDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * Another preset already has this name.
             *
             * Error codes: instance_name_taken
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The name is empty, or the FlagSet fails section 5.7's rules.
             *
             * Error codes: bad_flags
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/presets/from-instance — Capture an instance's FlagSet and extra_flags as a new preset. The instance is named in the body rather than in the path. */
    createPresetFromInstance: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["PresetFromInstanceRequest"];
        };
        };
        responses: {
            /** The captured preset. */
            "201": {
                content: {
                    "application/json": components["schemas"]["PresetDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No instance has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * Another preset already has this name.
             *
             * Error codes: instance_name_taken
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** DELETE /api/v1/tokens/{id} — Revoke. Soft and terminal: the row and its hash are retained so the secret can never become valid again. */
    deleteAPIToken: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The token is revoked. Revoking one twice is a no-op, not an error. */
            "204": {
                content?: never;
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No token has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** DELETE /api/v1/bench/runs/{id} — Delete a run and its points and results. Refused while its job is live or while it still owes stopped instances a restart. */
    deleteBenchRun: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The run is gone. */
            "204": {
                content?: never;
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No run has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The run's job is live, or it stopped production instances it has not restarted yet — deleting the row would lose the list the boot restore reads.
             *
             * Error codes: bench_running
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** DELETE /api/v1/github/token — Forget the stored GitHub token. The client that used it reverts to anonymous. */
    deleteGitHubToken: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The GitHub token is gone. */
            "204": {
                content?: never;
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** DELETE /api/v1/hf/token — Forget the stored Hugging Face token. The client that used it reverts to anonymous. */
    deleteHFToken: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The HF token is gone. */
            "204": {
                content?: never;
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** DELETE /api/v1/instances/{id} — Soft delete: stop and disable the unit, close the listener, stamp deleted_at, keep every row. ?purge=true is the explicit hard delete. */
    deleteInstance: {
        parameters: {
            query?: {
                /** Leave this instance's `token_instances` scope rows in place. */
                keep_tokens?: boolean;
                /** Hard delete: the row and all of its history and accounting cascade away. That history is the one thing in this system that cannot be recomputed. */
                purge?: boolean;
            };
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The instance was deleted. `hints` carries any command the daemon could not run itself. */
            "202": {
                content: {
                    "application/json": components["schemas"]["DeleteInstanceResponse"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No instance has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** DELETE /api/v1/llamacpp/versions/{id} — Remove a build's directory. Refused for the active build, for the rollback target, and for any directory a live process is executing out of (D25). */
    deleteLlamacppVersion: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The deletion was queued. */
            "202": {
                content: {
                    "application/json": components["schemas"]["JobReceiptDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No version has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The version is active, is the retained rollback target, or a live process is still executing out of its directory.
             *
             * Error codes: job_in_flight, version_active, version_in_use, version_is_rollback_target
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** DELETE /api/v1/models/{id} — Execute the delete preview. The row moves deleting → deleted and is kept; no SQL DELETE is ever issued against a model row. */
    deleteModel: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The delete was accepted; `plan` is what the job is executing. */
            "202": {
                content: {
                    "application/json": components["schemas"]["DeleteModelResponse"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No model has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * Instances still use this model, or a job already holds it. `details.instances` names them; a soft-deleted instance is never one of them.
             *
             * Error codes: job_in_flight, model_in_use
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** DELETE /api/v1/presets/{id} — Remove a preset. Built-ins refuse. */
    deletePreset: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The preset is gone. */
            "204": {
                content?: never;
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No preset has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This is a built-in preset.
             *
             * Error codes: conflict_generation
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** DELETE /api/v1/cache/strays/{id} — Forget a stray, and optionally remove the file it names. */
    deleteStray: {
        parameters: {
            query?: {
                /** Also unlink the file. Refused for a path outside the cache root that reported it. */
                delete_file?: boolean;
            };
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The stray was removed. */
            "204": {
                content?: never;
            };
            /**
             * The file is not inside the cache root that reported it.
             *
             * Error codes: setting_invalid
             */
            "400": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No stray has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** DELETE /api/v1/cache/roots/{id} — Detach a root: its catalog rows are removed and no file is touched. Refused while ANY instance references one of its models, soft-deleted ones included. */
    detachCacheRoot: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The root was detached. */
            "204": {
                content?: never;
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No cache root has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The root is primary, or instances still reference its models. `details.instances` marks which of them are soft-deleted; purging those instances is the stated remedy.
             *
             * Error codes: model_in_use, root_is_primary
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/cache/strays/{id}/dismiss — Hide a stray from the list without removing anything. */
    dismissStray: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The stray was dismissed. */
            "204": {
                content?: never;
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No stray has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/system/notifications/{id}/dismiss — Clear one card. The row is stamped, not deleted. */
    dismissSystemNotification: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The card was dismissed. */
            "204": {
                content?: never;
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No notification has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/system/diagnostics — The D50 support bundle: configuration, unit files, the journal tail and the toolchain report, redacted of every secret. */
    downloadDiagnostics: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The archive, as a download. */
            "200": {
                content: {
                    "application/gzip": components["schemas"]["DiagnosticsDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/fit/estimate — Will this model and these flags run on these GPUs, and where does the memory go: per-GPU placement, the KV and compute breakdown, the `-ngl auto` advisory, and a recommendation (section 8). */
    estimateFit: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["FitEstimateRequest"];
        };
        };
        responses: {
            /** The estimate. `verdict` is exactly `∀ g: per_gpu[g].ok`. */
            "200": {
                content: {
                    "application/json": components["schemas"]["FitReportDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * `source.model_id` names no model on this host.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The source cannot be measured yet: its GGUF header has not been parsed, or the Hub would not serve one.
             *
             * Error codes: fit_unavailable
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The source names neither a model nor a repository file, or the flags are invalid.
             *
             * Error codes: bad_flags
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/fit/estimate-batch — One report per quantization of a repository, plus the largest one that fits — this is what drives the quant picker (section 3.9). */
    estimateFitBatch: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["FitBatchRequest"];
        };
        };
        responses: {
            /** One report per file, in the order they were asked for. */
            "200": {
                content: {
                    "application/json": components["schemas"]["FitBatchReportDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No repository, no files, or invalid flags.
             *
             * Error codes: bad_flags
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This build has no Hugging Face client, so a remote peek is impossible.
             *
             * Error codes: internal_error
             */
            "503": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/bench/runs/{id}/export — Export a run: `json` (run + points + results, self-describing), `csv` (one row per result), or `md` (a provenance header plus a table, ready to paste into an issue). Each carries a `Content-Disposition` filename. */
    exportBenchRun: {
        parameters: {
            query?: {
                /** json, csv or md. Default json. */
                format?: "json" | "csv" | "md";
            };
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The export, in the requested format, with a `Content-Disposition` filename derived from the run's name. */
            "200": {
                content: {
                    "application/json": string;
                    "text/csv": string;
                    "text/markdown": string;
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No run has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * `?format=` is not one of json, csv, md.
             *
             * Error codes: bad_flags
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/llamacpp/active — The active build, with its build options and the devices it can see. */
    getActiveLlamacpp: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The active build. */
            "200": {
                content: {
                    "application/json": components["schemas"]["LlamacppVersionDetailDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No build is active. This is the ordinary state on a fresh install, not an error condition of this endpoint.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/tokens/{id} — One token, with its per-instance usage summary. */
    getAPIToken: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The token and its usage. */
            "200": {
                content: {
                    "application/json": components["schemas"]["TokenDetailDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No token has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/tokens/{id}/usage — This token's daily requests, errors, bytes and reported token counts, per instance. */
    getAPITokenUsage: {
        parameters: {
            query?: {
                /** Inclusive first day, YYYY-MM-DD UTC. */
                from?: string;
                /** How the client intends to fold the rows. The server returns the same per-day, per-instance rows either way; this is a hint the UI echoes. */
                group?: "day" | "instance";
                /** Inclusive last day, YYYY-MM-DD UTC. */
                to?: string;
            };
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** One row per day per instance. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.TokenUsageDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No token has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/auth/session — Whether this request carries a session, whether the host has been claimed, whether the wizard has finished, and when the session expires. */
    getAuthSession: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The session state of this request. */
            "200": {
                content: {
                    "application/json": components["schemas"]["SessionStateDTO"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/bench/runs/{id} — One run: its points, the captured environment, and the live job's `{points_done, points_total, current}` progress. */
    getBenchRun: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The run. */
            "200": {
                content: {
                    "application/json": components["schemas"]["BenchRunDetailDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No run has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/bench/runs/{id}/results — The run's results, flattened one row per measurement with all axes as columns. */
    getBenchRunResults: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The flattened results, in point order. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.BenchResultRowDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No run has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/cache/scans/{id} — One scan's progress and results. */
    getCacheScan: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The scan. */
            "200": {
                content: {
                    "application/json": components["schemas"]["CacheScanDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No scan has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/downloads/{id} — One download with its per-file progress. */
    getDownload: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The download. */
            "200": {
                content: {
                    "application/json": components["schemas"]["DownloadDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No download has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/github/token — Whether a GitHub token is stored, its masked hint, and what the last validation said. Never the token. */
    getGitHubToken: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The GitHub token's status. */
            "200": {
                content: {
                    "application/json": components["schemas"]["TokenStatusDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /healthz — Liveness only, no state. */
    getHealth: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The daemon is alive. */
            "200": {
                content: {
                    "application/json": components["schemas"]["Health"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/hf/card/{repo} — The model card, RENDERED AND SANITIZED SERVER-SIDE (D35), plus the raw markdown for a "view source" toggle. */
    getHFCard: {
        parameters: {
            query?: {
                /** Branch, tag or commit. Empty means `main`. */
                revision?: string;
            };
            header?: never;
            path: {
                /** May contain `/` — this is a multi-segment path parameter (a Hugging Face repo id such as `bartowski/Qwen3-8B-GGUF`), sent unescaped. */
                repo: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The card. A repository with no README answers with empty strings, not a 404: plenty of good repositories have none. */
            "200": {
                content: {
                    "application/json": components["schemas"]["HFCardDTO"];
                };
            };
            /**
             * The path did not name a repository id — `{repo...}` matches any number of segments, and only `name` or `org/name` is one — or, on the peek route, `?file=` was not given.
             *
             * Error codes: bad_request
             */
            "400": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The repository is gated, or private and unreachable with this host's credentials. A gated body carries `repo` and `request_url`; the UI links out, because access grants are browser-only on the Hub's side.
             *
             * Error codes: hf_gated, hf_private
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No such repository, revision or file.
             *
             * Error codes: file_not_in_repo
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The Hub could not be reached, or answered something unusable.
             *
             * Error codes: hf_unreachable
             */
            "502": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/hf/model/{repo} — One repository's metadata, its `gguf` summary, its gated flag and this host's local-availability annotations. */
    getHFModel: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** May contain `/` — this is a multi-segment path parameter (a Hugging Face repo id such as `bartowski/Qwen3-8B-GGUF`), sent unescaped. */
                repo: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The repository. */
            "200": {
                content: {
                    "application/json": components["schemas"]["HFModelDTO"];
                };
            };
            /**
             * The path did not name a repository id — `{repo...}` matches any number of segments, and only `name` or `org/name` is one — or, on the peek route, `?file=` was not given.
             *
             * Error codes: bad_request
             */
            "400": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The repository is gated, or private and unreachable with this host's credentials. A gated body carries `repo` and `request_url`; the UI links out, because access grants are browser-only on the Hub's side.
             *
             * Error codes: hf_gated, hf_private
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No such repository, revision or file.
             *
             * Error codes: file_not_in_repo
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The Hub could not be reached, or answered something unusable.
             *
             * Error codes: hf_unreachable
             */
            "502": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/hf/token — Whether a Hugging Face token is stored, its masked hint, and what the last validation said. Never the token. */
    getHFToken: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The HF token's status. */
            "200": {
                content: {
                    "application/json": components["schemas"]["TokenStatusDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/hf/tree/{repo} — The file tree grouped by quantization, with TRUE `lfs.size` totals, shard groups, mmproj candidates and `local_model_id`. */
    getHFTree: {
        parameters: {
            query?: {
                /** Branch, tag or commit. Empty means `main`. */
                revision?: string;
            };
            header?: never;
            path: {
                /** May contain `/` — this is a multi-segment path parameter (a Hugging Face repo id such as `bartowski/Qwen3-8B-GGUF`), sent unescaped. */
                repo: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The repository's downloadable groups. */
            "200": {
                content: {
                    "application/json": components["schemas"]["HFTreeDTO"];
                };
            };
            /**
             * The path did not name a repository id — `{repo...}` matches any number of segments, and only `name` or `org/name` is one — or, on the peek route, `?file=` was not given.
             *
             * Error codes: bad_request
             */
            "400": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The repository is gated, or private and unreachable with this host's credentials. A gated body carries `repo` and `request_url`; the UI links out, because access grants are browser-only on the Hub's side.
             *
             * Error codes: hf_gated, hf_private
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No such repository, revision or file.
             *
             * Error codes: file_not_in_repo
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The Hub could not be reached, or answered something unusable.
             *
             * Error codes: hf_unreachable
             */
            "502": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/instances/{id} — One instance, with its rendered argv and its last five starts. */
    getInstance: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The instance. */
            "200": {
                content: {
                    "application/json": components["schemas"]["InstanceDetailDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No instance has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/instances/{id}/command — The argv, environment and unit name — copyable and auditable. */
    getInstanceCommand: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The rendered command. */
            "200": {
                content: {
                    "application/json": components["schemas"]["InstanceCommandDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No instance has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/instances/{id}/logs — The unit's journal. SSE when Accept: text/event-stream. */
    getInstanceLogs: {
        parameters: {
            query?: {
                /** Ask for the live tail. */
                follow?: boolean;
                /** How many entries. */
                lines?: number;
                /** A journald cursor or timestamp to resume from. */
                since?: string;
            };
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The requested entries. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.JournalLineDTO]"];
                    "text/event-stream": string;
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No instance has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This daemon's identity cannot read the journal (D77).
             *
             * Error codes: journal_unavailable
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/instances/{id}/metrics — llama-server's /metrics, proxied. */
    getInstanceMetrics: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The upstream answer, wrapped. */
            "200": {
                content: {
                    "application/json": components["schemas"]["UpstreamBodyDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No instance has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The instance is not running, so there is no upstream to ask.
             *
             * Error codes: systemd_unavailable
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/instances/{id}/props — llama-server's /props, proxied. */
    getInstanceProps: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The upstream answer, wrapped. */
            "200": {
                content: {
                    "application/json": components["schemas"]["UpstreamBodyDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No instance has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The instance is not running, so there is no upstream to ask.
             *
             * Error codes: systemd_unavailable
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/instances/{id}/slots — llama-server's /slots, proxied. */
    getInstanceSlots: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The upstream answer, wrapped. */
            "200": {
                content: {
                    "application/json": components["schemas"]["UpstreamBodyDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No instance has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The instance is not running, so there is no upstream to ask.
             *
             * Error codes: systemd_unavailable
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/instances/{id}/status — The instance_status row plus a live read of the unit and the health probe. */
    getInstanceStatus: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** Stored and observed status. */
            "200": {
                content: {
                    "application/json": components["schemas"]["InstanceLiveStatusDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No instance has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/instances/{id}/usage — instance_usage_daily rows for both auth modes, plus llama-server's own counter, each labeled with its source. */
    getInstanceUsage: {
        parameters: {
            query?: {
                /** Inclusive first day, as YYYY-MM-DD. */
                from?: string;
                /** Inclusive last day, as YYYY-MM-DD. */
                to?: string;
            };
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The daily rollups. */
            "200": {
                content: {
                    "application/json": components["schemas"]["InstanceUsageDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No instance has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/jobs/{id} — One job: state, progress, error. */
    getJob: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The job. */
            "200": {
                content: {
                    "application/json": components["schemas"]["JobDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No job has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/llamacpp/versions/{id} — One version, including its build options, failing step and device list. */
    getLlamacppVersion: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The version. */
            "200": {
                content: {
                    "application/json": components["schemas"]["LlamacppVersionDetailDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No version has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/llamacpp/versions/{id}/log — The build log: `?offset=&limit=` plain text by default, the JSON envelope with `Accept: application/json`, and a live SSE tail with `Accept: text/event-stream`. */
    getLlamacppVersionLog: {
        parameters: {
            query?: {
                /** Maximum bytes to return. Default 262144, maximum 4194304. */
                limit?: number;
                /** Byte offset to read from. The previous page's `next_offset`, or 0 for the beginning. */
                offset?: number;
                /** On the SSE stream, how many buffered lines to replay before following. Default 200, capped by the 5000-line ring section 6.5 keeps. */
                tail?: number;
            };
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** A page of the build log as plain text, the same page as JSON when `Accept: application/json`, or a live `event: line` stream when `Accept: text/event-stream`. */
            "200": {
                content: {
                    "application/json": components["schemas"]["LogPageDTO"];
                    "text/event-stream": string;
                    "text/plain": string;
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No version has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/meta — Version, claim state and the port the management listener landed on — what install.sh polls. */
    getMeta: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** This daemon's identity and claim state. */
            "200": {
                content: {
                    "application/json": components["schemas"]["Meta"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The daemon is up but has not finished resolving its own identity.
             *
             * Error codes: internal_error
             */
            "503": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/models/{id} — One model with its files, its paired projector and the projector picker. */
    getModel: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The model. */
            "200": {
                content: {
                    "application/json": components["schemas"]["ModelDetailDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No model has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/models/{id}/metadata — The full GGUF key/value map, re-read from the file so a scan never has to retain a tokenizer table it will be asked about once. */
    getModelMetadata: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The metadata table. */
            "200": {
                content: {
                    "application/json": components["schemas"]["MetadataDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No model has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The file is not readable and no metadata was recorded.
             *
             * Error codes: model_missing
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/presets/{id} — One preset. */
    getPreset: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The preset. */
            "200": {
                content: {
                    "application/json": components["schemas"]["PresetDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No preset has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/settings — Every registered key's current value, plus the schema the form is generated from. Secrets are never here — they have their own validating endpoints. */
    getSettings: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The values and their schema. */
            "200": {
                content: {
                    "application/json": components["schemas"]["SettingsDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/setup/state — Claim state, whether this caller needs a setup token, and where the wizard stands. */
    getSetupState: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The wizard's state for this caller. */
            "200": {
                content: {
                    "application/json": components["schemas"]["SetupStateDTO"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/system/capabilities — The single object the UI reads to decide which controls to disable and which explanatory copy to show, so it never has to guess. */
    getSystemCapabilities: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** What this host lets the daemon do, and which degraded modes are in effect. */
            "200": {
                content: {
                    "application/json": components["schemas"]["CapabilitiesDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/system/disk — Total, free and used per cache root and for the state directory. */
    getSystemDisk: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** One row per filesystem of interest. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.DiskUsageDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/system/info — Version, uptime, service identity, systemd facts, ports, paths and host hardware. */
    getSystemInfo: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** This daemon and this host. */
            "200": {
                content: {
                    "application/json": components["schemas"]["SystemInfoDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/system/journal — The journal tail for a unit. SSE when Accept: text/event-stream. */
    getSystemJournal: {
        parameters: {
            query?: {
                /** How many entries to return. */
                lines?: number;
                /** The unit to read; defaults to the daemon's own. */
                unit?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The requested entries. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.JournalLineDTO]"];
                    "text/event-stream": string;
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This daemon's identity cannot read the journal (D77). An empty stream and a denied one must not look alike, so this is refused rather than answered with nothing; `details.hints` names the group to add.
             *
             * Error codes: journal_unavailable
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/system/toolchain — The latest probe: per-tool found/version/verdict with fix guidance. */
    getSystemToolchain: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** One row per tool. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.ToolchainCheckDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/update/status — Current version, latest known, the in-flight row, and the one self-update fact the confirmation gate last computed. */
    getUpdateStatus: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** This host's update state. */
            "200": {
                content: {
                    "application/json": components["schemas"]["UpdateStatusDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/llamacpp/versions — Install a llama.cpp build. The request resolves to a three-part id before anything is inserted, and then takes exactly one of D71's five branches. */
    installLlamacppVersion: {
        parameters: {
            query?: never;
            header?: {
                /** Optional replay key. A repeat within 10 minutes returns the original job with 200 instead of creating a second one; the same key with a different body inside the window is 422 idempotency_key_reused (D39/D65). */
                "Idempotency-Key"?: string;
            };
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["InstallLlamacppRequest"];
        };
        };
        responses: {
            /** Already installed with these options (`reused`), or an `Idempotency-Key` replay inside its window. */
            "200": {
                content: {
                    "application/json": components["schemas"]["InstallLlamacppResponse"];
                };
            };
            /** The build was queued. Watch `job_id`. */
            "202": {
                content: {
                    "application/json": components["schemas"]["InstallLlamacppResponse"];
                };
            };
            /**
             * The Idempotency-Key header is not a short printable ASCII token.
             *
             * Error codes: idempotency_key_invalid
             */
            "400": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This id is already being built, or it is installed with different build options, or a live process is executing out of the directory a forced rebuild would replace.
             *
             * Error codes: build_in_flight, version_in_use, version_options_differ
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The request could not be resolved to a version: an unknown channel or backend, a custom build with no usable git ref, or a channel lookup that failed.
             *
             * Error codes: bad_flags, idempotency_key_reused, resolve_failed
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/tokens — Every API token: prefix, scope, state, counts and last use. Never a secret — none is stored. */
    listAPITokens: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** Every token this host has minted, revoked ones included. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.APITokenDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/bench/runs — Every benchmark run, newest first, with its summary counters. */
    listBenchRuns: {
        parameters: {
            query?: {
                /** Maximum runs to return. Default 200. */
                limit?: number;
                /** Keep only runs against this model. */
                model_id?: string;
                /** Keep only runs in this state. */
                state?: "draft" | "queued" | "preflight" | "running" | "succeeded" | "partial" | "failed" | "canceled";
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The runs, newest first. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.BenchRunDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/cache/roots — Every known hub directory, primary first. */
    listCacheRoots: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The cache roots. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.CacheRootDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/downloads — Downloads with their per-file progress. */
    listDownloads: {
        parameters: {
            query?: {
                /** `active` is everything unfinished, `paused` included; `all` is the whole table, which is the receipt for what has landed on this disk. */
                state?: "active" | "all";
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The downloads. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.DownloadDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/events/log — The durable event log, newest first, paged backwards on the ULID cursor. */
    listEvents: {
        parameters: {
            query?: {
                /** Return only rows older than this event id. It is the last item's ULID from the previous page — ids sort by creation, which is what makes keyset paging possible here at all. */
                before?: string;
                /** Restrict to one category. */
                category?: "llamacpp" | "model" | "download" | "instance" | "token" | "bench" | "auth" | "update" | "system" | "gateway";
                /** Maximum rows to return. */
                limit?: string;
                /** Restrict to one subject's history. */
                subject_id?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The matching events, newest first. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.EventDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * `?category=` named something that is not an event category.
             *
             * Error codes: query_invalid
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/gateway/denials — Denial counters per instance and reason (section 2.9). */
    listGatewayDenials: {
        parameters: {
            query?: {
                /** Inclusive first day, YYYY-MM-DD UTC. */
                from?: string;
                /** Restrict to one instance. */
                instance_id?: string;
                /** Inclusive last day, YYYY-MM-DD UTC. */
                to?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** One row per instance, day and reason. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.GatewayDenialDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/instances — Config joined with status, plus the four derived flags. Soft-deleted instances are excluded unless ?include_deleted=true. */
    listInstances: {
        parameters: {
            query?: {
                /** Include soft-deleted instances. They keep their start history and accounting but hold neither their name nor their ports (D68). */
                include_deleted?: boolean;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** Every instance this host manages. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.InstanceDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/instances/{id}/starts — The start ledger: trigger, outcome, exit code, detail and argv — including preflight failures that never reached execve. */
    listInstanceStarts: {
        parameters: {
            query?: {
                /** How many rows. */
                limit?: number;
            };
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The ledger, newest first. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.InstanceStartDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No instance has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/jobs — The job queue, newest first. `?state=active` is the live set. */
    listJobs: {
        parameters: {
            query?: {
                /** Restrict to one job kind. */
                kind?: string;
                /** Maximum rows to return. */
                limit?: string;
                /** `active` restricts to the live states — queued, leased, running, paused and interrupted — which is the set that holds a subject against the one-live-job-per-subject index. Any other job state name restricts to it exactly. */
                state?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The matching jobs. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.JobDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * `?state=` or `?kind=` named something that is not a member of its enum.
             *
             * Error codes: query_invalid
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/llamacpp/releases — The releases of a channel, with the changelog rendered and sanitized server-side (D35), the resolved `nightly_tag`, and the api.github.com rate-limit headroom. */
    listLlamacppReleases: {
        parameters: {
            query?: {
                /** Which channel to list. `custom` has no listing: it is a git URL and a ref. */
                channel?: "stable" | "nightly";
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The channel's releases, newest first. */
            "200": {
                content: {
                    "application/json": components["schemas"]["ReleaseListDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The channel could not be listed: GitHub was unreachable and nothing usable was cached.
             *
             * Error codes: resolve_failed
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * `?channel=` is not `stable` or `nightly`. The custom channel resolves through `git ls-remote` and has no listing.
             *
             * Error codes: bad_flags
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/llamacpp/versions — Every llama.cpp build this host has, with its state, size, dates, `is_active`, `previous_active` and `in_use_by`. */
    listLlamacppVersions: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** Every version row, newest first. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.LlamacppVersionDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/models — The local model catalog, with the instances using each row. */
    listModels: {
        parameters: {
            query?: {
                /** Include rows in state `deleted`. Deleting a model never removes its row (§7.2), so those rows are history rather than catalog. */
                include_deleted?: boolean;
                /** Filter by model kind; repeatable as a comma-separated list. */
                kind?: "text" | "embedding" | "mmproj" | "unknown";
                /** Case-insensitive substring of the repo id or the primary file. */
                q?: string;
                /** Restrict to one cache root. */
                root_id?: string;
                /** Ordering. */
                sort?: "repo" | "size" | "recent";
                /** Filter by model state; repeatable as a comma-separated list. */
                state?: "planned" | "downloading" | "incomplete" | "verifying" | "ready" | "corrupt" | "missing" | "deleting" | "deleted";
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The catalog. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.ModelDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/presets — Every flag preset, built-ins first. */
    listPresets: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The presets. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.PresetDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/auth/sessions — The active admin sessions, with address, user agent and last-seen time. */
    listSessions: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** Every session that is neither revoked nor expired. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.SessionDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/cache/strays — Files in a cache root that belong to no model, largest first. */
    listStrays: {
        parameters: {
            query?: {
                /** Restrict to one cache root. */
                root_id?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The strays. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.StrayFileDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/system/gpus — The GPU inventory with live VRAM, utilization and temperature. */
    listSystemGPUs: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** One row per device. An empty list means no supported device was found, which is an answer rather than a failure. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.GPUDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/system/notifications — Undismissed notifications with their remediation actions. */
    listSystemNotifications: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The outstanding cards. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.NotificationDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/system/units — Installed unit files versus what this binary would render, with the exact repair command. Read-only — the daemon cannot write /etc. */
    listSystemUnits: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** One row per unit. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.UnitStatusDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/update/releases — Every release, with the changelog rendered server-side. */
    listUpdateReleases: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The cached release listing. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.UpdateReleaseDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/auth/login — Exchange the admin password for a session cookie and a CSRF cookie. */
    login: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["LoginRequest"];
        };
        };
        responses: {
            /** Authenticated. `lm_session` and `lm_csrf` are set. */
            "204": {
                content?: never;
            };
            /**
             * The password is wrong, or this host has no admin account.
             *
             * Error codes: bad_credentials
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This address has exhausted its login attempts (SPEC section 4). `details.retry_after_sec` says for how long.
             *
             * Error codes: locked_out
             */
            "429": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/auth/logout — Revoke this session and clear its cookies. */
    logout: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The session is revoked and both cookies are cleared. */
            "204": {
                content?: never;
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/models/{id}/pair-mmproj — Attach or detach a multimodal projector. Sets mmproj_auto = 0, so no later scan overrules the choice. */
    pairModelMmproj: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["PairMmprojRequest"];
        };
        };
        responses: {
            /** The updated model. */
            "200": {
                content: {
                    "application/json": components["schemas"]["ModelDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No model has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The named model is not a multimodal projector.
             *
             * Error codes: model_missing
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** PATCH /api/v1/tokens/{id} — Rename, disable, re-enable, rescope or rate-limit a token. Revoked is terminal and cannot be edited. */
    patchAPIToken: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["PatchAPITokenRequest"];
        };
        };
        responses: {
            /** The updated token. */
            "200": {
                content: {
                    "application/json": components["schemas"]["APITokenDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No token has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The token is revoked, which is terminal — the hash is retained so a leaked secret can never be re-minted into validity.
             *
             * Error codes: token_revoked
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The edit was refused; see createAPIToken for the codes.
             *
             * Error codes: bad_request, token_name_required, token_rate_limit_invalid, token_state_invalid
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** PATCH /api/v1/bench/runs/{id} — Rename or annotate a run. Its inputs and results are immutable. */
    patchBenchRun: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["PatchBenchRunRequest"];
        };
        };
        responses: {
            /** The updated run. */
            "200": {
                content: {
                    "application/json": components["schemas"]["BenchRunDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No run has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** PATCH /api/v1/instances/{id} — Edit an instance. The body must carry the `generation` the client read. */
    patchInstance: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["PatchInstanceRequest"];
        };
        };
        responses: {
            /** The updated instance, including `restart_required`. */
            "200": {
                content: {
                    "application/json": components["schemas"]["PatchInstanceResponse"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No instance has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The instance was edited by someone else, or the requested name is taken by another live instance.
             *
             * Error codes: conflict_generation, instance_name_taken
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The edited configuration was refused; see createInstance for the codes.
             *
             * Error codes: bad_flags, draft_vocab_mismatch, extra_flag_forbidden, instance_name_invalid, model_missing, ngl_auto_conflict, port_unavailable
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** PATCH /api/v1/presets/{id} — Rename or re-tune a preset. Built-ins refuse. */
    patchPreset: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["PresetInput"];
        };
        };
        responses: {
            /** The updated preset. */
            "200": {
                content: {
                    "application/json": components["schemas"]["PresetDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No preset has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This is a built-in preset, or another preset has that name.
             *
             * Error codes: conflict_generation, instance_name_taken
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** PATCH /api/v1/settings — Partial update. A value that fails its definition refuses the WHOLE patch — a half-applied settings form is worse than a refused one. */
    patchSettings: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The full post-patch value map, and whether a restart is owed. */
            "200": {
                content: {
                    "application/json": components["schemas"]["PatchSettingsDTO"];
                };
            };
            /**
             * A key is not in the registry, or its value fails that key's type, bounds, enum or extra check. `details.key` names it and nothing was written.
             *
             * Error codes: setting_invalid
             */
            "400": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/downloads/{id}/pause — Pause a download. The job releases its lease, the running transfers unwind, and every `.incomplete` file stands where it is for the resume to continue from. */
    pauseDownload: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The download after the change. */
            "202": {
                content: {
                    "application/json": components["schemas"]["DownloadDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No download has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The download's current state does not admit this change.
             *
             * Error codes: download_not_pausable
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/hf/peek/{repo} — The GGUF header read over HTTP Range BEFORE downloading: architecture, layers, KV heads, trained context, vocabulary, SWA window and the tensor summary (sections 3.6, 8.5). */
    peekHFFile: {
        parameters: {
            query: {
                /** The file to peek, inside the repository. Only shard 1 of a sharded set carries the geometry. */
                file: string;
                /** Branch, tag or commit. Empty means `main`. */
                revision?: string;
            };
            header?: never;
            path: {
                /** May contain `/` — this is a multi-segment path parameter (a Hugging Face repo id such as `bartowski/Qwen3-8B-GGUF`), sent unescaped. */
                repo: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The header's geometry. */
            "200": {
                content: {
                    "application/json": components["schemas"]["HFPeekDTO"];
                };
            };
            /**
             * The path did not name a repository id — `{repo...}` matches any number of segments, and only `name` or `org/name` is one — or, on the peek route, `?file=` was not given.
             *
             * Error codes: bad_request
             */
            "400": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The repository is gated, or private and unreachable with this host's credentials. A gated body carries `repo` and `request_url`; the UI links out, because access grants are browser-only on the Hub's side.
             *
             * Error codes: hf_gated, hf_private
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No such repository, revision or file.
             *
             * Error codes: file_not_in_repo
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The Hub could not be reached, or answered something unusable.
             *
             * Error codes: hf_unreachable
             */
            "502": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/instances/{id}/pin-ngl — Rewrite n_gpu_layers from auto to a count using the current fit estimate. An explicit config edit: it bumps generation and config_hash. */
    pinInstanceNGL: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The updated instance. */
            "200": {
                content: {
                    "application/json": components["schemas"]["PinNGLDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No instance has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This instance's n_gpu_layers is not `auto`, or no estimate can be made because the model's GGUF has not been parsed yet.
             *
             * Error codes: bad_flags, model_missing
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/llamacpp/plan — What installing this would do, before committing: the acquisition decision with its reason, the detected CUDA architectures, the missing toolchain items and a free-space check (section 6.3). */
    planLlamacppInstall: {
        parameters: {
            query?: {
                /** cpu or cuda. Empty means cpu. */
                backend?: "cpu" | "cuda";
                /** stable, nightly or custom. Empty means stable. */
                channel?: "stable" | "nightly" | "custom";
                /** Plan section 6.3's "otherwise" branch whatever the asset lookup says. */
                force_source?: boolean;
                /** The tag, branch or 40-hex commit of a custom build. */
                git_ref?: string;
                /** The remote of a custom build. */
                git_url?: string;
                /** Pin a release, as the install POST does. */
                tag?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** What would happen, and whether it can. */
            "200": {
                content: {
                    "application/json": components["schemas"]["PlanDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The request could not be resolved to a version: an unknown channel or backend, a git URL this daemon will not clone, or a channel lookup that failed.
             *
             * Error codes: bad_flags, resolve_failed
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/models/{id}/delete-preview — What deleting this model would free, with blobs refcounted across every snapshot in the repository (D28). */
    previewModelDelete: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The plan. */
            "200": {
                content: {
                    "application/json": components["schemas"]["DeletePreviewDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No model has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/system/toolchain/probe — Re-run the toolchain probe. */
    probeSystemToolchain: {
        parameters: {
            query?: never;
            header?: {
                /** Optional replay key. A repeat within 10 minutes returns the original job with 200 instead of creating a second one; the same key with a different body inside the window is 422 idempotency_key_reused (D39/D65). */
                "Idempotency-Key"?: string;
            };
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The probe was queued. */
            "202": {
                content: {
                    "application/json": components["schemas"]["JobReceiptDTO"];
                };
            };
            /**
             * The Idempotency-Key header is not a short printable ASCII token.
             *
             * Error codes: idempotency_key_invalid
             */
            "400": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/cache/roots/{id}/promote — Make this root primary — the single write path shared with PATCH /settings {hf.hub_dir}. Nothing is moved or copied. */
    promoteCacheRoot: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The root is now primary and a scan of it was queued. */
            "202": {
                content: {
                    "application/json": components["schemas"]["PromoteCacheRootResponse"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No cache root has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The root is not writable, so it can never receive downloads.
             *
             * Error codes: root_not_writable, root_path_protected
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** PUT /api/v1/github/token — Validate a GitHub token against `GET https://api.github.com/user` and store it sealed only if it is accepted. */
    putGitHubToken: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["PutTokenRequest"];
        };
        };
        responses: {
            /** The token was accepted and stored sealed. */
            "200": {
                content: {
                    "application/json": components["schemas"]["TokenStatusDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * GitHub refused this token.
             *
             * Error codes: github_token_invalid
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** PUT /api/v1/hf/token — Validate a Hugging Face token against `GET /api/whoami-v2` and store it sealed only if it is accepted. */
    putHFToken: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["PutTokenRequest"];
        };
        };
        responses: {
            /** The token was accepted and stored sealed. */
            "200": {
                content: {
                    "application/json": components["schemas"]["TokenStatusDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * Hugging Face refused this token.
             *
             * Error codes: hf_token_invalid
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** PATCH /api/v1/downloads/{id} — Change a download's queue priority. Moves the jobs row and the downloads row together, because the pool leases on the former. */
    reorderDownload: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["PatchDownloadRequest"];
        };
        };
        responses: {
            /** The reordered download. */
            "200": {
                content: {
                    "application/json": components["schemas"]["DownloadDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No download has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/instances/{id}/reset-failed — Clear the crash-loop latch and the backoff, start the crash-loop window over, and call systemd ResetFailed. */
    resetFailedInstance: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The desired state was written; the supervisor acts on its next pass. */
            "202": {
                content: {
                    "application/json": components["schemas"]["InstanceControlDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No instance has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No service manager is reachable on this host (F10), so there is nothing to ask. `details.hints` carries the manual systemctl command.
             *
             * Error codes: systemd_denied, systemd_unavailable
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/settings/reset — Delete the named override rows; the built-in defaults resume. */
    resetSettings: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["ResetSettingsRequest"];
        };
        };
        responses: {
            /** The values after the reset, and the unchanged schema. */
            "200": {
                content: {
                    "application/json": components["schemas"]["SettingsDTO"];
                };
            };
            /**
             * A key is not in the registry. Nothing was deleted.
             *
             * Error codes: setting_invalid
             */
            "400": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/instances/{id}/restart — Stop and start in one request. */
    restartInstance: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The desired state was written; the supervisor acts on its next pass. */
            "202": {
                content: {
                    "application/json": components["schemas"]["InstanceControlDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No instance has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No service manager is reachable on this host (F10), so there is nothing to ask. `details.hints` carries the manual systemctl command.
             *
             * Error codes: systemd_denied, systemd_unavailable
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/system/restart — Restart llamaman.service: commit, flush this 202, drain, hand the listeners to the fd store, then a non-blocking RestartNoWait. Instances are untouched. */
    restartSystem: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The restart was accepted and begins after this response is flushed. */
            "202": {
                content: {
                    "application/json": components["schemas"]["RestartDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * A build or a self-update is live; no service manager is reachable; or the name-scoped manage-units grant on llamaman.service was refused. Each carries the manual command in `details.hints`.
             *
             * Error codes: job_in_flight, restart_unavailable, systemd_denied
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * D93: this boot has not yet cleared its unit's start-limit counter. `details.retry_after_ms` is how long the UI disables the button for, rather than spending a start the revert deadline needs.
             *
             * Error codes: restart_rate_limited
             */
            "429": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/downloads/{id}/resume — Resume a paused download. It continues from the byte each file reached. */
    resumeDownload: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The download after the change. */
            "202": {
                content: {
                    "application/json": components["schemas"]["DownloadDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No download has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The download's current state does not admit this change.
             *
             * Error codes: download_not_pausable
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/downloads/{id}/retry — Run a failed or canceled download again, resuming from whatever is on disk. */
    retryDownload: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The download after the change. */
            "202": {
                content: {
                    "application/json": components["schemas"]["DownloadDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No download has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The download's current state does not admit this change.
             *
             * Error codes: download_not_pausable
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/llamacpp/versions/{id}/retry — Run a stopped build again. An `interrupted` build resumes against the warm worktree and cmake cache (D4); a failed or canceled one takes section 2.5's reuse-and-reset. */
    retryLlamacppVersion: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The build was queued again. Watch `job_id`. */
            "202": {
                content: {
                    "application/json": components["schemas"]["JobReceiptDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No version has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This version's last install is not in one of the three states a retry acts on (`failed`, `canceled`, `interrupted`).
             *
             * Error codes: build_not_retryable
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** DELETE /api/v1/auth/sessions/{id} — Revoke one session — a device the operator no longer uses, or their own. */
    revokeSession: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The session is revoked. */
            "204": {
                content?: never;
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No active session has that id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/llamacpp/rollback — Activate the retained previous build. This is the activation routine with `previous_active` as the target, revert path included. */
    rollbackLlamacpp: {
        parameters: {
            query?: never;
            header?: {
                /** Optional replay key. A repeat within 10 minutes returns the original job with 200 instead of creating a second one; the same key with a different body inside the window is 422 idempotency_key_reused (D39/D65). */
                "Idempotency-Key"?: string;
            };
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["ActivateLlamacppRequest"];
        };
        };
        responses: {
            /** The rollback was queued. */
            "202": {
                content: {
                    "application/json": components["schemas"]["JobReceiptDTO"];
                };
            };
            /**
             * The Idempotency-Key header is not a short printable ASCII token.
             *
             * Error codes: idempotency_key_invalid
             */
            "400": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * There is no retained previous build — nothing has been replaced yet, or `llamacpp.keep_previous` is off — or the same guards the activate endpoint applies refused.
             *
             * Error codes: activation_in_flight, bench_in_flight, job_in_flight, no_rollback_target, version_not_ready
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/instances/{id}/safe-start — One-shot start with -ngl 0 -c 2048 to isolate GPU from model problems. Never persisted. */
    safeStartInstance: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The desired state was written; the supervisor acts on its next pass. */
            "202": {
                content: {
                    "application/json": components["schemas"]["InstanceControlDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No instance has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No service manager is reachable on this host (F10), so there is nothing to ask. `details.hints` carries the manual systemctl command.
             *
             * Error codes: systemd_denied, systemd_unavailable
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/cache/scan — Walk a cache root and reconcile the catalog against it. Makes no network calls. */
    scanCache: {
        parameters: {
            query?: never;
            header?: {
                /** Optional replay key. A repeat within 10 minutes returns the original job with 200 instead of creating a second one; the same key with a different body inside the window is 422 idempotency_key_reused (D39/D65). */
                "Idempotency-Key"?: string;
            };
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["ScanRequest"];
        };
        };
        responses: {
            /** The scan was queued. */
            "202": {
                content: {
                    "application/json": components["schemas"]["JobReceiptDTO"];
                };
            };
            /**
             * The Idempotency-Key header is not a short printable ASCII token.
             *
             * Error codes: idempotency_key_invalid
             */
            "400": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No cache root has this id, or none is registered.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/hf/search — Search the Hub's GGUF repositories. `?cursor=` is the Hub's own opaque cursor, passed through unmodified. */
    searchHF: {
        parameters: {
            query?: {
                /** Restrict to one namespace. */
                author?: string;
                /** The previous page's `next_cursor`. */
                cursor?: string;
                /** Page size, up to 100. */
                limit?: number;
                /** Free text. Empty lists the most-downloaded GGUF repositories. */
                q?: string;
                /** Order. Empty means downloads. */
                sort?: "downloads" | "likes" | "lastModified" | "trendingScore";
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** One page of normalized search results. */
            "200": {
                content: {
                    "application/json": components["schemas"]["List[github.com"]["jlbyh2o"]["llamaman"]["internal"]["api.HFSearchResultDTO]"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The repository is gated, or private and unreachable with this host's credentials. A gated body carries `repo` and `request_url`; the UI links out, because access grants are browser-only on the Hub's side.
             *
             * Error codes: hf_gated, hf_private
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No such repository, revision or file.
             *
             * Error codes: file_not_in_repo
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The Hub could not be reached, or answered something unusable.
             *
             * Error codes: hf_unreachable
             */
            "502": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** PUT /api/v1/instances/{id}/autostart — Enable or disable the unit file, and nothing else — it never starts or stops. Answers hint=start_now when enabling on a stopped instance. */
    setInstanceAutostart: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["body"];
        };
        };
        responses: {
            /** The unit's enablement. */
            "200": {
                content: {
                    "application/json": components["schemas"]["AutostartDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No instance has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The manage-unit-files grant was withheld, or no manager is reachable. `details.hints` carries `sudo systemctl enable llamaman-instance@<name>.service`.
             *
             * Error codes: autostart_unavailable, systemd_unavailable
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/setup/password — Claim this host: create the admin account, burn the one-time token and log the browser in. Loopback callers need no `X-Setup-Token` (D38). */
    setupPassword: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["SetupPasswordRequest"];
        };
        };
        responses: {
            /** The host is claimed and the caller is logged in. */
            "204": {
                content?: never;
            };
            /**
             * The password does not meet the minimum length.
             *
             * Error codes: password_invalid
             */
            "400": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * A non-loopback caller presented no valid setup token (D38), or the Origin/Sec-Fetch-Site check said cross-site.
             *
             * Error codes: csrf_failed, setup_token_required
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * Another caller claimed this host first.
             *
             * Error codes: setup_already_claimed
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The request body carried no `Content-Type: application/json`, which is the shape a cross-origin request built to avoid a preflight has.
             *
             * Error codes: unsupported_media_type
             */
            "415": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/setup/skip — Skip a wizard step that section 11.2 marks skippable. */
    skipSetupStep: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["SetupSkipRequest"];
        };
        };
        responses: {
            /** The step is marked skipped. */
            "204": {
                content?: never;
            };
            /**
             * No such wizard step.
             *
             * Error codes: wizard_step_unknown
             */
            "400": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The step is not skippable, or an earlier step is unfinished.
             *
             * Error codes: wizard_step_locked
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/bench/runs/{id}/start — Queue a draft run. It waits for the bench lease, one sweep at a time. */
    startBenchRun: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The sweep was queued. Watch `job_id`. */
            "202": {
                content: {
                    "application/json": components["schemas"]["JobReceiptDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No run has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This run is not a draft, or a job already holds it.
             *
             * Error codes: bench_not_startable, job_in_flight
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/instances/{id}/start — Set desired_state=running and stamp pending_trigger='user'. The supervisor starts it. */
    startInstance: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The desired state was written; the supervisor acts on its next pass. */
            "202": {
                content: {
                    "application/json": components["schemas"]["InstanceControlDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No instance has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No service manager is reachable on this host (F10), so there is nothing to ask. `details.hints` carries the manual systemctl command.
             *
             * Error codes: systemd_denied, systemd_unavailable
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/instances/{id}/stop — Set desired_state=stopped. Answers hint=will_start_at_boot when autostart is on. */
    stopInstance: {
        parameters: {
            query?: {
                /** How long the gateway drains in-flight requests before the stop. */
                drain_sec?: number;
            };
            header?: never;
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The desired state was written; the supervisor acts on its next pass. */
            "202": {
                content: {
                    "application/json": components["schemas"]["InstanceControlDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No instance has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No service manager is reachable on this host (F10), so there is nothing to ask. `details.hints` carries the manual systemctl command.
             *
             * Error codes: systemd_denied, systemd_unavailable
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/events — Server-Sent Events. `?topics=` filters the stream; `Last-Event-ID` replays the `events` topic from the durable log. */
    streamEvents: {
        parameters: {
            query?: {
                /** Comma-separated topics to subscribe to. Empty means every topic. One of: instances, downloads, llamacpp, gpu, bench, jobs, events, notifications. */
                topics?: "instances" | "downloads" | "llamacpp" | "gpu" | "bench" | "jobs" | "events" | "notifications";
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** An event stream of `event: <topic>` / `id: <ulid>` / `data: {…}` frames, a `retry: 3000` directive and a 20 s `:keepalive` comment. */
            "200": {
                content: {
                    "text/event-stream": string;
                };
            };
            /**
             * An unrecognized entry in `?topics=`.
             *
             * Error codes: invalid_topic
             */
            "400": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** GET /api/v1/ports/suggest — The next free port not in the database and not bound. */
    suggestPort: {
        parameters: {
            query: {
                /** Which pool to draw from. */
                kind: "public" | "internal";
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** A port that was free when asked. */
            "200": {
                content: {
                    "application/json": components["schemas"]["PortSuggestionDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * `kind` is not one of public, internal, or the pool is exhausted.
             *
             * Error codes: port_unavailable
             */
            "422": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/instances/validate — Dry-run a FlagSet: render argv, check conflicts, return the three-valued draft verdict and a fit estimate. Never a 422 — it reports rather than refuses. */
    validateInstance: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
            "application/json": components["schemas"]["ValidateInstanceRequest"];
        };
        };
        responses: {
            /** The dry run's verdict. */
            "200": {
                content: {
                    "application/json": components["schemas"]["ValidateInstanceDTO"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * This host has not been claimed yet. The SPA routes to the wizard on this code alone.
             *
             * Error codes: setup_required
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
    /** POST /api/v1/models/{id}/verify — Re-stat every file and, when hf.verify_checksums is on, re-hash the ones whose blob name is a sha256. */
    verifyModel: {
        parameters: {
            query?: never;
            header?: {
                /** Optional replay key. A repeat within 10 minutes returns the original job with 200 instead of creating a second one; the same key with a different body inside the window is 422 idempotency_key_reused (D39/D65). */
                "Idempotency-Key"?: string;
            };
            path: {
                /** Path segment. */
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** The verify job was queued. */
            "202": {
                content: {
                    "application/json": components["schemas"]["JobReceiptDTO"];
                };
            };
            /**
             * The Idempotency-Key header is not a short printable ASCII token.
             *
             * Error codes: idempotency_key_invalid
             */
            "400": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No valid admin session accompanied the request.
             *
             * Error codes: unauthorized
             */
            "401": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * The CSRF double-submit, Origin or Sec-Fetch-Site check failed.
             *
             * Error codes: csrf_failed
             */
            "403": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * No model has this id.
             *
             * Error codes: not_found
             */
            "404": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * A job already holds this model.
             *
             * Error codes: job_in_flight
             */
            "409": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
            /**
             * An unhandled error. The message is a constant; the detail is in the journal.
             *
             * Error codes: internal_error
             */
            "500": {
                content: {
                    "application/json": components["schemas"]["ErrorEnvelope"];
                };
            };
        };
    };
}
