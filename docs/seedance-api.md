# Seedance 原生 API

支持火山方舟 Ark 的异步视频任务协议，无需把 `content[]` 转换成 OpenAI `messages` 或 Grok `prompt`。

## 配置

1. 创建 OpenAI 平台的 **API Key** 账号，填写 Ark API Key，Base URL 使用 `https://ark.cn-beijing.volces.com/api/v3`。兼容服务可填写自己的 `/api/v3` 或 `/v3` Base URL。
2. 在账号的端点能力中勾选 **Seedance (Ark)**。默认不启用，避免请求误调度到其他 OpenAI 账号。支持创建、编辑与批量编辑。
3. 将账号加入 OpenAI 分组并启用分组的「允许图片生成」媒体权限；合成分组也可路由到这些账号。
4. 配置模型映射，例如将公开模型名 `seedance-video` 映射到实际 `doubao-seedance-*` 模型或 `ep-*` 推理接入点。配置对应模型的输出 token 价格；本接口不使用 Grok 的按秒视频价格。
5. 按方舟的计价档分别定价（可选）：在分组或渠道定价里为 `<模型>@<分辨率>` 与 `<模型>@<分辨率>+video` 配输出 token 价格，例如 Seedance 2.5 按[方舟模型价格](https://www.volcengine.com/docs/82379/1099320)配 `doubao-seedance-2-5-260628@480p`、`@720p`（不含视频输入）、`@480p+video`、`@720p+video`、`@1080p`、`@1080p+video`。模型本身的价格作为兜底。

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
- **按计价档取价**：方舟按两个维度定价——输出分辨率 × 输入是否包含视频。网关据此给成功任务定档：分辨率取查询结果里的 `resolution`（样片 Step 1，即 `draft: true`，按 480p 计）；输入含视频指创建请求的 `content` 里有 `video_url`，基于样片生成正式视频（`draft_task`）时沿用样片那一步的判断（方舟规则：Step 2 的单价由 Step 1 是否含视频决定）。计费模型名写成 `<模型>@<分辨率>[+video]`，用量记录的 `model` 列即为档位名，「请求模型」列仍是客户端创建任务时请求的模型；档位名没有自己的逐模型倍率时沿用模型本身的倍率。只有 `<数字>p` 形式的分辨率算计价档（`@` 也会出现在真实模型名里，如 Vertex 上的 `claude-sonnet-4@20250514`，不会被当成档位）。
- 档位价没配、或上游没报可识别的分辨率时，按模型本身的价格计费并记告警日志（`seedance.billing_variant_unpriced` / `seedance.billing_variant_unknown`）；基于样片生成时读不到样片的计费快照，按本次请求内容判断并记 `seedance.draft_snapshot_missing`。
- 删除（DELETE）一个尚未结清的任务前，网关先查一次状态并结清，再转发删除——删除后任务记录就查不到了。
- Redis 保存任务绑定、创建时的计费快照（模型、账号、额度归属平台、创建时间）与结算索引 7 天，与方舟的任务保留期一致（任务记录保存 7 天；排队/运行最长 `execution_expires_after` = 72 小时；成功后 `video_url` 有效 24 小时）。
- 创建时快照缺失（写入失败等）时仍按查询结果中的 `usage.completion_tokens` 计费，模型名取上游返回值，并记错误日志；读快照或抢计费标记失败同样记日志，不会静默跳过。
- 不开放上游的任务列表接口，防止共享账号的任务泄露给其他用户。删除遵循上游语义，不自动退款。
- 异步创建的上游错误不自动重试，以免重复创建付费任务。

## 素材库（私域虚拟人像库）

方舟的素材库接口是管控面 OpenAPI，挂在站点根上：`POST /?Action=<Action>&Version=2024-01-01`，参数放在 JSON 请求体里。鉴权用网关的 API Key（`Authorization: Bearer`，不支持火山 AK/SK 签名），账号调度与任务接口相同（Key 所在分组里具备 seedance 能力的账号）。

```bash
curl -X POST "$SUB2API_BASE_URL/?Action=CreateAssetGroup&Version=2024-01-01" \
  -H "Authorization: Bearer $SUB2API_KEY" -H "Content-Type: application/json" \
  -d '{"Name":"figure_group_1","Description":"Figure group 1"}'

curl -X POST "$SUB2API_BASE_URL/?Action=ListAssets&Version=2024-01-01" \
  -H "Authorization: Bearer $SUB2API_KEY" -H "Content-Type: application/json" \
  -d '{"Filter":{"GroupType":"AIGC","GroupIds":["group-…"]},"PageNumber":1,"PageSize":20}'
```

- 开放的 Action：`CreateAssetGroup`、`CreateAsset`、`ListAssetGroups`、`ListAssets`、`GetAssetGroup`、`GetAsset`、`UpdateAssetGroup`、`UpdateAsset`、`DeleteAssetGroup`、`DeleteAsset`（官方 API 参考 2024-01-01 版的全部接口）。其他 Action 或版本返回 400 `InvalidActionOrVersion`；真人人像的认证与入库走方舟控制台扫码授权，没有 OpenAPI。
- 请求体与上游的回答原样透传（素材 ID、组 ID 即上游的原始 ID），生成时在 `content.<模态>_url.url` 里写 `asset://<素材 ID>` 引用。网关自己的错误同样按 OpenAPI 格式返回（`ResponseMetadata.Error.Code / Message`）。
- 素材按公开信息对待：同一上游账号下，各调用方建的素材组与素材彼此可见。
- 列表接口上游要求 `Filter.GroupType`（当前只有 `AIGC`）。
- 受 RPM、Key 额度窗口与用户/账号并发限制；不计费（没有上游对素材接口收费的依据）。每次调用记审计日志 `seedance.asset_action`，开启请求负载审计时请求与响应体另有记录。
- 前端页面只响应 GET/HEAD：`GET /` 仍是首页；任何非 GET/HEAD 请求都不会再拿到页面，未注册的路径返回 404。

协议依据：[火山官方 Go SDK](https://github.com/volcengine/volcengine-go-sdk/blob/master/service/arkruntime/model/content_generation.go)、[创建任务文档](https://www.volcengine.com/docs/82379/1520757)、[查询任务文档](https://www.volcengine.com/docs/82379/1521309)。
