package repository

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 规则：源码里每一个 `.Encrypt(` 调用点都必须被 EncryptedStores（落库、可轮换）或
// NonRotatableEncryptSites（不落库 / 不在本密钥环上，附原因）之一声明。
//
// 守护的是"新增一处密文存储却没登记进轮换注册表"这一类错误：测试遍历源码而不是
// 点名文件，新加的调用点当天就会被拦下；同时反向检查声明里的文件确实还有调用点，
// 避免注册表里留下过期条目。
func TestEveryEncryptCallSiteIsRegistered(t *testing.T) {
	backendRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	// 密码学实现本身的内部调用不是"存储密文的位置"。
	skipDirs := map[string]string{
		"internal/pkg/secretcipher": "the cipher implementation itself",
	}

	found := map[string][]string{}
	for _, root := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(backendRoot, root), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				rel, _ := filepath.Rel(backendRoot, path)
				if _, skip := skipDirs[filepath.ToSlash(rel)]; skip {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(backendRoot, path)
			rel = filepath.ToSlash(rel)
			for _, line := range encryptCallLines(t, path) {
				found[rel] = append(found[rel], line)
			}
			return nil
		})
		require.NoError(t, err)
	}
	require.NotEmpty(t, found, "the scanner must see the known call sites; an empty result means it is looking in the wrong place")

	registered := map[string]string{}
	for _, store := range EncryptedStores {
		for _, file := range store.SourceFiles {
			require.NotContains(t, registered, file, "a source file must map to exactly one store")
			registered[file] = store.Name
		}
	}
	for file, reason := range NonRotatableEncryptSites {
		require.NotContains(t, registered, file, "a file cannot be both rotatable and non-rotatable")
		require.NotEmpty(t, reason, "non-rotatable sites must state why")
		registered[file] = "non-rotatable: " + reason
	}

	var unmapped []string
	for file, lines := range found {
		if _, ok := registered[file]; !ok {
			unmapped = append(unmapped, file+" ("+strings.Join(lines, ", ")+")")
		}
	}
	sort.Strings(unmapped)
	require.Empty(t, unmapped, "Encrypt() call sites whose ciphertext destination is not declared in EncryptedStores or NonRotatableEncryptSites (internal/repository/encryption_key_rotation.go):\n  %s",
		strings.Join(unmapped, "\n  "))

	var stale []string
	for file := range registered {
		if _, ok := found[file]; !ok {
			stale = append(stale, file)
		}
	}
	sort.Strings(stale)
	require.Empty(t, stale, "registered source files with no Encrypt() call left: remove them from the registry")
}

// encryptCallLines 返回文件里每个 `<expr>.Encrypt(` 调用的行号。走 AST 而不是
// 文本匹配，注释和字符串里的字样不算。
func encryptCallLines(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	require.NoError(t, err, path)
	var lines []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Encrypt" {
			return true
		}
		lines = append(lines, "line "+strconv.Itoa(fset.Position(call.Pos()).Line))
		return true
	})
	return lines
}
