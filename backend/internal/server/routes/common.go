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
func RegisterCommonRoutes(r *gin.Engine, ready *readiness.Checker) {
	// 存活探针：进程在就是 200，不触碰任何依赖。
	r.GET(probe.LivenessPath, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// 就绪探针：PostgreSQL / Redis 并发探测，任一失败返回 503。
	r.GET(probe.ReadinessPath, ready.Handler())

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
