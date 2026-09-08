//go:build unit

package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/probe"
	"github.com/Wei-Shaw/sub2api/internal/server/readiness"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestSetupWizardStyleServerAnswersBothProbes 钉死"没有依赖检查项时两个探针都可用且 200"。
//
// 这正是安装向导阶段的形态：还没有数据库/Redis 配置，但 Pod 完全有能力提供服务——
// 它提供的就是向导本身。这两条路由过去只由主服务注册，向导 Pod 对 /readyz 返回 404，
// 于是配了就绪探针（deploy/README.md 推荐的做法）的部署里它永远 NotReady、
// Service 没有 endpoint，运维根本进不去此刻唯一要用的那个页面。
func TestSetupWizardStyleServerAnswersBothProbes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterProbeRoutes(r, readiness.NewChecker(5*time.Second))

	for _, path := range []string{probe.LivenessPath, probe.ReadinessPath} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			require.Equalf(t, http.StatusOK, w.Code,
				"%s 在没有依赖检查项时必须 200：向导 Pod 靠它才能进入 Ready", path)
		})
	}
}
