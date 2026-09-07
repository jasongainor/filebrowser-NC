package fbhttp

// /api/cnc/* — machine integration endpoints.
// See docs/INTEGRATION_WITH_HAAS_DASHBOARD.md for the wider design,
// docs/MULTI_MACHINE_DESIGN.md for the per-Machine.ID architecture.
//
// All endpoints accept an optional ?machine_id=... query param. If
// omitted, the registry resolves to the configured default
// (Cnc.Machines[0]). Single-machine installs continue to work
// without any change to existing API consumers.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/cncapi"
	"github.com/filebrowser/filebrowser/v2/settings"
)

// toolTableShareDir is the user-scope-relative folder where tool-table
// JSON dumps live. Dumps land at
// "<this>/<machine-id>/<RFC3339>.json" — visible in the regular file
// browser, downloadable, deletable.
const toolTableShareDir = "/cnc-tool-tables"

// cncSettingsBody is the wire shape the Machine settings tab POSTs and
// reads. MachineToken is GET-only (minted server-side). Machines is
// the canonical list; legacy haasHost/haasPort/cameraUrl fields are
// returned as a copy of Machines[0] for backwards compat with the
// pre-multi-machine settings UI.
type cncSettingsBody struct {
	Machines     []settings.Machine `json:"machines"`
	MachineToken string             `json:"machineToken,omitempty"` // GET only

	// Legacy mirrors of Machines[0] — the pre-multi-machine settings
	// UI POSTs these. Folded into Machines[0] on PUT if the request
	// doesn't include a Machines list.
	HaasHost  string `json:"haasHost,omitempty"`
	HaasPort  int    `json:"haasPort,omitempty"`
	CameraURL string `json:"cameraUrl,omitempty"`
}

func cncFromSettings(c settings.Cnc) cncSettingsBody {
	body := cncSettingsBody{
		Machines:     c.Machines,
		MachineToken: c.MachineToken,
	}
	if len(c.Machines) > 0 {
		m := c.Machines[0]
		body.HaasHost = m.Host
		body.HaasPort = m.Port
		body.CameraURL = m.CameraURL
	}
	return body
}

var cncSettingsGetHandler = withAdmin(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
	return renderJSON(w, r, cncFromSettings(d.settings.Cnc))
})

func cncSettingsPutHandler(registry *cnc.Registry) handleFunc {
	return withAdmin(func(_ http.ResponseWriter, r *http.Request, d *data) (int, error) {
		req := &cncSettingsBody{}
		if err := json.NewDecoder(r.Body).Decode(req); err != nil {
			return http.StatusBadRequest, err
		}

		// Two PUT shapes are accepted:
		//
		// 1. New multi-machine UI: req.Machines is set. We replace the
		//    list wholesale, validating each entry.
		// 2. Legacy single-machine UI: only haasHost/haasPort/cameraUrl
		//    are set. We fold into Machines[0] (creating one if needed),
		//    leaving any additional machines untouched.
		switch {
		case req.Machines != nil:
			cleaned, err := normalizeMachines(req.Machines, d.settings.Cnc.Machines)
			if err != nil {
				return http.StatusBadRequest, err
			}
			d.settings.Cnc.Machines = cleaned
		default:
			port := req.HaasPort
			if port <= 0 {
				port = settings.DefaultHaasPort
			}
			if port > 65535 {
				return http.StatusBadRequest, fmt.Errorf("port out of range")
			}
			if len(d.settings.Cnc.Machines) == 0 {
				d.settings.Cnc.Machines = []settings.Machine{{
					ID:         newMachineID(),
					Name:       "Machine 1",
					Brand:      settings.MachineBrandHaas,
					CameraType: "auto",
				}}
			}
			d.settings.Cnc.Machines[0].Host = req.HaasHost
			d.settings.Cnc.Machines[0].Port = port
			d.settings.Cnc.Machines[0].CameraURL = req.CameraURL
			if d.settings.Cnc.Machines[0].Brand == "" {
				d.settings.Cnc.Machines[0].Brand = settings.MachineBrandHaas
			}
			if d.settings.Cnc.Machines[0].CameraType == "" {
				d.settings.Cnc.Machines[0].CameraType = "auto"
			}
		}
		// Keep the legacy mirror fields populated as a fallback for
		// any code that hasn't migrated. EnsureMigrated() will skip
		// since Machines[0] now exists, so this is no-op cosmetic.
		if len(d.settings.Cnc.Machines) > 0 {
			d.settings.Cnc.HaasHost = d.settings.Cnc.Machines[0].Host
			d.settings.Cnc.HaasPort = d.settings.Cnc.Machines[0].Port
			d.settings.Cnc.CameraURL = d.settings.Cnc.Machines[0].CameraURL
		}

		if err := d.store.Settings.Save(d.settings); err != nil {
			return errToStatus(err), err
		}
		// Pick up new/removed machines in the live registry.
		registry.Refresh()
		return 0, nil
	})
}

