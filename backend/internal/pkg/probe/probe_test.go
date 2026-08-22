//go:build unit

package probe

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 对外 URL 是编排器配置里的契约，这里钉死，改动必须是有意识的。
func TestProbePathsAreStableContracts(t *testing.T) {
	require.Equal(t, "/health", LivenessPath)
	require.Equal(t, "/readyz", ReadinessPath)
}

func TestPathsCoversEveryDeclaredProbe(t *testing.T) {
	declared := Paths()
	require.NotEmpty(t, declared)
	for _, p := range declared {
		require.True(t, IsPath(p), "declared probe path %q must be recognised", p)
		require.NotEmpty(t, p)
		require.Equal(t, byte('/'), p[0], "probe path %q must be absolute", p)
	}

	// 返回的是副本：调用方改写不能污染声明。
	declared[0] = "/tampered"
	require.NotEqual(t, "/tampered", Paths()[0])
}

func TestIsPathIsExactMatch(t *testing.T) {
	for _, p := range Paths() {
		require.False(t, IsPath(p+"/"), "trailing slash must not match %q", p)
		require.False(t, IsPath(p+"x"), "prefix must not match %q", p)
		require.False(t, IsPath("/api"+p), "suffix must not match %q", p)
	}
	require.False(t, IsPath(""))
	require.False(t, IsPath("/"))
}
