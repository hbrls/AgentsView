package server_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func TestMachinesExposeLabelsWithoutChangingFilterKeys(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	te := setup(t, func(c *config.Config) { c.InstallationID = id })
	_, err := te.db.EnsureInstallationIdentity(t.Context(), id)
	require.NoError(t, err)
	te.seedSession(t, "current", "project", 5, func(s *db.Session) {
		s.Machine = id
		s.UserMessageCount = 3
	})
	te.seedSession(t, "historical", "project", 5, func(s *db.Session) {
		s.Machine = "old-host"
		s.UserMessageCount = 3
	})
	require.NoError(t, te.db.SetSyncState(t.Context(), "machine_label:"+id, "Laptop"))
	require.NoError(t, te.db.SetSyncState(t.Context(), "machine_alias:old-owner", id))
	w := te.get(t, "/api/v1/machines")
	assertStatus(t, w, http.StatusOK)
	resp := decode[struct {
		Machines []string          `json:"machines"`
		Labels   map[string]string `json:"machine_labels"`
		Aliases  map[string]string `json:"machine_aliases"`
	}](t, w)
	assert.ElementsMatch(t, []string{id, "old-host"}, resp.Machines)
	assert.Equal(t, map[string]string{id: "Laptop"}, resp.Labels)
	assert.Equal(t, map[string]string{"old-owner": id, "local": id}, resp.Aliases)
	for _, machine := range []string{id, "old-owner", "local", "old-owner," + id} {
		for _, path := range []string{"/api/v1/sessions", "/api/v1/sessions/sidebar-index"} {
			w = te.get(t, path+"?machine="+machine)
			assertStatus(t, w, http.StatusOK)
			page := decode[db.SessionPage](t, w)
			require.Len(t, page.Sessions, 1)
			assert.Equal(t, "current", page.Sessions[0].ID)
		}
		w = te.get(t, "/api/v1/analytics/summary?from="+tsSeed[:10]+"&to="+tsSeed[:10]+"&machine="+machine)
		assertStatus(t, w, http.StatusOK)
		assert.Equal(t, 1, decode[db.AnalyticsSummary](t, w).TotalSessions)
		w = te.get(t, "/api/v1/activity/report?preset=day&date="+tsSeed[:10]+"&timezone=UTC&machine="+machine)
		assertStatus(t, w, http.StatusOK)
		assert.Equal(t, 1, decode[activity.Report](t, w).Totals.Sessions)
	}
	w = te.get(t, "/api/v1/sessions?machine=Laptop")
	assertStatus(t, w, http.StatusOK)
	assert.Empty(t, decode[db.SessionPage](t, w).Sessions)
	w = te.upload(t, "imported.jsonl", claudeTranscriptWithMessageCount(4), "project=project&machine=old-owner")
	assertStatus(t, w, http.StatusOK)
	assert.Equal(t, "old-owner", decode[struct {
		Machine string `json:"machine"`
	}](t, w).Machine)
}
