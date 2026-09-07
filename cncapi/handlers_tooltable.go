package cncapi

// Shared logic behind /api/cnc/tool-table (POST read-live, GET
// latest), /api/cnc/tool-table/history, and /api/cnc/tool-table/edit.
// Mirrors http/cnc.go's cncToolTableReadHandler /
// cncToolTableLatestHandler / cncToolTableHistoryHandler and
// http/cnc_tool_table_edit.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/filebrowser/filebrowser/v2/cnc"
)

// ToolTableReadEnvelope is the wire shape for POST /api/cnc/tool-table
// (and GET, latest). Mirrors the map{"table":...} shape both
// cncToolTableReadHandler and cncToolTableLatestHandler build.
type ToolTableReadEnvelope struct {
	Table        *cnc.ToolTable `json:"table"`
	ReadError    string         `json:"read_error,omitempty"`
	PersistError string         `json:"persist_error,omitempty"`
}

// ToolTableReadLive triggers a live controller read of slots pockets
// (0/negative = the machine's configured default), persists the
// result (even a partial one on timeout/cancel), and returns the
// envelope. Mirrors cncToolTableReadHandler.
func (d Deps) ToolTableReadLive(ctx context.Context, machineID string, slots int) (ToolTableReadEnvelope, int, error) {
	st, resolvedID, status, err := d.ResolveStreamer(machineID)
	if err != nil {
		return ToolTableReadEnvelope{}, status, err
	}
	if slots < 1 || slots > 200 {
		return ToolTableReadEnvelope{}, 400, fmt.Errorf("slots must be 1..200")
	}
	tbl, readErr := st.ReadToolTable(ctx, slots)
	if tbl == nil {
		return ToolTableReadEnvelope{}, errToStatusDefault(readErr), readErr
	}
	env := ToolTableReadEnvelope{Table: tbl}
	if readErr != nil {
		env.ReadError = readErr.Error()
	}
	if err := d.PersistToolTable(resolvedID, tbl); err != nil {
		env.PersistError = err.Error()
	}
	return env, 0, nil
}

// ToolTableLatest returns the most recently persisted dump for
// machineID. found is false (status/err both zero) when there is none
// yet — the caller should render 204 No Content in that case, matching
// cncToolTableLatestHandler.
func (d Deps) ToolTableLatest(machineID string) (tbl *cnc.ToolTable, found bool, status int, err error) {
	_, resolvedID, status, err := d.ResolveStreamer(machineID)
	if err != nil {
		return nil, false, status, err
	}
	tbl, err = d.LatestToolTable(resolvedID)
	if err != nil {
		return nil, false, 500, err
	}
	if tbl == nil {
		return nil, false, 0, nil
	}
	return tbl, true, 0, nil
}

// ToolTableHistoryEntry is the lightweight summary returned by the
// history-list endpoint. Mirrors http/cnc.go's toolTableHistoryEntry.
type ToolTableHistoryEntry struct {
	Path           string    `json:"path"`
	Filename       string    `json:"filename"`
	ModifiedAt     time.Time `json:"modified_at"`
	SizeBytes      int64     `json:"size_bytes"`
	SlotsRequested int       `json:"slots_requested,omitempty"`
	SlotsRead      int       `json:"slots_read,omitempty"`
}

// ToolTableHistoryBody is the wire shape for GET
// /api/cnc/tool-table/history.
type ToolTableHistoryBody struct {
	MachineID string                  `json:"machine_id"`
	Folder    string                  `json:"folder"`
	Entries   []ToolTableHistoryEntry `json:"entries"`
}

// ToolTableHistory lists machineID's tool-table dump folder,
// newest-first. Mirrors cncToolTableHistoryHandler. shareRelRoot is
// the host's display-facing folder prefix ("/cnc-tool-tables" on
// filebrowser, "cnc-tool-tables" on cncd) — cosmetic only, used to
// build Folder/Path for a UI link.
func (d Deps) ToolTableHistory(machineID, shareRelRoot string) (ToolTableHistoryBody, int, error) {
	_, resolvedID, status, err := d.ResolveStreamer(machineID)
	if err != nil {
		return ToolTableHistoryBody{}, status, err
	}
	dir, err := d.ToolTableDirAbs(resolvedID)
	if err != nil {
		return ToolTableHistoryBody{}, 400, err
	}
	shareRel := path.Join(shareRelRoot, SanitizeMachineID(resolvedID))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return ToolTableHistoryBody{
				MachineID: resolvedID,
				Folder:    shareRel,
				Entries:   []ToolTableHistoryEntry{},
			}, 0, nil
		}
		return ToolTableHistoryBody{}, 500, err
	}

	out := make([]ToolTableHistoryEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		ent := ToolTableHistoryEntry{
			Path:       path.Join(shareRel, e.Name()),
			Filename:   e.Name(),
			ModifiedAt: info.ModTime(),
			SizeBytes:  info.Size(),
		}
		if buf, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
			var hdr struct {
				SlotsRequested int `json:"slots_requested"`
				SlotsRead      int `json:"slots_read"`
			}
			_ = json.Unmarshal(buf, &hdr)
			ent.SlotsRequested = hdr.SlotsRequested
			ent.SlotsRead = hdr.SlotsRead
		}
		out = append(out, ent)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ModifiedAt.After(out[j].ModifiedAt)
	})
	return ToolTableHistoryBody{MachineID: resolvedID, Folder: shareRel, Entries: out}, 0, nil
}

