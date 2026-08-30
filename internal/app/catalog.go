package app

import (
	"context"
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/hf/download"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/models"
	"github.com/jlbyh2o/llamaman/internal/settings"
)

// The model catalog and the downloader, wired into the composition root
// (DESIGN sections 2.6, 2.7, 3.7, 3.8, 7.2, 7.4).
//
// Both packages were complete and neither was ever constructed, which made
// `api.Config.Models` and `api.Config.Downloads` nil — and a nil there is not a
// missing feature but a 503 on nine endpoints: `GET /models`, `GET /cache/roots`,
// `GET /cache/strays`, `POST /cache/scan` and the whole of `/downloads`. The
// consequences compounded through the product rather than staying local to those
// routes:
//
//   - The wizard's model step could not detect or scan a cache, so a host with a
//     perfectly good `~/.cache/huggingface/hub` full of GGUFs produced no models.
//   - With no models, `POST /api/v1/instances` refused every request with
//     `422 model_missing`, so the wizard's last step could not be completed
//     either — on a fresh install the product could not be finished.
//   - The Model library, Downloads, the Dashboard's storage panel and both bench
//     model pickers had nothing to read.
//
// The three workers are registered with the queue here for the reason section
// 2.3's boot triage gives, the same reason the llama.cpp workers are registered
// in boot(): a job interrupted by the previous boot must find its DomainWriter
// already in place, so that the job row and the domain row move in one
// transaction.

// buildCatalog constructs the model service, the downloader and their workers.
//
// The models service is built FIRST, because the downloader holds a reference to
// it: section 2.6's `post_download` scan is what fills a freshly-downloaded
// model's GGUF geometry, and internal/models owns the mapping from a parsed
// header to the twenty columns that hold it. Duplicating that mapping in the
// downloader would be two implementations of one table.
func (d *daemon) buildCatalog() error {
	if d.store == nil {
		return fmt.Errorf("the catalog needs a store")
	}

	modelSvc, err := models.New(models.Config{
		Store: d.store,
		// D69: a model's resolved path is a `config_hash` input, so every write
		// that moves one recomputes the hash for every instance referencing it,
		// in the same transaction.
		Hashes:   d.instances,
		Events:   d.recorder,
		Queue:    d.queue,
		Settings: d.settings,
		Now:      d.opts.Now,
		StateDir: d.stateDir,
	})
	if err != nil {
		return fmt.Errorf("build the model service: %w", err)
	}
	d.models = modelSvc

	dl, err := download.New(download.Config{
		Store:  d.store,
		Client: d.hfClient,
		Events: d.recorder,
		Queue:  d.queue,
		Hashes: d.instances,
		// The completed download asks internal/models for the scan that fills
		// the geometry — see this function's doc comment.
		Scans:    scanRequester{svc: modelSvc},
		Settings: d.settings,
		Now:      d.opts.Now,
		Logger:   d.log,
	})
	if err != nil {
		return fmt.Errorf("build the download service: %w", err)
	}
	d.downloads = dl

	if err := d.queue.Register(models.NewScanWorker(modelSvc)); err != nil {
		return fmt.Errorf("register the cache-scan worker: %w", err)
	}
	if err := d.queue.Register(models.NewDeleteWorker(modelSvc)); err != nil {
		return fmt.Errorf("register the model-delete worker: %w", err)
	}
	if err := d.queue.Register(models.NewVerifyWorker(modelSvc, d.verifyChecksums)); err != nil {
		return fmt.Errorf("register the model-verify worker: %w", err)
	}
	if err := download.Register(d.queue, dl); err != nil {
		return fmt.Errorf("register the download worker: %w", err)
	}
	return nil
}

// verifyChecksums reads `settings['hf.verify_checksums']` per call rather than
// at construction, so turning the setting on in the UI takes effect on the next
// verify rather than at the next restart.
func (d *daemon) verifyChecksums(ctx context.Context) bool {
	if d.settings == nil {
		return false
	}
	on, err := d.settings.GetBool(ctx, download.KeyVerifyChecksums)
	if err != nil {
		return false
	}
	return on
}

// scanRequester adapts the model service to the downloader's Scans interface,
// dropping the job receipt the downloader has no use for.
//
// The receipt is dropped rather than logged because the worker that asked for
// the scan is already reporting its own progress on its own job; a second job id
// in that stream would be a second thing for a user to wonder about.
type scanRequester struct{ svc *models.Service }

func (s scanRequester) RequestScan(ctx context.Context, rootID string,
	trigger model.CacheScanTrigger) (model.CacheScan, error) {
	row, _, err := s.svc.RequestScan(ctx, rootID, trigger)
	return row, err
}

// A compile-time assertion that the settings cache still satisfies both
// packages' narrow settings interfaces. It is here rather than in a test because
// a mismatch is a build error the composition root should not be able to ship.
var (
	_ models.SettingsCache = (*settings.Cache)(nil)
	_ download.Settings    = (*settings.Cache)(nil)
)
