package api

import (
	"net/http"

	"github.com/udaykishore-resu/tcrp/web"
)

// handleDashboard serves the governance dashboard, and answers anything else
// with the same JSON error shape the rest of the API uses.
//
// The page is one embedded file with no external references: no CDN, no font
// service, no image host. That is a governance property rather than an
// aesthetic one — a console that fetches code from a third party at load time
// is a console whose behaviour is not covered by the platform's own evidence.
func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "no route for "+r.Method+" "+r.URL.Path)
		return
	}
	body := web.Dashboard()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("ETag", web.DashboardETag)
	w.Header().Set("Cache-Control", "no-cache")
	if match := r.Header.Get("If-None-Match"); match == web.DashboardETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
