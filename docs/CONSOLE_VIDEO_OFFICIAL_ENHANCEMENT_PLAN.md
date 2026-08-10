# Console 视频官方能力增强计划

> 仓库：`qq330335245/grok2api`（fork 自 `chenyme/grok2api`）  
> 基线：官方 **v3.1.2**（`main`）  
> 状态：计划定稿，按阶段实施  
> 约束：**不直接改老家盒子生产 grok2api 容器/配置**；本机 fork 开发，部署需用户明确同意。

---

## 1. 背景与目标

### 1.1 问题

无限画布等客户端按 **xAI 官方视频 API 形态** 调用 grok2api 时，Console 链路存在能力缺口或语义错误，例如：

- 多张 `reference_images` 被当成「多张首图」并在本地拒绝（`最多支持 1 张首图`）
- `image`（I2V 首帧）与 `reference_images`（R2V 参考）未分流
- Console 侧几乎只有 `grok-imagine-video`，缺少可用的 **`grok-imagine-video-1.5`**
- 缺少官方 **`/v1/videos/edits`**、**`/v1/videos/extensions`** 端到端实现

### 1.2 上游实测结论（旁路 Console SSO+DPoP，未改生产）

在 `https://console.x.ai` 上用官方 JSON 形状验证（模型/能力）：

| 能力 | 模型 | 结果 |
|------|------|------|
| 多参考 R2V（`reference_images`×4） | `grok-imagine-video` | 成功 |
| 多参考 R2V | `grok-imagine-video-1.5` | 成功 |
| 文生 T2V | `grok-imagine-video-1.5` | 成功 |
| 首帧 I2V（`image`） | `grok-imagine-video-1.5` | 成功 |
| 视频编辑 `POST /v1/videos/edits` | `grok-imagine-video` | 成功 |
| 视频扩展 `POST /v1/videos/extensions` | `grok-imagine-video` | 成功 |

结论：**上游已支持；缺口主要在 grok2api 网关/Console 适配层。**

### 1.3 目标

在 **保持 OpenAI/xAI 兼容对外 API** 的前提下，补齐 Console 视频路径，使客户端可：

1. 正确使用 `image` / `reference_images`（互斥、语义正确）
2. 多参考 R2V
3. 使用 `grok-imagine-video-1.5`（T2V/I2V/R2V）
4. 调用视频编辑、视频扩展
5. 校验/错误/模型目录与官方行为对齐

非目标（本期不做或仅预留）：

- 不强制改 Web/Build 的全部差异（可跟进对齐）
- 不实现 `reference_audios` / `file_id` / `storage_options` / `output.upload_url` 的完整官方 Files 生态（二期）
- 不自动部署到用户生产盒子

---

## 2. 现状架构（相关部分）

```
Client
  POST /v1/videos/generations  (及未来 edits/extensions)
       │
       ▼
inference handler  ──解析 body──► gateway.CreateVideo / …
       │                              │
       │                              ▼
       │                        media_jobs + 选号 (Console/Web/Build)
       │                              │
       ▼                              ▼
  poll GET /v1/videos/{id}     provider.GenerateVideo / …
  content GET …/content              │
                                     ├─ console → console.x.ai/v1/videos/* + DPoP
                                     ├─ web     → grok.com rest imagine
                                     └─ build   → cli-chat-proxy / api.x.ai
```

Console 出站关键点（官方源码）：

- 创建：`POST {base}/v1/videos/generations`
- 轮询：`GET {base}/v1/videos/{request_id}`
- 鉴权：SSO Cookie + DPoP（`/v1/dpop/token`）
- 成片域名：`vidgen.x.ai`

当前实现问题摘要：

| 层 | 问题 |
|----|------|
| Handler | `image` + `reference_images` 合并为单一列表；无 edits/extensions 路由 |
| Gateway VideoInput | 仅 `ReferenceURLs []string`，无法区分首帧/参考 |
| Console `GenerateVideo` | `consoleMaxVideoImages=1`；只写 `image`；模型写死 `grok-imagine-video`；分辨率仅 480p/720p |
| Catalog | Console 视频模型注册不完整（缺 1.5 能力矩阵） |

---

## 3. 增强清单（已与产品方对齐）

### P0 — 必须

