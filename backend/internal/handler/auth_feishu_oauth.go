package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/oauth"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// 飞书（Lark）OAuth 登录。
//
// 与钉钉的区别只有两处：身份主键取 union_id、租户限制看 tenant_key。其余（state cookie、
// pending session、绑定/注册/登录三个出口）复用 auth_oauth_pending_flow.go 的共用流程，
// 所以这里没有第二套会话或注册逻辑。
//
// 整个 provider 由 feishu_connect.enabled（FEISHU_CONNECT_ENABLED）控制：关着时路由压根不注册
// （见 server/routes/auth.go），公开设置不暴露 feishu_enabled，前端也就没有按钮。

const (
	feishuOAuthCookiePath          = "/api/v1/auth/oauth/feishu"
	feishuOAuthStateCookieName     = "feishu_oauth_state"
	feishuOAuthRedirectCookie      = "feishu_oauth_redirect"
	feishuOAuthIntentCookieName    = "feishu_oauth_intent"
	feishuOAuthBindUserCookieName  = "feishu_oauth_bind_user"
	feishuOAuthVerifierCookieName  = "feishu_oauth_verifier"
	feishuOAuthCookieMaxAgeSec     = 600 // 10 分钟，覆盖飞书授权码 5 分钟有效期
	feishuOAuthDefaultRedirectTo   = "/dashboard"
	feishuOAuthDefaultFrontendCB   = "/auth/feishu/callback"
	feishuOAuthPKCEChallengeMethod = "S256"
)

// ─── 配置 ──────────────────────────────────────────────────────────────────

// getFeishuOAuthConfig 返回生效配置。飞书只由环境变量 / config.yaml 配置，没有后台覆盖：
// 后台设置里多一份可写的凭证，就多一处可能与部署不一致的真相。
func (h *AuthHandler) getFeishuOAuthConfig() (config.FeishuConnectConfig, error) {
	if h == nil || h.cfg == nil {
		return config.FeishuConnectConfig{}, infraerrors.ServiceUnavailable("CONFIG_NOT_READY", "config not loaded")
	}
	if !h.cfg.Feishu.Enabled {
		return config.FeishuConnectConfig{}, infraerrors.NotFound("OAUTH_DISABLED", "feishu oauth login is disabled")
	}
	return h.cfg.Feishu, nil
}

func (h *AuthHandler) feishuClient(cfg config.FeishuConnectConfig) *FeishuClient {
	h.feishuClientMu.Lock()
	defer h.feishuClientMu.Unlock()
	newCfg := feishuClientConfig{
		AppID:       cfg.AppID,
		AppSecret:   cfg.AppSecret,
		TokenURL:    cfg.TokenURL,
		UserInfoURL: cfg.UserInfoURL,
	}
	if h.feishuClientInstance == nil || h.feishuClientInstance.cfg != newCfg {
		h.feishuClientInstance = &FeishuClient{
			cfg:        newCfg,
			httpClient: &http.Client{Timeout: feishuClientTimeout},
		}
	}
	return h.feishuClientInstance
}

// ─── Cookie ────────────────────────────────────────────────────────────────

func setFeishuCookie(c *gin.Context, name string, value string, maxAgeSec int, secure bool) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     feishuOAuthCookiePath,
		MaxAge:   maxAgeSec,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearFeishuCookie(c *gin.Context, name string, secure bool) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     feishuOAuthCookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ─── 上游错误 ──────────────────────────────────────────────────────────────

// feishuUpstreamRedirect 把飞书的错误码记进日志并带到前端错误页。
// 飞书的排障全靠这个码（20003 授权码无效 / 20004 过期 / 20010 用户无应用权限 /
// 20071 redirect_uri 不一致），泛化成 "internal error" 等于让运维猜。
func feishuUpstreamRedirect(c *gin.Context, frontendCallback, step string, err error) {
	var apiErr *FeishuAPIError
	code, message, httpStatus := "", infraerrors.Message(err), 0
	if errors.As(err, &apiErr) {
		code = apiErr.CodeString()
		message = apiErr.Message
		httpStatus = apiErr.HTTP
	}
	slog.Error("feishu upstream call failed",
		"step", step,
		"feishu_code", code,
		"feishu_msg", message,
		"http_status", httpStatus,
		"error", err.Error(),
	)
	if strings.TrimSpace(message) == "" {
		message = "feishu upstream call failed"
	}
	if code != "" {
		message = "feishu[" + code + "] " + message
	}
	redirectOAuthError(c, frontendCallback, "feishu_upstream_error", message, "")
}