// normalizeMachines validates + assigns IDs to a Machines list before
// it lands in storage. Kept under its original name (rather than
// inlined as cncapi.NormalizeMachines at the one call site) so
// cnc_serial_only_test.go keeps testing this exact code path; the
// actual validation lives in cncapi, shared with cncd's settings PUT.
func normalizeMachines(in []settings.Machine, existing []settings.Machine) ([]settings.Machine, error) {
	return cncapi.NormalizeMachines(in, existing)
}

// newMachineID mirrors cncapi.NewMachineID under its original name.
func newMachineID() string {
	return cncapi.NewMachineID()
}

// cncMachinesListHandler returns the configured Machines (id + name +
// host:port + camera). Auth: any logged-in user — the frontend store
// needs this to drive the machine switcher.
func cncMachinesListHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		return renderJSON(w, r, d.cncapiDeps(registry).MachinesList())
	})
}

// defaultToolSlotsForMachine looks up the machine's configured ToolSlots
// (per Settings → Machine) and returns the effective value, or
// settings.DefaultToolSlots if the machine isn't found in settings.
// Used as the default for /api/cnc/probe-tools and
// /api/cnc/tool-table when no explicit ?slots= is passed.
func defaultToolSlotsForMachine(d *data, machineID string) int {
	if d != nil && d.settings != nil {
		if m, ok := d.settings.Cnc.MachineByID(machineID); ok {
			return m.EffectiveToolSlots()
		}
	}
	return settings.DefaultToolSlots
}

var cncRegenerateTokenHandler = withAdmin(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
	tok, err := cncapi.NewMachineToken()
	if err != nil {
		return http.StatusInternalServerError, err
	}
	d.settings.Cnc.MachineToken = tok
	if err := d.store.Settings.Save(d.settings); err != nil {
		return errToStatus(err), err
	}
	return renderJSON(w, r, map[string]string{"machineToken": d.settings.Cnc.MachineToken})
})

// resolveStreamer pulls the streamer for ?machine_id= (or default
// when missing). Returns 404 + nil if no machine matches. Handlers
// short-circuit on a non-zero status.
func resolveStreamer(registry *cnc.Registry, r *http.Request) (*cnc.Streamer, string, int, error) {
	id := r.URL.Query().Get("machine_id")
	st, resolvedID := registry.Streamer(id)
	if st == nil {
		return nil, "", http.StatusNotFound, fmt.Errorf("no machine configured (id=%q)", id)
	}
	return st, resolvedID, 0, nil
}

func resolveAggregator(registry *cnc.Registry, r *http.Request) (*cnc.Aggregator, string, int, error) {
	id := r.URL.Query().Get("machine_id")
	ag, resolvedID := registry.Aggregator(id)
	if ag == nil {
		return nil, "", http.StatusNotFound, fmt.Errorf("no machine configured (id=%q)", id)
	}
	return ag, resolvedID, 0, nil
}

func cncStatusHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		body, code, err := d.cncapiDeps(registry).Status(r.URL.Query().Get("machine_id"), "/files")
		if err != nil {
			return code, err
		}
		return renderJSON(w, r, body)
	})
}

func cncCheckHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, _ *data) (int, error) {
		st, machineID, code, err := resolveStreamer(registry, r)
		if err != nil {
			return code, err
		}
		ag, _ := registry.Aggregator(machineID)
		if ag != nil {
			ag.Wake(0)
		}

		body := struct {
			MachineID string `json:"machine_id"`
			Bridge    struct {
				OK        bool    `json:"ok"`
				LatencyMs float64 `json:"latency_ms,omitempty"`
				Error     string  `json:"error,omitempty"`
				Address   string  `json:"address,omitempty"`
			} `json:"bridge"`
			Controller struct {
				OK        bool    `json:"ok"`
				LatencyMs float64 `json:"latency_ms,omitempty"`
				Error     string  `json:"error,omitempty"`
				Mode      string  `json:"mode,omitempty"`
			} `json:"controller"`
		}{MachineID: machineID}

		if st.IsRunning() {
			body.Bridge.Error = "stream in progress — connection check skipped to avoid disturbing the job"
			body.Controller.Error = body.Bridge.Error
			return renderJSON(w, r, body)
		}

		bridgeOK, bridgeLatency, bridgeAddr, bridgeErr := st.CheckBridge()
		body.Bridge.OK = bridgeOK
		body.Bridge.LatencyMs = bridgeLatency
		body.Bridge.Address = bridgeAddr
		if bridgeErr != nil {
			body.Bridge.Error = bridgeErr.Error()
			body.Controller.Error = "skipped (bridge unreachable)"
			return renderJSON(w, r, body)
		}

		ctrlOK, ctrlLatency, mode, ctrlErr := st.CheckController(r.Context())
		body.Controller.OK = ctrlOK
		body.Controller.LatencyMs = ctrlLatency
		body.Controller.Mode = mode
		if ctrlErr != nil {
			body.Controller.Error = ctrlErr.Error()
		}
		return renderJSON(w, r, body)
	})
}

// modelExtensions is the set of 3D model file extensions the siblings
// endpoint will surface as a candidate part-view source.
var modelExtensions = map[string]bool{
	".3mf":  true,
	".stl":  true,
	".step": true,
	".stp":  true,
	".x_t":  true,
	".x_b":  true,
	".iges": true,
	".igs":  true,
	".obj":  true,
	".ply":  true,
}

// cncSiblingsHandler — same as before; not multi-machine aware (the
// share is global to the install, not per-machine).
func cncSiblingsHandler(_ *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		raw := r.URL.Query().Get("path")
		if raw == "" {
			return http.StatusBadRequest, errors.New("path required")
		}
		clean := path.Clean(ensureLeading(raw))
		if strings.Contains(clean, "..") {
			return http.StatusBadRequest, errors.New("path must not escape the share")
		}

		dir := path.Dir(clean)
		base := strings.TrimSuffix(path.Base(clean), path.Ext(clean))
		baseLower := strings.ToLower(base)

		f, err := d.user.Fs.Open(dir)
		if err != nil {
			return errToStatus(err), err
		}
		defer f.Close()
		entries, err := f.Readdir(-1)
		if err != nil {
			return errToStatus(err), err
		}

		body := struct {
			ModelURL    string `json:"model_url,omitempty"`
			ModelName   string `json:"model_name,omitempty"`
			ModelPath   string `json:"model_path,omitempty"`
			DrawingURL  string `json:"drawing_url,omitempty"`
			DrawingName string `json:"drawing_name,omitempty"`
			DrawingPath string `json:"drawing_path,omitempty"`
		}{}

		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			ext := strings.ToLower(path.Ext(name))
			stem := strings.ToLower(strings.TrimSuffix(name, path.Ext(name)))
			if stem != baseLower {
				continue
			}
			full := path.Join(dir, name)
			if modelExtensions[ext] && body.ModelURL == "" {
				body.ModelURL = "/api/raw" + full + "?inline=true"
				body.ModelName = name
				body.ModelPath = full
			} else if ext == ".pdf" && body.DrawingURL == "" {
				body.DrawingURL = "/api/raw" + full + "?inline=true"
				body.DrawingName = name
				body.DrawingPath = full
			}
			if body.ModelURL != "" && body.DrawingURL != "" {
				break
			}
		}
		return renderJSON(w, r, body)
	})
}

