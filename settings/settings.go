package settings

import (
	"crypto/rand"
	"io/fs"
	"log"
	"strings"
	"time"

	"github.com/filebrowser/filebrowser/v2/rules"
)

const DefaultUsersHomeBasePath = "/users"
const DefaultLogoutPage = "/login"
const DefaultMinimumPasswordLength = 12
const DefaultFileMode = 0640
const DefaultDirMode = 0750

// AuthMethod describes an authentication method.
type AuthMethod string

// DefaultHaasPort is the Waveshare RS-232↔TCP bridge port the Pi opens
// to drip-feed and query the Haas. Override per-instance via the Machine
// settings tab.
const DefaultHaasPort = 4196

// Cnc holds the per-instance machine integration config that the
// /api/cnc/* endpoints read and the Machine settings tab edits.
//
// Multi-machine support (2026-05-10): Machines is the canonical list.
// Legacy fields (HaasHost/HaasPort/CameraURL) are kept on the struct
// for one-time migration from pre-multi-machine DBs and are NOT read
// by new code. EnsureMigrated() folds them into Machines[0] on first
// boot of the new binary.
type Cnc struct {
	// Machines is the canonical machine list. First entry is the
	// default for any /api/cnc/* call without an explicit machine_id.
	Machines []Machine `json:"machines"`

	// MachineToken is the long-lived bearer used by external services
	// (HA, monitoring, custom dashboards) to call /api/cnc/state or
	// /api/cnc/qcode without a filebrowser session. Global, not
	// per-machine — one token covers all machines under this install.
	MachineToken string `json:"machineToken"`

	// Discord drives push notifications via a bot the admin sets up
	// once. Off until both BotToken AND ChannelID are populated AND
	// at least one Category is enabled. See cnc/notify.go.
	Discord DiscordConfig `json:"discord,omitempty"`

	// Displays is the list of physical / kiosk surfaces that consume
	// the tool-list view. Each carries a machine pointer + a layout
	// config. See Display below + the /api/displays/{id} endpoint.
	Displays []Display `json:"displays,omitempty"`

	// BaselinePollSeconds is the always-on liveness cadence. The
	// aggregator polls the baseline metric set at this interval even
	// when no operator is at the dashboard, which is what makes
	// `connected` meaningful to non-interactive consumers (the e-paper
	// display polls once every ~100 minutes and would otherwise never
	// observe a wake window).
	//
	// A machine is reported connected while its last successful
	// round-trip is within 3x this interval. Raise it to be gentler on
	// the RS-232 link; lower it to detect a dropped controller sooner.
	// 0 uses cnc.defaultBaselineInterval (15s).
	BaselinePollSeconds int `json:"baselinePollSeconds,omitempty"`

	// Reporting sends program-run lifecycle data (open / parts / alarm /
	// close) to gmw-mes so machine time reaches Carbon. See
	// cnc/reporter.go and docs/RUN_REPORTING.md. Off (fully a no-op)
	// while GmwMesURL is empty — an existing install picks this up as
	// dormant config, nothing changes until an admin fills it in.
	Reporting ReportingConfig `json:"reporting,omitempty"`

	// ── Legacy fields (deprecated; migrated into Machines[0]) ──
	HaasHost  string `json:"haasHost,omitempty"`
	HaasPort  int    `json:"haasPort,omitempty"`
	CameraURL string `json:"cameraUrl,omitempty"`
}

// DefaultReportingTokenEnv is the environment variable Reporter reads
// the gmw-mes bot token from when ReportingConfig.TokenEnv is unset.
const DefaultReportingTokenEnv = "GMW_MES_BOT_TOKEN"

