//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeHeartbeatStore struct {
	mu         sync.Mutex
	heartbeats []InstanceInfo
	windows    []time.Duration
	removed    []InstanceInfo
	listed     []InstanceInfo

	heartbeatErr error
	listErr      error
	removeErr    error

	beat chan struct{}
}

func newFakeHeartbeatStore() *fakeHeartbeatStore {
	return &fakeHeartbeatStore{beat: make(chan struct{}, 64)}
}

func (s *fakeHeartbeatStore) Heartbeat(_ context.Context, inst InstanceInfo, _ time.Time, window time.Duration) error {
	s.mu.Lock()
	s.heartbeats = append(s.heartbeats, inst)
	s.windows = append(s.windows, window)
	err := s.heartbeatErr
	s.mu.Unlock()
	select {
	case s.beat <- struct{}{}:
	default:
	}
	return err
}

func (s *fakeHeartbeatStore) ListActive(context.Context, time.Time) ([]InstanceInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]InstanceInfo(nil), s.listed...), s.listErr
}

func (s *fakeHeartbeatStore) Remove(_ context.Context, inst InstanceInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removed = append(s.removed, inst)
	return s.removeErr
}

func (s *fakeHeartbeatStore) heartbeatCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.heartbeats)
}

func waitForBeats(t *testing.T, store *fakeHeartbeatStore, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-store.beat:
		case <-deadline:
			t.Fatalf("expected %d heartbeats, got %d", n, store.heartbeatCount())
		}
	}
}

func newTestRegistry(store InstanceHeartbeatStore) *InstanceRegistry {
	r := NewInstanceRegistry(store, "1.2.3")
	r.interval = 10 * time.Millisecond
	return r
}

func TestNewInstanceRegistryAssignsStableIdentity(t *testing.T) {
	r := newTestRegistry(newFakeHeartbeatStore())
	self := r.Self()

	require.NotEmpty(t, self.ID)
	require.NotEmpty(t, self.Hostname)
	require.Equal(t, "1.2.3", self.Version)
	require.Equal(t, self, r.Self(), "identity must not change over the process lifetime")
	require.NotEqual(t, self.ID, newTestRegistry(newFakeHeartbeatStore()).Self().ID, "each process gets its own id")
}

func TestInstanceRegistryHeartbeatsImmediatelyThenPeriodicallyAndDeregistersOnStop(t *testing.T) {
	store := newFakeHeartbeatStore()
	r := newTestRegistry(store)

	r.Start()
	waitForBeats(t, store, 3)

	r.Stop()
	stoppedAt := store.heartbeatCount()
	time.Sleep(5 * r.interval)
	require.Equal(t, stoppedAt, store.heartbeatCount(), "no heartbeats after Stop")

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Equal(t, []InstanceInfo{r.Self()}, store.removed, "Stop must deregister exactly once")
	for i, beat := range store.heartbeats {
		require.Equal(t, r.Self(), beat)
		require.Equal(t, instanceLivenessWindow, store.windows[i])
	}
}

func TestInstanceRegistryStartAndStopAreIdempotent(t *testing.T) {
	store := newFakeHeartbeatStore()
	r := newTestRegistry(store)

	r.Start()
	r.Start()
	waitForBeats(t, store, 1)
	r.Stop()
	r.Stop()

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.removed, 1)
}

func TestInstanceRegistryStopWithoutStartDoesNotDeregisterOrBlock(t *testing.T) {
	store := newFakeHeartbeatStore()
	r := newTestRegistry(store)

	done := make(chan struct{})
	go func() {
		r.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop without Start must not block")
	}
	require.Empty(t, store.removed)
}

func TestInstanceRegistryNilIsSafe(t *testing.T) {
	var r *InstanceRegistry
	require.NotPanics(t, func() {
		r.Start()
		r.Stop()
	})
}

// ActiveInstances 永远包含自己：首个心跳尚未落地或注册表被清空时，0 个实例是错的。
func TestInstanceRegistryActiveInstancesAlwaysIncludesSelf(t *testing.T) {
	store := newFakeHeartbeatStore()
	r := newTestRegistry(store)
	peer := InstanceInfo{ID: "peer", Hostname: "host-b", Version: "1.2.3"}

	instances, err := r.ActiveInstances(context.Background())
	require.NoError(t, err)
	require.Len(t, instances, 1)
	require.Equal(t, r.Self().ID, instances[0].ID)
	require.False(t, instances[0].LastSeen.IsZero())

	store.listed = []InstanceInfo{peer}
	instances, err = r.ActiveInstances(context.Background())
	require.NoError(t, err)
	require.Len(t, instances, 2)

	store.listed = []InstanceInfo{peer, r.Self()}
	instances, err = r.ActiveInstances(context.Background())
	require.NoError(t, err)
	require.Len(t, instances, 2, "self must not be duplicated")
}

func TestInstanceRegistryActiveInstancesIsUnknownWhenStoreFails(t *testing.T) {
	store := newFakeHeartbeatStore()
	store.listErr = errors.New("redis: connection refused")
	r := newTestRegistry(store)

	instances, err := r.ActiveInstances(context.Background())

	require.Error(t, err)
	require.ErrorContains(t, err, "connection refused")
	require.Nil(t, instances, "an error must never come with a count that could be mistaken for 1")
}

// 心跳失败/恢复按翻转记账，保证日志只在状态变化时出现。
func TestInstanceRegistryTracksHeartbeatFailureTransitions(t *testing.T) {
	store := newFakeHeartbeatStore()
	store.heartbeatErr = errors.New("redis down")
	r := newTestRegistry(store)

	r.Start()
	defer r.Stop()
	waitForBeats(t, store, 1)
	require.True(t, r.heartbeatFailing.Load())

	store.mu.Lock()
	store.heartbeatErr = nil
	store.mu.Unlock()
	require.Eventually(t, func() bool { return !r.heartbeatFailing.Load() }, 2*time.Second, 5*time.Millisecond)
}
