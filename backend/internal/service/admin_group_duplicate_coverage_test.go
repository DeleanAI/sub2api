//go:build unit

package service

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// groupFieldsIntentionallyNotDuplicated 声明复制分组时刻意不照搬的字段及原因。
//
// cloneGroupForDuplicate 是一张 60 多行的手写字段表，而 Group 有 60 多个字段：
// 新增一个字段忘了加进去，复制出来的分组就少一项配置，没有任何报错——管理员会
// 以为复制出来的分组和源分组一样。ModelPricing 与 LongContextPricingEnabled
// 就是这么漏掉的（两项都直接影响计费）。
//
// 护栏用反射逐字段比对真实的复制结果，而不是读源码里的赋值语句：写了赋值但赋错源
// （复制粘贴改漏一个字段名）同样会被抓到。
var groupFieldsIntentionallyNotDuplicated = map[string]string{
	"ID":                        "新分组由数据库分配",
	"Name":                      "复制品要改名（duplicateGroupName）",
	"Status":                    "复制出来一律先停用，避免立刻接流量",
	"DuplicateOperationID":      "本次复制操作的幂等键",
	"CreatedAt":                 "记账列",
	"UpdatedAt":                 "记账列",
	"Hydrated":                  "内存态标记，不是分组配置",
	"AccountCount":              "聚合统计，属于新分组自己",
	"ActiveAccountCount":        "聚合统计，属于新分组自己",
	"RateLimitedAccountCount":   "聚合统计，属于新分组自己",
	"AccountGroups":             "账号成员关系，复制分组不搬账号",
	"CodexModelsManifestConfig": "固定账号 manifest 指向源分组的账号 ID，复制后成员关系可能变化，重置为关闭",
}

// TestCloneGroupForDuplicateCoversEveryField 遍历 Group 的每个字段，要求复制结果里
// 它要么与源相同，要么在上面的声明表里写明为什么不同。
func TestCloneGroupForDuplicateCoversEveryField(t *testing.T) {
	groupType := reflect.TypeOf(Group{})
	source := &Group{}
	sourceValue := reflect.ValueOf(source).Elem()
	for i := 0; i < groupType.NumField(); i++ {
		field := groupType.Field(i)
		if !sourceValue.Field(i).CanSet() {
			continue
		}
		// AccountGroups 是账号关联对象，里面含 interface{} 字段，探针填不出来；
		// 它在声明表里已写明"复制分组不搬账号"，留零值即可。
		if _, declared := groupFieldsIntentionallyNotDuplicated[field.Name]; declared && field.Name == "AccountGroups" {
			continue
		}
		sourceValue.Field(i).Set(snapshotProbeValue(t, field.Type, field.Name, i+1))
	}

	cloned := cloneGroupForDuplicate(source, "op-1")
	clonedValue := reflect.ValueOf(cloned).Elem()

	var dropped []string
	for i := 0; i < groupType.NumField(); i++ {
		name := groupType.Field(i).Name
		if _, declared := groupFieldsIntentionallyNotDuplicated[name]; declared {
			continue
		}
		if !reflect.DeepEqual(sourceValue.Field(i).Interface(), clonedValue.Field(i).Interface()) {
			dropped = append(dropped, name)
		}
	}
	sort.Strings(dropped)
	require.Emptyf(t, dropped,
		"复制分组时这些字段没有被搬过去，也没在 groupFieldsIntentionallyNotDuplicated 里声明：\n  %s\n"+
			"管理员会以为复制出来的分组和源分组一致；计费类字段漏掉就是直接算错钱。"+
			"要么在 cloneGroupForDuplicate 里补上，要么在声明表里写明为什么不搬。",
		strings.Join(dropped, "\n  "))

	// 反向：声明表不许列出实际上被搬过去的字段，否则这张表会慢慢变成谎话。
	for name := range groupFieldsIntentionallyNotDuplicated {
		field, ok := groupType.FieldByName(name)
		require.Truef(t, ok, "声明表里的 Group.%s 已经不存在了", name)
		idx := field.Index[0]
		if name == "Name" || name == "Status" || name == "DuplicateOperationID" || name == "AccountGroups" {
			continue // 前三个被刻意改写成别的值；AccountGroups 上面没填探针值
		}
		require.NotEqualf(t, sourceValue.Field(idx).Interface(), clonedValue.Field(idx).Interface(),
			"Group.%s 其实被复制过去了，从声明表里删掉它", name)
	}
}
