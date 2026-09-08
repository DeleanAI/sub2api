//go:build unit

package config

import (
	"bufio"
	"fmt"
	"github.com/stretchr/testify/require"
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

// docEnvLiteral 匹配文档代码块里的 KEY=VALUE 赋值（含 compose 的 "- KEY=VALUE"）。
var docEnvLiteral = regexp.MustCompile(`^\s*(?:-\s+)?([A-Z][A-Z0-9_]{2,})=(.*)$`)

// docEnvPlaceholder 判断值是不是"读者必须自己替换"的占位记号。
//
// 只认 <...>、$(...)、${...}、... 这几种明确的占位语法。像
// change-me-to-64-hex-characters 这种看着像占位、实际会被原样粘贴的字符串
// 不算占位符——文档里出现的字面量，读者就是会照抄，所以它必须真的能用。
func docEnvPlaceholder(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	return strings.ContainsAny(value, "<>") ||
		strings.Contains(value, "$(") ||
		strings.Contains(value, "${") ||
		strings.Contains(value, "...")
}

// TestDocumentedEnvValuesAreAccepted 把文档代码块里的每一个字面量喂给真正的配置加载。
//
// 这条护栏针对的是真实发生过的形状：deploy/DOCKER.md 的 quickstart compose 片段里写着
// TOTP_ENCRYPTION_KEY=change-me-to-64-hex-characters，而它不是 64 位十六进制，
// 启动直接 fatal。读者照抄 quickstart 得到的是一个起不来的实例。
// 原来的护栏只检查"文档提到的变量有没有人读"，对值本身完全没有意见。
//
// 规则：文档里出现的字面量必须真的可用；要读者替换的东西请写成 <…> / $(…) / ${…}。
func TestDocumentedEnvValuesAreAccepted(t *testing.T) {
	root := repoRootDir(t)
	docs := []string{"deploy/DOCKER.md", "deploy/README.md", "deploy/.env.example"}

	// 一份能通过校验的最小配置，逐个变量在它之上覆盖，失败必然归因于被测的那一个。
	baseline := map[string]string{
		"SERVER_MODE":         "release",
		"JWT_SECRET":          strings.Repeat("j", 48),
		"TOTP_ENCRYPTION_KEY": strings.Repeat("ab", 32),
		"DATABASE_URL":        "postgres://u:p@127.0.0.1:5432/db?sslmode=disable",
		"REDIS_URL":           "redis://127.0.0.1:6379/0",
	}

	checked := 0
	for _, doc := range docs {
		raw, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			t.Fatal(err)
		}
		for lineNo, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") || strings.HasPrefix(strings.TrimSpace(line), "|") {
				continue
			}
			match := docEnvLiteral.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			name, value := match[1], strings.TrimSpace(match[2])
			value = strings.Trim(value, `"'`)
			if docEnvPlaceholder(value) {
				continue
			}
			if _, known := baseline[name]; !known {
				// 只覆盖 baseline 里的键：其余变量单独设一个值不足以断言整份配置有效，
				// 而误报会让这条护栏被人关掉。这几个正是格式最严、抄错代价最大的。
				continue
			}
			checked++
			t.Run(fmt.Sprintf("%s:%d/%s", filepath.Base(doc), lineNo+1, name), func(t *testing.T) {
				viper.Reset()
				t.Cleanup(viper.Reset)
				t.Setenv("CONFIG_FILE", "")
				t.Setenv("DATA_DIR", t.TempDir())
				for k, v := range baseline {
					t.Setenv(k, v)
				}
				t.Setenv(name, value)
				_, err := LoadForBootstrap()
				require.NoErrorf(t, err,
					"%s:%d 里的 %s=%s 会被配置校验拒绝：文档中的字面量读者会原样粘贴，"+
						"需要替换的值请写成 <…> / $(…) / ${…}", doc, lineNo+1, name, value)
			})
		}
	}
	require.Positivef(t, checked, "没有从文档代码块里扫到任何字面量，护栏和文档格式脱节了")
}