// ─── 授权 URL ──────────────────────────────────────────────────────────────

// buildFeishuAuthorizeURL 拼授权页地址。redirectURI 为空表示部署没显式配置回调地址，
// 此时不带该参数，由飞书用开发者后台登记的地址——但换 token 时也必须同样不带，
// 两处取值因此统一走 resolveFeishuRedirectURI。
func buildFeishuAuthorizeURL(cfg config.FeishuConnectConfig, state, redirectURI, codeChallenge string) (string, error) {
	base := strings.TrimSpace(cfg.AuthorizeURL)
	parsed, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("client_id", strings.TrimSpace(cfg.AppID))
	query.Set("response_type", "code")
	query.Set("state", state)
	if strings.TrimSpace(redirectURI) != "" {
		query.Set("redirect_uri", redirectURI)
	}
	if scopes := strings.TrimSpace(cfg.Scopes); scopes != "" {
		query.Set("scope", scopes)
	}
	if strings.TrimSpace(codeChallenge) != "" {
		query.Set("code_challenge", codeChallenge)
		query.Set("code_challenge_method", feishuOAuthPKCEChallengeMethod)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// resolveFeishuRedirectURI 是回调地址的唯一取值点：授权与换 token 必须传完全相同的值，
// 不一致时飞书返回 20071。配置留空 = 两边都不传。
func resolveFeishuRedirectURI(cfg config.FeishuConnectConfig) string {
	return strings.TrimSpace(cfg.RedirectURL)
}

// ─── FeishuOAuthStart ──────────────────────────────────────────────────────

// FeishuOAuthStart 启动飞书 OAuth。
// GET|POST /api/v1/auth/oauth/feishu/start?redirect=/dashboard&intent=login
func (h *AuthHandler) FeishuOAuthStart(c *gin.Context) {
	if !h.requireActionCaptchaForOAuthLoginStart(c) {
		return
	}
	cfg, err := h.getFeishuOAuthConfig()
	if err != nil {
		redirectOAuthError(c, feishuOAuthDefaultFrontendCB, "feishu_not_enabled", "", "")
		return
	}

	state, err := oauth.GenerateState()
	if err != nil {
		response.ErrorFrom(c, infraerrors.InternalServer("OAUTH_STATE_GEN_FAILED", "failed to generate oauth state").WithCause(err))
		return
	}

	redirectTo := sanitizeFrontendRedirectPath(c.Query("redirect"))
	if redirectTo == "" {
		redirectTo = feishuOAuthDefaultRedirectTo
	}

	browserSessionKey, err := generateOAuthPendingBrowserSession()
	if err != nil {
		response.ErrorFrom(c, infraerrors.InternalServer("OAUTH_BROWSER_SESSION_GEN_FAILED", "failed to generate oauth browser session").WithCause(err))
		return
	}

	secureCookie := isRequestHTTPS(c)
	setFeishuCookie(c, feishuOAuthStateCookieName, encodeCookieValue(state), feishuOAuthCookieMaxAgeSec, secureCookie)
	setFeishuCookie(c, feishuOAuthRedirectCookie, encodeCookieValue(redirectTo), feishuOAuthCookieMaxAgeSec, secureCookie)

	intent := normalizeOAuthIntent(c.Query("intent"))
	setFeishuCookie(c, feishuOAuthIntentCookieName, encodeCookieValue(intent), feishuOAuthCookieMaxAgeSec, secureCookie)
	captureOAuthPromoCode(c, secureCookie)

	setOAuthPendingBrowserCookie(c, browserSessionKey, secureCookie)
	clearOAuthPendingSessionCookie(c, secureCookie)

	// PKCE：verifier 只存在浏览器的 HttpOnly cookie 与飞书之间，服务端不落库。
	// 关掉 use_pkce 时 challenge 与 verifier 都不发——v3 端点会拒绝"只发 verifier"。
	codeChallenge := ""
	if cfg.UsePKCE {
		verifier, err := oauth.GenerateCodeVerifier()
		if err != nil {
			response.ErrorFrom(c, infraerrors.InternalServer("OAUTH_PKCE_GEN_FAILED", "failed to generate pkce verifier").WithCause(err))
			return
		}
		setFeishuCookie(c, feishuOAuthVerifierCookieName, encodeCookieValue(verifier), feishuOAuthCookieMaxAgeSec, secureCookie)
		codeChallenge = oauth.GenerateCodeChallenge(verifier)
	} else {
		clearFeishuCookie(c, feishuOAuthVerifierCookieName, secureCookie)
	}

	if intent == oauthIntentBindCurrentUser {
		bindCookieValue, err := h.buildOAuthBindUserCookieFromContext(c)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		setFeishuCookie(c, feishuOAuthBindUserCookieName, encodeCookieValue(bindCookieValue), feishuOAuthCookieMaxAgeSec, secureCookie)
	} else {
		clearFeishuCookie(c, feishuOAuthBindUserCookieName, secureCookie)
	}

	authURL, err := buildFeishuAuthorizeURL(cfg, state, resolveFeishuRedirectURI(cfg), codeChallenge)
	if err != nil {
		response.ErrorFrom(c, infraerrors.InternalServer("OAUTH_BUILD_URL_FAILED", "failed to build feishu authorization url").WithCause(err))
		return
	}

	respondOAuthStart(c, authURL)
}

// ─── FeishuOAuthCallback ───────────────────────────────────────────────────

// FeishuOAuthCallback 处理授权回调。
// GET /api/v1/auth/oauth/feishu/callback?code=...&state=...
func (h *AuthHandler) FeishuOAuthCallback(c *gin.Context) {
	cfg, cfgErr := h.getFeishuOAuthConfig()
	if cfgErr != nil {
		response.ErrorFrom(c, cfgErr)
		return
	}

	frontendCallback := strings.TrimSpace(cfg.FrontendRedirectURL)
	if frontendCallback == "" {
		frontendCallback = feishuOAuthDefaultFrontendCB
	}

	if providerErr := strings.TrimSpace(c.Query("error")); providerErr != "" {
		redirectOAuthError(c, frontendCallback, "provider_error", providerErr, c.Query("error_description"))
		return
	}

	code := strings.TrimSpace(c.Query("code"))
	state := strings.TrimSpace(c.Query("state"))
	if code == "" || state == "" {
		redirectOAuthError(c, frontendCallback, "missing_params", "missing code/state", "")
		return
	}

	secureCookie := isRequestHTTPS(c)
	defer func() {
		clearFeishuCookie(c, feishuOAuthStateCookieName, secureCookie)
		clearFeishuCookie(c, feishuOAuthRedirectCookie, secureCookie)
		clearFeishuCookie(c, feishuOAuthIntentCookieName, secureCookie)
		clearFeishuCookie(c, feishuOAuthVerifierCookieName, secureCookie)
		clearOAuthPromoCodeCookie(c, secureCookie)
	}()

	expectedState, err := readCookieDecoded(c, feishuOAuthStateCookieName)
	if err != nil || state != expectedState {
		redirectOAuthError(c, frontendCallback, "csrf", "state mismatch", "")
		return
	}
	redirectTo, _ := readCookieDecoded(c, feishuOAuthRedirectCookie)
	intent, _ := readCookieDecoded(c, feishuOAuthIntentCookieName)
	intent = normalizeOAuthIntent(intent)
	browserSessionKey, _ := readOAuthPendingBrowserCookie(c)
	if strings.TrimSpace(browserSessionKey) == "" {
		redirectOAuthError(c, frontendCallback, "missing_browser_session", "missing browser session cookie", "")
		return
	}
	codeVerifier := ""
	if cfg.UsePKCE {
		codeVerifier, _ = readCookieDecoded(c, feishuOAuthVerifierCookieName)
	}

	client := h.feishuClient(cfg)
	token, err := client.ExchangeCodeForUserToken(c.Request.Context(), code, resolveFeishuRedirectURI(cfg), codeVerifier)
	if err != nil {
		feishuUpstreamRedirect(c, frontendCallback, "exchange_code", err)
		return
	}
	userInfo, err := client.GetUserInfo(c.Request.Context(), token.AccessToken)
	if err != nil {
		feishuUpstreamRedirect(c, frontendCallback, "get_user_info", err)
		return
	}

	// 租户限制：唯一判定点，登录/注册/绑定都要先过这里。
	if !cfg.TenantAllowed(userInfo.TenantKey) {
		slog.Warn("feishu login rejected: tenant is not allowed",
			"tenant_key", userInfo.TenantKey,
			"union_id", userInfo.UnionID,
		)
		// tenant_key 不回传前端：它是企业内部标识，错误页只说明被拒绝。
		redirectOAuthError(c, frontendCallback, "tenant_rejected", "", "")
		return
	}

	identityKey := service.PendingAuthIdentityKey{ProviderType: "feishu", ProviderKey: "feishu", ProviderSubject: userInfo.UnionID}
	upstreamClaims := buildFeishuUpstreamClaims(userInfo)
	upstreamEmail := userInfo.PreferredEmail()

	// ─── 主动绑定 ───
	if intent == oauthIntentBindCurrentUser {
		targetUserID, err := h.readOAuthBindUserIDFromCookie(c, feishuOAuthBindUserCookieName)
		if err != nil {
			redirectOAuthError(c, frontendCallback, "invalid_state", "invalid bind user cookie", "")
			return
		}
		bindResolvedEmail := upstreamEmail
		if bindResolvedEmail == "" {
			bindResolvedEmail = buildFeishuSyntheticEmail(userInfo.UnionID)
		}
		if err := h.createOAuthPendingSession(c, oauthPendingSessionPayload{
			Intent: oauthIntentBindCurrentUser, Identity: identityKey,
			TargetUserID: &targetUserID, ResolvedEmail: bindResolvedEmail,
			RedirectTo: redirectTo, BrowserSessionKey: browserSessionKey,
			UpstreamIdentityClaims: upstreamClaims,
			CompletionResponse:     map[string]any{"redirect": redirectTo},
		}); err != nil {
			redirectOAuthError(c, frontendCallback, "session_error", infraerrors.Reason(err), infraerrors.Message(err))
			return
		}
		clearFeishuCookie(c, feishuOAuthBindUserCookieName, secureCookie)
		redirectToFrontendCallback(c, frontendCallback)
		return
	}

	// ─── 已绑定过：直接登录 ───
	if existing, _ := h.findOAuthIdentityUser(c.Request.Context(), identityKey); existing != nil {
		if err := h.createOAuthPendingSession(c, oauthPendingSessionPayload{
			Intent: oauthIntentLogin, Identity: identityKey, TargetUserID: &existing.ID,
			ResolvedEmail: existing.Email, RedirectTo: redirectTo, BrowserSessionKey: browserSessionKey,
			UpstreamIdentityClaims: upstreamClaims,
			CompletionResponse:     map[string]any{"redirect": redirectTo},
		}); err != nil {
			redirectOAuthError(c, frontendCallback, "session_error", infraerrors.Reason(err), infraerrors.Message(err))
			return
		}
		redirectToFrontendCallback(c, frontendCallback)
		return
	}

	signupBlocked := h.isFeishuSignupBlocked(c.Request.Context(), cfg)
	forceEmailOnSignup := h.isForceEmailOnThirdPartySignup(c.Request.Context())

	// ─── require_email=false：合成邮箱直接建号 ───
	if !cfg.RequireEmail {
		if signupBlocked {
			if err := h.createOAuthPendingSession(c, oauthPendingSessionPayload{
				Intent: oauthIntentLogin, Identity: identityKey, TargetUserID: nil,
				ResolvedEmail: "", RedirectTo: redirectTo, BrowserSessionKey: browserSessionKey,
				UpstreamIdentityClaims: upstreamClaims,
				CompletionResponse:     feishuBindLoginCompletionResponse(redirectTo),
			}); err != nil {
				redirectOAuthError(c, frontendCallback, "session_error", infraerrors.Reason(err), infraerrors.Message(err))
				return
			}
			redirectToFrontendCallback(c, frontendCallback)
			return
		}
		syntheticEmail := buildFeishuSyntheticEmail(userInfo.UnionID)
		if err := h.createOAuthPendingSession(c, oauthPendingSessionPayload{
			Intent: oauthIntentLogin, Identity: identityKey, TargetUserID: nil,
			ResolvedEmail: syntheticEmail, RedirectTo: redirectTo, BrowserSessionKey: browserSessionKey,
			UpstreamIdentityClaims: upstreamClaims,
			CompletionResponse:     map[string]any{"redirect": redirectTo, "synthetic_email": syntheticEmail},
		}); err != nil {
			redirectOAuthError(c, frontendCallback, "session_error", infraerrors.Reason(err), infraerrors.Message(err))
			return
		}
		redirectToFrontendCallback(c, frontendCallback)
		return
	}

	// ─── require_email=true 且上游没给邮箱：让用户补，或（注册被拦时）直接绑已有账号 ───
	if upstreamEmail == "" {
		completionResponse := map[string]any{
			"step":                      "email_completion",
			"requires_email_completion": true,
			"redirect":                  redirectTo,
		}
		if signupBlocked {
			completionResponse = feishuBindLoginCompletionResponse(redirectTo)
		}
		if err := h.createOAuthPendingSession(c, oauthPendingSessionPayload{
			Intent: oauthIntentLogin, Identity: identityKey, TargetUserID: nil,
			ResolvedEmail: "", RedirectTo: redirectTo, BrowserSessionKey: browserSessionKey,
			UpstreamIdentityClaims: upstreamClaims,
			CompletionResponse:     completionResponse,
		}); err != nil {
			redirectOAuthError(c, frontendCallback, "session_error", infraerrors.Reason(err), infraerrors.Message(err))
			return
		}
		redirectToFrontendCallback(c, frontendCallback)
		return
	}

	// ─── 有邮箱：走"新建账号 / 绑定已有账号"选择页 ───
	if err := h.createFeishuOAuthChoicePendingSession(
		c, identityKey, upstreamEmail, redirectTo, browserSessionKey,
		upstreamClaims, forceEmailOnSignup, signupBlocked,
	); err != nil {
		redirectOAuthError(c, frontendCallback, "session_error", infraerrors.Reason(err), infraerrors.Message(err))
		return
	}
	redirectToFrontendCallback(c, frontendCallback)
}

// createFeishuOAuthChoicePendingSession 建立"选择新建还是绑定"的待决会话。
// 与钉钉的差别：飞书不做 compat email 自动匹配——飞书返回的邮箱由管理员导入、
// 未经用户本人验证，拿它去认领一个同邮箱的既有账号等于把账号交给未验证的身份。
func (h *AuthHandler) createFeishuOAuthChoicePendingSession(
	c *gin.Context,
	identity service.PendingAuthIdentityKey,
	suggestedEmail string,
	redirectTo string,
	browserSessionKey string,
	upstreamClaims map[string]any,
	forceEmailOnSignup bool,
	signupBlocked bool,
) error {
	email := strings.TrimSpace(suggestedEmail)
	completionResponse := map[string]any{
		"step":                      oauthPendingChoiceStep,
		"adoption_required":         true,
		"redirect":                  strings.TrimSpace(redirectTo),
		"email":                     email,
		"resolved_email":            email,
		"existing_account_email":    "",
		"existing_account_bindable": false,
		"create_account_allowed":    !signupBlocked,
		"force_email_on_signup":     forceEmailOnSignup,
		"choice_reason":             "third_party_signup",
	}
	if forceEmailOnSignup {
		completionResponse["choice_reason"] = "force_email_on_signup"
	}
	if signupBlocked {
		completionResponse["step"] = "bind_login_required"
		completionResponse["existing_account_bindable"] = true
		completionResponse["choice_reason"] = "signup_blocked_redirect_to_bind"
	}

	return h.createOAuthPendingSession(c, oauthPendingSessionPayload{
		Intent:                 oauthIntentLogin,
		Identity:               identity,
		TargetUserID:           nil,
		ResolvedEmail:          email,
		RedirectTo:             redirectTo,
		BrowserSessionKey:      browserSessionKey,
		UpstreamIdentityClaims: upstreamClaims,
		CompletionResponse:     completionResponse,
	})
}

// ─── 注册 / 绑定出口 ───────────────────────────────────────────────────────

// CreateFeishuOAuthAccount 用待决会话创建新账号。
// POST /api/v1/auth/oauth/feishu/create-account
func (h *AuthHandler) CreateFeishuOAuthAccount(c *gin.Context) {
	h.createPendingOAuthAccount(c, "feishu")
}

// BindFeishuOAuthLogin 把飞书身份绑到已有账号并登录。
// POST /api/v1/auth/oauth/feishu/bind-login
func (h *AuthHandler) BindFeishuOAuthLogin(c *gin.Context) {
	h.bindPendingOAuthLogin(c, "feishu")
}

// ─── 辅助 ──────────────────────────────────────────────────────────────────

func buildFeishuSyntheticEmail(unionID string) string {
	return "feishu-" + strings.ToLower(strings.TrimSpace(unionID)) + service.FeishuConnectSyntheticEmailDomain
}

func feishuBindLoginCompletionResponse(redirectTo string) map[string]any {
	return map[string]any{
		"step":                      "bind_login_required",
		"existing_account_bindable": true,
		"create_account_allowed":    false,
		"redirect":                  strings.TrimSpace(redirectTo),
	}
}

// buildFeishuUpstreamClaims 汇总写进待决会话的上游身份信息。
// 只放身份与展示所需字段：open_id / user_id 便于运维在飞书后台对人，tenant_key 便于审计
// 是哪家企业进来的；access_token 不进会话（用完即弃，落库只会多一处泄露面）。
func buildFeishuUpstreamClaims(info *FeishuUserInfo) map[string]any {
	if info == nil {
		return map[string]any{}
	}
	claims := map[string]any{
		"provider":  "feishu",
		"union_id":  info.UnionID,
		"open_id":   info.OpenID,
		"issued_at": time.Now().UTC().Format(time.RFC3339),
	}
	if name := info.DisplayName(); name != "" {
		claims["username"] = name
		claims["display_name"] = name
	}
	if avatar := strings.TrimSpace(info.AvatarBig); avatar != "" {
		claims["avatar"] = avatar
	} else if avatar := strings.TrimSpace(info.AvatarURL); avatar != "" {
		claims["avatar"] = avatar
	}
	if email := info.PreferredEmail(); email != "" {
		claims["email"] = email
	}
	if tenant := strings.TrimSpace(info.TenantKey); tenant != "" {
		claims["tenant_key"] = tenant
	}
	if userID := strings.TrimSpace(info.UserID); userID != "" {
		claims["upstream_user_id"] = userID
	}
	if employeeNo := strings.TrimSpace(info.EmployeeNo); employeeNo != "" {
		claims["employee_no"] = employeeNo
	}
	return claims
}

// isFeishuSignupBlocked 判断"注册总开关关着且飞书没有豁免"。
// 豁免要求 allowed_tenant_keys 非空（配置校验里强制），否则任何飞书账号都能在
// 关闭注册的站点上建号——这正是关掉注册要防的事。
func (h *AuthHandler) isFeishuSignupBlocked(ctx context.Context, cfg config.FeishuConnectConfig) bool {
	if h.settingSvc == nil {
		return false
	}
	if h.settingSvc.IsRegistrationEnabled(ctx) {
		return false
	}
	return !(cfg.BypassRegistration && len(cfg.AllowedTenantKeyList()) > 0)
}
