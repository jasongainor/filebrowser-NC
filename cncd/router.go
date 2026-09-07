package cncd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/cncapi"
)

// wsWriteDeadline mirrors http.WSWriteDeadline (http/commands.go) so
// the stream endpoint behaves identically under a slow client.
const wsWriteDeadline = 10 * time.Second

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

// Deps is what NewRouter needs to serve the CNC API: the live
// registry (streamers/aggregators/queues/notifier, same as
// filebrowser wires up), the config store (for MachineToken and the
// Displays list), and the root directory this instance serves.
type Deps struct {
	Registry *cnc.Registry
	Config   *Store
	Root     string
}

// NewRouter builds the cncd HTTP handler: /api/cnc/*, /api/displays/{id},
// and a minimal /api/files/* for the UI to come. Paths and response
// shapes match fbhttp's (http/cnc.go, http/cnc_displays.go) so
// existing clients — renishaw-builder's POST /api/cnc/qcode bearer
// path, the e-paper firmware's GET /api/displays/{id} — work
// unchanged against either backend.
//
// There is no login here yet (see docs/CNCD.md): the only way to
// prove you're more than a read-only LAN visitor is the machine
// token bearer. Routes that have a session fallback in filebrowser
// (state, qcode, stream) have no such fallback here, so they require
// the bearer outright — missing or wrong token is 401, same status
// fbhttp already uses for a bad bearer on those routes.
func NewRouter(d Deps) http.Handler {
	resolver := cncapi.NewRootResolver(d.Root)

	r := mux.NewRouter()
	r.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	api := r.PathPrefix("/api").Subrouter()

	cncRouter := api.PathPrefix("/cnc").Subrouter()
	cncRouter.HandleFunc("/state", d.stateHandler).Methods("GET")
	cncRouter.HandleFunc("/qcode", d.qcodeHandler).Methods("POST")
	cncRouter.HandleFunc("/stream", d.streamHandler).Methods("GET")

	api.HandleFunc("/displays/{id}", d.displayFetchHandler).Methods("GET")

	files := api.PathPrefix("/files").Subrouter()
	files.HandleFunc("", d.filesListHandler(resolver)).Methods("GET")
	files.HandleFunc("", d.filesUploadHandler(resolver)).Methods("PUT")
	files.HandleFunc("", d.filesDeleteHandler(resolver)).Methods("DELETE")

	registerMCP(r, d)

	return r
}

// bearerOrUnauthorized checks the request's bearer against the
// configured machine token. Returns false (having already written a
// 401) when it doesn't match, or when no token is configured at all
// — an unconfigured daemon shouldn't silently accept every request as
// admin.
func (d Deps) bearerOrUnauthorized(w http.ResponseWriter, r *http.Request) bool {
	token := d.Config.Snapshot().MachineToken
	if token == "" || extractBearer(r) != token {
		writeError(w, http.StatusUnauthorized, nil)
		return false
	}
	return true
}

// resolveStreamer mirrors fbhttp's resolveStreamer (http/cnc.go): the
// ?machine_id= query param picks the machine, empty falls back to the
// registry's default (Cnc.Machines[0]).
func resolveStreamer(registry *cnc.Registry, r *http.Request) (*cnc.Streamer, string, error) {
	id := r.URL.Query().Get("machine_id")
	st, resolvedID := registry.Streamer(id)
	if st == nil {
		return nil, "", fmt.Errorf("no machine configured (id=%q)", id)
	}
	return st, resolvedID, nil
}

func resolveAggregator(registry *cnc.Registry, r *http.Request) (*cnc.Aggregator, string, error) {
	id := r.URL.Query().Get("machine_id")
	ag, resolvedID := registry.Aggregator(id)
	if ag == nil {
		return nil, "", fmt.Errorf("no machine configured (id=%q)", id)
	}
	return ag, resolvedID, nil
}

// stateHandler serves GET /api/cnc/state — the baseline-metric
// snapshot external tools (Home Assistant, monitoring dashboards)
// poll. Bearer-only: see the "no session fallback" note on NewRouter.
func (d Deps) stateHandler(w http.ResponseWriter, r *http.Request) {
	if !d.bearerOrUnauthorized(w, r) {
		return
	}
	ag, _, err := resolveAggregator(d.Registry, r)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	ag.Wake(0)
	_ = renderJSON(w, ag.Snapshot())
}

type qcodeBody struct {
	Q   int  `json:"q"`
	Var *int `json:"var,omitempty"`
}

// qcodeHandler serves POST /api/cnc/qcode — the one-shot macro-var
// query renishaw-builder drives over its bearer path today. Wire
// shape matches http/cnc.go's cncQueryBody exactly.
func (d Deps) qcodeHandler(w http.ResponseWriter, r *http.Request) {
	if !d.bearerOrUnauthorized(w, r) {
		return
	}
	st, _, err := resolveStreamer(d.Registry, r)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	req := &qcodeBody{}
	if err := json.NewDecoder(r.Body).Decode(req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Q <= 0 {
		writeError(w, http.StatusBadRequest, errors.New("q must be a positive integer"))
		return
	}
	res, err := st.Query(r.Context(), req.Q, req.Var)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, cnc.ErrConfigMissing) {
			status = http.StatusBadRequest
		}
		writeError(w, status, err)
		return
	}
	_ = renderJSON(w, res)
}

// streamHandler serves GET /api/cnc/stream — the WS status/event
// feed the machine dashboard subscribes to. Frame shapes match
// http/cnc.go's cncStreamHandler exactly (cnc.Event JSON).
func (d Deps) streamHandler(w http.ResponseWriter, r *http.Request) {
	if !d.bearerOrUnauthorized(w, r) {
		return
	}
	streamer, _, err := resolveStreamer(d.Registry, r)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	if err := writeJSONFrame(conn, cnc.Event{Type: "status", Status: streamer.Status()}); err != nil {
		return
	}

	events := streamer.Subscribe()
	defer streamer.Unsubscribe(events)

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			if _, _, err := conn.NextReader(); err != nil {
				return
			}
		}
	}()

	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			if err := writeJSONFrame(conn, ev); err != nil {
				return
			}
		case <-ping.C:
			_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteDeadline))
		case <-readDone:
			return
		case <-r.Context().Done():
			return
		}
	}
}

func writeJSONFrame(conn *websocket.Conn, v any) error {
	if err := conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline)); err != nil {
		return err
	}
	return conn.WriteJSON(v)
}
