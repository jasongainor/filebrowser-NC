package cncapi

// Shared logic behind GET/PUT /api/cnc/tool-library. Mirrors
// http/cnc_tool_library.go's cncToolLibraryGetHandler /
// cncToolLibraryPutHandler.

import (
	"fmt"
	"time"
)

// ToolLibraryBody is the wire shape for GET /api/cnc/tool-library.
// UploadedAt is `any` (not time.Time) so the empty/unloaded case can
// omit the key entirely via omitempty — a zero time.Time is never
// "empty" to encoding/json, so a typed field would leak
// "0001-01-01T00:00:00Z" into a response that used to have no
// uploaded_at key at all. Mirrors the anonymous map literal
// cncToolLibraryGetHandler used to build directly.
type ToolLibraryBody struct {
	Data          any   `json:"data"`
	Version       int   `json:"version,omitempty"`
	UploadedAt    any   `json:"uploaded_at,omitempty"`
	Loaded        bool  `json:"loaded"`
	AssignedSlots []int `json:"assigned_slots"`
}

// ToolLibraryGet mirrors cncToolLibraryGetHandler.
func (d Deps) ToolLibraryGet() ToolLibraryBody {
	empty := ToolLibraryBody{Data: []any{}, Loaded: false, AssignedSlots: []int{}}
	store := d.Registry.LibraryStore()
	if store == nil {
		return empty
	}
	lib := store.Library()
	if lib == nil {
		return empty
	}
	raw := lib.Raw()
	return ToolLibraryBody{
		Data:          raw.Data,
		Version:       raw.Version,
		UploadedAt:    raw.UploadedAt,
		Loaded:        true,
		AssignedSlots: lib.AssignedSlots(),
	}
}

// ToolLibraryPutBody is the wire shape for PUT /api/cnc/tool-library.
type ToolLibraryPutBody struct {
	Loaded        bool      `json:"loaded"`
	UploadedAt    time.Time `json:"uploaded_at"`
	AssignedSlots []int     `json:"assigned_slots"`
	Count         int       `json:"count"`
}

// toolLibraryUploadCap bounds the request body so a malicious operator
// can't dump a few GB into the config dir. Mirrors http/cnc_tool_library.go's
// constant of the same name.
const ToolLibraryUploadCap = 4 * 1024 * 1024

// ToolLibraryPut replaces the stored library with raw (already
// size-capped by the caller — see ToolLibraryUploadCap). Mirrors
// cncToolLibraryPutHandler.
func (d Deps) ToolLibraryPut(raw []byte) (ToolLibraryPutBody, int, error) {
	store := d.Registry.LibraryStore()
	if store == nil {
		return ToolLibraryPutBody{}, 503, fmt.Errorf("tool-library store not initialised")
	}
	lib, err := store.Replace(raw)
	if err != nil {
		return ToolLibraryPutBody{}, 400, err
	}
	rawLib := lib.Raw()
	return ToolLibraryPutBody{
		Loaded:        true,
		UploadedAt:    rawLib.UploadedAt,
		AssignedSlots: lib.AssignedSlots(),
		Count:         len(rawLib.Data),
	}, 0, nil
}
