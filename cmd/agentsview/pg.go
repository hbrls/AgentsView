package main

import "go.kenn.io/agentsview/internal/postgres"

// pgReplica registers the PostgreSQL backend with the shared replica commands.
type pgReplica struct {
	postgres.Backend
}
