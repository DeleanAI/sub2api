// Package probe 是进程级探针端点的唯一声明处。
//
// 探针路径有三个消费方：路由注册（真正的 handler）、访问日志中间件（探针被编排器
// 高频命中，不写访问日志）、内嵌前端中间件（探针必须绕过 SPA 的 index.html 兜底，
// 否则编排器拿到的是一段 HTML 而不是状态码）。三处都从这里读取，新增一个探针只改
// 这一个文件，消费方与遍历式测试自动覆盖。
package probe

const (
	// LivenessPath 只回答「进程还活着吗」。它永远不依赖外部依赖：数据库抖动时如果
	// liveness 一起失败，编排器会重启所有副本，把可恢复的故障放大成 crash loop。
	LivenessPath = "/health"

	// ReadinessPath 回答「这个实例现在能不能服务」。依赖不可达时返回 503，编排器据此
	// 把实例从负载均衡中摘掉，而不是重启它。
	ReadinessPath = "/readyz"
)

// paths 按声明顺序列出全部探针路径。
var paths = [...]string{LivenessPath, ReadinessPath}

// Paths 返回全部探针路径的副本，供消费方与遍历式测试使用。
func Paths() []string {
	out := make([]string, len(paths))
	copy(out, paths[:])
	return out
}

// IsPath 判断 path 是否恰好是某个探针端点（精确匹配，不做前缀匹配）。
func IsPath(path string) bool {
	for _, p := range paths {
		if path == p {
			return true
		}
	}
	return false
}
