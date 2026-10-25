package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A factory start carries the whole prompt; a spec of a few hundred kilobytes
// (an upstream port with its diff) must be read, not refused as too large.
func TestFactoryStartReadsLargePrompt(t *testing.T) {
	body, err := json.Marshal(map[string]string{"prompt": strings.Repeat("x", 1<<20)})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	newTestDaemon().handleFactoryStart(rec, httptest.NewRequest(http.MethodPost, "/factory", strings.NewReader(string(body))))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "project_root required") {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestFactoryStartRefusesRunawayBody(t *testing.T) {
	body := `{"prompt":"` + strings.Repeat("x", maxRequestBytes) + `"}`
	rec := httptest.NewRecorder()
	newTestDaemon().handleFactoryStart(rec, httptest.NewRequest(http.MethodPost, "/factory", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "too large") {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
}
