package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// `fit_observations` queries (DESIGN sections 2.11 and 8.7, D32).
//
// This table is the fit calculator's only empirical input, and its whole
// contract is two statements: the supervisor appends one row per load, beside
// the prediction that was made; the calculator reads back the most recent rows
// for a `(arch, backend, llamacpp_tag)` key and takes a median. Retention is
// section 2.11's 2000-row trim and lives in retention.go with the other nightly
// jobs.

const fitObservationColumns = `id, at, arch, llamacpp_tag, backend, gpu_name,
	n_layer, n_embd, n_head, n_head_kv, n_vocab,
	n_ctx, n_batch, n_ubatch, n_parallel,
	flash_attn, type_k, type_v, n_gpu_layers,
	predicted_bytes, actual_weights_bytes, actual_kv_bytes,
	actual_compute_bytes, actual_total_bytes, oom, source`

// InsertFitObservation appends one observation.
//
// It is called from inside the transaction that recorded the load it describes
// — the supervisor's first-`ready` write (§5.8) or the bench finalizer (§10) —
// so an observation can never outlive the run it was taken from.
func (s *Store) InsertFitObservation(ctx context.Context, tx Tx, o model.FitObservation) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO fit_observations (`+fitObservationColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?,
		         ?, ?, ?, ?, ?,
		         ?, ?, ?, ?,
		         ?, ?, ?, ?,
		         ?, ?, ?,
		         ?, ?, ?, ?)`,
		o.ID, o.At, o.Arch, o.LlamacppTag, string(o.Backend), o.GPUName,
		o.NLayer, o.NEmbd, o.NHead, o.NHeadKV, o.NVocab,
		o.NCtx, o.NBatch, o.NUbatch, o.NParallel,
		boolArg(o.FlashAttn), o.TypeK, o.TypeV, o.NGpuLayers,
		o.PredictedBytes, o.ActualWeightsBytes, o.ActualKVBytes,
		o.ActualComputeBytes, o.ActualTotalBytes, boolInt(o.OOM), string(o.Source))
	if err != nil {
		return fmt.Errorf("insert fit observation %s: %w", o.ID, err)
	}
	return nil
}

// DefaultFitObservationLimit is the window D32 corrects from: "the median
// observed ratio over the last 20 observations". It is the store's default so
// that a caller which forgets to bound the read cannot accidentally average four
// months of history.
const DefaultFitObservationLimit = 20

// FitObservations returns the most recent observations for one calibration key,
// NEWEST FIRST.
//
// Newest first is not presentation: it is what makes the limit a WINDOW. A
// correction that stopped being true — a llama.cpp release that changed its
// allocator, a driver update — has to age out, and it only ages out if the rows
// the median sees are the recent ones.
//
// Ordering is by id rather than by `at` for the reason every listing in this
// package orders by id: the id is a ULID minted at insert, so it sorts by
// creation with no ties to break, while two observations recorded in the same
// millisecond would tie on `at`.
func (s *Store) FitObservations(ctx context.Context, tx Tx, key model.FitCalibrationKey,
	limit int) ([]model.FitObservation, error) {

	if limit <= 0 {
		limit = DefaultFitObservationLimit
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT `+fitObservationColumns+`
		   FROM fit_observations
		  WHERE arch = ? AND backend = ? AND llamacpp_tag = ?
		  ORDER BY id DESC
		  LIMIT ?`,
		key.Arch, string(key.Backend), key.LlamacppTag, limit)
	if err != nil {
		return nil, fmt.Errorf("list fit observations for %s/%s/%s: %w",
			key.Arch, key.Backend, key.LlamacppTag, err)
	}
	defer rows.Close()

	var out []model.FitObservation
	for rows.Next() {
		o, err := scanFitObservation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list fit observations: %w", err)
	}
	return out, nil
}

// FitObservationsForInstance returns the observations recorded for one
// instance's model architecture, whatever their source, newest first. It backs
// the instance page's "reported by llama.cpp" panel (D33), which shows what this
// build actually allocated beside what was predicted.
func (s *Store) FitObservationsForInstance(ctx context.Context, tx Tx, arch string,
	limit int) ([]model.FitObservation, error) {

	if limit <= 0 {
		limit = DefaultFitObservationLimit
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT `+fitObservationColumns+`
		   FROM fit_observations
		  WHERE arch = ?
		  ORDER BY id DESC
		  LIMIT ?`, arch, limit)
	if err != nil {
		return nil, fmt.Errorf("list fit observations for %s: %w", arch, err)
	}
	defer rows.Close()

	var out []model.FitObservation
	for rows.Next() {
		o, err := scanFitObservation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list fit observations: %w", err)
	}
	return out, nil
}

// CountFitObservations reports how many rows one key has, which is what the API
// shows beside `confidence: "modeled"` to say how far off a correction is
// ("2 of 3 observations recorded").
func (s *Store) CountFitObservations(ctx context.Context, tx Tx,
	key model.FitCalibrationKey) (int, error) {

	var n int
	err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM fit_observations
		  WHERE arch = ? AND backend = ? AND llamacpp_tag = ?`,
		key.Arch, string(key.Backend), key.LlamacppTag).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count fit observations: %w", err)
	}
	return n, nil
}

func scanFitObservation(sc scanner) (model.FitObservation, error) {
	var (
		o         model.FitObservation
		backend   string
		source    string
		flashAttn *int64
		oom       int64
	)
	err := sc.Scan(
		&o.ID, &o.At, &o.Arch, &o.LlamacppTag, &backend, &o.GPUName,
		&o.NLayer, &o.NEmbd, &o.NHead, &o.NHeadKV, &o.NVocab,
		&o.NCtx, &o.NBatch, &o.NUbatch, &o.NParallel,
		&flashAttn, &o.TypeK, &o.TypeV, &o.NGpuLayers,
		&o.PredictedBytes, &o.ActualWeightsBytes, &o.ActualKVBytes,
		&o.ActualComputeBytes, &o.ActualTotalBytes, &oom, &source)
	if err != nil {
		if err == sql.ErrNoRows {
			return model.FitObservation{}, ErrNotFound
		}
		return model.FitObservation{}, fmt.Errorf("scan fit observation: %w", err)
	}
	o.Backend = model.Backend(backend)
	o.Source = model.FitObservationSource(source)
	o.FlashAttn = boolPtr(flashAttn)
	o.OOM = oom != 0
	return o, nil
}