func cncProbeToolsHandler(registry *cnc.Registry) handleFunc {
	return withAdmin(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		st, machineID, code, err := resolveStreamer(registry, r)
		if err != nil {
			return code, err
		}
		slots := defaultToolSlotsForMachine(d, machineID)
		if q := r.URL.Query().Get("slots"); q != "" {
			n, err := strconv.Atoi(q)
			if err != nil || n < 1 || n > 200 {
				return http.StatusBadRequest, fmt.Errorf("slots must be 1..200")
			}
			slots = n
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		rep, err := st.ProbeTools(ctx, slots)
		if err != nil {
			return errToStatus(err), err
		}
		return renderJSON(w, r, rep)
	})
}

// cncProbeToolLifeHandler — operator-triggered macro-range scan to
// figure out which Haas macros carry tool-life data on this firmware.
// See cnc/probe_life.go + docs/TOOL_LIFE_RESEARCH.md. Admin-only
// because it ties up the bridge for tens of seconds; not appropriate
// during a job.
func cncProbeToolLifeHandler(registry *cnc.Registry) handleFunc {
	return withAdmin(func(w http.ResponseWriter, r *http.Request, _ *data) (int, error) {
		st, _, code, err := resolveStreamer(registry, r)
		if err != nil {
			return code, err
		}
		parseInt := func(key string) (int, error) {
			q := r.URL.Query().Get(key)
			if q == "" {
				return 0, nil
			}
			n, perr := strconv.Atoi(q)
			if perr != nil {
				return 0, fmt.Errorf("%s: %w", key, perr)
			}
			return n, nil
		}
		start, perr := parseInt("start")
		if perr != nil {
			return http.StatusBadRequest, perr
		}
		end, perr := parseInt("end")
		if perr != nil {
			return http.StatusBadRequest, perr
		}
		step, perr := parseInt("step")
		if perr != nil {
			return http.StatusBadRequest, perr
		}
		// 90 s ceiling matches probe-tools — 500 macros × 150 ms ≈ 75 s,
		// the worst case the probe code itself allows.
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		rep, err := st.ProbeToolLife(ctx, start, end, step)
		if err != nil {
			return errToStatus(err), err
		}
		return renderJSON(w, r, rep)
	})
}

// cncToolTableReadHandler reads the live tool table from the controller,
// writes it to <user-scope>/cnc-tool-tables/<machine-id>/<RFC3339>.json,
// and returns the table. Partial reads (timeout / cancel) still persist
// so the operator never loses progress on a long read.
func cncToolTableReadHandler(registry *cnc.Registry) handleFunc {
	return withAdmin(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		cd := d.cncapiDeps(registry)
		machineID := r.URL.Query().Get("machine_id")
		_, resolvedID, code, err := cd.ResolveStreamer(machineID)
		if err != nil {
			return code, err
		}
		// Default to the machine's configured ToolSlots so an operator
		// who set "20 pockets" once doesn't have to remember to pass
		// ?slots=20 on every read. Explicit ?slots= still overrides.
		slots := cncapi.DefaultToolSlotsForMachine(cd.Store.Snapshot(), resolvedID)
		if q := r.URL.Query().Get("slots"); q != "" {
			n, err := strconv.Atoi(q)
			if err != nil || n < 1 || n > 200 {
				return http.StatusBadRequest, fmt.Errorf("slots must be 1..200")
			}
			slots = n
		}
		// Worst case: 9600 baud, 200 slots, every slot populated, 4
		// bases each, ~500 ms per round trip = 400 s. Allow 15 min so
		// even a fully-loaded toolchanger at the lowest baud completes.
		// Operator triggers and walks away — the request idles cheap.
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
		defer cancel()
		env, code, err := cd.ToolTableReadLive(ctx, machineID, slots)
		if err != nil {
			return code, err
		}
		envelope := map[string]any{"table": env.Table}
		if env.ReadError != "" {
			envelope["read_error"] = env.ReadError
		}
		if env.PersistError != "" {
			envelope["persist_error"] = env.PersistError
		}
		return renderJSON(w, r, envelope)
	})
}

