# grok2api admin frontend — local dev preview

Use **Vite HMR** for docs/UI changes. **Do not** run full `pnpm build` on p30 for
every tweak — use GitHub Actions (see below).

## GitHub CI（推荐发版路径）

Workflow：`.github/workflows/ci-artifacts-pages.yml`

```text
push feat/** 或 main / 手动 Run
  → frontend dist artifact（grok2api-frontend-dist）
  → linux arm64 + amd64 二进制 artifact
  → Cloudflare Pages 项目 grok2api-admin（仅管理前端静态站）
```

- Secrets：`CLOUDFLARE_API_TOKEN`、`CLOUDFLARE_ACCOUNT_ID`（与 infinite-canvas 相同）
- **不会**自动部署老家盒子上的生产 grok2api
- 本机 runtime 可下载 artifact：解压 frontend dist 到 `frontend/dist`，二进制放到 runtime 目录后重启本地进程
- Pages 上的管理端：在 UI 里把 API Base 指到你的 HTTPS/局域网 API（注意 CORS / Mixed Content）

## Prerequisites

1. Local API running (this lab):

```bash
curl -sS http://127.0.0.1:18000/healthz
# expect {"ok":true}
```

2. Frontend deps installed under `frontend/node_modules` (vite + tsc present).

## Start dev server

```bash
cd /root/work/grok2api/frontend
npm run dev:local
# or: VITE_DEV_API_TARGET=http://127.0.0.1:18000 npm run dev
```

- UI: `http://127.0.0.1:5173`
- Vite proxies `/api`, `/v1`, `/healthz`, `/readyz`, `/swagger` → `VITE_DEV_API_TARGET`
  (default **`http://127.0.0.1:18000`**)
- Change docs / i18n under `src/` → HMR, no full rebuild

Login uses the same admin credentials as the local runtime
(`bootstrapAdmin` in `/root/work/grok2api-local-runtime/config.yaml`).

## Access via 盒子 frpc (LAN)

**Prefer not to expose raw `vite` dev over frpc** (thousands of module requests → blank/slow). Options:

1. **Simplest admin UI via frp:** open API static UI  
   `http://192.168.15.144:22226/` (serves `frontend-dist` from grok2api)
2. **Or** `vite preview` of `frontend/dist` on port 5173 → frp `:25173`

Local HMR on the device: `npm run dev:local` → `http://127.0.0.1:5173`.

| Service | URL |
|---------|-----|
| Admin Vite preview/dev | `http://192.168.15.144:25173` |
| API + built admin | `http://192.168.15.144:22226` |

Open the **admin UI via :25173**. Browser calls stay same-origin to Vite (`/api`, `/v1`), and Vite proxies to local `:18000` — **no CORS** for that path.

Optional HMR through frp:

```bash
export VITE_HMR_HOST=192.168.15.144
export VITE_HMR_CLIENT_PORT=25173
npm run dev:local
```

### CORS / HTTPS notes

- Admin dev through frp+Vite proxy: generally **no CORS pain**.
- Calling API from another origin (e.g. canvas on `:22300`) needs CORS on the API (see backend middleware defaults).
- frp mappings are **HTTP only**; do not mix with production HTTPS pages (Mixed Content).

## Optional: other API ports

```bash
VITE_DEV_API_TARGET=http://127.0.0.1:8000 npm run dev
```

## Production frontend bundle (rare)

```bash
cd /root/work/grok2api/frontend
npm run build
# then copy dist → runtime frontend-dist if testing via :18000 static UI
rsync -a --delete dist/ /root/work/grok2api-local-runtime/frontend-dist/
```

## Backend (Go) while developing API

```bash
# already running in lab as:
# /root/work/grok2api-local-runtime/grok2api --config .../config.yaml
#
# After Go source changes, rebuild binary then restart that process only.
cd /root/work/grok2api/backend
go build -o /root/work/grok2api-local-runtime/grok2api.new ./cmd/grok2api
```

Frontend dev server does **not** require rebuilding the Go binary for pure UI/docs edits.
