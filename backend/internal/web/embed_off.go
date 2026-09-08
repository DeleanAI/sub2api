//go:build !embed

// Package web provides embedded web assets for the application.
package web

import (
	"context"
	"errors"
	"io/fs"
	"net/http"

	"github.com/gin-gonic/gin"
)

// errNotEmbedded 是不带 embed tag 编译时所有前端入口的统一失败原因。
var errNotEmbedded = errors.New("frontend not embedded (build with -tags embed)")

// PublicSettingsProvider is an interface to fetch public settings
// This stub is needed for compilation when frontend is not embedded
type PublicSettingsProvider interface {
	GetPublicSettingsForInjection(ctx context.Context) (any, error)
}

// FrontendServer is a stub for non-embed builds
type FrontendServer struct{}

// AvailableVariants 在非 embed 构建里恒为空：二进制里没有任何前端产物。
func AvailableVariants() []string { return nil }

// VariantFS 与 embed 构建保持同一个签名和同一种错误，调用方不需要按构建标签分支。
func VariantFS(name string) (fs.FS, error) {
	return nil, unknownVariantError(name, nil, errNotEmbedded)
}

// NewFrontendServer returns an error when frontend is not embedded
func NewFrontendServer(settingsProvider PublicSettingsProvider, overrideDir, variant string) (*FrontendServer, error) {
	return nil, errNotEmbedded
}

// InvalidateCache is a no-op for non-embed builds
func (s *FrontendServer) InvalidateCache() {}

// Middleware returns a handler that returns 404 for non-embed builds
func (s *FrontendServer) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.String(http.StatusNotFound, "Frontend not embedded. Build with -tags embed to include frontend.")
		c.Abort()
	}
}

func ServeEmbeddedFrontend(overrideDir, variant string) (gin.HandlerFunc, error) {
	return nil, errNotEmbedded
}

func HasEmbeddedFrontend() bool {
	return false
}

// EmbeddedFrontendLayoutError 在非 embed 构建里没有产物可查，恒为 nil。
func EmbeddedFrontendLayoutError() error { return nil }
