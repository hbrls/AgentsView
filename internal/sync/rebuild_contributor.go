package sync

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
)

type rebuildOperations struct {
	rebuildFTS                        func(context.Context, *db.DB) error
	rebuildUsageIndexes               func(context.Context, *db.DB) error
	reopen                            func(*db.DB) error
	listActiveWorktreeMappingMachines func(context.Context, *db.DB) ([]string, error)
	applyWorktreeMappings             func(context.Context, *db.DB, string) (db.ApplyWorktreeProjectMappingsResult, error)
}

var productionRebuildOperations = rebuildOperations{
	rebuildFTS: func(ctx context.Context, database *db.DB) error { return database.RebuildFTS(ctx) },
	rebuildUsageIndexes: func(ctx context.Context, database *db.DB) error {
		return database.RebuildBulkImportIndexes(ctx)
	},
	reopen: func(database *db.DB) error { return database.Reopen() },
	listActiveWorktreeMappingMachines: func(
		ctx context.Context, database *db.DB,
	) ([]string, error) {
		return database.ListActiveWorktreeProjectMappingMachines(ctx)
	},
	applyWorktreeMappings: func(
		ctx context.Context, database *db.DB, machine string,
	) (db.ApplyWorktreeProjectMappingsResult, error) {
		return database.ApplyWorktreeProjectMappingsFromSync(ctx, machine)
	},
}

func (ops rebuildOperations) withDefaults() rebuildOperations {
	if ops.rebuildFTS == nil {
		ops.rebuildFTS = productionRebuildOperations.rebuildFTS
	}
	if ops.rebuildUsageIndexes == nil {
		ops.rebuildUsageIndexes = productionRebuildOperations.rebuildUsageIndexes
	}
	if ops.reopen == nil {
		ops.reopen = productionRebuildOperations.reopen
	}
	if ops.listActiveWorktreeMappingMachines == nil {
		ops.listActiveWorktreeMappingMachines = productionRebuildOperations.listActiveWorktreeMappingMachines
	}
	if ops.applyWorktreeMappings == nil {
		ops.applyWorktreeMappings = productionRebuildOperations.applyWorktreeMappings
	}
	return ops
}

func mergeSyncStats(dst *SyncStats, src SyncStats) {
	dst.TotalSessions += src.TotalSessions
	dst.Synced += src.Synced
	dst.CwdUpdated += src.CwdUpdated
	dst.LinksUpdated += src.LinksUpdated
	dst.Skipped += src.Skipped
	dst.Failed += src.Failed
	dst.OrphanedCopied += src.OrphanedCopied
	dst.Tombstoned += src.Tombstoned
	dst.Warnings = append(dst.Warnings, src.Warnings...)
	dst.Aborted = dst.Aborted || src.Aborted
	dst.Anomalies.merge(src.Anomalies)
	dst.filesOK += src.filesOK
	dst.filesDiscovered += src.filesDiscovered
	dst.nonContainerDiscovered += src.nonContainerDiscovered
	dst.messagesIndexed += src.messagesIndexed
	dst.parserExcludedFiles += src.parserExcludedFiles
	dst.parserExcludedIDs = append(dst.parserExcludedIDs, src.parserExcludedIDs...)
	dst.sourceMissingArchiveMembers = append(
		dst.sourceMissingArchiveMembers, src.sourceMissingArchiveMembers...,
	)
	dst.cwdFilteredSessions += src.cwdFilteredSessions
	dst.cwdFilteredFiles += src.cwdFilteredFiles
	if !dst.deferredRetryOverflow {
		if src.deferredRetryOverflow {
			dst.deferredRetryOverflow = true
			dst.deferredRetryPaths = nil
		} else {
			for _, path := range src.deferredRetryPaths {
				dst.retainDeferredRetryPath(path)
			}
		}
	}
	dst.Deferred += src.Deferred
}
