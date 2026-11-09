package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// web_admin.go contains the actions that need more than the service user can
// do on its own: changing the password, writing a configuration file and
// restarting a systemd unit. The first is a plain file write in the state
// directory; the other two go through "x-node helper", which runs as a separate
// root service.

// runHelper sends one JSON request to the root helper service and returns its
// output. The helper listens on a unix socket that belongs to root:x-node, so
// the daemon needs no root of its own.
func runHelper(request helperRequest) (string, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	conn, err := net.DialTimeout("unix", helperSocketPath, 5*time.Second)
	if err != nil {
		return "", fmt.Errorf("the root helper is not running: %w", err)
	}
	defer conn.Close()
	// The upgrade action can download a whole release; the other actions answer
	// immediately, so the generous limit costs nothing.
	_ = conn.SetDeadline(time.Now().Add(20 * time.Minute))
	if _, err := conn.Write(payload); err != nil {
		return "", err
	}
	if unixConn, ok := conn.(*net.UnixConn); ok {
		_ = unixConn.CloseWrite()
	}
	raw, err := io.ReadAll(io.LimitReader(conn, 1<<20))
	if err != nil {
		return "", err
	}
	var response struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", fmt.Errorf("the root helper returned an unreadable answer: %w", err)
	}
	if !response.OK {
		if response.Error == "" {
			response.Error = "the root helper refused the request"
		}
		return "", errors.New(response.Error)
	}
	return strings.TrimSpace(string(raw)), nil
}

type passwordRequest struct {
	Current string `json:"currentPassword"`
	New     string `json:"newPassword"`
}

// handlePassword changes the web login password. The current password is
// required even though the session is already authenticated: a stolen cookie
// must not be enough to lock the owner out.
func (w *webServer) handlePassword(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(rw, http.StatusMethodNotAllowed, errorResponse{Error: "method_not_allowed"})
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, 4<<10)
	var request passwordRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_request"})
		return
	}
	creds, _ := w.credentials()
	if !verifyWebPassword(creds.PasswordHash, request.Current) {
		writeJSON(rw, http.StatusForbidden, errorResponse{Error: "bad_password", Message: "The current password is wrong"})
		return
	}
	password := strings.TrimSpace(request.New)
	if len(password) < 12 {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "weak_password", Message: "The new password must be at least 12 characters"})
		return
	}
	if len(password) > 200 {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "weak_password", Message: "The new password is too long"})
		return
	}
	if verifyWebPassword(creds.PasswordHash, password) {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "same_password", Message: "The new password is the same as the old one"})
		return
	}
	hash, err := hashWebPassword(password)
	if err != nil {
		writeJSON(rw, http.StatusInternalServerError, errorResponse{Error: "hash_failed", Message: err.Error()})
		return
	}
	creds.PasswordHash = hash
	if err := saveWebCredentials(w.credsPath, creds); err != nil {
		writeJSON(rw, http.StatusInternalServerError, errorResponse{Error: "save_failed", Message: err.Error()})
		return
	}
	// Reload the signer from the file so the session key and the file stay in
	// sync. The current cookie stays valid: rotating the key would log the
	// operator out of the page they are using.
	if err := w.useCredentials(creds); err != nil {
		writeJSON(rw, http.StatusInternalServerError, errorResponse{Error: "reload_failed", Message: err.Error()})
		return
	}
	w.log.info("web: password changed")
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
}

type restartRequest struct {
	Service string `json:"service"`
}

// handleRestart queues a restart through the helper. The answer is written
// before the service is touched, otherwise restarting x-node would kill the
// connection before the browser sees the result.
func (w *webServer) handleRestart(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(rw, http.StatusMethodNotAllowed, errorResponse{Error: "method_not_allowed"})
		return
	}
	if !requestHelperInstalled() {
		writeJSON(rw, http.StatusConflict, errorResponse{
			Error:   "helper_missing",
			Message: "The root helper is not installed; run the installer again to enable restarts",
		})
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, 4<<10)
	var request restartRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_request"})
		return
	}
	service := strings.TrimSpace(request.Service)
	switch service {
	case "x-node", "yggdrasil", "x-node-dns":
	default:
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_service", Message: "service must be x-node, yggdrasil or x-node-dns"})
		return
	}
	w.log.info("web: %s restart requested", service)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "restarting": service})
	go func() {
		// Let the response reach the browser before systemd stops the unit.
		time.Sleep(500 * time.Millisecond)
		if _, err := runHelper(helperRequest{Action: "restart", Service: service}); err != nil {
			w.log.warn("web: could not restart %s: %v", service, err)
		}
	}()
}

// handleConfigGet returns a configuration file for the editor.
func (w *webServer) handleConfigGet(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(rw, http.StatusMethodNotAllowed, errorResponse{Error: "method_not_allowed"})
		return
	}
	file := strings.TrimSpace(r.URL.Query().Get("file"))
	path, _, err := helperConfigTarget(file)
	if err != nil {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_file", Message: err.Error()})
		return
	}
	// yggdrasil.conf is readable by the yggdrasil group only, so the helper
	// reads both files when it is available. Without the helper the main
	// configuration is still readable by the service user.
	if requestHelperInstalled() {
		out, err := runHelper(helperRequest{Action: "read-config", File: file})
		if err != nil {
			writeJSON(rw, http.StatusInternalServerError, errorResponse{Error: "read_failed", Message: err.Error()})
			return
		}
		var data struct {
			Path     string    `json:"path"`
			Content  string    `json:"content"`
			Size     int       `json:"size"`
			Modified time.Time `json:"modified"`
		}
		if err := json.Unmarshal([]byte(out), &data); err != nil {
			writeJSON(rw, http.StatusInternalServerError, errorResponse{Error: "read_failed", Message: err.Error()})
			return
		}
		writeJSON(rw, http.StatusOK, map[string]any{
			"ok":       true,
			"file":     file,
			"path":     data.Path,
			"content":  data.Content,
			"size":     data.Size,
			"modified": data.Modified,
			"helper":   true,
		})
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		writeJSON(rw, http.StatusInternalServerError, errorResponse{Error: "read_failed", Message: err.Error()})
		return
	}
	info, _ := os.Stat(path)
	modified := time.Time{}
	if info != nil {
		modified = info.ModTime()
	}
	writeJSON(rw, http.StatusOK, map[string]any{
		"ok":       true,
		"file":     file,
		"path":     path,
		"content":  string(raw),
		"size":     len(raw),
		"modified": modified,
		"helper":   false,
	})
}

