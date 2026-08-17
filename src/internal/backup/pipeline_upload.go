package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"backup-operator/internal/meta"
	"backup-operator/internal/safe"
	"backup-operator/internal/secrets"
	"backup-operator/metrics"
	"backup-operator/storage"

	"github.com/go-logr/logr"
)

const (
	uploadMaxRetries      = 3
	uploadBaseDelay       = 2 * time.Second
	defaultMaxConcurrency = 4
)

func (p *Pipeline) fanOutDumps(
	ctx context.Context,
	dests []*secrets.Destination,
	target, dumpFile, objectPath string,
	log logr.Logger,
) []meta.DestinationResult {
	results := make([]meta.DestinationResult, len(dests))
	var wg sync.WaitGroup
	sem := make(chan struct{}, p.maxConcurrency)
	for i, dest := range dests {
		results[i] = meta.DestinationResult{
			Name:        dest.Name,
			StorageType: dest.StorageType,
			Status:      meta.StatusFailed,
		}
		wg.Add(1)
		go func(idx int, d *secrets.Destination) {
			defer wg.Done()
			defer safe.Goroutine(log, "upload", d.Name)
			sem <- struct{}{}
			defer func() { <-sem }()
			err := p.uploadDumpWithRetry(ctx, d, target, dumpFile, objectPath, log)
			if err != nil {
				log.Error(err, "destination upload failed", "destination", d.Name)
				metrics.SetDestinationFailed(target, d.Name, true)
				results[idx].Error = err.Error()
				return
			}
			metrics.SetDestinationFailed(target, d.Name, false)
			metrics.SetLastSuccess(target, d.Name, time.Now())
			results[idx].Status = meta.StatusSuccess
		}(i, dest)
	}
	wg.Wait()
	return results
}

// uploadMeta uploads the meta.json sidecar to all destinations that had a
// successful dump upload. Retries up to 3 times with exponential backoff
// to avoid "phantom backups" (dump exists but meta.json is missing).
//
// Returns (attempted, succeeded): how many destinations were eligible (had a
// successful dump) and how many actually received the meta. The caller treats
// succeeded==0 with attempted>0 as a run failure — a dump with no sidecar is
// invisible to the metrics refresher, the UI, and the restore listing, so
// reporting it as success would be a silent phantom backup. A meta-upload
// failure now logs at Error (was Info) so it surfaces in the worker log.
func (p *Pipeline) uploadMeta(
	ctx context.Context,
	dests []*secrets.Destination,
	results []meta.DestinationResult,
	metaPath string,
	metaBytes []byte,
	log logr.Logger,
) (attempted, succeeded int) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i, dest := range dests {
		if results[i].Status != meta.StatusSuccess {
			continue
		}
		attempted++
		wg.Add(1)
		go func(d *secrets.Destination) {
			defer wg.Done()
			defer safe.Goroutine(log, "meta-upload", d.Name)
			st, err := p.getStorage(d)
			if err != nil {
				log.Error(err, "meta upload: init storage failed", "destination", d.Name)
				return
			}
			const maxRetries = 3
			backoff := 2 * time.Second
			for attempt := 1; attempt <= maxRetries; attempt++ {
				if err := st.Upload(ctx, metaPath, bytes.NewReader(metaBytes)); err != nil {
					if attempt < maxRetries {
						log.Info("meta upload failed, retrying", "destination", d.Name, "attempt", attempt, "err", err.Error())
						t := time.NewTimer(backoff)
						select {
						case <-ctx.Done():
							t.Stop() // don't leak the timer on cancel
							return
						case <-t.C:
							backoff *= 2
						}
						continue
					}
					log.Error(err, "meta upload failed after retries; dump has no sidecar on this destination", "destination", d.Name, "attempts", maxRetries)
					return
				}
				mu.Lock()
				succeeded++
				mu.Unlock()
				break
			}
		}(dest)
	}
	wg.Wait()
	return attempted, succeeded
}

// uploadDumpWithRetry wraps uploadDumpOne with exponential backoff for
// transient failures. Only RetryableError triggers a retry; PermanentError
// and other errors abort immediately.
func (p *Pipeline) uploadDumpWithRetry(
	ctx context.Context,
	d *secrets.Destination,
	target, dumpFile, objectPath string,
	log logr.Logger,
) error {
	var lastErr error
	for attempt := 0; attempt < uploadMaxRetries; attempt++ {
		lastErr = p.uploadDumpOne(ctx, d, target, dumpFile, objectPath)
		if lastErr == nil {
			return nil
		}

		var retryable *RetryableError
		if !errors.As(lastErr, &retryable) {
			return lastErr
		}

		if attempt < uploadMaxRetries-1 {
			delay := uploadBaseDelay * time.Duration(1<<uint(attempt))
			log.Info("retrying upload after transient failure",
				"destination", d.Name,
				"attempt", attempt+1,
				"delay", delay.String(),
				"err", lastErr.Error(),
			)
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop() // don't leak the timer on cancel
				return ctx.Err()
			case <-t.C:
			}
		}
	}
	return lastErr
}

func (p *Pipeline) uploadDumpOne(
	ctx context.Context,
	d *secrets.Destination,
	target, dumpFile, objectPath string,
) error {
	st, err := p.getStorage(d)
	if err != nil {
		return &PermanentError{Op: "init storage", Err: err}
	}

	start := time.Now()
	dump, err := os.Open(dumpFile)
	if err != nil {
		return fmt.Errorf("open dump: %w", err)
	}
	defer func() { _ = dump.Close() }()

	info, err := dump.Stat()
	if err != nil {
		return fmt.Errorf("stat dump: %w", err)
	}
	localSize := info.Size()

	if err := st.Upload(ctx, objectPath, dump); err != nil {
		return classifyUploadError("upload dump", err)
	}
	metrics.ObserveUploadDuration(target, d.Name, d.StorageType, time.Since(start))

	if err := verifyUploadSize(ctx, st, objectPath, localSize, p.logger); err != nil {
		return err
	}
	return nil
}

// verifyUploadSize checks that the uploaded object's size matches the local
// file. Catches silent truncation, network corruption, or partial writes.
func verifyUploadSize(ctx context.Context, st storage.Storage, objectPath string, expected int64, log logr.Logger) error {
	objs, err := st.List(ctx, objectPath)
	if err != nil {
		log.V(1).Info("post-upload verify: list failed, skipping", "path", objectPath, "err", err.Error())
		return nil
	}
	for _, o := range objs {
		if o.Path == objectPath || strings.HasSuffix(o.Path, "/"+path.Base(objectPath)) {
			if o.Size != expected {
				return &RetryableError{
					Op:  "upload verify",
					Err: fmt.Errorf("size mismatch for %s: local=%d remote=%d", objectPath, expected, o.Size),
				}
			}
			log.V(1).Info("post-upload verify passed", "path", objectPath, "size", expected)
			return nil
		}
	}
	log.V(1).Info("post-upload verify: object not found in listing, skipping", "path", objectPath)
	return nil
}
