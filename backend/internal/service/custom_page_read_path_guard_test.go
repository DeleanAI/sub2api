//go:build unit

package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"
)

// customPageRequestPathMethods 是请求路径上的读取方法：一次页面/附件请求经过的
// CustomPageService 方法。它们必须完全走数据库。
//
// 声明在这里而不是"整份文件不许 import os"：同一个文件里还有一次性的磁盘导入器，
// 它当然要读盘，于是文件级的护栏在这一层结构性失效——handler 侧的 import 护栏
// 挡不住"把 os.ReadFile 加进 CustomPageService.GetPage"，因为那个文件早就 import 了
// os/io.fs/path.filepath。按方法查，才查到了真正要保护的东西。
var customPageRequestPathMethods = []string{"GetPage", "ListPages", "GetAsset", "ListAssets"}

// TestCustomPageReadPathNeverTouchesTheFilesystem 逐方法检查请求路径上的读取实现，
// 递归展开它们调用的同包私有方法，要求整条链上没有文件系统调用。
func TestCustomPageReadPathNeverTouchesTheFilesystem(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "custom_page_service.go", nil, 0)
	require.NoError(t, err)

	// 收集本文件里 CustomPageService 的所有方法体，供递归展开。
	bodies := map[string]*ast.FuncDecl{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil {
			continue
		}
		bodies[fn.Name.Name] = fn
	}
	for _, name := range customPageRequestPathMethods {
		require.Containsf(t, bodies, name, "custom_page_service.go 里找不到方法 %s：护栏和源码脱节了", name)
	}

	forbidden := map[string]struct{}{"os": {}, "fs": {}, "filepath": {}, "ioutil": {}}
	seen := map[string]bool{}
	var walk func(t *testing.T, entry, name string)
	walk = func(t *testing.T, entry, name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		fn, ok := bodies[name]
		if !ok {
			return
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := fun.X.(*ast.Ident); ok {
					if _, bad := forbidden[pkg.Name]; bad {
						require.Failf(t, "读取路径碰了文件系统",
							"%s 经 %s 调用了 %s.%s：自定义页面一律从数据库读，"+
								"磁盘目录只在一次性导入器里出现（issue #7 的根因）",
							entry, name, pkg.Name, fun.Sel.Name)
					}
				}
				// s.someHelper(...) —— 展开同包私有方法。
				if _, ok := fun.X.(*ast.Ident); ok && fun.Sel != nil {
					walk(t, entry, fun.Sel.Name)
				}
			case *ast.Ident:
				walk(t, entry, fun.Name)
			}
			return true
		})
	}
	for _, name := range customPageRequestPathMethods {
		seen = map[string]bool{}
		walk(t, name, name)
	}
}
