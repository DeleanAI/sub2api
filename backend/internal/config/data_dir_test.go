package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDataDirPrefersEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", "  "+dir+"  ")

	if got := ResolveDataDir("./fallback"); got != dir {
		t.Fatalf("ResolveDataDir() = %q, want DATA_DIR %q", got, dir)
	}
}

func TestResolveDataDirFallsBackToCallerValue(t *testing.T) {
	t.Setenv("DATA_DIR", "")
	if _, err := os.Stat(containerDataDir); err == nil {
		t.Skipf("%s exists on this machine; the container branch takes precedence", containerDataDir)
	}

	if got := ResolveDataDir("./custom-fallback"); got != "./custom-fallback" {
		t.Fatalf("ResolveDataDir() = %q, want caller fallback", got)
	}
}

func TestDefaultRuntimeDataDirIsAbsoluteAndFollowsEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)

	got := DefaultRuntimeDataDir()
	if !filepath.IsAbs(got) {
		t.Fatalf("DefaultRuntimeDataDir() = %q, want absolute path", got)
	}
	if got != dir {
		t.Fatalf("DefaultRuntimeDataDir() = %q, want %q", got, dir)
	}
}

func TestNormalizeRuntimeDataDirResolvesRelativeAgainstCWDOnce(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}

	got := normalizeRuntimeDataDir("./data")
	if got != filepath.Join(cwd, "data") {
		t.Fatalf("normalizeRuntimeDataDir(./data) = %q, want %q", got, filepath.Join(cwd, "data"))
	}

	t.Setenv("DATA_DIR", t.TempDir())
	if got := normalizeRuntimeDataDir("   "); got != os.Getenv("DATA_DIR") {
		t.Fatalf("normalizeRuntimeDataDir(blank) = %q, want DATA_DIR fallback %q", got, os.Getenv("DATA_DIR"))
	}
}

// 派生目录必须挂在同一个数据目录下，而不是各自再解析一次：这是 issue #7 里
// "pages 跟 DATA_DIR、public 跟 CWD" 两套口径分叉的根因。
func TestDerivedDirsHangOffTheSameDataDir(t *testing.T) {
	base := filepath.Join(string(filepath.Separator), "srv", "sub2api-data")

	if got := StaticOverrideDir(base); got != filepath.Join(base, "public") {
		t.Fatalf("StaticOverrideDir = %q", got)
	}
	if got := CustomPagesImportDir(base); got != filepath.Join(base, "pages") {
		t.Fatalf("CustomPagesImportDir = %q", got)
	}
}

func TestLoadMakesPricingDataDirAbsolute(t *testing.T) {
	resetViperWithJWTSecret(t)
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	t.Setenv("PRICING_DATA_DIR", "./relative-data")

	cfg, err := LoadForBootstrap()
	if err != nil {
		t.Fatalf("LoadForBootstrap: %v", err)
	}
	if !filepath.IsAbs(cfg.Pricing.DataDir) {
		t.Fatalf("Pricing.DataDir = %q, want absolute", cfg.Pricing.DataDir)
	}
	if filepath.Base(cfg.Pricing.DataDir) != "relative-data" {
		t.Fatalf("Pricing.DataDir = %q, want configured relative dir resolved against CWD", cfg.Pricing.DataDir)
	}
}

func TestLoadDefaultsPricingDataDirToDataDirEnv(t *testing.T) {
	resetViperWithJWTSecret(t)
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)

	cfg, err := LoadForBootstrap()
	if err != nil {
		t.Fatalf("LoadForBootstrap: %v", err)
	}
	if cfg.Pricing.DataDir != dir {
		t.Fatalf("Pricing.DataDir = %q, want DATA_DIR %q", cfg.Pricing.DataDir, dir)
	}
}
