package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestRejectForeignOrigin(t *testing.T) {
	e := echo.New()
	e.Use(RejectForeignOrigin("https://brass-ledger.app/"))
	ok := func(c echo.Context) error { return c.NoContent(http.StatusNoContent) }
	e.POST("/api/me/bcp-profile", ok)
	e.GET("/api/me", ok)

	cases := []struct {
		name, method, origin string
		want                 int
	}{
		{"frontend POST", http.MethodPost, "https://brass-ledger.app", http.StatusNoContent},
		{"sibling subdomain POST", http.MethodPost, "https://evil.brass-ledger.app", http.StatusForbidden},
		{"other site POST", http.MethodPost, "https://evil.example", http.StatusForbidden},
		{"opaque origin POST", http.MethodPost, "null", http.StatusForbidden},
		{"no Origin (not a browser page)", http.MethodPost, "", http.StatusNoContent},
		{"other site GET", http.MethodGet, "https://evil.example", http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := "/api/me/bcp-profile"
			if tc.method == http.MethodGet {
				path = "/api/me"
			}
			req := httptest.NewRequest(tc.method, path, nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