// ReportingConfig points cnc.Reporter at gmw-mes's machine-runs API
// (docs/machine-runs.md in the gmw-mes repo). Deliberately does NOT
// carry the bot token itself — only the NAME of the environment
// variable holding it, so the token value never round-trips through
// settings.json, the admin UI, a settings.Save() write, or a log line.
// Reporter reads os.Getenv(TokenEnvName()) at send time, every time.
type ReportingConfig struct {
	// GmwMesURL is the base URL of the gmw-mes instance, e.g.
	// "http://127.0.0.1:5401". Empty disables reporting entirely —
	// Reporter no-ops on every hook when this is unset.
	GmwMesURL string `json:"gmwMesUrl,omitempty"`
	// TokenEnv is the name of the environment variable holding the
	// X-Bot-Token bearer. Defaults to DefaultReportingTokenEnv.
	TokenEnv string `json:"tokenEnv,omitempty"`
	// MachineIDs optionally maps this install's own Machine.ID values
	// to the machine id gmw-mes should see. A machine absent from
	// this map reports its own registry ID verbatim.
	MachineIDs map[string]string `json:"machineIds,omitempty"`
}

// Enabled reports whether reporting is wired up enough to fire.
func (r ReportingConfig) Enabled() bool {
	return strings.TrimSpace(r.GmwMesURL) != ""
}

// TokenEnvName returns the configured env var name, or
// DefaultReportingTokenEnv when unset.
func (r ReportingConfig) TokenEnvName() string {
	if r.TokenEnv != "" {
		return r.TokenEnv
	}
	return DefaultReportingTokenEnv
}

// GmwMesMachineID maps a registry machine id to the id gmw-mes should
// see, via MachineIDs when present, otherwise the id unchanged.
func (r ReportingConfig) GmwMesMachineID(registryID string) string {
	if v, ok := r.MachineIDs[registryID]; ok && v != "" {
		return v
	}
	return registryID
}

// Display is one physical surface (typically a reTerminal E1001
// e-paper) that renders the reconciled tool list for one machine.
// Layout fields steer the on-device rendering — the firmware reads
// them verbatim from /api/displays/{id}.
type Display struct {
	// ID is the stable identifier the firmware embeds in its config
	// (SD config.json -> display_id). Generated on creation.
	ID string `json:"id"`
	// Name is the operator-facing label (admin UI only).
	Name string `json:"name,omitempty"`
	// MachineID points at the Machine whose tool list this display
	// shows. Required; CRUD rejects creates with an unknown ID.
	MachineID string `json:"machineId"`
	// Token is an optional bearer the firmware sends as ?token=...
	// or Authorization: Bearer <t>. When set the endpoint enforces
	// it; empty means LAN-permissive (the default — there is no
	// reasonable threat model on an isolated shop network).
	Token string `json:"token,omitempty"`
	// Resolution is [width,height] in physical pixels. E1001 = 800×480.
	Resolution [2]int `json:"resolution,omitempty"`
	// PocketGrid is [columns,rows] for the page-1 pocket map. E1001
	// 800×480 fits 2×10 cleanly (80 px wide cells).
	PocketGrid [2]int `json:"pocketGrid,omitempty"`
	// LibraryPageSize is the number of rows per library page. With
	// six fields visible and 14 px row height a 480-px screen fits
	// ~24 rows; 20 leaves room for the status line + page indicator.
	LibraryPageSize int `json:"libraryPageSize,omitempty"`
	// Fields is the ordered list of tool-list fields to render in the
	// library table. Known values: pocket, tool_number, description,
	// diameter, length, wear. Defaults to a sensible 6-column layout
	// when empty.
	Fields []string `json:"fields,omitempty"`
	// Units overrides the machine's default. Empty falls back to the
	// machine-level value (typically "in" for Haas NGC).
	Units string `json:"units,omitempty"`
	// PollIntervalPoweredS is the cadence the firmware refetches when
	// USB-powered. PollIntervalBatteryS is the slower cadence for
	// optional battery operation. Either 0 falls back to a default.
	PollIntervalPoweredS int `json:"pollIntervalPoweredS,omitempty"`
	PollIntervalBatteryS int `json:"pollIntervalBatteryS,omitempty"`
}

// Defaults below are applied at GET-time so a Display created with
// minimal config (just MachineID) still renders correctly. We don't
// rewrite the stored Display — admins editing the JSON can leave a
// field empty to mean "use the default."
const (
	DefaultDisplayResX                = 800
	DefaultDisplayResY                = 480
	DefaultDisplayPocketCols          = 2
	DefaultDisplayPocketRows          = 10
	DefaultDisplayLibraryPageSize     = 20
	DefaultDisplayPollPoweredSeconds  = 60
	DefaultDisplayPollBatterySeconds  = 900
)

