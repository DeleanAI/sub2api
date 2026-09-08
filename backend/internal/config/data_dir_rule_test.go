//go:build unit

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNothingElseReadsDataDirEnv 遍历整个 backend 源码树，要求只有 ResolveDataDir
// 会读 DATA_DIR 环境变量。
//
// 这条规则原本只是 internal/config/data_dir.go 顶上的一句注释，而实际有三处各自读了
// 一遍、兜底还互不相同：配置文件搜索路径没有"容器里 /app/data 可写"这一档，日志路径
// 直接退到写死的 /app/data/logs，插件根目录退到 ./data。于是同一次部署里，运行期数据
// 在一个目录、日志在另一个、插件在第三个。写下来却没人执行的规则比没有更糟——
// 它让每个读到注释的人以为这件事已经被保证了。
func TestNothingElseReadsDataDirEnv(t *testing.T) {
	root := repoRootDir(t)
	backend := filepath.Join(root, "backend")

	// 唯一允许读它的地方。
	const ruleFile = "internal/config/data_dir.go"

	var offenders []string
	err := filepath.WalkDir(backend, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", "testdata", "ent", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(backend, path)
		if relErr != nil {
			return relErr
		}
		if filepath.ToSlash(rel) == ruleFile {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(body), "\n") {
			if strings.Contains(line, `"DATA_DIR"`) {
				offenders = append(offenders, filepath.ToSlash(rel)+":"+itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.Emptyf(t, offenders,
		"这些地方绕过了 ResolveDataDir 自己读 DATA_DIR，兜底一定会和它分叉：\n  %s\n"+
			"改为调用 config.ResolveDataDir / config.DefaultRuntimeDataDir。",
		strings.Join(offenders, "\n  "))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
