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

	found := map[string][]encryptCallSite{}
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
			found[rel] = append(found[rel], encryptCallSites(t, path)...)
			return nil
		})
		require.NoError(t, err)
	}
	require.NotEmpty(t, found, "the scanner must see the known call sites; an empty result means it is looking in the wrong place")

	// 归属键可以是整个文件（该文件所有 Encrypt 调用都写往同一处），也可以是
	// "path/file.go:Type.Method" 这样的单个函数——一个文件里同时存在"落库要轮换"和
	// "不落库"两类密文时（如插件管理器），只能按调用点归属。按函数而不是按行号：
	// 行号会随无关改动漂移（合并一次上游就全体误报），函数名只在调用点真正搬家时才变。
	registered := map[string]string{}
	claim := func(key, owner string) {
		require.NotContains(t, registered, key, "an Encrypt call site must map to exactly one owner: %s", key)
		registered[key] = owner
	}
	for _, store := range EncryptedStores {
		for _, site := range store.SourceFiles {
			claim(site, store.Name)
		}
	}
	for site, reason := range NonRotatableEncryptSites {
		require.NotEmpty(t, reason, "non-rotatable sites must state why")
		claim(site, "non-rotatable: "+reason)
	}

	var unmapped []string
	used := map[string]bool{}
	for file, sites := range found {
		// 整文件归属只在该文件确实只有一个调用点时成立。
		//
		// 否则往一个"已登记"的文件里再加一个 Encrypt（例如给 users 表加一列
		// totp_recovery_codes_encrypted）会被这条整文件登记一并遮住，护栏保持全绿——
		// 而新那一处的密文根本不会被轮换，换钥匙之后就永远读不出来了。
		if _, ok := registered[file]; ok {
			require.Lenf(t, sites, 1,
				"%s 有 %d 个 Encrypt 调用点，却只按整个文件登记了一次：\n"+
					"  一个文件里出现多个调用点时必须逐个按 \"%s:<函数>\" 登记，"+
					"否则新增的那一处会被这条整文件登记遮住、静默不参与轮换。\n"+
					"  调用点：%s", file, len(sites), file, describeEncryptCallSites(sites))
			used[file] = true
			continue
		}
		byFunc := map[string][]encryptCallSite{}
		for _, site := range sites {
			byFunc[site.Func] = append(byFunc[site.Func], site)
		}
		for fn, inFunc := range byFunc {
			key := file + ":" + fn
			if _, ok := registered[key]; !ok {
				unmapped = append(unmapped, key+" ("+describeEncryptCallSites(inFunc)+")")
				continue
			}
			// 同一条理由在函数粒度上再成立一次：一条函数级登记只能替一个调用点担保，
			// 同一函数里再加一处写别处的 Encrypt 同样会被遮住。真需要时把各自的去向
			// 拆进各自的函数，再分别登记。
			require.Lenf(t, inFunc, 1,
				"%s 里有 %d 个 Encrypt 调用点（%s），一条函数级登记只能对应一个：把不同去向拆进不同函数后分别登记",
				key, len(inFunc), describeEncryptCallSites(inFunc))
			used[key] = true
		}
	}
	sort.Strings(unmapped)
	require.Empty(t, unmapped, "Encrypt() call sites whose ciphertext destination is not declared in EncryptedStores or NonRotatableEncryptSites (internal/repository/encryption_key_rotation.go):\n  %s",
		strings.Join(unmapped, "\n  "))

	var stale []string
	for site := range registered {
		if !used[site] {
			stale = append(stale, site)
		}
	}
	sort.Strings(stale)
	require.Empty(t, stale, "registered call sites with no Encrypt() call left (a renamed or moved function counts as stale): update or remove them in the registry")
}

// encryptCallSite 是一个 `<expr>.Encrypt(` 调用及其所在的顶层函数。
type encryptCallSite struct {
	Line int
	// Func 形如 "Type.Method"（方法）或 "Func"（普通函数）；包级变量初始化里的调用记为 "<package>"。
	Func string
}

// encryptCallSites 返回文件里每个 `<expr>.Encrypt(` 调用及其所在函数。走 AST 而不是
// 文本匹配，注释和字符串里的字样不算。
func encryptCallSites(t *testing.T, path string) []encryptCallSite {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	require.NoError(t, err, path)
	var sites []encryptCallSite
	for _, decl := range file.Decls {
		owner := "<package>"
		if fn, ok := decl.(*ast.FuncDecl); ok {
			owner = funcDeclName(fn)
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Encrypt" {
				return true
			}
			sites = append(sites, encryptCallSite{Line: fset.Position(call.Pos()).Line, Func: owner})
			return true
		})
	}
	return sites
}

// funcDeclName 返回 "Type.Method"（方法，指针接收者去掉 *、泛型接收者去掉类型参数）或 "Func"。
func funcDeclName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	switch r := recv.(type) {
	case *ast.IndexExpr:
		recv = r.X
	case *ast.IndexListExpr:
		recv = r.X
	}
	if id, ok := recv.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

func describeEncryptCallSites(sites []encryptCallSite) string {
	parts := make([]string, 0, len(sites))
	for _, site := range sites {
		parts = append(parts, site.Func+" 第 "+strconv.Itoa(site.Line)+" 行")
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
