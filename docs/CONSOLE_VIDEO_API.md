# Console 视频 API（fork 增强）

> 版本标签：`3.1.2-console-video.1`（相对官方 v3.1.2）  
> 分支：`feat/console-video-official-capabilities`  
> 范围：本 fork 在 **Grok Console** 路径上对齐 xAI 官方视频形态；**不自动部署生产盒子**。

更完整的实施计划见 [CONSOLE_VIDEO_OFFICIAL_ENHANCEMENT_PLAN.md](./CONSOLE_VIDEO_OFFICIAL_ENHANCEMENT_PLAN.md)。

---

## 1. 鉴权

```http
Authorization: Bearer g2a_xxx_xxx
Content-Type: application/json
```

---

## 2. 模型

| Public model | Provider | 能力 |
|--------------|----------|------|
| `grok-imagine-video` | Console / Web 等 | T2V、I2V、R2V；**edits / extensions（仅 Console）** |
| `grok-imagine-video-1.5` | Console（及 Build Super 等） | T2V、I2V、R2V；分辨率可 **1080p** |
| `Console/grok-imagine-video` | 固定 Console | 同上 |
| `Console/grok-imagine-video-1.5` | 固定 Console | 同上 |

说明：

- **编辑 / 扩展**当前仅支持上游模型 **`grok-imagine-video`**（官方实测；1.5 会返回明确错误）。
- 路由会优先把 edits/extensions 派到 **Console** 账号池；Web/Build 适配器会拒绝这两种模式。

---

## 3. 接口

### 3.1 `POST /v1/videos/generations`

异步创建。成功响应：

```json
{ "request_id": "video_…" }
```

| 字段 | 说明 |
|------|------|
| `model` | 必填 |
| `prompt` | T2V/R2V 通常需要；纯 I2V 可省略 |
| `duration` | 1–15，默认 8 |
| `aspect_ratio` | `1:1` `16:9` `9:16` `4:3` `3:4` `3:2` `2:3` 等 |
| `resolution` | `480p` / `720p`；**1.5 另允许 `1080p`** |
| `image` | I2V 首帧：`{ "url" }` 或 `{ "file_id" }` |
| `reference_images` | R2V 参考图数组（多张） |
| `output` / `storage_options` | 暂不支持，传了会 400 |

**互斥规则（官方）：** `image` 与 `reference_images` **不能同时出现**。

示例（多参考 R2V）：

```json
{
  "model": "grok-imagine-video-1.5",
  "prompt": "cinematic product turntable",
  "duration": 6,
  "resolution": "720p",
  "reference_images": [
    { "url": "https://example.com/a.png" },
    { "url": "https://example.com/b.png" }
  ]
}
```

### 3.2 `POST /v1/videos/edits`

```json
{
  "model": "grok-imagine-video",
  "prompt": "轻微电影感调色，保持主体动作",
  "video": { "url": "https://… 或 data:video/mp4;base64,…" }
}
```

- **不要**传 `duration`（会 400）。
- 与 generations 共用轮询 / content。

### 3.3 `POST /v1/videos/extensions`

```json
{
  "model": "grok-imagine-video",
  "prompt": "镜头继续推进，动作自然延伸",
  "duration": 6,
  "video": { "url": "…" }
}
```

- `duration`：**扩展段**秒数，**2–10**，默认 **6**（不是成片总时长）。

### 3.4 轮询与下载

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/v1/videos/{request_id}` | `pending` / `done` / `failed` |
| `GET` | `/v1/videos/{request_id}/content` | 成片流（优先本地归档） |

`done` 时 `video.url` 指向网关 content URL（可配置 public base）。

---

## 4. 错误与状态码

### 4.1 创建阶段（同步 HTTP）

| 情况 | HTTP | `error.code`（OpenAI 风格） |
|------|------|------------------------------|
| 参数非法、互斥、缺字段 | 400 | `invalid_request` |
| 模型不存在 | 404 | `model_not_found` |
| 输入过大 / 临时 file 失效 | 400 | `invalid_request` |
| 无可用 Console 账号（edit/extend） | 503 | `upstream_unavailable` |
| 客户端额度 | 429 | `billing_limit_exceeded` 等 |

### 4.2 任务失败（轮询 `status=failed`）

| 内部 `ErrorCode` | 对外 `error.code` | 典型含义 |
|------------------|-------------------|----------|
| `invalid_argument` | `invalid_argument` | 上游/本地参数问题 |
| `rate_limited` | `rate_limit_exceeded` | 速率或视频额度受限 |
| `quota_exhausted` | `resource_exhausted` | 额度不足 |
| `provider_unavailable` / `account_unavailable` | `service_unavailable` | 账号/鉴权不可用 |
| `generation_failed` 等 | `internal_error` | 其它生成失败 |

文案已尽量避免把 R2V 说成「多张首图」；I2V 字段称为 **image（首帧）**，R2V 称为 **reference_images**。

---

## 5. 配额

- Console / Web / Build 的 **视频类配额**（`QuotaModeVideo` 等）在 **generations、edits、extensions** 之间**共享同一视频额度池**（成功完成后按既有逻辑扣减）。
- Console 免费档视频次数通常很少（实测约数次/号）；联调请准备多账号或小文件。

---

## 6. 大 body / data URL 注意

- `data:image/…;base64,` 与 `data:video/mp4;base64,` 均可能使请求体很大。
- 过大 data URL 容易触发上游 **Auth context expired** 或网关 body 上限。
- **建议**：
  1. 优先使用 **HTTPS** 可拉取 URL；
  2. 编辑/扩展用 **较短、较小的 mp4**；
  3. 图片尽量压缩后再传；
  4. 未来可走「先落本地 media 再出站 HTTPS」（本期未做完整 Files 生态）。

网关对视频输入 JSON 有 **约 32 MiB** 持久化上限（`media.MaxInputJSONBytes`）。

---

## 7. Provider 差异（摘要）

| 能力 | Console | Web | Build |
|------|---------|-----|-------|
| 多参考 R2V | ✅ | ✅（既有） | ❌ 最多 1 张 image |
| `grok-imagine-video-1.5` | ✅ | — | Super/付费路径 |
| edits / extensions | ✅ | ❌ 明确拒绝 | ❌ 明确拒绝 |
| 1080p | 仅 1.5 | 视上游 | 视上游 |

---

## 8. 相关文件

- Handler：`backend/internal/transport/http/inference/handler.go`
- Gateway：`backend/internal/application/gateway/video.go`
- Console：`backend/internal/infra/provider/console/media.go`
- Swagger 注释：`backend/internal/transport/http/swagger_annotations.go`（`make swagger` 再生 `backend/docs/*`）
