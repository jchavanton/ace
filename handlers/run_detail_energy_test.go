package handlers

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A call whose stream was rebuilt has two rtp_stats blocks carrying the
// running totals (20, then 80 voice frames); the page shows the whole call.
func TestRunPageShowsEachCallsWholeCallEnergy(t *testing.T) {
	r, _ := apiServer(t, `{"label":"probe","action":"call","result":"PASS","rtp_stats":[{"Rx":{"voice_frames":20}},{"Rx":{"voice_frames":80}}]}`)
	r.SetFuncMap(template.FuncMap{"mul": func(a, b int) int { return a * b }})
	r.LoadHTMLGlob("../templates/*.html")
	run := runToCompletion(t, r)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/runs/"+run.Run.ID, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("run page: %d", w.Code)
	}
	page := w.Body.String()
	if !strings.Contains(page, "8000 / 0 ms") || strings.Contains(page, "2000 / 0 ms") {
		t.Fatalf("run page should show the call's 8000 ms once, not the partial 2000 ms block")
	}
}
