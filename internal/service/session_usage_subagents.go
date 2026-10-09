// ABOUTME: presentation-time combination of a session's own usage with
// ABOUTME: the usage of every subagent transcript spawned beneath it.
package service

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

// SessionUsageWithSubagents returns rootID's usage with every reachable
// subagent and the forks inside those subagent subtrees folded in.
//
// Claude Code writes each Task-tool subagent to its own transcript under
// <session>/subagents/, which agentsview ingests as a separate session. A
// parent's own rows therefore omit whatever its subagents spent, which
// understates the cost of the work the caller asked for. This combines the
// two at read time only: nothing is persisted, no child row is duplicated
// under the parent, and day aggregates (which already count subagent
// sessions as first-class spend) are untouched.
//
// Traversal matches GetSessionUsageRollup: breadth-first through all children,
// including subagents plus forks created inside a subagent subtree, cycle-safe.
// Root-level forks are traversed but not included. When the root has no
// delegated descendants the store's own-session result is returned unchanged.
//
// Rows come from GetSessionUsageRows, which dedups across the whole id set,
// so a message recorded in both the parent and a child transcript is
// counted once. Breakdown entries stay in that call's global order
// (timestamp, then root-before-descendants, then message ordinal) and are
// renumbered from 1; entries from a child carry its id in
// SubagentSessionID while keeping their real Source.
func SessionUsageWithSubagents(
	ctx context.Context, store db.Store, rootID string, includeBreakdown bool,
) (*db.SessionUsage, error) {
	descendants, err := delegatedDescendants(ctx, store, rootID)
	if err != nil {
		return nil, err
	}
	return sessionUsageWithDescendants(
		ctx, store, rootID, descendants, includeBreakdown,
	)
}

func sessionUsageWithDescendants(
	ctx context.Context,
	store db.Store,
	rootID string,
	descendants []db.Session,
	includeBreakdown bool,
) (*db.SessionUsage, error) {
	root, err := store.GetSessionUsage(ctx, rootID, includeBreakdown)
	if err != nil || root == nil {
		return nil, err
	}
	if len(descendants) == 0 {
		root.TokenBreakdownComplete =
			!sessionHasPositiveTokens(root.TotalOutputTokens, root.PeakContextTokens) ||
				root.BreakdownCount > 0
		return root, nil
	}

	var rowSet *activity.SessionUsageRows
	if provider, ok := store.(sessionUsageRowsProvider); ok {
		ids, idsErr := sessionUsageIDsContext(ctx, rootID, descendants)
		if idsErr != nil {
			return nil, idsErr
		}
		rowSet, err = provider.GetSessionUsageRows(
			ctx, ids)
		if err != nil {
			return nil, err
		}
	}
	if rowSet == nil {
		return combineSubagentUsageFromSessions(
			ctx, store, root, descendants, includeBreakdown)
	}
	rootStoredOutputTokens := root.TotalOutputTokens
	storedRoot, err := store.GetSession(ctx, rootID)
	if err != nil {
		return nil, err
	}
	if storedRoot != nil {
		rootStoredOutputTokens = storedRoot.TotalOutputTokens
	}
	tokenExpectations := make(map[string]sessionTokenExpectation, len(descendants)+1)
	tokenExpectations[rootID] = sessionTokenExpectationFromUsage(root)
	for _, descendant := range descendants {
		tokenExpectations[descendant.ID] = sessionTokenExpectationFromSession(descendant)
	}
	return combineSubagentUsageFromRows(
		ctx, rootID, root, rootStoredOutputTokens, descendants, rowSet.Rows,
		rowSet.RawOutputTokensBySession,
		rowSet.DiscardedContributingSessions,
		rowSet.CanonicalTokenCoverageBySession, tokenExpectations,
		includeBreakdown)
}

func sessionUsageIDsContext(
	ctx context.Context, rootID string, descendants []db.Session,
) ([]string, error) {
	ids := make([]string, 0, len(descendants)+1)
	ids = append(ids, rootID)
	for _, descendant := range descendants {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ids = append(ids, descendant.ID)
	}
	return ids, nil
}

