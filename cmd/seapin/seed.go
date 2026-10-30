package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"seapin/internal/catalog"
	"seapin/internal/cidkey"
	"seapin/internal/config"
	"seapin/internal/policy"
	"seapin/internal/store"
)

// runSeed stores a local file under a CID, applying the same content policy
// as ingest. Raw CIDs are verified against the file; dag-pb CIDs cannot be
// (they depend on the original chunking), so the operator vouches for those.
func runSeed(ctx context.Context, log *slog.Logger, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: seapin seed <cid> <file>")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	id, err := cidkey.Normalize(args[0])
	if err != nil {
		return err
	}
	f, err := os.Open(args[1])
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()

	head := make([]byte, policy.SniffLen)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return err
	}
	contentType, detected, ok := policy.Classify(head[:n])

	cat, err := catalog.Open(ctx, cfg.PGDSN)
	if err != nil {
		return err
	}
	defer cat.Close()

	switch {
	case size > policy.MaxObjectSize:
		if err := cat.MarkBlocked(ctx, id, detected, size, "too_large"); err != nil {
			return err
		}
		return fmt.Errorf("blocked: %d bytes is over the %d byte cap", size, policy.MaxObjectSize)
	case !ok:
		if err := cat.MarkBlocked(ctx, id, detected, size, "not_allowed:"+detected); err != nil {
			return err
		}
		return fmt.Errorf("blocked: %s is not an accepted type", detected)
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	checked, err := cidkey.VerifyRaw(id, f)
	if err != nil {
		return err
	}
	if !checked {
		log.Warn("content not verified against the CID (not a raw sha2-256 CID)", "cid", id)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	st, err := store.New(cfg.S3)
	if err != nil {
		return err
	}
	if err := st.Put(ctx, cidkey.ObjectKey(id), f, size, contentType); err != nil {
		return fmt.Errorf("s3 put: %w", err)
	}
	if err := cat.MarkCached(ctx, id, contentType, size, "seed"); err != nil {
		return err
	}
	log.Info("seeded", "cid", id, "type", contentType, "bytes", size, "verified", checked)
	return nil
}
