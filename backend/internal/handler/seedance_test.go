//go:build unit

package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestSeedanceHandlerLifecycleAndOwnership(t *testing.T) {
	h, slots, bindings, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
	var owner int64
	upstream.call = func(req *http.Request, id int64) (*http.Response, error) {
		body := `{"id":"task-ark","status":"queued"}`
		if req.Method == http.MethodPost {
			owner = id
		} else {
			require.Equal(t, owner, id)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}
	newContext := func(method string) (*gin.Context, *httptest.ResponseRecorder) {
		c, w := grokMediaSlotContext(context.Background(), method == http.MethodPost)
		key, _ := middleware.GetAPIKeyFromContext(c)
		key.Group.Platform = service.PlatformOpenAI
		body := ""
		if method == http.MethodPost {
			body = `{"model":"doubao-seedance","content":[{"type":"text","text":"waves"}]}`
		}
		c.Request = httptest.NewRequest(method, "/api/v3/contents/generations/tasks", strings.NewReader(body))
		c.Params = gin.Params{{Key: "task_id", Value: "task-ark"}}
		return c, w
	}
	c, w := newContext(http.MethodPost)
	h.SeedanceTasks(c)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Positive(t, owner)
	require.Len(t, bindings.pending, 1)
	slots.assertReleased(t)
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		c, w = newContext(method)
		h.SeedanceTasks(c)
		require.Equal(t, 200, w.Code, w.Body.String())
		slots.assertReleased(t)
	}
	for _, other := range []string{"user", "key", "group", "task", "provider"} {
		c, w = newContext(http.MethodGet)
		key, _ := middleware.GetAPIKeyFromContext(c)
		switch other {
		case "user":
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 11, Concurrency: 5})
		case "key":
			key.ID = 21
		case "group":
			group := int64(25)
			key.GroupID = &group
		case "task":
			c.Params = gin.Params{{Key: "task_id", Value: "other"}}
		case "provider":
			c.Params = gin.Params{{Key: "request_id", Value: "task-ark"}}
		}
		before := upstream.calls
		if other == "provider" {
			h.GrokVideoStatus(c)
		} else {
			h.SeedanceTasks(c)
		}
		require.Equal(t, 404, w.Code, other+": "+w.Body.String())
		require.Equal(t, before, upstream.calls)
		slots.assertReleased(t)
	}
	c, _ = newContext(http.MethodGet)
	key, _ := middleware.GetAPIKeyFromContext(c)
	subject, _ := middleware.GetAuthSubjectFromContext(c)
	result := &service.OpenAIForwardResult{Usage: service.OpenAIUsage{OutputTokens: 12345}, ResponseID: "seedance:task-ark", UpstreamTaskStatus: "succeeded"}
	for i := range 20 {
		billed, _, _ := h.gatewayService.PrepareSeedanceCompletionBilling(context.Background(), zap.NewNop(), subject.UserID, key.ID, result.ResponseID, result)
		if i == 0 {
			require.NotNil(t, billed)
			require.Equal(t, "doubao-seedance", billed.BillingModel)
			require.Equal(t, 12345, billed.Usage.OutputTokens)
			require.Zero(t, billed.VideoCount)
		} else {
			require.Nil(t, billed)
		}
	}
	require.Len(t, bindings.billed, 1)
}

func seedanceTestContext(t *testing.T, method, body, taskID string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	c, w := grokMediaSlotContext(context.Background(), method == http.MethodPost)
	key, _ := middleware.GetAPIKeyFromContext(c)
	key.Group.Platform = service.PlatformOpenAI
	c.Request = httptest.NewRequest(method, "/api/v3/contents/generations/tasks", strings.NewReader(body))
	if taskID != "" {
		c.Params = gin.Params{{Key: "task_id", Value: taskID}}
	}
	return c, w
}

