//go:build unit

package config

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// 文档里的环境变量表是运维照着抄的契约。issue #6 的形状：deploy/DOCKER.md 把 DATABASE_URL 标成必填，
// 而代码从未读过它——变量存在、值也正确，程序却照样连 localhost，排查无从下手。
//
// 这个测试让每一行文档都对应一个真实的读取点。读取点有三类，全部从声明推导而不是手写名单，
// 所以新增的配置键、新增的 os.Getenv、新增的 compose 插值当天就被覆盖：
//   - viper：setDefaults 注册的全部键经 EnvName 映射出的变量名（AutomaticEnv 只认注册过的键，
//     这与 TestConfigKeysAreEnvReachable 守的是同一条边界）；
//   - direct：后端源码里以字面量读取的变量——os.Getenv / os.LookupEnv、setup 包的 getEnvOrDefault /
//     getEnvIntOrDefault、viper.BindEnv 别名。这就是 `grep -rn 'os.Getenv' backend/` 的机械化版本；
//   - compose：deploy/docker-compose*.yml 用 ${NAME} 插值消费的 .env 变量。只对 README 生效：
//     它的表描述的是 .env（compose 再映射成容器变量），而 DOCKER.md 描述的是容器环境本身，
//     变量必须被程序读到。
var (
	docEnvRow            = regexp.MustCompile("^\\|\\s*`([A-Z][A-Z0-9_]*)`\\s*\\|")
	composeInterpolation = regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*)`)
	directEnvReads       = []*regexp.Regexp{
		regexp.MustCompile(`os\.(?:Getenv|LookupEnv)\("([A-Z][A-Z0-9_]*)"\)`),
		regexp.MustCompile(`getEnv(?:IntOrDefault|OrDefault)\("([A-Z][A-Z0-9_]*)"`),
		regexp.MustCompile(`viper\.BindEnv\("[^"]+",\s*"([A-Z][A-Z0-9_]*)"\)`),
	}
)

func repoRootDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "deploy")); err != nil {
		t.Fatalf("repo root %s has no deploy directory: %v", root, err)
	}
	return root
}

func viperEnvNames(t *testing.T) map[string]struct{} {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
	setDefaults()
	names := map[string]struct{}{}
	for _, key := range viper.AllKeys() {
		names[EnvName(key)] = struct{}{}
	}
	return names
}

// directEnvNames 返回后端源码（不含测试）里以字面量读取的环境变量名 → 读取位置。
func directEnvNames(t *testing.T, backendDir string) map[string]string {
	t.Helper()
	names := map[string]string{}
	err := filepath.WalkDir(backendDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(backendDir, path)
		for lineNo, line := range strings.Split(string(content), "\n") {
			for _, re := range directEnvReads {
				for _, match := range re.FindAllStringSubmatch(line, -1) {
					if _, seen := names[match[1]]; !seen {
						names[match[1]] = fmt.Sprintf("%s:%d", rel, lineNo+1)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", backendDir, err)
	}
	return names
}

// composeEnvNames 返回 deploy/docker-compose*.yml 插值消费的变量名 → 位置。
func composeEnvNames(t *testing.T, deployDir string) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(deployDir, "docker-compose*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no compose files under %s (err=%v)", deployDir, err)
	}
	names := map[string]string{}
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for lineNo, line := range strings.Split(string(content), "\n") {
			for _, match := range composeInterpolation.FindAllStringSubmatch(line, -1) {
				if _, seen := names[match[1]]; !seen {
					names[match[1]] = fmt.Sprintf("%s:%d", filepath.Base(file), lineNo+1)
				}
			}
		}
	}
	return names
}

func TestDocumentedEnvVarsAreRead(t *testing.T) {
	root := repoRootDir(t)
	viperNames := viperEnvNames(t)
	directNames := directEnvNames(t, filepath.Join(root, "backend"))
	composeNames := composeEnvNames(t, filepath.Join(root, "deploy"))

	docs := []struct {
		path         string
		allowCompose bool
	}{
		{path: "deploy/DOCKER.md", allowCompose: false},
		{path: "deploy/README.md", allowCompose: true},
	}

	var failures []string
	for _, doc := range docs {
		file, err := os.Open(filepath.Join(root, doc.path))
		if err != nil {
			t.Fatal(err)
		}
		rows := 0
		scanner := bufio.NewScanner(file)
		for lineNo := 1; scanner.Scan(); lineNo++ {
			match := docEnvRow.FindStringSubmatch(scanner.Text())
			if match == nil {
				continue
			}
			rows++
			name := match[1]
			if _, ok := viperNames[name]; ok {
				continue
			}
			if _, ok := directNames[name]; ok {
				continue
			}
			if _, ok := composeNames[name]; ok && doc.allowCompose {
				continue
			}
			hint := "nothing in the backend reads it: register a viper default for its key, read it with os.Getenv(\"" + name + "\"), or drop the row"
			if _, ok := composeNames[name]; ok {
				hint = "only " + composeNames[name] + " interpolates it; a container variable must be read by the backend itself"
			}
			failures = append(failures, fmt.Sprintf("%s:%d documents %s but %s", doc.path, lineNo, name, hint))
		}
		_ = file.Close()
		if err := scanner.Err(); err != nil {
			t.Fatal(err)
		}
		if rows == 0 {
			t.Fatalf("%s: no environment variable table rows found; the table format changed and this guard no longer sees it", doc.path)
		}
	}

	sort.Strings(failures)
	if len(failures) > 0 {
		t.Fatalf("%d documented environment variables are never read:\n  %s", len(failures), strings.Join(failures, "\n  "))
	}
}
