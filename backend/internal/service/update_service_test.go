//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type updateServiceCacheStub struct {
	data string
}

func (s *updateServiceCacheStub) GetUpdateInfo(context.Context) (string, error) {
	if s.data == "" {
		return "", errors.New("cache miss")
	}
	return s.data, nil
}

func (s *updateServiceCacheStub) SetUpdateInfo(_ context.Context, data string, _ time.Duration) error {
	s.data = data
	return nil
}

type updateServiceGitHubClientStub struct {
	release        *GitHubRelease
	recentReleases []*GitHubRelease
	recentErr      error
}

func (s *updateServiceGitHubClientStub) FetchLatestRelease(context.Context, string) (*GitHubRelease, error) {
	return s.release, nil
}

func (s *updateServiceGitHubClientStub) FetchRecentReleases(context.Context, string, int) ([]*GitHubRelease, error) {
	return s.recentReleases, s.recentErr
}

func (s *updateServiceGitHubClientStub) DownloadFile(context.Context, string, string, int64) error {
	panic("DownloadFile should not be called when no update is available")
}

func (s *updateServiceGitHubClientStub) FetchChecksumFile(context.Context, string) ([]byte, error) {
	panic("FetchChecksumFile should not be called when no update is available")
}

func TestUpdateServicePerformUpdateNoUpdateReturnsSentinel(t *testing.T) {
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{
			release: &GitHubRelease{
				TagName: "v0.1.132",
				Name:    "v0.1.132",
			},
		},
		"0.1.132",
		"release",
		allowAllInAppUpdatePolicy(),
	)

	err := svc.PerformUpdate(context.Background())

	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoUpdateAvailable))
	require.ErrorIs(t, err, ErrNoUpdateAvailable)
}

func newRollbackTestService(current string, releases []*GitHubRelease) *UpdateService {
	return NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{recentReleases: releases},
		current,
		"release",
		allowAllInAppUpdatePolicy(),
	)
}

func TestUpdateServiceListRollbackVersionsFiltersAndCaps(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.148", PublishedAt: "2026-07-09T00:00:00Z"},                       // newer than current: excluded
		{TagName: "v0.1.147", PublishedAt: "2026-07-08T00:00:00Z"},                       // current: excluded
		{TagName: "v0.1.146-rc1", PublishedAt: "2026-07-07T12:00:00Z", Prerelease: true}, // prerelease: excluded
		{TagName: "v0.1.146", PublishedAt: "2026-07-07T00:00:00Z"},
		{TagName: "v0.1.145", PublishedAt: "2026-07-06T00:00:00Z", Draft: true}, // draft: excluded
		{TagName: "v0.1.144", PublishedAt: "2026-07-05T00:00:00Z"},
		{TagName: "v0.1.144", PublishedAt: "2026-07-05T00:00:00Z"}, // duplicate: excluded
		{TagName: "v0.1.143", PublishedAt: "2026-07-04T00:00:00Z"},
		{TagName: "v0.1.142", PublishedAt: "2026-07-03T00:00:00Z"}, // beyond cap of 3: excluded
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, "0.1.146", versions[0].Version)
	require.Equal(t, "0.1.144", versions[1].Version)
	require.Equal(t, "0.1.143", versions[2].Version)
}

func TestUpdateServiceListRollbackVersionsSortsUnorderedInput(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.144"},
		{TagName: "v0.1.146"},
		{TagName: "v0.1.145"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, "0.1.146", versions[0].Version)
	require.Equal(t, "0.1.145", versions[1].Version)
	require.Equal(t, "0.1.144", versions[2].Version)
}

func TestUpdateServiceListRollbackVersionsEmptyWhenNoneOlder(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.147"},
		{TagName: "v0.1.148"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Empty(t, versions)
}

func TestUpdateServiceListRollbackVersionsPropagatesFetchError(t *testing.T) {
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{recentErr: errors.New("github unavailable")},
		"0.1.147",
		"release",
		allowAllInAppUpdatePolicy(),
	)

	_, err := svc.ListRollbackVersions(context.Background())

	require.Error(t, err)
	require.Contains(t, err.Error(), "github unavailable")
}

