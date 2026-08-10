# Changelog

本 fork（`qq330335245/grok2api`）相对上游 `chenyme/grok2api` 的变更记录。

## 3.1.2-console-video.1 — 2026-08-10

### Added

- Console 视频官方能力增强（开发分支 `feat/console-video-official-capabilities`）：
  - `image`（I2V 首帧）与 `reference_images`（R2V）语义分离且互斥
  - 多参考 R2V 出站 `reference_images[]`
  - 模型目录注册 `Console/grok-imagine-video-1.5`（含 1080p 规则）
  - `POST /v1/videos/edits`、`POST /v1/videos/extensions`（Console 优先）
  - 用户文档：`docs/CONSOLE_VIDEO_API.md`、实施计划 `docs/CONSOLE_VIDEO_OFFICIAL_ENHANCEMENT_PLAN.md`
  - Swagger 注释覆盖 generations / edits / extensions / content

### Changed

- 视频任务失败码映射：`invalid_argument` / `rate_limit_exceeded` / `resource_exhausted` / `service_unavailable`
- 创建阶段校验错误改为 HTTP 400 `invalid_request`（不再误报成上游 502）
- 错误文案避免「多张首图」误导 R2V；Build 单图限制改为「1 张 image 输入」

### Notes

- **未**改动用户生产盒子上的 grok2api 部署；需显式同意后再发布。
- Web/Build 暂不支持 edits/extensions；`reference_audios` / 完整 Files API 为后续项。
