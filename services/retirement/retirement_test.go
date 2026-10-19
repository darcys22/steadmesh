package retirement

import (
	"context"
	"errors"
	"github.com/darcys22/steadmesh/services/store"
	"io"
	"log/slog"
	"testing"
)

type failedCleanupStore struct{ listed bool }

func (s *failedCleanupStore) PurgeExpiredPoolState(context.Context, int) error {
	return errors.New("temporary retention cleanup failure")
}
func (s *failedCleanupStore) DueRetirements(context.Context, int) ([]store.DueRetirement, error) {
	s.listed = true
	return nil, nil
}
func (s *failedCleanupStore) FinishRetirement(context.Context, string, store.WorkRelease) (*store.Retirement, error) {
	return nil, nil
}
func TestRetentionFailureDoesNotBlockRetirement(t *testing.T) {
	s := &failedCleanupStore{}
	l := &Loop{Store: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	l.Tick(context.Background())
	if !s.listed {
		t.Fatal("private cleanup failure blocked retirement")
	}
}