1. **`image` / `reference_images` 语义分离**  
   - Handler 不再无脑合并  
   - 官方互斥：`image` 与 `reference_images` 不同开（400）  
   - Gateway / Job 持久化能还原两种输入  

2. **多参考 R2V**  
   - Console 出站发送 `reference_images: [{url}, …]`  
   - 取消「合计当首图且 max=1」的错误限制  
   - 合理上限（建议对齐官方/域常量，如 ≤8，可配置）  

3. **`grok-imagine-video-1.5`（Console）**  
   - 模型目录 / 路由 / 出站 `model` 字段  
   - T2V / I2V / R2V  
   - 分辨率：1.5 允许 1080p（按官方）；v1 仍 480p/720p  

4. **视频编辑**  
   - 对外：`POST /v1/videos/edits`  
   - 出站：`POST console.x.ai/v1/videos/edits`  
   - Body：`model` + `prompt` + `video.{url|file_id}`  
   - 异步：同 `request_id` 轮询 / content  

5. **视频扩展**  
   - 对外：`POST /v1/videos/extensions`  
   - 出站：`POST console.x.ai/v1/videos/extensions`  
   - `duration` = **扩展段** 秒数（官方 2–10，默认 6）  
   - 输入视频约束：mp4、时长等按官方文档校验（能做的做，上游错误原样映射）  

### P1 — 配套

6. OpenAPI / 前端 API 文档补充 edits、extensions、1.5、reference_images  
7. 错误码映射（额度 429、审核、invalid_argument 等）与文案（避免「首图」误导 R2V）  
8. 大 body / data URL：文档说明；可选「先落本地 media 再出站 HTTPS」（降低 Auth expired）  
9. 单测：handler 互斥与分流、console payload 形状、edits/extensions 路由  
10. 无限画布对齐（另仓）：编辑/扩展入口、1.5 设置（本计划不阻塞网关）  

### P2 — 二期

11. `reference_audios`  
12. `file_id` / Files API 完整链路  
13. `storage_options` / `output.upload_url`  
14. Web/Build 与 Console 字段语义对齐（Build 亦有 max=1 类限制）  

---

## 4. 对外 API 契约（目标）

### 4.1 `POST /v1/videos/generations`

| 字段 | 说明 |
|------|------|
| `model` | `grok-imagine-video` \| `grok-imagine-video-1.5`（及带 `Console/` 前缀的公共 id） |
| `prompt` | T2V/R2V 需要；纯 I2V 可空 |
| `duration` | 1–15，默认 8 |
| `aspect_ratio` | 官方枚举 |
| `resolution` | 480p/720p；**1.5 另允许 1080p** |
| `image` | 可选，I2V 首帧；与 `reference_images` **互斥** |
| `reference_images` | 可选，R2V；多张 |

### 4.2 `POST /v1/videos/edits`

```json
{
  "model": "grok-imagine-video",
  "prompt": "…",
  "video": { "url": "https://… or data:video/mp4;base64,…" }
}
```

### 4.3 `POST /v1/videos/extensions`

```json
{
  "model": "grok-imagine-video",
  "prompt": "…",
  "duration": 6,
  "video": { "url": "…" }
}
```

### 4.4 轮询 / 下载（已有，保持）

- `GET /v1/videos/{request_id}` → `pending|done|failed|…`  
- `GET /v1/videos/{request_id}/content`  

---

## 5. 内部设计要点

### 5.1 输入模型

扩展 `gateway.VideoInput`（名称可调整）：

```text
Prompt, Duration, AspectRatio, Resolution, PublicModel
FirstFrameURL   optional   // 来自 image
ReferenceURLs   []string   // 仅 reference_images
SourceVideoURL  optional   // edits/extensions
Mode            generate | edit | extend
```

`media_jobs.input_json` 需能持久化上述结构（兼容旧 `image_urls` 仅参考/混合历史数据）。

### 5.2 Console 出站 payload

**generate**

- 仅 first frame → `image.url`  
- 仅 references → `reference_images`  
- 禁止同时  
- `model` 按路由上游名（含 1.5）  

**edit / extend**

- `video.url` + `prompt`（+ extend `duration`）  
- 模型默认/校验：`grok-imagine-video`（按官方；若上游日后放开 1.5 再开）  

### 5.3 与 Web/Build

