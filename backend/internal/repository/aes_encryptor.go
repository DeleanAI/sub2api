package repository

import (
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// NewAESEncryptor 构造落库密文加密器（AES-256-GCM 密钥环）。
//
// 返回的是 config 里主密钥 + 历史密钥组成的 secretcipher.Ring：加密只用主密钥并
// 写入 key-id 前缀，解密能处理带前缀的新格式、历史密钥加密的密文以及无前缀的
// 旧格式。格式与密钥规则本身都定义在 internal/pkg/secretcipher，这里只做接线。
func NewAESEncryptor(cfg *config.Config) (service.SecretEncryptor, error) {
	return cfg.Totp.KeyRing()
}
