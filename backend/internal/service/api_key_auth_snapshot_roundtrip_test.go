package service

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// TestAuthGroupSnapshotRoundTripCoversEveryField 是认证快照分组字段的声明驱动护栏：
// 遍历 APIKeyAuthGroupSnapshot 的每个字段，在 Group 上填入非零值，走真实链路
// snapshotFromAPIKey → JSON 编解码（L2 缓存形态）→ snapshotToAPIKey，然后逐字段断言：
//   - 快照里该字段非零（snapshotFromAPIKey 漏映射时失败）；
//   - 还原后的 Group 字段与原值相等（snapshotToAPIKey 漏映射时失败）。
//
// 快照结构体每新增一个字段，本测试当天就覆盖它——不需要再手写一条 round-trip 测试。
func TestAuthGroupSnapshotRoundTripCoversEveryField(t *testing.T) {
	snapshotType := reflect.TypeOf(APIKeyAuthGroupSnapshot{})
	groupType := reflect.TypeOf(Group{})

	original := &Group{}
	originalValue := reflect.ValueOf(original).Elem()
	for i := 0; i < snapshotType.NumField(); i++ {
		field := snapshotType.Field(i)
		groupField, ok := groupType.FieldByName(field.Name)
		require.True(t, ok, "snapshot field %s has no Group counterpart", field.Name)
		require.Equal(t, groupField.Type, field.Type, "snapshot field %s type drifted from Group", field.Name)
		originalValue.FieldByName(field.Name).Set(snapshotProbeValue(t, field.Type, field.Name, i+1))
	}
	// Status 影响 IsActive 等判定，但快照只要求原样搬运，任意非零字符串即可。

	groupID := original.ID
	apiKey := &APIKey{
		ID:      1,
		UserID:  2,
		GroupID: &groupID,
		Key:     "k-snapshot-roundtrip",
		Status:  StatusActive,
		User:    &User{ID: 2, Status: StatusActive, Role: RoleUser, Balance: 1, Concurrency: 1},
		Group:   original,
	}
	svc := NewAPIKeyService(nil, nil, nil, nil, nil, nil, &config.Config{})

	snapshot := svc.snapshotFromAPIKey(context.Background(), apiKey)
	require.NotNil(t, snapshot)
	require.NotNil(t, snapshot.Group)

	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	var decoded APIKeyAuthSnapshot
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.NotNil(t, decoded.Group)

	restored := svc.snapshotToAPIKey(apiKey.Key, &decoded)
	require.NotNil(t, restored)
	require.NotNil(t, restored.Group)

	snapshotValue := reflect.ValueOf(*decoded.Group)
	restoredValue := reflect.ValueOf(*restored.Group)
	for i := 0; i < snapshotType.NumField(); i++ {
		name := snapshotType.Field(i).Name
		require.False(t, snapshotValue.Field(i).IsZero(),
			"snapshotFromAPIKey does not map Group.%s into APIKeyAuthGroupSnapshot", name)
		require.Equal(t, originalValue.FieldByName(name).Interface(), restoredValue.FieldByName(name).Interface(),
			"snapshotToAPIKey does not restore Group.%s from the snapshot", name)
	}
}

// snapshotProbeValue 为字段类型构造一个可经 JSON 往返且非零的探针值。
// 字段名参与取值，保证相邻字段的值彼此不同（映射错位时能被发现）。
func snapshotProbeValue(t *testing.T, typ reflect.Type, name string, seed int) reflect.Value {
	t.Helper()
	switch {
	case typ == reflect.TypeOf(map[string]map[string]float64{}):
		// VideoModelPrices 经 NormalizeVideoModelPrices 规范化：键必须已是规范族名与分辨率。
		return reflect.ValueOf(map[string]map[string]float64{VideoPriceFamilyGrokImagineVideo: {VideoBillingResolution480P: float64(seed) + 0.25}})
	case typ == reflect.TypeOf(time.Time{}):
		return reflect.ValueOf(time.Date(2026, time.January, seed%28+1, 3, 4, 5, 0, time.UTC))
	}
	value := reflect.New(typ).Elem()
	fillSnapshotProbe(t, value, name, seed)
	require.False(t, value.IsZero(), "probe for %s (%s) must be non-zero", name, typ)
	return value
}

func fillSnapshotProbe(t *testing.T, v reflect.Value, name string, seed int) {
	t.Helper()
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(seed))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(seed))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(seed) + 0.5)
	case reflect.String:
		v.SetString(fmt.Sprintf("probe-%s-%d", name, seed))
	case reflect.Ptr:
		elem := reflect.New(v.Type().Elem())
		fillSnapshotProbe(t, elem.Elem(), name, seed)
		v.Set(elem)
	case reflect.Slice:
		elem := reflect.New(v.Type().Elem()).Elem()
		fillSnapshotProbe(t, elem, name, seed)
		v.Set(reflect.Append(reflect.MakeSlice(v.Type(), 0, 1), elem))
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key := reflect.New(v.Type().Key()).Elem()
		fillSnapshotProbe(t, key, name+"-key", seed)
		val := reflect.New(v.Type().Elem()).Elem()
		fillSnapshotProbe(t, val, name+"-val", seed)
		m.SetMapIndex(key, val)
		v.Set(m)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Date(2026, time.January, seed%28+1, 3, 4, 5, 0, time.UTC)))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			field := v.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			fillSnapshotProbe(t, v.Field(i), name+"."+field.Name, seed+i+1)
		}
	default:
		t.Fatalf("snapshot probe does not know how to fill %s (%s)", name, v.Type())
	}
}

