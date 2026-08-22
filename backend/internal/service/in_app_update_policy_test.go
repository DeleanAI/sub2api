//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type stubLiveInstanceCounter struct {
	instances []InstanceInfo
	err       error
}

func (s *stubLiveInstanceCounter) ActiveInstances(context.Context) ([]InstanceInfo, error) {
	return s.instances, s.err
}

func writableProbeOK() error { return nil }

func singleInstance() []InstanceInfo {
	return []InstanceInfo{{ID: "self", Hostname: "host-a", Version: "1.0.0"}}
}

// 三类阻断原因逐一验证：裁决字段、409 错误的 code/reason/message/metadata 必须一致。
func TestInAppUpdatePolicyBlocks(t *testing.T) {
	cases := []struct {
		name         string
		counter      LiveInstanceCounter
		probe        func() error
		wantCode     InAppUpdateBlockCode
		wantLive     int
		wantContains []string
	}{
		{
			name: "multiple live instances",
			counter: &stubLiveInstanceCounter{instances: []InstanceInfo{
				{ID: "self", Hostname: "host-b", Version: "1.0.1"},
				{ID: "peer-1", Hostname: "host-a", Version: "1.0.0"},
				{ID: "peer-2", Hostname: "host-c", Version: "1.0.1"},
			}},
			probe:        writableProbeOK,
			wantCode:     InAppUpdateBlockMultipleInstances,
			wantLive:     3,
			wantContains: []string{"3 live instances", "host-a@1.0.0, host-b@1.0.1, host-c@1.0.1"},
		},
		{
			name:         "instance count unknown",
			counter:      &stubLiveInstanceCounter{err: errors.New("dial tcp 10.0.0.9:6379: connect: connection refused")},
			probe:        writableProbeOK,
			wantCode:     InAppUpdateBlockInstanceCountUnknown,
			wantLive:     LiveInstancesUnknown,
			wantContains: []string{"unknown", "connection refused"},
		},
		{
			name:         "executable directory not writable",
			counter:      &stubLiveInstanceCounter{instances: singleInstance()},
			probe:        func() error { return errors.New("open /app/.sub2api-write-check-1: read-only file system") },
			wantCode:     InAppUpdateBlockExecutableNotWritable,
			wantLive:     1,
			wantContains: []string{"read-only file system"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := NewInAppUpdatePolicy(tc.counter, tc.probe)

			decision := policy.Evaluate(context.Background())
			require.False(t, decision.Allowed)
			require.Equal(t, tc.wantCode, decision.BlockCode)
			require.Equal(t, tc.wantLive, decision.LiveInstances)
			for _, want := range tc.wantContains {
				require.Contains(t, decision.BlockReason, want)
			}
			require.Contains(t, decision.BlockReason, upgradeByImageHint, "every refusal must point at image-based upgrades")

			err := policy.Require(context.Background())
			require.ErrorIs(t, err, ErrInAppUpdateDisabled)
			require.Equal(t, http.StatusConflict, infraerrors.Code(err))
			require.Equal(t, "IN_APP_UPDATE_DISABLED", infraerrors.Reason(err))
			require.Equal(t, decision.BlockReason, infraerrors.Message(err))
			require.Equal(t, string(tc.wantCode), infraerrors.FromError(err).Metadata["block_code"])
		})
	}
}

func TestInAppUpdatePolicyAllowsSingleWritableInstance(t *testing.T) {
	policy := NewInAppUpdatePolicy(&stubLiveInstanceCounter{instances: singleInstance()}, writableProbeOK)

	decision := policy.Evaluate(context.Background())

	require.True(t, decision.Allowed)
	require.Empty(t, decision.BlockCode)
	require.Empty(t, decision.BlockReason)
	require.Equal(t, 1, decision.LiveInstances)
	require.NoError(t, policy.Require(context.Background()))
}

// 拓扑先于文件系统：已经因为多实例被拒时不再碰磁盘。
func TestInAppUpdatePolicyChecksTopologyBeforeFilesystem(t *testing.T) {
	probeCalled := false
	policy := NewInAppUpdatePolicy(
		&stubLiveInstanceCounter{instances: append(singleInstance(), InstanceInfo{ID: "peer", Hostname: "host-b"})},
		func() error { probeCalled = true; return nil },
	)

	decision := policy.Evaluate(context.Background())

	require.Equal(t, InAppUpdateBlockMultipleInstances, decision.BlockCode)
	require.False(t, probeCalled)
}

// 没有策略就没有「允许」：nil 策略与注册表不可用同样 fail closed。
func TestInAppUpdatePolicyNilFailsClosed(t *testing.T) {
	var policy *InAppUpdatePolicy

	decision := policy.Evaluate(context.Background())
	require.False(t, decision.Allowed)
	require.Equal(t, InAppUpdateBlockInstanceCountUnknown, decision.BlockCode)
	require.Equal(t, LiveInstancesUnknown, decision.LiveInstances)

	err := policy.Require(context.Background())
	require.ErrorIs(t, err, ErrInAppUpdateDisabled)
}

func TestProbeDirWritable(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, probeDirWritable(dir))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "probe must not leave its temp file behind")

	require.Error(t, probeDirWritable(filepath.Join(dir, "does-not-exist")))
}
