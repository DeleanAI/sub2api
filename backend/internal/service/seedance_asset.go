package service

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
)

// 方舟素材库（私域虚拟人像库）Asset API 的转发。
//
// 上游是管控面 OpenAPI：POST <API 根>/?Action=<Action>&Version=2024-01-01，参数在 JSON 请求体里。网关原样透传请求体
// 与上游的回答（Action / Version 照旧放在查询参数里），只把鉴权换成 Seedance 账号的 Key。素材按公开信息对待：网关
// 不区分调用方各自建的素材组与素材。素材接口不计费——没有上游对这些调用收费的依据。

// SeedanceAssetAPIVersion 是素材库 OpenAPI 的版本；开放的 Action 按这个版本的接口列表声明，别的版本不认。
const SeedanceAssetAPIVersion = "2024-01-01"

// seedanceAssetActions 是网关开放的素材库 Action：方舟私域虚拟人像库 API 参考（2024-01-01 版）的全部 10 个接口。
// 真人人像的认证与入库走方舟控制台扫码授权，没有 OpenAPI；声明以外的 Action 一律拒绝。
var seedanceAssetActions = map[string]bool{
	"CreateAssetGroup": true,
	"CreateAsset":      true,
	"ListAssetGroups":  true,
	"ListAssets":       true,
	"GetAssetGroup":    true,
	"GetAsset":         true,
	"UpdateAssetGroup": true,
	"UpdateAsset":      true,
	"DeleteAssetGroup": true,
	"DeleteAsset":      true,
}

// SeedanceAssetError 是网关自己对素材库调用的回答，按方舟 OpenAPI 的错误格式写出（WriteSeedanceAssetError）。
type SeedanceAssetError struct {
	Status  int
	Code    string
	Message string
}

func (e *SeedanceAssetError) Error() string { return e.Code + ": " + e.Message }

// LookupSeedanceAssetAction 核对 Action 与 Version 在网关开放的范围内。
func LookupSeedanceAssetAction(action, version string) *SeedanceAssetError {
	if !seedanceAssetActions[action] || version != SeedanceAssetAPIVersion {
		return &SeedanceAssetError{Status: http.StatusBadRequest, Code: "InvalidActionOrVersion",
			Message: fmt.Sprintf("unsupported Action %q or Version %q (Version must be %s)", action, version, SeedanceAssetAPIVersion)}
	}
	return nil
}

// buildSeedanceAssetURL：素材库接口挂在方舟 API 的根上（base_url 的形式见 splitSeedanceBase）。
func buildSeedanceAssetURL(base, action, version string) string {
	root, _ := splitSeedanceBase(base)
	return root + "/?" + url.Values{"Action": {action}, "Version": {version}}.Encode()
}

// SeedanceAssetUpstreamEndpoint 是运维记录里的上游端点：素材库接口同一个路径，按 Action 区分。
func SeedanceAssetUpstreamEndpoint(action string) string {
	return "/?Action=" + action
}

// ForwardSeedanceAsset 把一次素材库调用发给 account，原样回写上游的回答（状态码、响应体与允许透传的响应头），
// 返回上游的状态码。
func (s *OpenAIGatewayService) ForwardSeedanceAsset(ctx context.Context, c *gin.Context, account *Account, action, version string, body []byte) (int, error) {
	resp, respBody, _, err := s.seedanceAccountRequest(ctx, c, account, http.MethodPost, func(base string) (string, error) {
		return buildSeedanceAssetURL(base, action, version), nil
	}, body)
	if err != nil {
		return 0, err
	}
	writeGrokMediaResponse(c, resp, respBody, s.responseHeaderFilter)
	return resp.StatusCode, nil
}

// WriteSeedanceAssetError 按方舟 OpenAPI 的错误格式（ResponseMetadata.Error）写出网关自己的回答。
func WriteSeedanceAssetError(c *gin.Context, action, version string, e *SeedanceAssetError) {
	requestID := ""
	if c.Request != nil {
		requestID, _ = c.Request.Context().Value(ctxkey.ClientRequestID).(string)
	}
	c.JSON(e.Status, gin.H{"ResponseMetadata": gin.H{
		"RequestId": requestID, "Action": action, "Version": version, "Service": "ark",
		"Error": gin.H{"Code": e.Code, "Message": e.Message},
	}})
}
