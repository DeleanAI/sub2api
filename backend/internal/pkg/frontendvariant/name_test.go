//go:build unit

package frontendvariant

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// nameSamples 是 Go 侧与 JS 侧共用的对账单，见 frontend/scripts/variant-name.samples.json。
// 这里不硬编码任何一个名字：样本表加一行，两侧当天就都被覆盖。
type nameSamples struct {
	Valid   []string `json:"valid"`
	Invalid []string `json:"invalid"`
}

func loadNameSamples(t *testing.T) nameSamples {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// backend/internal/pkg/frontendvariant -> 仓库根
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", ".."))
	path := filepath.Join(root, "frontend", "scripts", "variant-name.samples.json")
	raw, err := os.ReadFile(path) //nolint:gosec // 固定的仓库内路径
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var samples nameSamples
	if err := json.Unmarshal(raw, &samples); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(samples.Valid) == 0 || len(samples.Invalid) == 0 {
		t.Fatalf("%s must carry both valid and invalid samples; got %d/%d",
			path, len(samples.Valid), len(samples.Invalid))
	}
	return samples
}

func TestValidateNameMatchesSharedSamples(t *testing.T) {
	samples := loadNameSamples(t)
	for _, name := range samples.Valid {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil (sample table says valid)", name, err)
		}
	}
	for _, name := range samples.Invalid {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error (sample table says invalid)", name)
		}
	}
}

func TestDefaultVariantNameIsValid(t *testing.T) {
	if err := ValidateName(Default); err != nil {
		t.Fatalf("默认变体名 %q 必须符合规则，否则默认配置直接启动失败: %v", Default, err)
	}
}