// cncToolTableLatestHandler returns the latest persisted tool-table
// dump (or 204 No Content if none exists yet). Reading is a normal
// user op — operators want the dashboard to show the last-known table
// without needing admin to trigger a fresh read.
func cncToolTableLatestHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		tbl, found, code, err := d.cncapiDeps(registry).ToolTableLatest(r.URL.Query().Get("machine_id"))
		if err != nil {
			return code, err
		}
		if !found {
			w.WriteHeader(http.StatusNoContent)
			return 0, nil
		}
		return renderJSON(w, r, map[string]any{"table": tbl})
	})
}

// cncToolTableHistoryHandler lists the per-machine dump folder so the
// dashboard can show "previous reads" without scraping the file UI.
// Newest-first.
func cncToolTableHistoryHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		body, code, err := d.cncapiDeps(registry).ToolTableHistory(
			r.URL.Query().Get("machine_id"), toolTableShareDir)
		if err != nil {
			return code, err
		}
		return renderJSON(w, r, body)
	})
}

// cncChaptersHandler parses an NC file for operation-header comments
// and returns the TOC for the dashboard's chapter list. Read-only,
// no streamer interaction. Single query param: file_path.
func cncChaptersHandler() handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		if !d.authz().CanModify() {
			return http.StatusForbidden, nil
		}
		filePath := r.URL.Query().Get("file_path")
		if filePath == "" {
			return http.StatusBadRequest, errors.New("file_path required")
		}
		clean := path.Clean(ensureLeading(filePath))
		if strings.Contains(clean, "..") {
			return http.StatusBadRequest, errors.New("file_path must not escape the share")
		}
		absPath, err := d.pathResolver().FullPath(clean)
		if err != nil {
			return http.StatusBadRequest, err
		}
		list, err := cnc.BuildChapters(absPath, clean)
		if err != nil {
			return http.StatusInternalServerError, err
		}
		return renderJSON(w, r, list)
	})
}

// cncToolTableDiffHandler joins two persisted tool-table dumps and
// returns the SlotDiff list. Operators tracking wear use this to spot
// which tools shifted between probes. Filenames come from the
// /api/cnc/tool-table/history endpoint (newest-first dropdown).
//
// Query params (all optional):
//
//	machine_id  — standard machine selector
//	old, new    — basenames inside the machine's history folder. If
//	              `new` is omitted, defaults to the newest dump; if
//	              `old` is omitted, defaults to the second-newest.
//	dia_tol     — diameter drift threshold in inches (default 0.005)
//	len_tol     — length drift threshold in inches (default 0.002)
func cncToolTableDiffHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		_, machineID, code, err := resolveStreamer(registry, r)
		if err != nil {
			return code, err
		}
		dir, derr := toolTableDirAbs(d, machineID)
		if derr != nil {
			return http.StatusBadRequest, derr
		}
		entries, derr := os.ReadDir(dir)
		if derr != nil {
			if os.IsNotExist(derr) {
				return http.StatusNotFound, fmt.Errorf("no tool-table history for this machine")
			}
			return http.StatusInternalServerError, derr
		}
		jsonNames := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			jsonNames = append(jsonNames, e.Name())
		}
		// Filenames are RFC3339 timestamps with colons replaced by
		// hyphens, so a string sort puts oldest-first.
		sort.Strings(jsonNames)

		q := r.URL.Query()
		oldName := q.Get("old")
		newName := q.Get("new")
		if newName == "" {
			if len(jsonNames) == 0 {
				return http.StatusNotFound, fmt.Errorf("no dumps in history")
			}
			newName = jsonNames[len(jsonNames)-1]
		}
		if oldName == "" {
			// Default to the entry just before `new` in chronological
			// order. If new is the oldest available, there's nothing to
			// compare against and we return a 400 — operator likely hit
			// the diff button with only one read on file.
			idx := -1
			for i, n := range jsonNames {
				if n == newName {
					idx = i
					break
				}
			}
			if idx <= 0 {
				return http.StatusBadRequest, fmt.Errorf(
					"need at least two reads on file to diff (or pass ?old=<filename>)")
			}
			oldName = jsonNames[idx-1]
		}
		// Reject filenames that try to break out of the history dir.
		if strings.ContainsRune(oldName, '/') || strings.ContainsRune(newName, '/') {
			return http.StatusBadRequest, errors.New("filenames must be basenames")
		}

		oldTable, err := readToolTableDump(dir, oldName)
		if err != nil {
			return http.StatusBadRequest, fmt.Errorf("old read: %w", err)
		}
		newTable, err := readToolTableDump(dir, newName)
		if err != nil {
			return http.StatusBadRequest, fmt.Errorf("new read: %w", err)
		}

		diaTol, _ := strconv.ParseFloat(q.Get("dia_tol"), 64)
		lenTol, _ := strconv.ParseFloat(q.Get("len_tol"), 64)
		diff := cnc.DiffToolTables(oldTable, newTable, diaTol, lenTol)
		return renderJSON(w, r, diff)
	})
}

