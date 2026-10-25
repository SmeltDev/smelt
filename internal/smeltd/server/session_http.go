package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/smeltdev/smelt/internal/smeltd"
)

// maxRequestBytes bounds a session request body. A factory start carries the
// whole prompt (a spec with diffs can be hundreds of kilobytes), and the API
// is only on the user's own socket, so the bound is generous; it only stops a
// runaway body.
const maxRequestBytes = 32 << 20

// registerSessionRoutes adds the session API to mux. It sits behind the
// transport's normal auth: the Unix socket is user-local, and the TCP listener
// demands the bearer token.
//
//	POST /factory              start a factory session
//	GET  /factory[?root=<path>] list sessions (newest first), optionally one project's
//	GET  /factory/{id}         one session with its review findings
//	POST /factory/{id}/cancel  stop a session
//	POST /factory/{id}/sync    queue a base merge for the end of the current round
func registerSessionRoutes(mux *http.ServeMux, d *daemon) {
	mux.HandleFunc("POST /factory", d.handleFactoryStart)
	mux.HandleFunc("GET /factory", d.handleSessionList)
	mux.HandleFunc("GET /factory/{id}", d.handleSessionGet)
	mux.HandleFunc("POST /factory/{id}/cancel", d.handleSessionCancel)
	mux.HandleFunc("POST /factory/{id}/sync", d.handleSessionSync)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeAPIError answers a refusal with the status it carries.
func writeAPIError(w http.ResponseWriter, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		if ae.code != "" {
			writeJSON(w, ae.status, map[string]any{"code": ae.code, "active": ae.active, "message": ae.msg})
			return
		}
		http.Error(w, ae.msg, ae.status)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func (d *daemon) handleFactoryStart(w http.ResponseWriter, r *http.Request) {
	var req smeltd.FactoryRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes)).Decode(&req); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ProjectRoot == "" {
		http.Error(w, "project_root required", http.StatusBadRequest)
		return
	}
	// This response is safe to retry: no session has been admitted. The store
	// checks again at admission for transfers racing request validation.
	if req.Continue == "" && d.sessions.hasHandedOver() {
		writeAPIError(w, admissionRefusal(&handoverError{}))
		return
	}
	sess, err := d.startFactory(req)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, smeltd.SessionStartResponse{ID: sess.ID})
}

func (d *daemon) handleSessionList(w http.ResponseWriter, r *http.Request) {
	root := r.URL.Query().Get("root")
	if root == "" {
		var all []smeltd.Session
		for _, p := range d.snapshot(nil).Projects {
			all = append(all, p.Sessions...)
		}
		writeJSON(w, http.StatusOK, smeltd.SessionList{Sessions: all})
		return
	}
	ps := d.lookupProject(root)
	if ps == nil {
		http.Error(w, fmt.Sprintf("the smelt daemon is not tracking %q", root), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, smeltd.SessionList{Sessions: d.sessions.forProject(ps.root)})
}

func (d *daemon) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	entry := d.sessions.get(r.PathValue("id"))
	if entry == nil {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, entry.detail())
}

func (d *daemon) handleSessionCancel(w http.ResponseWriter, r *http.Request) {
	found, _ := d.sessions.cancelSession(r.PathValue("id"))
	if !found {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	// Cancelling is idempotent: a session that already ended is not an error, so
	// a viewer that raced the session's own end gets the same answer.
	w.WriteHeader(http.StatusAccepted)
}

func (d *daemon) handleSessionSync(w http.ResponseWriter, r *http.Request) {
	entry := d.sessions.get(r.PathValue("id"))
	if entry == nil {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	var req smeltd.SyncRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes)).Decode(&req); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Base == "" {
		http.Error(w, "base required", http.StatusBadRequest)
		return
	}
	queued, err := entry.queueSync(req.Base)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusAccepted, queued)
}