// DefaultDisplayFields is the order the firmware should render library
// columns when Display.Fields is empty.
var DefaultDisplayFields = []string{
	"pocket", "tool_number", "description", "diameter", "length", "wear", "diameter_wear",
}

// Resolved returns the Display with all zero-value layout fields
// substituted for their defaults. The HTTP layer calls this before
// returning the config so the firmware never has to deal with
// "what's the default?"
func (d Display) Resolved() Display {
	out := d
	if out.Resolution[0] == 0 {
		out.Resolution[0] = DefaultDisplayResX
	}
	if out.Resolution[1] == 0 {
		out.Resolution[1] = DefaultDisplayResY
	}
	if out.PocketGrid[0] == 0 {
		out.PocketGrid[0] = DefaultDisplayPocketCols
	}
	if out.PocketGrid[1] == 0 {
		out.PocketGrid[1] = DefaultDisplayPocketRows
	}
	if out.LibraryPageSize <= 0 {
		out.LibraryPageSize = DefaultDisplayLibraryPageSize
	}
	if len(out.Fields) == 0 {
		out.Fields = append([]string{}, DefaultDisplayFields...)
	}
	if out.PollIntervalPoweredS <= 0 {
		out.PollIntervalPoweredS = DefaultDisplayPollPoweredSeconds
	}
	if out.PollIntervalBatteryS <= 0 {
		out.PollIntervalBatteryS = DefaultDisplayPollBatterySeconds
	}
	return out
}

// DiscordConfig drives push notifications to a Discord channel.
// BotToken is stored write-only — GET handlers mask it before
// returning, so the admin can verify "something is set" without the
// raw value bouncing around the UI. PUT replaces it; the admin
// rotates by pasting a new value.
//
// Categories is a list of opt-in event types — empty list disables
// notifications even when the token is set. Known values:
//   "machine_info"     — status changes (running on/off, recovery)
//   "failures"         — alarms / errors / dial failures
//   "operation_starts" — Send or Attach initiated from the dashboard
type DiscordConfig struct {
	BotToken   string   `json:"botToken,omitempty"`
	ChannelID  string   `json:"channelId,omitempty"`
	Categories []string `json:"categories,omitempty"`
}

// Enabled reports whether notifications are wired up enough to fire.
func (d DiscordConfig) Enabled() bool {
	return d.BotToken != "" && d.ChannelID != "" && len(d.Categories) > 0
}

// CategoryEnabled reports whether `cat` is in the admin-selected list.
func (d DiscordConfig) CategoryEnabled(cat string) bool {
	for _, c := range d.Categories {
		if c == cat {
			return true
		}
	}
	return false
}

