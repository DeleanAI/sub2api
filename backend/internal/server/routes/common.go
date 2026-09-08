package routes

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/probe"
	"github.com/Wei-Shaw/sub2api/internal/server/readiness"

	"github.com/gin-gonic/gin"
)

// RegisterCommonRoutes 注册通用路由（健康检查、就绪探针、状态等）。
//
// liveness 与 readiness 必须独立失败：前者只看进程，后者看依赖。把两者合成一个端点
// 意味着数据库抖动时编排器会重启每一个副本。
// RegisterProbeRoutes 只注册两个探针。安装向导服务器也必须注册它们：
//
// 向导阶段的 Pod 完全有能力提供服务——它提供的就是向导本身。但这两条路由过去只由
// 主服务注册，于是向导 Pod 对 /readyz 返回 404，配了就绪探针（deploy/README.md 推荐）
// 的部署会让它永远 NotReady、Service 没有 endpoint，运维根本进不去向导页面，
// 而向导正是他们此刻要用的东西。
func RegisterProbeRoutes(r *gin.Engine, ready *readiness.Checker) {
	// 存活探针：进程在就是 200，不触碰任何依赖。
	r.GET(probe.LivenessPath, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// 就绪探针：向导模式下 checker 没有依赖检查项，恒 200——此时"能不能服务"
	// 的答案就是"能，提供向导"。主服务传入的 checker 会并发探测 PostgreSQL / Redis。
	r.GET(probe.ReadinessPath, ready.Handler())
}

func RegisterCommonRoutes(r *gin.Engine, ready *readiness.Checker) {
	RegisterProbeRoutes(r, ready)

	// Claude Code 遥测日志（忽略，直接返回200）
	r.POST("/api/event_logging/batch", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	// Setup status endpoint (always returns needs_setup: false in normal mode)
	// This is used by the frontend to detect when the service has restarted after setup
	r.GET("/setup/status", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"code": 0,
			"data": gin.H{
				"needs_setup": false,
				"step":        "completed",
			},
		})
	})
}
