//go:build integration

// Package pgtest provides a disposable Postgres for integration tests. One
// container serves a test binary; each test gets its own database.
package pgtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
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
		if local := os.Getenv("STEADMESH_TEST_DATABASE_URL"); local != "" {
			u, err := url.Parse(local)
			if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") || u.Path != "/steadmesh_test_admin" {
				startErr = fmt.Errorf("STEADMESH_TEST_DATABASE_URL must point to loopback /steadmesh_test_admin, an explicitly disposable database")
				return
			}
			adminURL = local
			return
		}
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
	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, adminURL)
		if err != nil {
			t.Errorf("connect for test DB cleanup: %v", err)
			return
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop disposable test DB: %v", err)
		}
	})
	return u.String()
}
