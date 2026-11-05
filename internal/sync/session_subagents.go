package sync

import (
	"context"
	"errors"

	"go.kenn.io/agentsview/internal/parser"
)

var subagentTranscriptPaths = parser.ClaudeSubagentTranscriptPaths

// SyncSessionWithSubagentsContext refreshes one session and the candidate
// Claude subagent transcripts in its root tree. Child discovery still runs
// when the requested-session refresh fails so callers can retain archived data
// while ingesting any descendants that remain available.
func (e *Engine) SyncSessionWithSubagentsContext(
	ctx context.Context,
	sessionID string,
) error {
	return e.syncSessionWithSubagentsContext(ctx, sessionID, false)
}

// TrySyncSessionWithSubagentsContext refreshes without waiting for another sync.
func (e *Engine) TrySyncSessionWithSubagentsContext(ctx context.Context, sessionID string) error {
	return e.syncSessionWithSubagentsContext(ctx, sessionID, true)
}

func (e *Engine) syncSessionWithSubagentsContext(ctx context.Context, sessionID string, try bool) error {
	parentErr := e.syncSingleSessionContext(ctx, sessionID, try)
	if errors.Is(parentErr, ErrSyncInProgress) {
		return parentErr
	}

	sourcePath := e.db.GetSessionFilePath(ctx, sessionID)
	if sourcePath == "" {
		sourcePath = e.FindSourceFile(ctx, sessionID)
	}
	paths := subagentTranscriptPaths(sourcePath)
	if len(paths) == 0 {
		return parentErr
	}

	var subagentErr error
	if strings.HasPrefix(sourcePath, "s3://") {
		subagentErr = e.syncS3SubagentTranscriptsContext(
			ctx, sessionID, parentAgent, paths, try)
	} else {
		subagentErr = e.syncPathsWithLockContext(ctx, paths, false, try)
	}
	return errors.Join(parentErr, subagentErr)
}
