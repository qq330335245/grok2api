package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// CORS allows browser clients (e.g. infinite-canvas on another origin) to call /v1 APIs.
// Reflects allowed Origin; handles OPTIONS preflight.
func CORS(allowedOrigins ...string) gin.HandlerFunc {
	allowAll := false
	set := map[string]struct{}{}
	for _, o := range allowedOrigins {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if o == "*" {
			allowAll = true
			continue
		}
		set[strings.TrimRight(o, "/")] = struct{}{}
	}
	// Sensible defaults for this lab (Pages + local/frp HTTP dev previews).
	if len(set) == 0 && !allowAll {
		for _, o := range []string{
			"https://i-canvas.konsin.de5.net",
			"https://infinite-canvas-a3o.pages.dev",
			// Vite dev on p30 / via 盒子 frpc (plain HTTP).
			"http://127.0.0.1:3000",
			"http://localhost:3000",
			"http://192.168.15.144:22300",
		} {
			set[o] = struct{}{}
		}
		allowAll = false
	}
	return func(c *gin.Context) {
		origin := strings.TrimSpace(c.GetHeader("Origin"))
		origin = strings.TrimRight(origin, "/")
		allowOrigin := ""
		if origin != "" {
			if allowAll {
				allowOrigin = origin
			} else if _, ok := set[origin]; ok {
				allowOrigin = origin
			}
		}
		if allowOrigin != "" {
			c.Header("Access-Control-Allow-Origin", allowOrigin)
			c.Header("Vary", "Origin")
			c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Requested-With, anthropic-version")
			c.Header("Access-Control-Expose-Headers", "Content-Type, X-Request-Id")
			c.Header("Access-Control-Max-Age", "86400")
		}
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