func TestUpdateServiceRollbackToVersionRejectsDisallowedTargets(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.148"},
		{TagName: "v0.1.147"},
		{TagName: "v0.1.146"},
		{TagName: "v0.1.145"},
		{TagName: "v0.1.144"},
		{TagName: "v0.1.143"},
		{TagName: "v0.1.142"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	for _, target := range []string{
		"",         // empty
		"0.1.147",  // current version
		"v0.1.147", // current version with prefix
		"0.1.148",  // newer than current
		"0.1.142",  // older than the 3 most recent
		"9.9.9",    // nonexistent
	} {
		err := svc.RollbackToVersion(context.Background(), target)
		require.ErrorIs(t, err, ErrRollbackVersionNotAllowed, "target %q should be rejected", target)
	}
}

func TestUpdateServiceRollbackToVersionAcceptsVPrefix(t *testing.T) {
	// No platform asset in the release: the target passes the allowlist check
	// and fails later at asset lookup, proving the version itself was accepted.
	releases := []*GitHubRelease{
		{TagName: "v0.1.147"},
		{TagName: "v0.1.146"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	err := svc.RollbackToVersion(context.Background(), "v0.1.146")

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrRollbackVersionNotAllowed)
	require.Contains(t, err.Error(), "no compatible release found")
}

func allowAllInAppUpdatePolicy() *InAppUpdatePolicy {
	return NewInAppUpdatePolicy(&stubLiveInstanceCounter{instances: singleInstance()}, writableProbeOK)
}

// panickingGitHubClient 证明拒绝发生在任何网络访问之前。
type panickingGitHubClient struct{}

func (panickingGitHubClient) FetchLatestRelease(context.Context, string) (*GitHubRelease, error) {
	panic("GitHub must not be contacted when the in-app update policy refuses")
}

func (panickingGitHubClient) FetchRecentReleases(context.Context, string, int) ([]*GitHubRelease, error) {
	panic("GitHub must not be contacted when the in-app update policy refuses")
}

func (panickingGitHubClient) DownloadFile(context.Context, string, string, int64) error {
	panic("nothing may be downloaded when the in-app update policy refuses")
}

func (panickingGitHubClient) FetchChecksumFile(context.Context, string) ([]byte, error) {
	panic("nothing may be downloaded when the in-app update policy refuses")
}

// binaryReplacingOperations 枚举所有会替换运行中二进制的入口；每一个都必须先过策略。
var binaryReplacingOperations = map[string]func(ctx context.Context, svc *UpdateService) error{
	"PerformUpdate":     func(ctx context.Context, svc *UpdateService) error { return svc.PerformUpdate(ctx) },
	"Rollback":          func(ctx context.Context, svc *UpdateService) error { return svc.Rollback(ctx) },
	"RollbackToVersion": func(ctx context.Context, svc *UpdateService) error { return svc.RollbackToVersion(ctx, "0.1.146") },
}

func TestUpdateServiceRefusesEveryBinaryReplacingOperationWhenPolicyBlocks(t *testing.T) {
	policies := map[string]*InAppUpdatePolicy{
		"multiple instances": NewInAppUpdatePolicy(&stubLiveInstanceCounter{instances: append(singleInstance(), InstanceInfo{ID: "peer", Hostname: "host-b"})}, writableProbeOK),
		"count unknown":      NewInAppUpdatePolicy(&stubLiveInstanceCounter{err: errors.New("redis down")}, writableProbeOK),
		"read-only exe dir":  NewInAppUpdatePolicy(&stubLiveInstanceCounter{instances: singleInstance()}, func() error { return errors.New("read-only file system") }),
		"nil policy":         nil,
	}

	for policyName, policy := range policies {
		for opName, op := range binaryReplacingOperations {
			t.Run(policyName+"/"+opName, func(t *testing.T) {
				svc := NewUpdateService(&updateServiceCacheStub{}, panickingGitHubClient{}, "0.1.147", "release", policy)

				err := op(context.Background(), svc)

				require.ErrorIs(t, err, ErrInAppUpdateDisabled)
				require.NotEmpty(t, infraerrors.Message(err), "refusal must carry an operator-facing reason")
			})
		}
	}
}

// 裁决随调用实时评估，不随 release 信息一起缓存。
func TestUpdateServiceCheckUpdateCarriesFreshInAppUpdateDecision(t *testing.T) {
	counter := &stubLiveInstanceCounter{instances: singleInstance()}
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{release: &GitHubRelease{TagName: "v0.1.148", Name: "v0.1.148"}},
		"0.1.147",
		"release",
		NewInAppUpdatePolicy(counter, writableProbeOK),
	)

	info, err := svc.CheckUpdate(context.Background(), true)
	require.NoError(t, err)
	require.True(t, info.HasUpdate)
	require.True(t, info.InAppUpdateAllowed)
	require.Empty(t, info.InAppUpdateBlockedCode)
	require.Empty(t, info.InAppUpdateBlockedReason)
	require.Equal(t, 1, info.LiveInstances)

	// 第二个实例上线；下一次（命中缓存的）检查必须立刻反映出来。
	counter.instances = append(singleInstance(), InstanceInfo{ID: "peer", Hostname: "host-b", Version: "0.1.147"})
	info, err = svc.CheckUpdate(context.Background(), false)
	require.NoError(t, err)
	require.True(t, info.Cached)
	require.False(t, info.InAppUpdateAllowed)
	require.Equal(t, string(InAppUpdateBlockMultipleInstances), info.InAppUpdateBlockedCode)
	require.Contains(t, info.InAppUpdateBlockedReason, "2 live instances")
	require.Equal(t, 2, info.LiveInstances)

	counter.err = errors.New("redis down")
	info, err = svc.CheckUpdate(context.Background(), false)
	require.NoError(t, err)
	require.False(t, info.InAppUpdateAllowed)
	require.Equal(t, string(InAppUpdateBlockInstanceCountUnknown), info.InAppUpdateBlockedCode)
	require.Equal(t, LiveInstancesUnknown, info.LiveInstances)
}
