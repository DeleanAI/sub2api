package routes

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newGatewayRoutesTestRouterWithGroup(group *service.Group) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterGatewayRoutes(
		router,
		&handler.Handlers{
			Gateway:       &handler.GatewayHandler{},
			OpenAIGateway: &handler.OpenAIGatewayHandler{},
			AsyncImage:    handler.NewAsyncImageHandler(nil, nil),
		},
		servermiddleware.APIKeyAuthMiddleware(func(c *gin.Context) {
			groupID := int64(1)
			c.Set(string(servermiddleware.ContextKeyAPIKey), &service.APIKey{
				GroupID: &groupID,
				Group:   group,
			})
			c.Next()
		}),
		nil,
		nil,
		nil,
		nil,
		nil,
		&config.Config{
			Gateway: config.GatewayConfig{
				MaxBodySize:     1024 * 1024,
				TextMaxBodySize: 1024 * 1024,
			},
		},
	)
	return router
}

func allowlistGroup(platform string, enabled bool, models ...string) *service.Group {
	return &service.Group{
		Platform: platform,
		ModelAllowlist: service.GroupModelAllowlist{
			Enabled: enabled,
			Models:  models,
		},
	}
}

// TestGatewayRoutesGroupModelAllowlistMountedOnEveryGatewayRoute follows the
// source-level route assertion convention of prompt_audit_route_coverage_test.go:
// every gateway chain must mount groupModelAllowlist after api key auth and
// before the composite rewrite (gateway.go + rootRoute helper).
func TestGatewayRoutesGroupModelAllowlistMountedOnEveryGatewayRoute(t *testing.T) {
	routeSource, err := os.ReadFile("gateway.go")
	require.NoError(t, err)
	source := string(routeSource)

	// rootRoute helper：apiKeyAuth 之后、compositeTarget 之前。
	//
	// 断言的是**顺序关系**而不是整条链的字面量：链上随时可能插入别的中间件
	// （负载审计就是这么加进来的），字面量断言会在每一次这种改动上假失败，
	// 而它真正要保护的东西——白名单必须在鉴权之后、composite 改写之前——没有变。
	requireOrderedInHelper(t, source,
		`r.Handle(method, path, limit,`,
		[]string{"gin.HandlerFunc(apiKeyAuth)", "groupModelAllowlist", "compositeTarget", "handler)"})

	chains := []struct {
		group     string
		auth      string
		marker    string
		composite string
	}{
		{group: "gateway", auth: "gin.HandlerFunc(apiKeyAuth)", marker: "gateway.Use(groupModelAllowlist)", composite: "gateway.Use(compositeTarget)"},
		{group: "gemini", auth: "middleware.APIKeyAuthWithSubscriptionGoogle(apiKeyService, subscriptionService, cfg)", marker: "gemini.Use(groupModelAllowlist)", composite: "gemini.Use(compositeGeminiTarget)"},
		{group: "antigravityV1", auth: "gin.HandlerFunc(apiKeyAuth)", marker: "antigravityV1.Use(groupModelAllowlist)", composite: "antigravityV1.Use(requireGroupAnthropic)"},
		{group: "antigravityV1Beta", auth: "middleware.APIKeyAuthWithSubscriptionGoogle(apiKeyService, subscriptionService, cfg)", marker: "antigravityV1Beta.Use(groupModelAllowlist)", composite: "antigravityV1Beta.Use(requireGroupGoogle)"},
	}
	for _, chain := range chains {
		re := regexp.MustCompile(
			regexp.QuoteMeta(chain.group+".Use("+chain.auth) +
				`[\s\S]{0,400}?` + regexp.QuoteMeta(chain.marker) +
				`[\s\S]{0,400}?` + regexp.QuoteMeta(chain.composite))
		require.Regexp(t, re, source,
			"%s chain must mount groupModelAllowlist after auth and before %s", chain.group, chain.composite)
	}

	// codexDirect 链是一条 Use 调用，同样只断言顺序。
	requireOrderedInHelper(t, source,
		`codexDirect.Use(`,
		[]string{"gin.HandlerFunc(apiKeyAuth)", "groupModelAllowlist", "compositeTarget"})

	// 所有带 apiKeyAuth 的根路径路由必须收敛到 rootRoute，避免漏挂。
	stray := regexp.MustCompile(`\br\.(GET|POST|PUT|PATCH|DELETE)\("[^"]+",[^(]*apiKeyAuth`)
	require.NotRegexp(t, stray, source,
		"root alias routes must use rootRoute so the allowlist cannot be forgotten")
}

// TestGatewayRoutesGroupModelAllowlistBlocksCompositeModelBeforeRewrite asserts
// admission is decided on the client-written public model before the composite
// route middleware can rewrite it to an upstream model.
func TestGatewayRoutesGroupModelAllowlistBlocksCompositeModelBeforeRewrite(t *testing.T) {
	router := newGatewayRoutesTestRouterWithGroup(allowlistGroup(service.PlatformComposite, true, "claude-*"))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"grok-4.3","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)
	require.Contains(t, w.Body.String(), "not_found_error")
	require.Contains(t, w.Body.String(), "grok-4.3")
}

