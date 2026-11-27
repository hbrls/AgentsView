// Package backendcontract centralizes the compile-time checks that every
// storage backend implements its role contract. A backend that omits a method
// fails `go build` here rather than at runtime.
//
// Roles are defined in internal/storage: the SQLite archive, remote replicas,
// and the derived DuckDB mirror. Every backend also implements db.Store, the
// server-facing read-and-curation surface.
package backendcontract

import (
	"go.kenn.io/agentsview/internal/db"
	postgresstore "go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/storage"
)

// Every backend serves the HTTP read-and-curation surface.
var (
	_ db.Store = (*db.DB)(nil)
	_ db.Store = (*postgresstore.Store)(nil)
	_ db.Store = (*postgresstore.HostedStore)(nil)
)

// Remote replicas: push target and read-only serve.
var (
	_ storage.Replica      = postgresstore.Backend{}
	_ storage.Pusher       = (*postgresstore.Sync)(nil)
	_ storage.ReplicaStore = (*postgresstore.Store)(nil)
)

// Replicas that serve semantic search over pushed embeddings.
var (
	_ storage.VectorSearchProvider = postgresstore.Backend{}
	_ storage.VectorSearchStore    = (*postgresstore.Store)(nil)
)

// The archive supplies push watermarks to replicas.
var _ storage.SyncStateStore = (*db.DB)(nil)
