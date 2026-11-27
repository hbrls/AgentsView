package main

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction/filing"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/storage"
)

// pgReplica is the PostgreSQL replica as this binary registers it: the
// storage.Replica contract from internal/postgres plus the PostgreSQL-only
// HTTP capabilities `pg serve` adds on top of db.Store.
type pgReplica struct {
	postgres.Backend
}
