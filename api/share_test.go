package api

import (
	"net/http"
	"net/http/httptest"
	"telecloud/database"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestSecurityHeadersMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Initialize a dummy in-memory database to prevent database.RODB nil-pointer panics
	_ = database.InitDB("sqlite", ":memory:", "")

	r := gin.New()
	r.Use(securityHeadersMiddleware())

	// Set up dummy routes to test the middleware headers
	r.GET("/s/:token/cbz/page", func(c *gin.Context) {
		c.String(http.StatusOK, "cbz page")
	})
	r.GET("/s/:token/epub/resource/content.xhtml", func(c *gin.Context) {
		c.String(http.StatusOK, "epub content")
	})
	r.GET("/s/:token/stream", func(c *gin.Context) {
		c.String(http.StatusOK, "other stream")
	})

	tests := []struct {
		name               string
		method             string
		path               string
		expectXFrame       bool
		expectCSP          bool
	}{
		{
			name:         "CBZ Page - X-Frame-Options SAMEORIGIN + strict CSP",
			method:       "GET",
			path:         "/s/dummy-token/cbz/page",
			expectXFrame: true,
			expectCSP:    true,
		},
		{
			name:         "EPUB Resource - X-Frame-Options SAMEORIGIN + strict CSP",
			method:       "GET",
			path:         "/s/dummy-token/epub/resource/content.xhtml",
			expectXFrame: true,
			expectCSP:    true,
		},
		{
			name:         "Standard Shared Stream - X-Frame-Options SAMEORIGIN, no special CSP",
			method:       "GET",
			path:         "/s/dummy-token/stream",
			expectXFrame: true,
			expectCSP:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(tt.method, tt.path, nil)
			r.ServeHTTP(w, req)

			xFrame := w.Header().Get("X-Frame-Options")
			if tt.expectXFrame {
				if xFrame != "SAMEORIGIN" {
					t.Errorf("expected X-Frame-Options to be SAMEORIGIN, got %q", xFrame)
				}
			} else {
				if xFrame != "" {
					t.Errorf("expected X-Frame-Options to be omitted, got %q", xFrame)
				}
			}

			csp := w.Header().Get("Content-Security-Policy")
			if tt.expectCSP {
				if csp == "" {
					t.Errorf("expected Content-Security-Policy to be set, got empty")
				}
				if csp != "default-src 'none'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; sandbox" {
					t.Errorf("unexpected CSP: %q", csp)
				}
			} else {
				if csp != "" {
					t.Errorf("expected no Content-Security-Policy, got %q", csp)
				}
			}
		})
	}
}
