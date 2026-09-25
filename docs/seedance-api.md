# Seedance 原生 API

支持火山方舟 Ark 的异步视频任务协议，无需把 `content[]` 转换成 OpenAI `messages` 或 Grok `prompt`。

## 配置

1. 创建 OpenAI 平台的 **API Key** 账号，填写 Ark API Key，Base URL 使用 `https://ark.cn-beijing.volces.com/api/v3`。兼容服务可填写自己的 `/api/v3` 或 `/v3` Base URL。
2. 在账号的端点能力中勾选 **Seedance (Ark)**。默认不启用，避免请求误调度到其他 OpenAI 账号。支持创建、编辑与批量编辑。
3. 将账号加入 OpenAI 分组并启用分组的「允许图片生成」媒体权限；合成分组也可路由到这些账号。
4. 配置模型映射，例如将公开模型名 `seedance-video` 映射到实际 `doubao-seedance-*` 模型或 `ep-*` 推理接入点。配置对应模型的输出 token 价格；本接口不使用 Grok 的按秒视频价格。

## 调用

```bash
curl "$SUB2API_BASE_URL/api/v3/contents/generations/tasks" \
  -H "Authorization: Bearer $SUB2API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "seedance-video",
    "content": [{"type": "text", "text": "海浪轻轻拍打沙滩"}],
    "duration": 5,
    "resolution": "720p",
    "ratio": "16:9",
    "generate_audio": true
  }'

# 使用创建响应中的原生 id 查询，直至 succeeded / failed / cancelled 等终态。
curl "$SUB2API_BASE_URL/api/v3/contents/generations/tasks/$TASK_ID" \
  -H "Authorization: Bearer $SUB2API_KEY"

curl -X DELETE "$SUB2API_BASE_URL/api/v3/contents/generations/tasks/$TASK_ID" \
  -H "Authorization: Bearer $SUB2API_KEY"
```

亦支持 `/v3`、`/v1` 和无版本前缀别名。Ark SDK 的 Base URL 可改为 `$SUB2API_BASE_URL/api/v3`。文本、图片、视频、音频内容、角色及扩展参数原样传递，仅按账号配置改写模型名；响应保持上游原生格式。

`callback_url`、`execution_expires_after`、`tools` 等字段同样原样转发。唯一的路由规则：

- **样片（Draft）转正式视频**：`content` 里的 `draft_task` 只能引用自己（同一用户、同一 API Key、同一分组）创建的样片任务，请求会发往创建该样片的账号（样片只存在于那个上游账号上）；引用不到返回 404，引用的多个任务分属不同账号返回 400，持有样片的账号暂不可用返回 503。

## 任务与计费

- 查询和删除只能访问同一用户、同一 API Key、同一分组创建的任务，并始终使用原提交账号；不会转到其他账号查询。
- 每个经网关创建、最终成功的任务**恰好计费一次**，在第一次观察到 `succeeded` 时按上游 `usage.completion_tokens` 计费：可能是调用方的查询，也可能是网关的后台补查。创建时不扣费；失败、取消、过期的任务不计费；重复观察由共享缓存声明和持久化用量去重共同保护。
- 后台补查：方舟会把结果直接推给 `callback_url`，调用方未必再经网关查询，而方舟对成功任务照常收费。网关因此把每个新任务登记到结算索引，调用方正常轮询时第一次查到成功即结清、不会发生补查；否则创建后 10 分钟起补查，间隔随任务年龄增长、最长 1 小时一次，结清或进入终态即停。后台结清的用量行 `inbound_endpoint` 为 `gateway:seedance-settlement`。
- 删除（DELETE）一个尚未结清的任务前，网关先查一次状态并结清，再转发删除——删除后任务记录就查不到了。
- Redis 保存任务绑定、创建时的计费快照（模型、账号、额度归属平台、创建时间）与结算索引 7 天，与方舟的任务保留期一致（任务记录保存 7 天；排队/运行最长 `execution_expires_after` = 72 小时；成功后 `video_url` 有效 24 小时）。
- 创建时快照缺失（写入失败等）时仍按查询结果中的 `usage.completion_tokens` 计费，模型名取上游返回值，并记错误日志；读快照或抢计费标记失败同样记日志，不会静默跳过。
- 不开放上游的任务列表接口，防止共享账号的任务泄露给其他用户。删除遵循上游语义，不自动退款。
- 异步创建的上游错误不自动重试，以免重复创建付费任务。

协议依据：[火山官方 Go SDK](https://github.com/volcengine/volcengine-go-sdk/blob/master/service/arkruntime/model/content_generation.go)、[创建任务文档](https://www.volcengine.com/docs/82379/1520757)、[查询任务文档](https://www.volcengine.com/docs/82379/1521309)。