// Machine is one configured CNC controller.
type Machine struct {
	// ID is stable across renames. Generated on creation; never
	// edited. The default-machine selector uses this.
	ID string `json:"id"`
	// Name is the operator-facing label, freely editable.
	Name string `json:"name"`
	// Brand identifies the controller family. Only "haas" is wired
	// today; the field exists so a per-brand send protocol / state
	// dialect can be slotted in without a settings-schema migration.
	// Empty values normalize to "haas" on save.
	Brand string `json:"brand,omitempty"`
	// Host:Port is the Waveshare RS-232↔TCP bridge.
	Host string `json:"host"`
	Port int    `json:"port"`
	// Serial configures a direct RS-232 connection to the machine — a
	// Raspberry Pi with a USB→RS-232 adapter owning the Haas' serial
	// header instead of going through the Waveshare bridge. Empty
	// Device (the default) means "keep using Host:Port over TCP";
	// nothing here changes behavior for existing installs. See
	// docs/SERIAL_TRANSPORT.md and EffectiveSerial for the Haas
	// Setting → field mapping and defaults.
	Serial MachineSerial `json:"serial,omitempty"`
	// ToolSlots is the magazine capacity for tool-table reads. Operators
	// set this to their machine's actual slot count (e.g. 20 for a
	// 20-pocket carousel) so reads cover the whole magazine without
	// probing the unreachable upper range. 0 falls back to
	// DefaultToolSlots.
	ToolSlots int `json:"toolSlots,omitempty"`
	// CameraURL is optional. CameraType picks the rendering path.
	CameraURL string `json:"cameraUrl,omitempty"`
	// CameraType is one of "auto" / "hls" / "mjpeg" / "iframe" /
	// "none". Empty normalizes to "auto" (legacy URL-suffix dispatch).
	// "iframe" is required for UniFi Protect / Reolink web UI URLs
	// since browsers cannot play raw RTSP/RTSPS.
	CameraType string `json:"cameraType,omitempty"`
	// RequirePreflight, when true, refuses /api/cnc/start if the
	// preflight comparison flags any tools as missing / empty pocket
	// for the program's T-codes. The wizard already soft-warns; this
	// flips the check to a hard server-side gate so an operator can't
	// "I know what I'm doing" past a missing tool. Off by default —
	// the operator-side controller prep is still on them.
	RequirePreflight bool `json:"requirePreflight,omitempty"`
	// AxesEnabled controls which axes the /machine dashboard renders
	// rows for. X, Y, Z are always present; A, B, C are optional —
	// some machines have a 4th or 5th axis, most don't. Stored as a
	// list of uppercase letters; empty / unset defaults to X+Y+Z.
	AxesEnabled []string `json:"axesEnabled,omitempty"`
	// PositionToleranceIn is the in-inches drift between commanded
	// and machine position that flips the dashboard's Δ-CMD readout
	// from green to amber. 0 / unset falls back to 0.001".
	PositionToleranceIn float64 `json:"positionToleranceIn,omitempty"`
	// DPRNTCapture enables a per-write scavenger on the streaming
	// socket that surfaces Haas DPRNT macro output as live events
	// on the WS feed. Off by default — adds a 1-2ms per-line read
	// during a job. Operators using DPRNT for in-cycle probing /
	// measurement output should turn it on.
	DPRNTCapture bool `json:"dprntCapture,omitempty"`
	// AutoSendEnabled gates the /api/cnc/auto-send pipeline for this
	// machine. When on, sends that pass an all-green preflight (no
	// missing / empty / warn tools, no spindle swap) can fire in
	// one step instead of the operator clicking through the wizard.
	// Hard-block preflight still applies if RequirePreflight is on.
	// CYCLE START is NOT triggered — operators still press the
	// physical button. Off by default.
	AutoSendEnabled bool `json:"autoSendEnabled,omitempty"`
	// NoProbeSlots is the list of pocket numbers that must NOT be
	// touched by a tool-probe routine — typically the probe itself
	// (e.g. OMP-40 in T20) and non-cutting hardware like a chip fan or
	// dust shoe. Surfaced in the tool table as a "🚫 probe" badge so an
	// operator running the controller-side Probe All Tools macro can
	// see at a glance which slots they must exclude. Empty by default.
	//
	// Background: operator hit the OMP with the spindle when "probe
	// all tools" was on but the probe slot wasn't excluded. This list
	// makes the exclusion visually obvious next to the tool readout.
	NoProbeSlots []int `json:"noProbeSlots,omitempty"`
}

// MachineSerial is the direct-serial transport config for one Machine.
// Device is the only field an operator must set — a Raspberry Pi with a
// USB→RS-232 adapter typically shows up as /dev/ttyUSB0 or
// /dev/serial/by-id/…. Every other field has a Haas-sensible zero-value
// default; see EffectiveSerial.
type MachineSerial struct {
	// Device is the tty path on the Pi. Empty means "no direct serial —
	// use Host:Port over TCP to the Waveshare bridge", which is the
	// existing behavior for every machine configured before this field
	// existed.
	Device string `json:"device,omitempty"`
	// Baud must match Haas Setting 11 (Baud Rate Selection).
	Baud int `json:"baud,omitempty"`
	// DataBits must match Haas Setting 37 (RS-232 Data Bits): 7 or 8.
	DataBits int `json:"dataBits,omitempty"`
	// Parity must match Haas Setting 12 (Parity Selection):
	// "even" | "odd" | "none" | "mark" | "space".
	Parity string `json:"parity,omitempty"`
	// StopBits must match Haas Setting 13 (Stop Bits): 1 or 2.
	StopBits int `json:"stopBits,omitempty"`
	// FlowControl must match Haas Setting 14 (Synchronization /
	// Handshake): "xonxoff" | "rtscts" | "none".
	FlowControl string `json:"flowControl,omitempty"`
}

