package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/localserver"
)

type server struct {
	mux      *http.ServeMux
	vitePort int
	viteURL  string
}

func newServer(vitePort int, viteURL string) *server {
	s := &server{
		mux:      http.NewServeMux(),
		vitePort: vitePort,
		viteURL:  viteURL,
	}
	s.routes()
	return s
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *server) routes() {
	s.mux.HandleFunc("GET /api/sessions", s.listSessions)
	s.mux.HandleFunc("POST /api/sessions", s.startSession)
	s.mux.HandleFunc("DELETE /api/sessions/{id}", s.stopSession)
	s.mux.HandleFunc("GET /api/sessions/{id}/stream", s.streamSession)
	s.mux.HandleFunc("POST /api/sessions/{id}/input", s.writeInput)
	s.mux.HandleFunc("POST /api/sessions/{id}/resize", s.resizeSession)
	s.mux.HandleFunc("GET /api/remotes", s.listRemotes)

	if s.vitePort > 0 {
		target := s.viteURL
		if target == "" {
			target = fmt.Sprintf("http://localhost:%d", s.vitePort)
		}
		viteURL, err := url.Parse(target)
		if err != nil {
			slog.Error("invalid VITE_URL", "url", target, "err", err)
			return
		}
		proxy := httputil.NewSingleHostReverseProxy(viteURL)
		s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			proxy.ServeHTTP(w, r)
		})
	} else {
		fs := http.FileServer(http.Dir("ui/dist"))
		s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			// For SPA routing: serve index.html for non-file paths
			if !strings.Contains(r.URL.Path, ".") {
				http.ServeFile(w, r, "ui/dist/index.html")
				return
			}
			fs.ServeHTTP(w, r)
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write json response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// listRemotes handles GET /api/remotes — returns enrolled remote servers.
func (s *server) listRemotes(w http.ResponseWriter, r *http.Request) {
	remotes, err := localserver.LoadRemotes(localserver.StateDir())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type remoteInfo struct {
		Name string `json:"name"`
		Host string `json:"host"`
	}
	result := make([]remoteInfo, 0, len(remotes))
	for _, rm := range remotes {
		result = append(result, remoteInfo{Name: rm.Name, Host: rm.Host})
	}
	writeJSON(w, http.StatusOK, map[string]any{"remotes": result})
}

// listSessions handles GET /api/sessions?remote=host
func (s *server) listSessions(w http.ResponseWriter, r *http.Request) {
	client, err := clientForRequest(r, 30*time.Second)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = client.Close() }()

	resp, err := client.ListSessions(r.Context(), &bridgev1.ListSessionsRequest{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	type sessionInfo struct {
		SessionID string `json:"sessionId"`
		ProjectID string `json:"projectId"`
		Provider  string `json:"provider"`
		Status    string `json:"status"`
		CreatedAt string `json:"createdAt"`
		RepoPath  string `json:"repoPath"`
	}

	sessions := make([]sessionInfo, 0, len(resp.GetSessions()))
	for _, ss := range resp.GetSessions() {
		createdAt := ""
		if ss.GetCreatedAt() != nil {
			createdAt = ss.GetCreatedAt().AsTime().Format(time.RFC3339)
		}
		sessions = append(sessions, sessionInfo{
			SessionID: ss.GetSessionId(),
			ProjectID: ss.GetProjectId(),
			Provider:  ss.GetProvider(),
			Status:    ss.GetStatus().String(),
			CreatedAt: createdAt,
			RepoPath:  ss.GetRepoPath(),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// startSession handles POST /api/sessions
func (s *server) startSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Remote    string `json:"remote"`
		Project   string `json:"project"`
		Provider  string `json:"provider"`
		RepoPath  string `json:"repoPath"`
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	// Use the remote from the body if set, otherwise fall back to query param
	if body.Remote != "" {
		q := r.URL.Query()
		q.Set("remote", body.Remote)
		r.URL.RawQuery = q.Encode()
	}

	client, err := clientForRequest(r, 30*time.Second)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = client.Close() }()

	project := body.Project
	if project == "" {
		project = "local"
	}

	sessionID := body.SessionID
	if sessionID == "" {
		sessionID = uuid.NewString()
	}

	req := &bridgev1.StartSessionRequest{
		ProjectId: project,
		Provider:  body.Provider,
		RepoPath:  body.RepoPath,
		SessionId: sessionID,
	}

	resp, err := client.StartSession(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"sessionId": resp.GetSessionId()})
}

// stopSession handles DELETE /api/sessions/{id}?remote=host
func (s *server) stopSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	client, err := clientForRequest(r, 30*time.Second)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = client.Close() }()

	_, err = client.StopSession(r.Context(), &bridgev1.StopSessionRequest{
		SessionId: id,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

// streamSession handles GET /api/sessions/{id}/stream (SSE)
func (s *server) streamSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	role := r.URL.Query().Get("role")
	clientID := r.URL.Query().Get("clientId")

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Already wrote header, write an error event
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"error\",\"message\":\"streaming unsupported\"}\n\n")
		return
	}

	client, err := clientForRequest(r, 30*time.Minute)
	if err != nil {
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"error\",\"message\":%s}\n\n", jsonStr(err.Error()))
		flusher.Flush()
		return
	}
	defer func() { _ = client.Close() }()

	attachRole := bridgev1.AttachRole_ATTACH_ROLE_OBSERVER
	if role == "writer" {
		attachRole = bridgev1.AttachRole_ATTACH_ROLE_WRITER
	}

	stream, err := client.AttachSession(r.Context(), &bridgev1.AttachSessionRequest{
		SessionId: id,
		ClientId:  clientID,
		AfterSeq:  0,
		Role:      attachRole,
	})
	if err != nil {
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"error\",\"message\":%s}\n\n", jsonStr(err.Error()))
		flusher.Flush()
		return
	}

	var replayThrough uint64
	ready := false
	markReady := func() error {
		if ready {
			return nil
		}
		ready = true
		_, err := fmt.Fprint(w, "data: {\"type\":\"ready\"}\n\n")
		flusher.Flush()
		return err
	}
	err = stream.RecvAll(r.Context(), func(ev *bridgev1.AttachSessionEvent) error {
		switch ev.Type {
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_ATTACHED:
			replayThrough = ev.LastSeq
			_, err := fmt.Fprintf(w, "data: {\"type\":\"geometry\",\"cols\":%d,\"rows\":%d}\n\n", ev.Cols, ev.Rows)
			flusher.Flush()
			if err == nil && replayThrough == 0 {
				return markReady()
			}
			return err
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_OUTPUT:
			encoded := base64.StdEncoding.EncodeToString(ev.Payload)
			_, err := fmt.Fprintf(w, "id: %d\ndata: {\"type\":\"output\",\"data\":%s,\"cols\":%d,\"rows\":%d}\n\n", ev.Seq, jsonStr(encoded), ev.Cols, ev.Rows)
			flusher.Flush()
			if err == nil && ev.Seq >= replayThrough {
				return markReady()
			}
			return err
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_ERROR:
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"error\",\"message\":%s}\n\n", jsonStr(ev.Error))
			flusher.Flush()
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_SESSION_EXIT:
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"end\"}\n\n")
			flusher.Flush()
		case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_REPLAY_GAP:
			msg := fmt.Sprintf("replay gap: oldest=%d last=%d", ev.OldestSeq, ev.LastSeq)
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"error\",\"message\":%s}\n\n", jsonStr(msg))
			flusher.Flush()
		}
		return nil
	})

	if err != nil {
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"error\",\"message\":%s}\n\n", jsonStr(err.Error()))
		flusher.Flush()
	}

	_, _ = fmt.Fprintf(w, "data: {\"type\":\"end\"}\n\n")
	flusher.Flush()
}

