package db

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// Stats holds database-wide statistics.
type Stats struct {
	SessionCount         int     `json:"session_count"`
	MessageCount         int     `json:"message_count"`
	ProjectCount         int     `json:"project_count"`
	MachineCount         int     `json:"machine_count"`
	EarliestSession      *string `json:"earliest_session"`
	EarliestAccountUsage *string `json:"earliest_account_usage,omitempty"`
}

// rootSessionFilter is the WHERE clause shared by session list
// and stats to exclude sub-agent, fork, and trashed sessions.
const rootSessionFilter = `message_count > 0
	AND relationship_type NOT IN ('subagent', 'fork')
	AND deleted_at IS NULL`

func nonSourceBackedAgentPlaceholders() string {
	agents := nonSourceBackedAgents()
	placeholders := make([]string, len(agents))
	for i := range agents {
		placeholders[i] = "?"
	}
	return strings.Join(placeholders, ", ")
}

func nonSourceBackedAgentArgs() []any {
	agents := nonSourceBackedAgents()
	args := make([]any, len(agents))
	for i, a := range agents {
		args[i] = string(a)
	}
	return args
}

func nonSourceBackedAgents() []parser.AgentType {
	var agents []parser.AgentType
	for _, def := range parser.Registry {
		if def.FileBased || def.Type == parser.AgentDevin {
			continue
		}
		agents = append(agents, def.Type)
	}
	return agents
}

// FileBackedSessionCount returns the number of root sessions protected by local
// resync discovery. This includes literal file-backed sessions plus Devin's
// provider-backed local CLI archive sessions.
func (db *DB) FileBackedSessionCount(
	ctx context.Context,
) (int, error) {
	args := nonSourceBackedAgentArgs()
	args = append(args, localMachine)
	seenPrefixes := make(map[string]struct{}, len(excludedIDPrefixes))
	for _, prefix := range excludedIDPrefixes {
		if prefix == "" {
			continue
		}
		if _, seen := seenPrefixes[prefix]; seen {
			continue
		}
		seenPrefixes[prefix] = struct{}{}
		conditions = append(conditions, `substr(id, 1, length(?)) <> ?`)
		args = append(args, prefix, prefix)
	}
	seenAgents := make(map[string]struct{}, len(excludedAgents))
	for _, exclusion := range excludedAgents {
		if exclusion.Agent == "" {
			continue
		}
		if _, seen := seenAgents[exclusion.Agent]; seen {
			continue
		}
		seenAgents[exclusion.Agent] = struct{}{}
		if exclusion.KeepJSONLRows {
			conditions = append(conditions,
				`(agent <> ? OR file_path LIKE '%.jsonl')`)
			args = append(args, exclusion.Agent)
			continue
		}
		conditions = append(conditions, `agent <> ?`)
		args = append(args, exclusion.Agent)
	}

	var count int
	err := db.getReader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions
		 WHERE agent NOT IN (`+nonSourceBackedAgentPlaceholders()+`)
		 AND `+rootSessionFilter+`
		 AND `+strings.Join(conditions, "\n AND "),
		args...,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf(
			"counting local rebuild-owned file sessions: %w", err,
		)
	}
	return count, nil
}

func (db *DB) fileBackedSessionCount(
	ctx context.Context, machine, idPrefix string, scoped bool,
) (int, error) {
	machinePredicate := ""
	args := nonSourceBackedAgentArgs()
	if scoped {
		machinePredicate = " AND machine = ?"
		args = append(args, machine)
	}
	if idPrefix != "" {
		machinePredicate += " AND substr(id, 1, length(?)) = ?"
		args = append(args, idPrefix, idPrefix)
	}
	var count int
	err := db.getReader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions
		 WHERE agent NOT IN (`+nonSourceBackedAgentPlaceholders()+`)
		 AND `+rootSessionFilter,
		args...,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf(
			"counting file-backed sessions: %w", err,
		)
	}
	return count, nil
}

// GetStats counts sessions with the sidebar's canonical root rule.
// Other totals exclude subagents and forks, and include continuations.
func (db *DB) GetStats(
	ctx context.Context,
	excludeOneShot, excludeAutomated bool,
) (Stats, error) {
	filter := rootSessionFilter
	if excludeOneShot {
		if !excludeAutomated {
			filter += " AND (user_message_count > 1 OR is_automated = 1)"
		} else {
			filter += " AND user_message_count > 1"
		}
	}
	if excludeAutomated {
		filter += " AND is_automated = 0"
	}
	billingStart := "NULL"
	if db.hasCursorUsageTable(ctx) {
		billingStart = "(SELECT MIN(occurred_at) FROM cursor_usage_events)"
	}
	count, args := StatsRootCountSQL(SQLiteQueryDialect(), excludeOneShot, excludeAutomated)
	query := `SELECT ` + count + `, COALESCE(SUM(message_count), 0),
		COUNT(DISTINCT project), COUNT(DISTINCT machine),
		MIN(COALESCE(NULLIF(started_at, ''), created_at)), ` + billingStart + `
		FROM sessions WHERE ` + filter

	var s Stats
	err := db.getReader().QueryRowContext(ctx, query, args...).Scan(
		&s.SessionCount,
		&s.MessageCount,
		&s.ProjectCount,
		&s.MachineCount,
		&s.EarliestSession,
		&s.EarliestAccountUsage,
	)
	if err != nil {
		return Stats{}, fmt.Errorf("fetching stats: %w", err)
	}
	return s, nil
}