// EffectiveSerial returns m.Serial with every zero-valued field resolved
// to a Haas-sensible default. Deliberately NOT the true Haas factory
// default (8 data bits / no parity / no handshake) — an unattended DNC
// drip-feed over serial is worthless without flow control (Setting
// 14 = XON/XOFF is a hard requirement; see docs/SERIAL_TRANSPORT.md and
// docs/PROGRAM_DELIVERY_AND_LIBRARY_SYNC_TODO.md section B), and 7
// data bits / even parity / 1 stop bit (7E1) is the traditional pairing
// for XON/XOFF DNC on Haas + older Fanuc-style controls. Operators whose
// Haas is actually left at factory defaults (8N1, no handshake) must set
// every field explicitly.
//
//	Haas Setting                          → field        → default
//	11  Baud Rate Selection               → Baud         → 9600
//	37  RS-232 Data Bits                  → DataBits     → 7
//	12  Parity Selection                  → Parity       → "even"
//	13  Stop Bits                         → StopBits     → 1
//	14  Synchronization (Handshake)       → FlowControl  → "xonxoff"
func (m Machine) EffectiveSerial() MachineSerial {
	s := m.Serial
	if s.Baud <= 0 {
		s.Baud = 9600
	}
	if s.DataBits <= 0 {
		s.DataBits = 7
	}
	if strings.TrimSpace(s.Parity) == "" {
		s.Parity = "even"
	}
	if s.StopBits <= 0 {
		s.StopBits = 1
	}
	if strings.TrimSpace(s.FlowControl) == "" {
		s.FlowControl = "xonxoff"
	}
	return s
}

// IsNoProbeSlot reports whether slot n is in the no-probe list.
// Linear scan — the list is short (a Haas magazine is at most 200
// slots; the no-probe list is typically 1-3 entries).
func (m Machine) IsNoProbeSlot(n int) bool {
	for _, s := range m.NoProbeSlots {
		if s == n {
			return true
		}
	}
	return false
}

