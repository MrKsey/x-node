package main

import (
	"context"
	"net/http"
	"time"
)

// updateCacheTTL keeps the dashboard from spending the anonymous GitHub rate
// limit on every reload. "Check again" passes ?refresh=1 to bypass it.
const updateCacheTTL = 30 * time.Minute

// currentUpdate returns the cached release check or asks GitHub. A failed
// refresh keeps the last successful answer and reports the error next to it,
// so a temporary network problem does not hide a known update.
func (w *webServer) currentUpdate(force bool) (*updateInfo, error) {
	w.updateMu.Lock()
	if !force && w.updateCache != nil && time.Since(w.updateCheckedAt) < updateCacheTTL {
		info := *w.updateCache
		w.updateMu.Unlock()
		return &info, nil
	}
	w.updateMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	info, err := checkForUpdate(ctx, daemonVersion)
	if err != nil {
		w.updateMu.Lock()
		cached := w.updateCache
		w.updateMu.Unlock()
		if cached != nil {
			stale := *cached
			stale.Error = err.Error()
			return &stale, nil
		}
		return nil, err
	}
	w.updateMu.Lock()
	w.updateCache = info
	w.updateCheckedAt = time.Now()
	w.updateMu.Unlock()
	copy := *info
	return &copy, nil
}

func (w *webServer) handleUpdateGet(rw http.ResponseWriter, r *http.Request) {
	info, err := w.currentUpdate(r.URL.Query().Get("refresh") != "")
	if err != nil {
		writeJSON(rw, http.StatusBadGateway, errorResponse{Error: "update_failed", Message: err.Error()})
		return
	}
	w.updateMu.Lock()
	info.Helper = requestHelperInstalled()
	info.Running = w.updateRunning
	if info.Error == "" {
		info.Error = w.updateError
	}
	w.updateMu.Unlock()
	writeJSON(rw, http.StatusOK, info)
}

// handleUpdatePost starts an upgrade through the root helper. The answer is
// sent before the helper replaces the binary and restarts the service, so the
// browser sees "started" instead of a dropped connection.
func (w *webServer) handleUpdatePost(rw http.ResponseWriter, r *http.Request) {
	if !requestHelperInstalled() {
		writeJSON(rw, http.StatusConflict, errorResponse{
			Error:   "helper_missing",
			Message: "The root helper is not installed; run \"sudo x-node update upgrade\" on the server",
		})
		return
	}
	w.updateMu.Lock()
	if w.updateRunning {
		w.updateMu.Unlock()
		writeJSON(rw, http.StatusConflict, errorResponse{Error: "already_running", Message: "An upgrade is already running"})
		return
	}
	w.updateMu.Unlock()

	info, err := w.currentUpdate(false)
	if err != nil {
		writeJSON(rw, http.StatusBadGateway, errorResponse{Error: "update_failed", Message: err.Error()})
		return
	}
	if !info.Available {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "already_latest", Message: "x-node is already the latest release"})
		return
	}
	if !info.CanUpgrade {
		writeJSON(rw, http.StatusBadRequest, errorResponse{
			Error:   "no_asset",
			Message: "The release has no archive for this platform; update from the release page",
		})
		return
	}

	w.updateMu.Lock()
	w.updateRunning = true
	w.updateError = ""
	w.updateMu.Unlock()
	w.log.info("web: upgrading x-node %s to %s", info.Current, info.Latest)
	writeJSON(rw, http.StatusAccepted, map[string]any{
		"ok": true, "upgrading": true, "from": info.Current, "to": info.Latest,
	})

	go func() {
		out, err := runHelper(helperRequest{Action: "upgrade"})
		w.updateMu.Lock()
		w.updateRunning = false
		if err != nil {
			w.updateError = err.Error()
			w.updateMu.Unlock()
			w.log.warn("web: upgrade failed: %v", err)
			return
		}
		w.updateError = ""
		w.updateMu.Unlock()
		w.log.info("web: upgrade finished: %s", out)
	}()
}