func TestGatewayRoutesGroupModelAllowlistAllowsListedCompositeModel(t *testing.T) {
	router := newGatewayRoutesTestRouterWithGroup(allowlistGroup(service.PlatformComposite, true, "claude-*"))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.NotEqual(t, http.StatusNotFound, w.Code, "allowlisted model must pass admission: %s", w.Body.String())
}

func TestGatewayRoutesGroupModelAllowlistCoversRootAliasRoutes(t *testing.T) {
	router := newGatewayRoutesTestRouterWithGroup(allowlistGroup(service.PlatformOpenAI, true, "gpt-5.4"))

	paths := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/responses", `{"model":"gpt-4.1"}`},
		{http.MethodPost, "/responses/compact", `{"model":"gpt-4.1"}`},
		{http.MethodPost, "/chat/completions", `{"model":"gpt-4.1"}`},
		{http.MethodPost, "/embeddings", `{"model":"gpt-4.1","input":"hi"}`},
		{http.MethodPost, "/images/generations", `{"model":"gpt-4.1"}`},
		{http.MethodPost, "/images/edits", `{"model":"gpt-4.1"}`},
		{http.MethodPost, "/videos/generations", `{"model":"gpt-4.1"}`},
		{http.MethodPost, "/messages/count_tokens", `{"model":"gpt-4.1","messages":[]}`},
		{http.MethodPost, "/tts", `{"model":"gpt-4.1"}`},
		{http.MethodPost, "/stt", `{"model":"gpt-4.1"}`},
		{http.MethodPost, "/alpha/search", `{"model":"gpt-4.1"}`},
		{http.MethodGet, "/realtime?model=gpt-4.1", ""},
		{http.MethodPost, "/v1/responses", `{"model":"gpt-4.1"}`},
		{http.MethodPost, "/v1/messages", `{"model":"gpt-4.1"}`},
		{http.MethodPost, "/v1/messages/count_tokens", `{"model":"gpt-4.1","messages":[]}`},
		{http.MethodPost, "/v1/chat/completions", `{"model":"gpt-4.1"}`},
		{http.MethodPost, "/v1/embeddings", `{"model":"gpt-4.1","input":"hi"}`},
		{http.MethodPost, "/v1/images/generations", `{"model":"gpt-4.1"}`},
		{http.MethodPost, "/v1/videos/generations", `{"model":"gpt-4.1"}`},
		{http.MethodPost, "/v1/live", `{"session":{"model":"gpt-4.1"},"sdp":"v=0"}`},
		{http.MethodPost, "/backend-api/codex/responses", `{"model":"gpt-4.1"}`},
		{http.MethodPost, "/backend-api/codex/realtime/calls", `{"session":{"model":"gpt-4.1"},"sdp":"v=0"}`},
		{http.MethodPost, "/antigravity/v1/messages", `{"model":"gemini-2.5-pro","messages":[]}`},
	}

	for _, tc := range paths {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		if tc.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusNotFound, w.Code, "%s %s should be denied by the allowlist, got body: %s", tc.method, tc.path, w.Body.String())
		require.Contains(t, w.Body.String(), "not available for this group", "%s %s", tc.method, tc.path)
	}
}

func TestGatewayRoutesGroupModelAllowlistSkipsWebSocketUpgrade(t *testing.T) {
	router := newGatewayRoutesTestRouterWithGroup(allowlistGroup(service.PlatformOpenAI, true, "gpt-5.4"))

	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.NotEqual(t, http.StatusNotFound, w.Code,
		"WS upgrade requests must be admitted and per-frame checked in the handler, got: %s", w.Body.String())
}

func TestGatewayRoutesGroupModelAllowlistModelFreeRoutesUnaffected(t *testing.T) {
	router := newGatewayRoutesTestRouterWithGroup(allowlistGroup(service.PlatformGrok, true, "grok-4.6"))

	// 不携带模型的入口天然不受白名单影响（状态查询/内容下载/模型列表）。
	for _, path := range []string{
		"/v1/videos/generations/req-1",
		"/v1/videos/req-1/content",
		"/v1/images/tasks/task-1",
		"/v1/custom-voices",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.NotContains(t, w.Body.String(), "not available for this group",
			"%s should not be blocked by the allowlist middleware, got: %s", path, w.Body.String())
	}
}

// requireOrderedInHelper 在 source 里找到以 prefix 开头的那一行，断言 tokens 按给定
// 顺序出现在这一行里。
//
// 只钉顺序、不钉整行：中间件链会增删（负载审计、限流……），钉整行等于每加一个
// 中间件就假失败一次，而顺序才是这条护栏要保护的不变式。
func requireOrderedInHelper(t *testing.T, source, prefix string, tokens []string) {
	t.Helper()
	var line string
	for _, candidate := range strings.Split(source, "\n") {
		if strings.Contains(candidate, prefix) {
			line = candidate
			break
		}
	}
	require.NotEmptyf(t, line, "找不到以 %q 开头的中间件链，护栏和 gateway.go 脱节了", prefix)

	cursor := 0
	for _, token := range tokens {
		idx := strings.Index(line[cursor:], token)
		require.GreaterOrEqualf(t, idx, 0,
			"中间件链里找不到 %q（或它排在前一个之前）：\n  %s", token, strings.TrimSpace(line))
		cursor += idx + len(token)
	}
}
