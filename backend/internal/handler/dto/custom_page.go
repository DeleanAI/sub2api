package dto

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// CustomPageSummary 管理端页面列表项。
type CustomPageSummary struct {
	Slug        string    `json:"slug"`
	ContentSize int       `json:"content_size"`
	AssetCount  int       `json:"asset_count"`
	AssetBytes  int64     `json:"asset_bytes"`
	UpdatedBy   *int64    `json:"updated_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// CustomPage 管理端页面详情（含正文）。
type CustomPage struct {
	Slug        string    `json:"slug"`
	Content     string    `json:"content"`
	ContentSize int       `json:"content_size"`
	UpdatedBy   *int64    `json:"updated_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// CustomPageAsset 管理端附件元数据（不含二进制内容）。
type CustomPageAsset struct {
	Path        string    `json:"path"`
	ContentType string    `json:"content_type"`
	Size        int       `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// CustomPageLimits 把服务层声明的限额原样暴露给前端，前端不再自己硬编码一份。
type CustomPageLimits struct {
	MaxContentSize int `json:"max_content_size"`
	MaxAssetSize   int `json:"max_asset_size"`
	MaxAssets      int `json:"max_assets"`
	MaxSlugLength  int `json:"max_slug_length"`
}

// CustomPageListResponse 列表响应：页面 + 限额。
type CustomPageListResponse struct {
	Pages  []CustomPageSummary `json:"pages"`
	Limits CustomPageLimits    `json:"limits"`
}

func CustomPageLimitsFromService() CustomPageLimits {
	return CustomPageLimits{
		MaxContentSize: service.MaxCustomPageContentSize,
		MaxAssetSize:   service.MaxCustomPageAssetSize,
		MaxAssets:      service.MaxCustomPageAssets,
		MaxSlugLength:  service.MaxCustomPageSlugLength,
	}
}

func CustomPageSummaryFromService(in *service.CustomPageSummary) CustomPageSummary {
	return CustomPageSummary{
		Slug:        in.Slug,
		ContentSize: in.ContentSize,
		AssetCount:  in.AssetCount,
		AssetBytes:  in.AssetBytes,
		UpdatedBy:   in.UpdatedBy,
		CreatedAt:   in.CreatedAt,
		UpdatedAt:   in.UpdatedAt,
	}
}

func CustomPageFromService(in *service.CustomPage) CustomPage {
	return CustomPage{
		Slug:        in.Slug,
		Content:     in.Content,
		ContentSize: in.ContentSize,
		UpdatedBy:   in.UpdatedBy,
		CreatedAt:   in.CreatedAt,
		UpdatedAt:   in.UpdatedAt,
	}
}

func CustomPageAssetFromService(in *service.CustomPageAsset) CustomPageAsset {
	return CustomPageAsset{
		Path:        in.Path,
		ContentType: in.ContentType,
		Size:        in.Size,
		CreatedAt:   in.CreatedAt,
		UpdatedAt:   in.UpdatedAt,
	}
}