// writeInput handles POST /api/sessions/{id}/input
func (s *server) writeInput(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body struct {
		Data     string `json:"data"` // base64-encoded
		Remote   string `json:"remote"`
		ClientID string `json:"clientId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if body.Remote != "" {
		q := r.URL.Query()
		q.Set("remote", body.Remote)
		r.URL.RawQuery = q.Encode()
	}

	data, err := base64.StdEncoding.DecodeString(body.Data)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid base64 data: "+err.Error())
		return
	}

	client, err := clientForRequest(r, 30*time.Second)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = client.Close() }()

	_, err = client.WriteInput(r.Context(), &bridgev1.WriteInputRequest{
		SessionId: id,
		ClientId:  body.ClientID,
		Data:      data,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// resizeSession handles POST /api/sessions/{id}/resize
func (s *server) resizeSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body struct {
		Cols     uint32 `json:"cols"`
		Rows     uint32 `json:"rows"`
		Remote   string `json:"remote"`
		ClientID string `json:"clientId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if body.Remote != "" {
		q := r.URL.Query()
		q.Set("remote", body.Remote)
		r.URL.RawQuery = q.Encode()
	}

	client, err := clientForRequest(r, 30*time.Second)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = client.Close() }()

	_, err = client.ResizeSession(r.Context(), &bridgev1.ResizeSessionRequest{
		SessionId: id,
		ClientId:  body.ClientID,
		Cols:      body.Cols,
		Rows:      body.Rows,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// jsonStr returns a JSON-encoded string value (with quotes) for safe embedding in SSE data.
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
