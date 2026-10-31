package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagedashboard"
)

func TestUsageDashboardManagementAuth(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "usage-test-password")
	s := newTestServer(t)
	s.usageDashboard = usagedashboard.New(filepath.Join(t.TempDir(), "usage.sqlite"))
	t.Cleanup(s.CloseUsageDashboard)
	for _, tc := range []struct {
		method, path, body string
		auth               bool
		want               int
	}{
		{"GET", "/usage.html", "", false, 200},
		{"GET", "/v8/management/observability/usage/summary", "", false, 401},
		{"GET", "/v8/management/observability/usage/prices", "", false, 401},
		{"PUT", "/v8/management/observability/usage/prices", `[]`, false, 401},
		{"GET", "/v8/management/observability/usage/summary", "", true, 200},
		{"GET", "/v8/management/observability/usage/summary?days=91", "", true, 400},
		{"PUT", "/v8/management/observability/usage/prices", `[{"model":"x","input":-1}]`, true, 400},
		{"PUT", "/v8/management/observability/usage/prices", `[{"model":"x","input":1}]`, true, 200},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Content-Type", "application/json")
		if tc.auth {
			req.Header.Set("Authorization", "Bearer usage-test-password")
		}
		rr := httptest.NewRecorder()
		s.engine.ServeHTTP(rr, req)
		if rr.Code != tc.want {
			t.Fatalf("%s %s: %d: %s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
		if tc.path == "/usage.html" && (!strings.Contains(rr.Body.String(), "Clé de gestion") || rr.Header().Get("Content-Security-Policy") == "") {
			t.Fatal("missing login or CSP")
		}
	}
	s.usageDashboard = nil
	rr := httptest.NewRecorder()
	s.engine.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/usage.html", nil))
	if rr.Code != 404 {
		t.Fatal("disabled page still available")
	}
}