type configRequest struct {
	File    string `json:"file"`
	Content string `json:"content"`
}

// handleConfigPut validates the document in the helper and replaces the file.
// For the main configuration the web overlay is removed: after "apply" the
// file is the only source of truth, so the operator sees exactly what runs.
func (w *webServer) handleConfigPut(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeJSON(rw, http.StatusMethodNotAllowed, errorResponse{Error: "method_not_allowed"})
		return
	}
	if !requestHelperInstalled() {
		writeJSON(rw, http.StatusConflict, errorResponse{
			Error:   "helper_missing",
			Message: "The root helper is not installed; run the installer again to enable config changes",
		})
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, 600<<10)
	var request configRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_request", Message: err.Error()})
		return
	}
	file := strings.TrimSpace(request.File)
	path, _, err := helperConfigTarget(file)
	if err != nil {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_file", Message: err.Error()})
		return
	}
	if _, err := runHelper(helperRequest{Action: "write-config", File: file, Content: request.Content}); err != nil {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_config", Message: err.Error()})
		return
	}
	if file == "main" {
		// The overlay is a partial file the dashboard owns. Keeping it after a
		// full-file edit would silently override the values just written.
		if err := os.Remove(w.live.get().statePath(settingsFileName)); err != nil && !os.IsNotExist(err) {
			w.log.warn("web: could not remove the settings overlay: %v", err)
		}
	}
	w.log.info("web: %s configuration written to %s", file, path)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "path": path})
}

type manualPeerRequest struct {
	Action string `json:"action"`
	URI    string `json:"uri"`
	From   string `json:"from"`
	To     string `json:"to"`
}

// handleManualPeers adds, removes or replaces a pinned peer. Pinned peers are
// stored in the settings overlay, so no root is needed and the peer supervisor
// picks the change up on its next check. The admin socket is updated right away
// so the link appears in the table without waiting for that check.
func (w *webServer) handleManualPeers(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(rw, http.StatusMethodNotAllowed, errorResponse{Error: "method_not_allowed"})
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, 8<<10)
	var request manualPeerRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_request"})
		return
	}
	before := w.live.get()
	next := before
	list := append([]string(nil), before.ManualPeers...)
	action := strings.ToLower(strings.TrimSpace(request.Action))
	uri := strings.TrimSpace(request.URI)
	from := strings.TrimSpace(request.From)
	to := strings.TrimSpace(request.To)
	socket := newAdminSocket(before.AdminSocket, peerAdminTimeout)

	switch action {
	case "add":
		if err := validatePeerURI(uri); err != nil {
			writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_uri", Message: err.Error()})
			return
		}
		if containsString(list, uri) {
			writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "manualPeers": list, "unchanged": true})
			return
		}
		list = append(list, uri)
		if err := socket.AddPeer(uri); err != nil {
			w.log.warn("web: manual peer %s added to the config but not to Yggdrasil: %v", uri, err)
		}
	case "remove":
		if err := validatePeerURI(uri); err != nil {
			writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_uri", Message: err.Error()})
			return
		}
		filtered := list[:0]
		found := false
		for _, item := range list {
			if peerKeyOfURI(item) == peerKeyOfURI(uri) {
				found = true
				continue
			}
			filtered = append(filtered, item)
		}
		if !found {
			writeJSON(rw, http.StatusNotFound, errorResponse{Error: "not_found", Message: "This peer is not a manual one"})
			return
		}
		list = filtered
		if err := socket.RemovePeer(uri); err != nil {
			w.log.warn("web: manual peer %s removed from the config but not from Yggdrasil: %v", uri, err)
		}
	case "replace":
		if err := validatePeerURI(from); err != nil {
			writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_uri", Message: err.Error()})
			return
		}
		if err := validatePeerURI(to); err != nil {
			writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_uri", Message: err.Error()})
			return
		}
		found := false
		for index, item := range list {
			if peerKeyOfURI(item) == peerKeyOfURI(from) {
				list[index] = to
				found = true
				break
			}
		}
		if !found {
			writeJSON(rw, http.StatusNotFound, errorResponse{Error: "not_found", Message: "This peer is not a manual one"})
			return
		}
		if err := socket.RemovePeer(from); err != nil {
			w.log.warn("web: could not remove %s: %v", from, err)
		}
		if err := socket.AddPeer(to); err != nil {
			w.log.warn("web: could not add %s: %v", to, err)
		}
	default:
		writeJSON(rw, http.StatusBadRequest, errorResponse{Error: "bad_action", Message: "action must be add, remove or replace"})
		return
	}

	next.ManualPeers = cleanPeerList(list)
	if err := saveSettingsOverlay(next); err != nil {
		writeJSON(rw, http.StatusInternalServerError, errorResponse{Error: "save_failed", Message: err.Error()})
		return
	}
	w.live.set(next)
	w.log.info("web: manual peer list is now %d link(s)", len(next.ManualPeers))
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "manualPeers": next.ManualPeers})
}
