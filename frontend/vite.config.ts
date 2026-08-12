import path from "node:path";

import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// Local runtime (this lab) listens on 18000; upstream default was 8000.
const devApiTarget = (process.env.VITE_DEV_API_TARGET || "http://127.0.0.1:18000").replace(/\/$/, "");

export default defineConfig({
  plugins: [react(), tailwindcss()],
  define: {
    __GROK2API_DEV_API_TARGET__: JSON.stringify(process.env.VITE_DEV_API_TARGET ?? ""),
  },
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  server: {
    host: "0.0.0.0",
    port: Number(process.env.VITE_DEV_PORT || 5173),
    strictPort: true,
    hmr: process.env.VITE_HMR_CLIENT_PORT
      ? {
          host: process.env.VITE_HMR_HOST || undefined,
          clientPort: Number(process.env.VITE_HMR_CLIENT_PORT),
          protocol: (process.env.VITE_HMR_PROTOCOL as "ws" | "wss" | undefined) || "ws",
        }
      : undefined,
    proxy: {
      // Admin UI + OpenAPI traffic → local grok2api process (HMR stays on Vite).
      "/api": devApiTarget,
      "/v1": devApiTarget,
      "/healthz": devApiTarget,
      "/readyz": devApiTarget,
      "/swagger": devApiTarget,
    },
  },
  preview: {
    host: "0.0.0.0",
    port: Number(process.env.VITE_PREVIEW_PORT || 4173),
  },
  build: {
    outDir: "dist",
    sourcemap: false,
  },
});