// 基于样片生成正式视频：样片只存在于创建它的账号上，请求必须发回那个账号；只能引用自己的任务。
func TestSeedanceDraftReferenceRoutesToOwningAccount(t *testing.T) {
	h, slots, bindings, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
	var createdOn []int64
	upstream.call = func(_ *http.Request, id int64) (*http.Response, error) {
		createdOn = append(createdOn, id)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"cgt-final"}`))}, nil
	}
	// 不带引用的创建落在调度器默认选的账号上；样片故意绑在另一个账号，才能区分「按样片路由」与「碰巧选中」。
	c, w := seedanceTestContext(t, http.MethodPost, `{"model":"doubao-seedance","content":[{"type":"text","text":"waves"}]}`, "")
	h.SeedanceTasks(c)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Len(t, createdOn, 1)
	draftOwner := int64(1)
	if createdOn[0] == draftOwner {
		draftOwner = 3
	}
	groupID := int64(24)
	require.NoError(t, h.gatewayService.BindGrokMediaVideoRequestAccount(context.Background(), &groupID, service.SeedanceTaskKey("cgt-draft"), 10, 20, draftOwner))
	slots.assertReleased(t)

	c, w = seedanceTestContext(t, http.MethodPost, `{"model":"doubao-seedance","content":[{"type":"draft_task","draft_task":{"id":"cgt-draft"}}]}`, "")
	h.SeedanceTasks(c)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Equal(t, draftOwner, createdOn[len(createdOn)-1], "正式视频必须发往持有样片的账号")
	slots.assertReleased(t)

	// 引用不属于自己（或不存在）的任务：拒绝，且不打上游。
	before := upstream.calls
	c, w = seedanceTestContext(t, http.MethodPost, `{"model":"doubao-seedance","content":[{"type":"draft_task","draft_task":{"id":"someone-elses"}}]}`, "")
	h.SeedanceTasks(c)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	require.Equal(t, before, upstream.calls)
	slots.assertReleased(t)

	// 引用的两个任务分属不同账号：无法同时满足，拒绝且不打上游。
	require.NoError(t, h.gatewayService.BindGrokMediaVideoRequestAccount(context.Background(), &groupID, service.SeedanceTaskKey("cgt-a"), 10, 20, 1))
	bindings.others = map[string]int64{bindings.key: bindings.owner}
	require.NoError(t, h.gatewayService.BindGrokMediaVideoRequestAccount(context.Background(), &groupID, service.SeedanceTaskKey("cgt-b"), 10, 20, 2))
	c, w = seedanceTestContext(t, http.MethodPost, `{"model":"doubao-seedance","content":[{"type":"draft_task","draft_task":{"id":"cgt-a"}},{"type":"draft_task","draft_task":{"id":"cgt-b"}}]}`, "")
	h.SeedanceTasks(c)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Equal(t, before, upstream.calls)
	slots.assertReleased(t)
}

// 创建时快照缺失仍然计费（用量来自上游查询结果本身），并留错误日志；抢标记出错按已计费处理，同样留痕。
func TestSeedanceCompletionBillingIsNeverSilent(t *testing.T) {
	h, _, bindings, _ := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
	c, _ := seedanceTestContext(t, http.MethodGet, "", "cgt-x")
	key, _ := middleware.GetAPIKeyFromContext(c)
	subject, _ := middleware.GetAuthSubjectFromContext(c)
	result := &service.OpenAIForwardResult{Usage: service.OpenAIUsage{OutputTokens: 40594}, UpstreamModel: "doubao-seedance-2-0-mini-260615", ResponseID: service.SeedanceTaskKey("cgt-x"), UpstreamTaskStatus: "succeeded"}

	core, logs := observer.New(zap.WarnLevel)
	billed, _, outcome := h.gatewayService.PrepareSeedanceCompletionBilling(context.Background(), zap.New(core), subject.UserID, key.ID, result.ResponseID, result)
	require.Equal(t, service.SeedanceBillingReady, outcome)
	require.NotNil(t, billed, "缺快照也要计费")
	require.Equal(t, "doubao-seedance-2-0-mini-260615", billed.BillingModel)
	require.Equal(t, 40594, billed.Usage.OutputTokens)
	require.Equal(t, service.StableGrokVideoBillingRequestID(result.ResponseID), billed.RequestID)
	require.Equal(t, 1, logs.FilterMessage("grok_media.video_billing_without_pending").Len())

	bindings.claimErr = errors.New("redis down")
	core, logs = observer.New(zap.WarnLevel)
	other := *result
	other.ResponseID = service.SeedanceTaskKey("cgt-y")
	billed, _, outcome = h.gatewayService.PrepareSeedanceCompletionBilling(context.Background(), zap.New(core), subject.UserID, key.ID, other.ResponseID, &other)
	require.Nil(t, billed)
	require.Equal(t, service.SeedanceBillingRetry, outcome, "抢标记出错：保留在结算索引里稍后再试")
	require.Equal(t, 1, logs.FilterMessage("grok_media.video_billing_claim_failed").Len())
}

type seedanceMemSettlementStore struct{ due map[string]time.Time }

func (m *seedanceMemSettlementStore) ScheduleSeedanceSettlement(_ context.Context, member string, dueAt time.Time) error {
	m.due[member] = dueAt
	return nil
}
func (m *seedanceMemSettlementStore) DueSeedanceSettlements(context.Context, time.Time, int) ([]string, error) {
	return nil, nil
}
func (m *seedanceMemSettlementStore) SeedanceSettlementPending(_ context.Context, member string) (bool, error) {
	_, ok := m.due[member]
	return ok, nil
}
func (m *seedanceMemSettlementStore) RemoveSeedanceSettlement(_ context.Context, member string) error {
	delete(m.due, member)
	return nil
}

// 透传 + 结算：创建时登记到结算索引；调用方查到成功并计费后移出；删除仍未结清的任务前先补查一次状态。
func TestSeedanceHandlerTracksTasksUntilSettled(t *testing.T) {
	h, slots, _, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
	store := &seedanceMemSettlementStore{due: map[string]time.Time{}}
	h.gatewayService.AttachSeedanceSettlement(service.NewSeedanceSettlementService(store, h.gatewayService, nil, nil, nil, nil))
	var calls []string
	status := `{"id":"task-ark","status":"queued"}`
	upstream.call = func(req *http.Request, _ int64) (*http.Response, error) {
		calls = append(calls, req.Method)
		body := status
		if req.Method == http.MethodPost {
			body = `{"id":"task-ark"}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}

	c, w := seedanceTestContext(t, http.MethodPost, `{"model":"doubao-seedance","content":[{"type":"text","text":"waves"}],"callback_url":"https://client.example/hook"}`, "")
	h.SeedanceTasks(c)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Len(t, store.due, 1, "创建即登记，带 callback_url 的请求照样原样透传")
	slots.assertReleased(t)

	// 删除一个还没结清的任务：先查状态（仍在排队，不计费），再转发 DELETE。
	calls = nil
	c, w = seedanceTestContext(t, http.MethodDelete, "", "task-ark")
	h.SeedanceTasks(c)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Equal(t, []string{http.MethodGet, http.MethodDelete}, calls)
	require.Len(t, store.due, 1, "排队中的任务仍待结算")
	slots.assertReleased(t)

	// 调用方查到成功：计费并移出索引。
	status = `{"id":"task-ark","status":"succeeded","usage":{"completion_tokens":5}}`
	c, w = seedanceTestContext(t, http.MethodGet, "", "task-ark")
	h.SeedanceTasks(c)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Empty(t, store.due, "计费后移出结算索引")
	slots.assertReleased(t)
}
