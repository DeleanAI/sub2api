package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 飞书（Lark）OAuth 上游客户端。
//
// 只有两个上游调用，都以用户身份完成，不需要 app_access_token：
//  1. POST <token_url>            授权码换 user_access_token（RFC 6749 授权码模式）
//  2. GET  <userinfo_url>         用 user_access_token 取用户资料
//
// 飞书的错误有两种形状：token 端点走 OAuth 标准的 error/error_description（HTTP 4xx），
// 用户信息端点走 code/msg（HTTP 200 也可能 code != 0）。两者统一成 FeishuAPIError，
// 由 handler 决定怎么告诉用户——错误码是排障的唯一线索，绝不能吞掉。

const feishuClientTimeout = 10 * time.Second

// feishuClientConfig 是 FeishuClient 需要的最小配置子集。可比较，用于判断配置是否变化。
type feishuClientConfig struct {
	AppID       string
	AppSecret   string
	TokenURL    string
	UserInfoURL string
}

type FeishuClient struct {
	cfg        feishuClientConfig
	httpClient *http.Client
}

// FeishuUserToken 是 token 端点成功时的响应。
// 不请求 offline_access，所以不会有 refresh_token，也不需要保存令牌：
// 登录流程在回调里一次性用掉它换用户资料。
type FeishuUserToken struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
}

// FeishuUserInfo 是 /authen/v1/user_info 的 data 段。
// 字段权限：email / enterprise_email / mobile / user_id 需要应用申请对应权限，
// 拿不到时为空字符串——这是常态，不是错误。
type FeishuUserInfo struct {
	OpenID          string `json:"open_id"`
	UnionID         string `json:"union_id"`
	UserID          string `json:"user_id"`
	Name            string `json:"name"`
	EnName          string `json:"en_name"`
	AvatarURL       string `json:"avatar_url"`
	AvatarBig       string `json:"avatar_big"`
	Email           string `json:"email"`
	EnterpriseEmail string `json:"enterprise_email"`
	TenantKey       string `json:"tenant_key"`
	EmployeeNo      string `json:"employee_no"`
}

// PreferredEmail 返回该用户可用于建号的邮箱：企业邮箱优先于个人邮箱。
// 飞书文档明确这两个字段是管理员导入、未经用户本人实时验证的，所以调用方
// 必须把它当"建议值"而不是已验证凭证（require_email=true 时仍走注册确认流程）。
func (u *FeishuUserInfo) PreferredEmail() string {
	if u == nil {
		return ""
	}
	if email := strings.TrimSpace(u.EnterpriseEmail); email != "" {
		return email
	}
	return strings.TrimSpace(u.Email)
}

// DisplayName 返回展示名：中文名优先，其次英文名。
func (u *FeishuUserInfo) DisplayName() string {
	if u == nil {
		return ""
	}
	if name := strings.TrimSpace(u.Name); name != "" {
		return name
	}
	return strings.TrimSpace(u.EnName)
}

// FeishuAPIError 承载飞书返回的错误码。Code 是飞书业务码（20003 授权码无效、
// 20004 过期、20010 用户无应用使用权限、20071 redirect_uri 不一致……），
// ErrCode 是 token 端点的 OAuth error 字段。
type FeishuAPIError struct {
	HTTP    int
	Code    int
	ErrCode string
	Message string
}

func (e *FeishuAPIError) Error() string {
	return fmt.Sprintf("feishu api error code=%d oauth_error=%s msg=%s http=%d", e.Code, e.ErrCode, e.Message, e.HTTP)
}

// CodeString 给日志和前端用：优先业务码，其次 OAuth error。
func (e *FeishuAPIError) CodeString() string {
	if e == nil {
		return ""
	}
	if e.Code != 0 {
		return fmt.Sprintf("%d", e.Code)
	}
	return e.ErrCode
}

func parseFeishuErr(raw []byte, status int) error {
	var v struct {
		Code             int    `json:"code"`
		Msg              string `json:"msg"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &v)
	msg := strings.TrimSpace(v.ErrorDescription)
	if msg == "" {
		msg = strings.TrimSpace(v.Msg)
	}
	if msg == "" {
		// 响应体不是预期的 JSON（网关错误页等）：截断保留，别把整页 HTML 塞进日志。
		msg = truncateForLog(string(raw), 200)
	}
	return &FeishuAPIError{HTTP: status, Code: v.Code, ErrCode: strings.TrimSpace(v.Error), Message: msg}
}

func truncateForLog(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// ExchangeCodeForUserToken 用授权码换 user_access_token。
//
// redirectURI 必须与授权时拼进 URL 的完全一致，否则飞书返回 20071；
// codeVerifier 只在授权阶段发过 code_challenge 时才带——v3 端点对 PKCE 语义严格，
// 发了 verifier 却没发过 challenge 会被拒绝（20049）。
func (c *FeishuClient) ExchangeCodeForUserToken(ctx context.Context, code, redirectURI, codeVerifier string) (*FeishuUserToken, error) {
	body := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     c.cfg.AppID,
		"client_secret": c.cfg.AppSecret,
		"code":          code,
	}
	if strings.TrimSpace(redirectURI) != "" {
		body["redirect_uri"] = redirectURI
	}
	if strings.TrimSpace(codeVerifier) != "" {
		body["code_verifier"] = codeVerifier
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.TokenURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, parseFeishuErr(raw, resp.StatusCode)
	}

	var out struct {
		Code int `json:"code"`
		FeishuUserToken
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &FeishuAPIError{HTTP: resp.StatusCode, Message: "malformed token response: " + truncateForLog(string(raw), 200)}
	}
	// HTTP 200 + code != 0 也是失败；只看 HTTP 状态会把失败当成功，拿着空令牌继续走。
	if out.Code != 0 || strings.TrimSpace(out.AccessToken) == "" {
		return nil, parseFeishuErr(raw, resp.StatusCode)
	}
	token := out.FeishuUserToken
	return &token, nil
}

// GetUserInfo 用 user_access_token 取用户资料。
func (c *FeishuClient) GetUserInfo(ctx context.Context, userAccessToken string) (*FeishuUserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.UserInfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+userAccessToken)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, parseFeishuErr(raw, resp.StatusCode)
	}

	var out struct {
		Code int            `json:"code"`
		Msg  string         `json:"msg"`
		Data FeishuUserInfo `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &FeishuAPIError{HTTP: resp.StatusCode, Message: "malformed user_info response: " + truncateForLog(string(raw), 200)}
	}
	if out.Code != 0 {
		return nil, parseFeishuErr(raw, resp.StatusCode)
	}
	// union_id 是账号在本应用开发者名下的稳定标识，缺了它就没有可信的身份主键，
	// 宁可失败也不能退回 open_id：open_id 在换应用后会变，会把同一个人认成新用户。
	if strings.TrimSpace(out.Data.UnionID) == "" {
		return nil, &FeishuAPIError{HTTP: resp.StatusCode, Message: "user_info response has no union_id; the app likely lacks the contact:user.id:readonly permission"}
	}
	info := out.Data
	return &info, nil
}
