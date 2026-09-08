//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEveryEncryptedStorePointsAtRealSchema 遍历 EncryptedStores，在一个真正迁移过的
// 数据库里核对每条声明的表名、主键列、密文列都确实存在。
//
// 这条护栏是必要的，因为 EncryptedStores 里的表名是一个字符串，而单元测试的 SQLite
// 库是按这个字符串**建出来**的——写错名字，单元测试照样全绿。真实后果是轮换命令跑到
// 那条 store 时报 relation "..." does not exist，一行失败就不写新指纹；操作者照文档
// 清掉 TOTP_ENCRYPTION_KEY_PREVIOUS 重启后，release 模式因指纹对不上拒绝启动。
//
// 这正是本仓库真实发生过的事：合并上游 0.2.x 时把插件配置登记成了 "plugins"，
// 而实际表名是 sub2api_plugin_installations（229_plugins.sql）。
func TestEveryEncryptedStorePointsAtRealSchema(t *testing.T) {
	ctx := context.Background()
	require.NotEmpty(t, EncryptedStores)

	for _, store := range EncryptedStores {
		store := store
		t.Run(store.Name, func(t *testing.T) {
			var tableExists bool
			require.NoError(t, integrationDB.QueryRowContext(ctx,
				`SELECT EXISTS (SELECT 1 FROM information_schema.tables
				 WHERE table_schema = 'public' AND table_name = $1)`, store.Table()).Scan(&tableExists))
			require.Truef(t, tableExists,
				"EncryptedStores 里 %s 声明的表 %q 在迁移后的库里不存在：轮换跑到这条会报 "+
					`relation "%s" does not exist，一行失败就不写新指纹，`+
					"随后按文档退役旧密钥会让每个副本都起不来", store.Name, store.Table(), store.Table())

			for _, column := range []string{store.IDColumn(), store.Column()} {
				var columnExists bool
				require.NoError(t, integrationDB.QueryRowContext(ctx,
					`SELECT EXISTS (SELECT 1 FROM information_schema.columns
					 WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2)`,
					store.Table(), column).Scan(&columnExists))
				require.Truef(t, columnExists,
					"EncryptedStores 里 %s 声明的列 %s.%s 不存在", store.Name, store.Table(), column)
			}
		})
	}
}
