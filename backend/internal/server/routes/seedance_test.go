package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSeedanceNativeRoutes(t *testing.T) {
	router := newGatewayRoutesTestRouter()
	for _, prefix := range []string{"/api/v3", "/v3", "/v1", ""} {
		for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
			path := prefix + "/contents/generations/tasks"
			if method != http.MethodPost {
				path += "/task-1"
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(`{"model":"seedance","content":[{"type":"text","text":"waves"}]}`)))
			require.NotEqual(t, http.StatusNotFound, w.Code, method+" "+path)
		}
	}
}

// 素材库挂在站点根上：POST /?Action=… 必须到达素材库处理器（这里依赖未装配，回的是它自己的方舟格式错误，
// 而不是 404 或页面）。
func TestSeedanceAssetRouteReachesHandler(t *testing.T) {
	w := httptest.NewRecorder()
	newGatewayRoutesTestRouter(service.PlatformOpenAI).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/?Action=ListAssetGroups&Version=2024-01-01", strings.NewReader(`{"Filter":{"GroupType":"AIGC"}}`)))
	require.NotEqual(t, http.StatusNotFound, w.Code)
	require.Contains(t, w.Body.String(), `"ResponseMetadata"`, w.Body.String())
}

func TestSeedanceRejectsOtherPlatforms(t *testing.T) {
	for _, platform := range []string{service.PlatformGrok, service.PlatformAnthropic, service.PlatformGemini} {
		w := httptest.NewRecorder()
		newGatewayRoutesTestRouter(platform).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v3/contents/generations/tasks", strings.NewReader(`{"model":"seedance","content":[{}]}`)))
		require.Equal(t, http.StatusForbidden, w.Code)
	}
}