// EffectiveAxes returns the axes to render for this machine. Defaults
// to X/Y/Z when AxesEnabled is empty. Letters are uppercased and
// deduped; A/B/C are accepted but anything else is dropped.
func (m Machine) EffectiveAxes() []string {
	if len(m.AxesEnabled) == 0 {
		return []string{"X", "Y", "Z"}
	}
	allow := map[string]bool{"X": true, "Y": true, "Z": true, "A": true, "B": true, "C": true}
	seen := map[string]bool{}
	out := make([]string, 0, len(m.AxesEnabled))
	for _, a := range m.AxesEnabled {
		u := strings.ToUpper(strings.TrimSpace(a))
		if !allow[u] || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	if len(out) == 0 {
		return []string{"X", "Y", "Z"}
	}
	return out
}

// EffectivePositionTolerance returns the green/amber threshold in
// inches. Default 0.001" — same as the Haas's own position-drift
// tolerance for most setups.
func (m Machine) EffectivePositionTolerance() float64 {
	if m.PositionToleranceIn <= 0 {
		return 0.001
	}
	return m.PositionToleranceIn
}

// MachineBrandHaas is the only brand wired into the streamer/aggregator
// today. Other brands round-trip through settings but no protocol code
// reads them yet.
const MachineBrandHaas = "haas"

// DefaultToolSlots is the fallback magazine size when a machine's
// ToolSlots is 0. 30 covers most older Haas mills; operators with
// 200-slot tombstones should set ToolSlots explicitly.
const DefaultToolSlots = 30

// EffectiveToolSlots returns the machine's ToolSlots clamped to the
// valid Haas tool-table range. 0 means use the default.
func (m Machine) EffectiveToolSlots() int {
	if m.ToolSlots <= 0 {
		return DefaultToolSlots
	}
	if m.ToolSlots > 200 {
		return 200
	}
	return m.ToolSlots
}

// EnsureMigrated folds legacy single-machine fields into Machines[0]
// if Machines is empty. Idempotent; safe to call on every Settings
// load. Returns true when a migration actually happened so the caller
// can persist.
func (c *Cnc) EnsureMigrated() bool {
	if len(c.Machines) > 0 {
		return false
	}
	if c.HaasHost == "" && c.HaasPort == 0 && c.CameraURL == "" {
		// Brand-new install — no machines yet, nothing to migrate.
		return false
	}
	port := c.HaasPort
	if port == 0 {
		port = DefaultHaasPort
	}
	c.Machines = []Machine{{
		ID:         "primary",
		Name:       "Machine 1",
		Brand:      MachineBrandHaas,
		Host:       c.HaasHost,
		Port:       port,
		CameraURL:  c.CameraURL,
		CameraType: "auto",
	}}
	return true
}

// MachineByID returns the matching Machine and true, or zero + false.
func (c *Cnc) MachineByID(id string) (Machine, bool) {
	for _, m := range c.Machines {
		if m.ID == id {
			return m, true
		}
	}
	return Machine{}, false
}

// DefaultMachineID returns the ID treated as the default when an
// API call doesn't specify one. First entry of Machines, or "" if
// no machines are configured.
func (c *Cnc) DefaultMachineID() string {
	if len(c.Machines) == 0 {
		return ""
	}
	return c.Machines[0].ID
}

// Settings contain the main settings of the application.
type Settings struct {
	Key                   []byte              `json:"key"`
	Signup                bool                `json:"signup"`
	HideLoginButton       bool                `json:"hideLoginButton"`
	CreateUserDir         bool                `json:"createUserDir"`
	UserHomeBasePath      string              `json:"userHomeBasePath"`
	Defaults              UserDefaults        `json:"defaults"`
	AuthMethod            AuthMethod          `json:"authMethod"`
	LogoutPage            string              `json:"logoutPage"`
	Branding              Branding            `json:"branding"`
	Tus                   Tus                 `json:"tus"`
	Commands              map[string][]string `json:"commands"`
	Shell                 []string            `json:"shell"`
	Rules                 []rules.Rule        `json:"rules"`
	MinimumPasswordLength uint                `json:"minimumPasswordLength"`
	FileMode              fs.FileMode         `json:"fileMode"`
	DirMode               fs.FileMode         `json:"dirMode"`
	HideDotfiles          bool                `json:"hideDotfiles"`
	Cnc                   Cnc                 `json:"cnc"`
}

// GetRules implements rules.Provider.
func (s *Settings) GetRules() []rules.Rule {
	return s.Rules
}

// Server specific settings.
type Server struct {
	Root                  string `json:"root"`
	BaseURL               string `json:"baseURL"`
	Socket                string `json:"socket"`
	TLSKey                string `json:"tlsKey"`
	TLSCert               string `json:"tlsCert"`
	Port                  string `json:"port"`
	Address               string `json:"address"`
	Log                   string `json:"log"`
	EnableThumbnails      bool   `json:"enableThumbnails"`
	ResizePreview         bool   `json:"resizePreview"`
	EnableExec            bool   `json:"enableExec"`
	TypeDetectionByHeader bool   `json:"typeDetectionByHeader"`
	ImageResolutionCal    bool   `json:"imageResolutionCalculation"`
	AuthHook              string `json:"authHook"`
	TokenExpirationTime   string `json:"tokenExpirationTime"`
}

// Clean cleans any variables that might need cleaning.
func (s *Server) Clean() {
	s.BaseURL = strings.TrimSuffix(s.BaseURL, "/")
}

func (s *Server) GetTokenExpirationTime(fallback time.Duration) time.Duration {
	if s.TokenExpirationTime == "" {
		return fallback
	}

	duration, err := time.ParseDuration(s.TokenExpirationTime)
	if err != nil {
		log.Printf("[WARN] Failed to parse tokenExpirationTime: %v", err)
		return fallback
	}
	return duration
}

// GenerateKey generates a key of 512 bits.
func GenerateKey() ([]byte, error) {
	b := make([]byte, 64)
	_, err := rand.Read(b)
	// Note that err == nil only if we read len(b) bytes.
	if err != nil {
		return nil, err
	}

	return b, nil
}