// ToolTableEditRequest is the wire shape for POST
// /api/cnc/tool-table/edit.
type ToolTableEditRequest struct {
	Slot         int      `json:"slot"`
	LengthGeom   *float64 `json:"length_geom,omitempty"`
	LengthWear   *float64 `json:"length_wear,omitempty"`
	DiameterGeom *float64 `json:"diameter_geom,omitempty"`
	DiameterWear *float64 `json:"diameter_wear,omitempty"`
	CopyFromSlot int      `json:"copy_from_slot,omitempty"`
}

// ToolTableEdit applies a local-only tool-table override on top of
// the latest persisted dump and persists a new one. Refuses while a
// job is streaming. Mirrors cncToolTableEditHandler exactly.
func (d Deps) ToolTableEdit(req ToolTableEditRequest, machineID string) (*cnc.ToolTable, int, error) {
	st, resolvedID, status, err := d.ResolveStreamer(machineID)
	if err != nil {
		return nil, status, err
	}
	if st.IsRunning() {
		return nil, 409, errors.New("can't edit tool table during a streaming job")
	}
	if req.Slot < 1 || req.Slot > 200 {
		return nil, 400, errors.New("slot must be 1-200")
	}
	if req.CopyFromSlot != 0 && (req.CopyFromSlot < 1 || req.CopyFromSlot > 200) {
		return nil, 400, errors.New("copy_from_slot must be 1-200")
	}
	if req.CopyFromSlot == req.Slot {
		return nil, 400, errors.New("copy_from_slot must differ from slot")
	}
	if req.CopyFromSlot == 0 &&
		req.LengthGeom == nil && req.LengthWear == nil &&
		req.DiameterGeom == nil && req.DiameterWear == nil {
		return nil, 400, errors.New("nothing to apply: pass copy_from_slot or at least one numeric field")
	}

	dir, err := d.ToolTableDirAbs(resolvedID)
	if err != nil {
		return nil, 400, err
	}
	latestPath, err := NewestJSONIn(dir)
	if err != nil {
		return nil, 500, err
	}
	if latestPath == "" {
		return nil, 400, errors.New("no tool-table read on file yet — read at least once before editing")
	}
	buf, err := os.ReadFile(latestPath)
	if err != nil {
		return nil, 500, err
	}
	var prev cnc.ToolTable
	if err := json.Unmarshal(buf, &prev); err != nil {
		return nil, 500, fmt.Errorf("parse latest dump: %w", err)
	}

	var copySrc *cnc.ToolTableSlot
	if req.CopyFromSlot != 0 {
		copySrc = findToolSlot(&prev, req.CopyFromSlot)
		if copySrc == nil {
			return nil, 404, fmt.Errorf("copy_from_slot %d not present in latest read", req.CopyFromSlot)
		}
	}

	next := prev
	next.Slots = make([]cnc.ToolTableSlot, len(prev.Slots))
	copy(next.Slots, prev.Slots)

	target := -1
	for i := range next.Slots {
		if next.Slots[i].Slot == req.Slot {
			target = i
			break
		}
	}
	if target < 0 {
		next.Slots = append(next.Slots, cnc.ToolTableSlot{Slot: req.Slot})
		target = len(next.Slots) - 1
	}

	now := time.Now().UTC()
	row := &next.Slots[target]
	row.Errors = nil
	row.Empty = false

	if copySrc != nil {
		row.LengthGeom = cloneFloatPtr(copySrc.LengthGeom)
		row.LengthWear = cloneFloatPtr(copySrc.LengthWear)
		row.DiameterGeom = cloneFloatPtr(copySrc.DiameterGeom)
		row.DiameterWear = cloneFloatPtr(copySrc.DiameterWear)
	} else {
		if req.LengthGeom != nil {
			row.LengthGeom = cloneFloatPtr(req.LengthGeom)
		}
		if req.LengthWear != nil {
			row.LengthWear = cloneFloatPtr(req.LengthWear)
		}
		if req.DiameterGeom != nil {
			row.DiameterGeom = cloneFloatPtr(req.DiameterGeom)
		}
		if req.DiameterWear != nil {
			row.DiameterWear = cloneFloatPtr(req.DiameterWear)
		}
	}
	if row.LengthGeom != nil && row.LengthWear != nil {
		v := *row.LengthGeom + *row.LengthWear
		row.EffectiveLength = &v
	} else {
		row.EffectiveLength = nil
	}
	if row.DiameterGeom != nil && row.DiameterWear != nil {
		v := *row.DiameterGeom + *row.DiameterWear
		row.EffectiveDiameter = &v
	} else {
		row.EffectiveDiameter = nil
	}
	row.ManuallyEdited = true
	row.EditedAt = now

	next.SlotsRead = 0
	for _, s := range next.Slots {
		if s.LengthGeom != nil || s.LengthWear != nil ||
			s.DiameterGeom != nil || s.DiameterWear != nil {
			next.SlotsRead++
		}
	}
	next.ReadAt = now
	next.DurationMs = 0
	next.Source = "edit"

	if err := d.PersistToolTable(resolvedID, &next); err != nil {
		return nil, 500, err
	}
	return &next, 0, nil
}

func findToolSlot(t *cnc.ToolTable, slot int) *cnc.ToolTableSlot {
	for i := range t.Slots {
		if t.Slots[i].Slot == slot {
			return &t.Slots[i]
		}
	}
	return nil
}

func cloneFloatPtr(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