// groupFieldsIntentionallyAbsentFromAuthSnapshot 声明哪些 Group 字段刻意不进认证快照。
//
// 认证快照是网关热路径唯一读到的分组视图。少一个字段的症状是"这个分组配置在网关上
// 完全不起作用"，而且不会有任何报错——历史上 model_pricing 就是这么漏掉的。
//
// 只有"快照少了字段"这一个方向会造成这种故障，而原来的 round-trip 护栏遍历的是
// 快照结构体：往 Group 上加一个新字段而忘了加进快照，它全绿。这里把方向反过来，
// 遍历 Group：新增字段必须要么进快照，要么在这张表里写下不进的理由。
var groupFieldsIntentionallyAbsentFromAuthSnapshot = map[string]string{
	"Description":             "管理页展示文案，网关不读",
	"SortOrder":               "管理页排序，网关不读",
	"DuplicateOperationID":    "复制分组的幂等键，只在管理写路径用",
	"CreatedAt":               "记账列",
	"UpdatedAt":               "记账列",
	"Hydrated":                "内存态标记（这份 Group 是否已完整加载），不是分组配置",
	"AccountCount":            "聚合统计，管理页用",
	"ActiveAccountCount":      "聚合统计，管理页用",
	"RateLimitedAccountCount": "聚合统计，管理页用",
	"AccountGroups":           "关联对象，不是分组配置",
	"DefaultValidityDays":     "建密钥时用，鉴权链路不读",
	// 批量图片走 GroupRepo.GetByIDLite 现取分组（batch_image_public.go:1008），
	// 不经过认证快照。
	"BatchImageDiscountMultiplier": "批量图片结算按 GetByIDLite 现取分组，不走认证快照",
	"BatchImageHoldMultiplier":     "同上",
	// 账号侧准入校验按现取的分组判断（account_service.go），不在网关鉴权热路径上。
	"RequireOAuthOnly":  "账号准入校验用，按现取分组判断",
	"RequirePrivacySet": "同上",
}

// TestEveryGroupFieldIsEitherInTheAuthSnapshotOrDeclaredAbsent 遍历 Group 的每个字段，
// 要求它要么出现在 APIKeyAuthGroupSnapshot 里，要么在上面的声明表里写明为什么不进。
//
// 没有第三种"默默不带"的状态：漏带的字段在网关上静默失效，而调用方看到的是一个
// 完全正常的响应。
func TestEveryGroupFieldIsEitherInTheAuthSnapshotOrDeclaredAbsent(t *testing.T) {
	snapshotType := reflect.TypeOf(APIKeyAuthGroupSnapshot{})
	inSnapshot := map[string]bool{}
	for i := 0; i < snapshotType.NumField(); i++ {
		inSnapshot[snapshotType.Field(i).Name] = true
	}

	groupType := reflect.TypeOf(Group{})
	var undeclared []string
	for i := 0; i < groupType.NumField(); i++ {
		name := groupType.Field(i).Name
		if inSnapshot[name] {
			continue
		}
		if reason, ok := groupFieldsIntentionallyAbsentFromAuthSnapshot[name]; ok {
			require.NotEmptyf(t, reason, "Group.%s 声明为不进快照，但没写理由", name)
			continue
		}
		undeclared = append(undeclared, name)
	}
	sort.Strings(undeclared)
	require.Emptyf(t, undeclared,
		"这些 Group 字段既不在认证快照里，也没在 groupFieldsIntentionallyAbsentFromAuthSnapshot 里声明：\n  %s\n"+
			"网关热路径只读快照，漏带的字段会在网关上静默失效（model_pricing 就是这么漏的）。"+
			"要么加进 APIKeyAuthGroupSnapshot 并在 snapshotFromAPIKey/snapshotToAPIKey 里映射，"+
			"要么在声明表里写明为什么不需要。",
		strings.Join(undeclared, "\n  "))

	// 反向：声明表里不许出现快照其实带了的字段（否则这张表会慢慢变成谎话）。
	for name := range groupFieldsIntentionallyAbsentFromAuthSnapshot {
		require.Falsef(t, inSnapshot[name], "Group.%s 其实在快照里，从声明表里删掉它", name)
		_, ok := groupType.FieldByName(name)
		require.Truef(t, ok, "声明表里的 Group.%s 已经不存在了", name)
	}
}