- 本期 **Console 优先** 完整打通  
- Web：已有多参考能力则避免回归  
- Build：可单独 issue 对齐 max 图与 reference_images  

---

## 6. 实施阶段

### Phase 0 — 工程基线

- [x] 计划文档  
- [x] GitHub fork：`qq330335245/grok2api` ← `chenyme/grok2api`  
- [x] 开发分支：`feat/console-video-official-capabilities`  
- [ ] CI 本地：`go test` 相关包可跑  

### Phase 1 — 生成路径语义修复（P0-1,2,3）

1. [x] Handler：解析并互斥校验 `image` / `reference_images`  
2. [x] Gateway / job 输入结构升级 + 迁移兼容  
3. [x] Console `GenerateVideo`：  
   - 多参考  
   - 正确字段  
   - 1.5 模型与分辨率  
4. [x] Catalog / model routes：Console 1.5  
5. [x] 相关单测（console/gateway/inference/cli/web）  

### Phase 2 — edits / extensions（P0-4,5）

1. [x] Handler 注册 `/v1/videos/edits`、`/v1/videos/extensions`  
2. [x] Gateway 统一 `Mode`（generate|edit|extend）+ `SourceVideoURL`  
3. [x] Console 出站 edits/extensions + 轮询复用；Web/Build 明确拒绝  
4. [x] 相关单测（console payload 路径、handler 校验）  
5. [ ] 大视频 data URL 说明/可选中转（Phase 3 文档）  

### Phase 3 — 打磨（P1）

1. 错误映射与文案  
2. Swagger / 控制台 API 文档  
3. 配额 kind=video 与多接口共享说明  
4. CHANGELOG / VERSION 策略（fork 可用 `3.1.2-console-video.1` 或文档标注）  

### Phase 4 — 二期（P2，另开计划）

- reference_audios、Files、Web/Build 对齐、画布产品  

---

## 7. 测试计划

### 7.1 自动化

- Handler：互斥 400；只 image；只 reference_images×N；1.5 分辨率  
- Console payload 单测（httptest）：generations / edits / extensions 路径与 JSON  
- 回归：旧客户端只传「一张图」仍可 I2V  

### 7.2 手动（旁路或 fork 私有部署，**禁止默认打生产**）

沿用 `/root/work/grok2api-console-video-probe` 工具集：

1. R2V 多图 × v1 / 1.5  
2. T2V 1.5  
3. I2V 1.5  
4. edit / extend（小 mp4）  

### 7.3 验收标准

- 官方形状请求不再被「最多 1 张首图」误杀  
- 多参考、1.5、edit、extend 在 Console 账号池下可完成并拉 content  
- 生产盒子未自动变更  

---

## 8. 风险与缓解

| 风险 | 缓解 |
|------|------|
| Console 免费 video 额度少（约 2 次/号） | 多账号；测试用小文件；文档说明 |
| 大 data URL → Auth context expired | 压缩；或先存 media 再出站 URL |
| 输入结构变更不兼容旧 job | input_json 双读旧 `image_urls` |
| 与官方 grok2api 未来合并冲突 | 小步提交；逻辑集中 console/media + handler + gateway video |
| 误部署生产 | 计划约束 + PR 仅面向 fork |

---

## 9. 参考

- xAI Docs：Video Generation / Image-to-Video / Reference-to-Video / Editing / Extension  
- REST：`/v1/videos/generations|edits|extensions`，`GET /v1/videos/{id}`  
- 本机实测产物：`/root/work/grok2api-console-video-probe/videos/`  
- 旁路工具：`backend/cmd/console-video-*-probe`（开发辅助，可不进主发布物）  

---

## 10. 进度记录

| 日期 | 事项 |
|------|------|
| 2026-08-10 | 旁路实测 gen/R2V/1.5/edit/extend；计划定稿 |
| 2026-08-10 | 确认 `qq330335245/grok2api` 已是官方 fork 且与 `chenyme/grok2api@main` **identical**（v3.1.2）；旧定制在独立仓 `grok2api-egress-enhanced`，本期不混入。开发分支 `feat/console-video-official-capabilities` |
| 2026-08-10 | Phase 1：image/reference 分流、多参考 R2V、Console 1.5（`f711ba1`） |
| 2026-08-10 | Phase 2：edits/extensions 端到端（Console 优先） |