// combineSubagentUsageFromRows builds the combined result from one deduped,
// globally ordered usage-row set. rootID is the id the rows were queried
// under, which is what decides whether a row belongs to the parent.
func combineSubagentUsageFromRows(
	ctx context.Context,
	rootID string,
	root *db.SessionUsage,
	rootStoredOutputTokens int,
	descendants []db.Session,
	rows []activity.UsageRow,
	rawOutputTokensBySession map[string]int,
	discardedContributingSessions map[string]struct{},
	canonicalTokenCoverageBySession map[string]activity.SessionTokenCoverage,
	tokenExpectations map[string]sessionTokenExpectation,
	includeBreakdown bool,
) (*db.SessionUsage, error) {
	out, err := newCombinedSessionUsage(ctx, root, descendants)
	if err != nil {
		return nil, err
	}
	combined, err := accumulateCombinedUsageRows(
		ctx, out, rootID, rows, includeBreakdown,
	)
	if err != nil {
		return nil, err
	}
	out.Breakdown = combined.breakdown
	out.Models, err = sortedKeys(ctx, combined.models)
	if err != nil {
		return nil, err
	}
	var outputCostCovered bool
	out.TotalOutputTokens, outputCostCovered, err = combinedOutputTokens(
		ctx, rootID, rootStoredOutputTokens, descendants, combined.outputBySession,
		rawOutputTokensBySession)
	if err != nil {
		return nil, err
	}
	if out.TotalOutputTokens > 0 {
		out.HasTokenData = true
	}
	var sessionCostCovered bool
	sessionCostCovered, out.TokenBreakdownComplete, err = combinedSessionCoverage(
		ctx, rootID,
		rootStoredOutputTokens > 0 || root.PeakContextTokens > 0,
		descendants, combined.usageRowsBySession,
		discardedContributingSessions, canonicalTokenCoverageBySession,
		tokenExpectations)
	if err != nil {
		return nil, err
	}
	out.HasCost = combined.hasCostSettlement && combined.allPriced && outputCostCovered &&
		sessionCostCovered && (!combined.hasComputedCost || out.TokenBreakdownComplete)
	if out.HasCost {
		out.Cost = combined.cost
		out.CostSource = export.CombinedCostSource(
			combined.hasComputedCost, combined.hasReportedCost)
		out.AICredits = db.AICreditsFromCost(out.Agent, out.Cost)
	}
	out.CostUSD = db.CostUSDFromCost(out.HasCost, out.Cost)
	if len(combined.unpriced) > 0 {
		out.UnpricedModels, err = sortedKeys(ctx, combined.unpriced)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func combinedSessionCoverage(
	ctx context.Context,
	rootID string,
	rootHasPositiveTokens bool,
	descendants []db.Session,
	usageRowsBySession map[string]struct{},
	discardedSessions map[string]struct{},
	canonicalTokenCoverageBySession map[string]activity.SessionTokenCoverage,
	tokenExpectations map[string]sessionTokenExpectation,
) (costCovered bool, breakdownCovered bool, err error) {
	hasRows := func(id string) bool {
		if _, ok := usageRowsBySession[id]; ok {
			return true
		}
		_, ok := discardedSessions[id]
		return ok
	}
	hasCategoryCoverage := func(id string) bool {
		expected := tokenExpectations[id]
		if !expected.hasPositiveTokens() {
			return true
		}
		coverage, ok := canonicalTokenCoverageBySession[id]
		return ok && coverage.OutputTokens >= expected.outputTokens &&
			coverage.PeakContextTokens >= expected.peakContextTokens
	}
	rootHasRows := hasRows(rootID)
	costCovered = !rootHasPositiveTokens || rootHasRows
	breakdownCovered = hasCategoryCoverage(rootID)
	for _, descendant := range descendants {
		if err := ctx.Err(); err != nil {
			return false, false, err
		}
		sessionHasRows := hasRows(descendant.ID)
		hasPositiveTokens := descendant.TotalOutputTokens > 0 ||
			descendant.PeakContextTokens > 0
		if hasPositiveTokens && !sessionHasRows {
			costCovered = false
		}
		breakdownCovered = breakdownCovered && hasCategoryCoverage(descendant.ID)
	}
	return costCovered, breakdownCovered, nil
}

type sessionTokenExpectation struct {
	outputTokens      int
	peakContextTokens int
}

func (e sessionTokenExpectation) hasPositiveTokens() bool {
	return e.outputTokens > 0 || e.peakContextTokens > 0
}

func sessionTokenExpectationFromUsage(
	usage *db.SessionUsage,
) sessionTokenExpectation {
	if usage == nil {
		return sessionTokenExpectation{}
	}
	return sessionTokenExpectation{
		outputTokens:      usage.TotalOutputTokens,
		peakContextTokens: usage.PeakContextTokens,
	}
}

func sessionTokenExpectationFromSession(
	session db.Session,
) sessionTokenExpectation {
	return sessionTokenExpectation{
		outputTokens:      session.TotalOutputTokens,
		peakContextTokens: session.PeakContextTokens,
	}
}

type combinedUsageRows struct {
	cost               money.Money
	hasComputedCost    bool
	hasReportedCost    bool
	hasCostSettlement  bool
	allPriced          bool
	models             map[string]struct{}
	unpriced           map[string]struct{}
	breakdown          []db.SessionUsageBreakdownEntry
	outputBySession    map[string]int
	usageRowsBySession map[string]struct{}
}

func accumulateCombinedUsageRows(
	ctx context.Context,
	out *db.SessionUsage,
	rootID string,
	rows []activity.UsageRow,
	includeBreakdown bool,
) (combinedUsageRows, error) {
	combined := combinedUsageRows{
		allPriced: true,
		models:    make(map[string]struct{}), unpriced: make(map[string]struct{}),
		breakdown:          make([]db.SessionUsageBreakdownEntry, 0, len(rows)),
		outputBySession:    make(map[string]int),
		usageRowsBySession: make(map[string]struct{}),
	}
	allocated, err := activity.AllocateUsageCostsContext(ctx, rows)
	if err != nil {
		return combinedUsageRows{}, err
	}
	for i, row := range rows {
		if err := ctx.Err(); err != nil {
			return combinedUsageRows{}, err
		}
		allocation := allocated[i]
		if !allocation.Contributes {
			continue
		}
		combined.hasCostSettlement = true
		recordRollupCostSource(allocation.CostSource,
			&combined.hasComputedCost, &combined.hasReportedCost)
		if allocation.Priced {
			combined.cost, err = money.Add(combined.cost, allocation.Cost)
			if err != nil {
				return combinedUsageRows{}, fmt.Errorf(
					"summing session usage with subagents: %w", err)
			}
		} else {
			combined.allPriced = false
			if row.Contributes {
				combined.unpriced[row.Model] = struct{}{}
			}
		}
		if !row.Contributes {
			continue
		}
		combined.usageRowsBySession[usageRowSourceSessionID(row)] = struct{}{}
		combined.outputBySession[row.SessionID] += row.OutputTokens
		combined.models[row.Model] = struct{}{}
		out.BreakdownCount++
		if includeBreakdown {
			combined.breakdown = append(combined.breakdown, usageRowBreakdownEntry(
				row, rootID, out.BreakdownCount, allocation.Cost, allocation.Priced))
		}
	}
	return combined, nil
}

// combinedOutputTokens totals output tokens over the included sessions
// without double-counting a message that appears in more than one
// transcript.
//
// A session's stored total_output_tokens includes output-bearing messages
// even when they lack the raw usage payload required for cost rows. Preserve
// that rowless residual while counting usage-row output only from the globally
// deduplicated survivors. Survivors remain a fallback for legacy sessions
// whose stored total is incomplete.
func combinedOutputTokens(
	ctx context.Context,
	rootID string,
	rootStoredOutputTokens int,
	descendants []db.Session,
	outputBySession map[string]int,
	rawOutputTokensBySession map[string]int,
) (total int, costCovered bool, err error) {
	costCovered = true
	for _, output := range outputBySession {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		total += output
	}
	addSession := func(id string, stored int) {
		residual := stored - rawOutputTokensBySession[id]
		if residual > 0 {
			total += residual
			costCovered = false
		}
	}
	addSession(rootID, rootStoredOutputTokens)
	for _, descendant := range descendants {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		addSession(descendant.ID, descendant.TotalOutputTokens)
	}
	return total, costCovered, nil
}

func usageRowSourceSessionID(row activity.UsageRow) string {
	if row.SourceSessionID != "" {
		return row.SourceSessionID
	}
	return row.SessionID
}

// combineSubagentUsageFromSessions is the fallback for stores that expose no
// usage-row provider. It merges per-session results instead, which cannot
// dedup rows shared between a parent and a child transcript; every store in
// this repo implements GetSessionUsageRows, so the primary path is what
// production takes.
func combineSubagentUsageFromSessions(
	ctx context.Context,
	store db.Store,
	root *db.SessionUsage,
	descendants []db.Session,
	includeBreakdown bool,
) (*db.SessionUsage, error) {
	out, err := newCombinedSessionUsage(ctx, root, descendants)
	if err != nil {
		return nil, err
	}
	combined := newSessionUsageAccumulator(ctx, out, len(root.Breakdown))
	if err := combined.add(root, ""); err != nil {
		return nil, err
	}
	breakdownComplete := !sessionHasPositiveTokens(
		root.TotalOutputTokens, root.PeakContextTokens) || root.BreakdownCount > 0
	for _, descendant := range descendants {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		usage, err := store.GetSessionUsage(
			ctx, descendant.ID, includeBreakdown)
		if err != nil {
			return nil, err
		}
		if sessionHasPositiveTokens(
			descendant.TotalOutputTokens, descendant.PeakContextTokens,
		) && (usage == nil || usage.BreakdownCount == 0) {
			breakdownComplete = false
		}
		if err := combined.add(usage, descendant.ID); err != nil {
			return nil, err
		}
	}
	out.Breakdown = combined.breakdown
	out.TokenBreakdownComplete = breakdownComplete
	out.Models, err = sortedKeys(ctx, combined.models)
	if err != nil {
		return nil, err
	}
	out.HasCost = combined.contributing && combined.allPriced &&
		len(combined.unpriced) == 0
	if out.HasCost {
		out.Cost = combined.cost
		out.CostSource = export.CombinedCostSource(
			combined.hasComputedCost, combined.hasReportedCost)
		out.AICredits = db.AICreditsFromCost(out.Agent, out.Cost)
	}
	out.CostUSD = db.CostUSDFromCost(out.HasCost, out.Cost)
	if len(combined.unpriced) > 0 {
		out.UnpricedModels, err = sortedKeys(ctx, combined.unpriced)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func sessionHasPositiveTokens(outputTokens, peakContextTokens int) bool {
	return outputTokens > 0 || peakContextTokens > 0
}

type sessionUsageAccumulator struct {
	ctx             context.Context
	out             *db.SessionUsage
	models          map[string]struct{}
	unpriced        map[string]struct{}
	breakdown       []db.SessionUsageBreakdownEntry
	cost            money.Money
	hasComputedCost bool
	hasReportedCost bool
	contributing    bool
	allPriced       bool
}

func newSessionUsageAccumulator(
	ctx context.Context, out *db.SessionUsage, breakdownSize int,
) *sessionUsageAccumulator {
	return &sessionUsageAccumulator{
		ctx: ctx, out: out, models: make(map[string]struct{}),
		unpriced: make(map[string]struct{}), allPriced: true,
		breakdown: make([]db.SessionUsageBreakdownEntry, 0, breakdownSize),
	}
}

func (combined *sessionUsageAccumulator) add(
	usage *db.SessionUsage, subagentID string,
) error {
	if err := combined.ctx.Err(); err != nil || usage == nil {
		return err
	}
	if usage.HasTokenData && !usage.HasCost {
		combined.allPriced = false
	}
	for _, model := range usage.Models {
		if err := combined.ctx.Err(); err != nil {
			return err
		}
		combined.models[model] = struct{}{}
	}
	for _, model := range usage.UnpricedModels {
		if err := combined.ctx.Err(); err != nil {
			return err
		}
		combined.unpriced[model] = struct{}{}
	}
	if usage.BreakdownCount > 0 {
		combined.contributing = true
		if usage.HasCost {
			cost, err := money.Add(combined.cost, usage.Cost)
			if err != nil {
				return fmt.Errorf("summing subagent session usage: %w", err)
			}
			combined.cost = cost
			recordRollupCostSource(usage.CostSource,
				&combined.hasComputedCost, &combined.hasReportedCost)
		} else {
			combined.allPriced = false
		}
	}
	combined.out.BreakdownCount += usage.BreakdownCount
	for _, entry := range usage.Breakdown {
		if err := combined.ctx.Err(); err != nil {
			return err
		}
		entry.Ordinal = len(combined.breakdown) + 1
		entry.SubagentSessionID = subagentID
		combined.breakdown = append(combined.breakdown, entry)
	}
	return nil
}

// newCombinedSessionUsage seeds the combined result with the root's identity
// and the session-level token aggregates of everything included. Peak context
// is the maximum rather than a sum, because each session's peak is an
// independent high-water mark.
//
// The output-token total here sums stored per-session aggregates, which
// double-counts a message echoed across transcripts. That is the best the
// row-less fallback path can do; combineSubagentUsageFromRows overwrites it
// with a deduplicated total from the rows themselves.
func newCombinedSessionUsage(
	ctx context.Context, root *db.SessionUsage, descendants []db.Session,
) (*db.SessionUsage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := &db.SessionUsage{
		SessionID:         root.SessionID,
		Agent:             root.Agent,
		Project:           root.Project,
		TotalOutputTokens: root.TotalOutputTokens,
		PeakContextTokens: root.PeakContextTokens,
		HasTokenData:      root.HasTokenData,
	}
	for _, descendant := range descendants {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if descendant.RelationshipType == "subagent" {
			out.SubagentCount++
		}
		out.TotalOutputTokens += descendant.TotalOutputTokens
		if descendant.PeakContextTokens > out.PeakContextTokens {
			out.PeakContextTokens = descendant.PeakContextTokens
		}
		if descendant.HasTotalOutputTokens || descendant.HasPeakContextTokens {
			out.HasTokenData = true
		}
	}
	return out, nil
}

// usageRowBreakdownEntry renders one deduped usage row as a breakdown entry,
// tagging it with its session id when it did not come from the root.
func usageRowBreakdownEntry(
	row activity.UsageRow,
	rootID string,
	ordinal int,
	cost money.Money,
	priced bool,
) db.SessionUsageBreakdownEntry {
	// UsageRow carries the ordinal in its COALESCE(message_ordinal, -1)
	// convention; the breakdown entry uses a pointer with nil for "not
	// tied to a message".
	var messageOrdinal *int
	if row.MessageOrdinal >= 0 {
		v := int(row.MessageOrdinal)
		messageOrdinal = &v
	}
	label := db.SessionUsageBreakdownLabel(messageOrdinal, row.UsageSource)
	entry := db.SessionUsageBreakdownEntry{
		Ordinal:                  ordinal,
		MessageOrdinal:           messageOrdinal,
		Source:                   row.UsageSource,
		Label:                    label,
		Timestamp:                row.Timestamp,
		Model:                    row.Model,
		InputTokens:              row.InputTokens,
		OutputTokens:             row.OutputTokens,
		CacheCreationInputTokens: row.CacheCreationTokens,
		CacheReadInputTokens:     row.CacheReadTokens,
		WebSearchRequests:        row.WebSearchRequests,
		Cost:                     cost,
		HasCost:                  priced,
	}
	sourceSessionID := usageRowSourceSessionID(row)
	if sourceSessionID != rootID {
		entry.SubagentSessionID = sourceSessionID
	}
	return entry
}

// sortedKeys returns the set's keys sorted; never nil, so JSON renders "[]"
// rather than "null" (matching the per-backend session usage paths).
func sortedKeys(
	ctx context.Context, set map[string]struct{},
) ([]string, error) {
	out := make([]string, 0, len(set))
	for key := range set {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	if err := sortStringsContext(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

func sortStringsContext(ctx context.Context, values []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(values) < 2 {
		return nil
	}
	scratch := make([]string, len(values))
	source, target := values, scratch
	passes := 0
	for width := 1; width < len(values); width *= 2 {
		for left := 0; left < len(values); left += 2 * width {
			if err := ctx.Err(); err != nil {
				return err
			}
			middle := min(left+width, len(values))
			right := min(left+2*width, len(values))
			i, j := left, middle
			for k := left; k < right; k++ {
				if k&255 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				if j >= right || i < middle && source[i] <= source[j] {
					target[k] = source[i]
					i++
				} else {
					target[k] = source[j]
					j++
				}
			}
		}
		source, target = target, source
		passes++
		if width > len(values)/2 {
			break
		}
	}
	if passes%2 != 0 {
		for i := range values {
			if i&255 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			values[i] = source[i]
		}
	}
	return ctx.Err()
}
