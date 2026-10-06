//go:build integration

// Package pgtest provides a disposable Postgres for integration tests. One
// container serves a test binary; each test gets its own database.
package pgtest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

var (
	once      sync.Once
	container *postgres.PostgresContainer
	adminURL  string
	startErr  error
	seq       atomic.Int64
)

// Main runs the package's tests and removes the container afterwards.
func Main(m *testing.M) {
	code := m.Run()
	if container != nil {
		_ = testcontainers.TerminateContainer(container)
	}
	os.Exit(code)
}

// URL returns the connection string of a fresh, empty database.
func URL(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	once.Do(func() {
		image := os.Getenv("STEADMESH_TEST_POSTGRES_IMAGE")
		if image == "" {
			image = "postgres:17-alpine"
		}
		container, startErr = postgres.Run(ctx, image,
			postgres.WithDatabase("steadmesh"), postgres.WithUsername("steadmesh"), postgres.WithPassword("steadmesh"),
			postgres.BasicWaitStrategies())
		if startErr == nil {
			adminURL, startErr = container.ConnectionString(ctx, "sslmode=disable")
		}
	})
	if startErr != nil {
		t.Fatalf("start postgres: %v", startErr)
	}
	name := fmt.Sprintf("t%d_%d", os.Getpid(), seq.Add(1))
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	return strings.Replace(adminURL, "/steadmesh?", "/"+name+"?", 1)
}
