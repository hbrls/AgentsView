package main

import (
	"context"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func archiveModeAfterRecovery(ctx context.Context, cfg config.Config) (bool, error) {
	pending, err := db.CompactRecoveryPending(cfg.DBPath)
	if err != nil {
		return false, err
	}
	if pending {
		lock, err := acquireWriteOwnerLock(ctx, writeLockDataDir(cfg))
		if err != nil {
			return false, err
		}
		defer lock.Close()
		if err := db.RecoverCompactManifest(cfg.DBPath); err != nil {
			return false, err
		}
		return db.ArchiveOnlyAt(ctx, cfg.DBPath)
	}
	return db.ArchiveOnlyAt(ctx, cfg.DBPath)
}