func readToolTableDump(dir, name string) (*cnc.ToolTable, error) {
	buf, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	var t cnc.ToolTable
	if err := json.Unmarshal(buf, &t); err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	return &t, nil
}

func toolTableDirAbs(d *data, machineID string) (string, error) {
	rel := path.Join(toolTableShareDir, sanitizeMachineID(machineID))
	return d.pathResolver().FullPath(rel)
}

// sanitizeMachineID strips path separators and traversal chars from the
// machine ID so a malicious settings entry can't escape the dump dir.
// Machine IDs are server-minted (random base64) but defense in depth.
func sanitizeMachineID(id string) string {
	if id == "" {
		return "default"
	}
	repl := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		"..", "_",
		string(os.PathSeparator), "_",
	)
	cleaned := repl.Replace(id)
	if cleaned == "" {
		return "default"
	}
	return cleaned
}

type cncStartBody struct {
	FilePath  string `json:"file_path"`
	MachineID string `json:"machine_id,omitempty"` // optional; ?machine_id= also accepted
	// Method tells the streamer how the operator has prepared the
	// controller — "mem" (Memory-tab Receive) or "dnc" (DNC drip-feed).
	// The Pi-side bytes are identical for both; the field is recorded
	// on the job so the activity log + dashboard can tag entries.
	// Empty / unknown values default to "mem".
	Method string `json:"method,omitempty"`
	// QueueID, when present, marks that queue row "sending" (and
	// demotes any other in-flight row) before the streamer starts.
	// Optional — sends not initiated from the queue panel skip this.
	QueueID string `json:"queue_id,omitempty"`
}

func cncStartHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		if !d.authz().CanModify() {
			return http.StatusForbidden, nil
		}
		req := &cncStartBody{}
		if err := json.NewDecoder(r.Body).Decode(req); err != nil {
			return http.StatusBadRequest, err
		}
		jobID, code, err := d.cncapiDeps(registry).Start(cncapi.StartRequest{
			FilePath:  req.FilePath,
			MachineID: req.MachineID,
			Method:    req.Method,
			QueueID:   req.QueueID,
		}, r.URL.Query().Get("machine_id"))
		if err != nil {
			return code, err
		}
		return renderJSON(w, r, map[string]string{"job_id": jobID})
	})
}

// cncPreflightHandler joins the NC source's tool references against
// the machine's latest persisted tool-table dump. Read-only; the
// streamer is not touched. Returns the per-tool status list the
// SendWizard renders before the operator hits Send.
func cncPreflightHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		if !d.authz().CanModify() {
			return http.StatusForbidden, nil
		}
		pf, code, err := d.cncapiDeps(registry).Preflight(
			r.URL.Query().Get("file_path"), r.URL.Query().Get("machine_id"))
		if err != nil {
			return code, err
		}
		return renderJSON(w, r, pf)
	})
}

func cncStopHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		if !d.authz().CanModify() {
			return http.StatusForbidden, nil
		}
		stopped, code, err := d.cncapiDeps(registry).Stop(r.URL.Query().Get("machine_id"))
		if err != nil {
			return code, err
		}
		return renderJSON(w, r, map[string]bool{"stopped": stopped})
	})
}

func cncStateHandler(registry *cnc.Registry) handleFunc {
	session := withUser(func(w http.ResponseWriter, r *http.Request, _ *data) (int, error) {
		ag, _, code, err := resolveAggregator(registry, r)
		if err != nil {
			return code, err
		}
		ag.Wake(0)
		return renderJSON(w, r, ag.Snapshot())
	})
	return func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			got := strings.TrimPrefix(auth, "Bearer ")
			if d.settings.Cnc.MachineToken == "" || got != d.settings.Cnc.MachineToken {
				return http.StatusUnauthorized, nil
			}
			ag, _, code, err := resolveAggregator(registry, r)
			if err != nil {
				return code, err
			}
			ag.Wake(0)
			return renderJSON(w, r, ag.Snapshot())
		}
		return session(w, r, d)
	}
}

func cncRecoveryAckHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		if !d.authz().CanModify() {
			return http.StatusForbidden, nil
		}
		code, err := d.cncapiDeps(registry).RecoveryAck(r.URL.Query().Get("machine_id"))
		if err != nil {
			return code, err
		}
		return renderJSON(w, r, map[string]bool{"acknowledged": true})
	})
}

type cncQueryBody struct {
	Q   int  `json:"q"`
	Var *int `json:"var,omitempty"`
}

func cncQueryHandler(registry *cnc.Registry) handleFunc {
	session := withUser(func(w http.ResponseWriter, r *http.Request, _ *data) (int, error) {
		st, _, code, err := resolveStreamer(registry, r)
		if err != nil {
			return code, err
		}
		return runQuery(w, r, st)
	})
	return func(w http.ResponseWriter, r *http.Request, d *data) (int, error) {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			got := strings.TrimPrefix(auth, "Bearer ")
			if d.settings.Cnc.MachineToken == "" || got != d.settings.Cnc.MachineToken {
				return http.StatusUnauthorized, nil
			}
			st, _, code, err := resolveStreamer(registry, r)
			if err != nil {
				return code, err
			}
			return runQuery(w, r, st)
		}
		return session(w, r, d)
	}
}

func cncStreamHandler(registry *cnc.Registry) handleFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, _ *data) (int, error) {
		streamer, _, code, err := resolveStreamer(registry, r)
		if err != nil {
			return code, err
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return http.StatusInternalServerError, err
		}
		defer conn.Close()

		if err := writeJSONFrame(conn, cnc.Event{Type: "status", Status: streamer.Status()}); err != nil {
			return 0, nil
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
					return 0, nil
				}
				if err := writeJSONFrame(conn, ev); err != nil {
					return 0, nil
				}
			case <-ping.C:
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(WSWriteDeadline))
			case <-readDone:
				return 0, nil
			case <-r.Context().Done():
				return 0, nil
			}
		}
	})
}

func writeJSONFrame(conn *websocket.Conn, v any) error {
	if err := conn.SetWriteDeadline(time.Now().Add(WSWriteDeadline)); err != nil {
		return err
	}
	return conn.WriteJSON(v)
}

func runQuery(w http.ResponseWriter, r *http.Request, streamer *cnc.Streamer) (int, error) {
	req := &cncQueryBody{}
	if err := json.NewDecoder(r.Body).Decode(req); err != nil {
		return http.StatusBadRequest, err
	}
	if req.Q <= 0 {
		return http.StatusBadRequest, errors.New("q must be a positive integer")
	}

	res, err := streamer.Query(r.Context(), req.Q, req.Var)
	switch {
	case errors.Is(err, cnc.ErrConfigMissing):
		return http.StatusBadRequest, err
	case err != nil:
		return errToStatus(err), err
	}
	return renderJSON(w, r, res)
}

func ensureLeading(p string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}
	return "/" + p
}
