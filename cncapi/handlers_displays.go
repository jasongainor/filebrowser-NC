package cncapi

// Shared logic behind admin CRUD on /api/cnc/displays (list, create,
// update, delete). The unauthenticated firmware endpoint,
// GET /api/displays/{id}, is a separate, already-shared code path
// (see BuildMachineToolList; the display-fetch handlers themselves —
// http/cnc_displays.go's cncDisplayFetchHandler and
// cncd/display.go's displayFetchHandler — stay host-specific since
// their token-gate wiring differs only in how trivial plumbing is
// glued together, not in any logic worth a third copy). Mirrors
// http/cnc_displays.go's list/create/update/delete handlers.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/filebrowser/filebrowser/v2/settings"
)

// NewDisplayID returns a 16-hex-char random ID. crypto/rand-backed so
// two admins creating displays simultaneously don't collide. Mirrors
// http/cnc_displays.go's newDisplayID.
func NewDisplayID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// DisplayListItem is a Display plus the process-local liveness the
// admin UI needs to render "last seen" / stale. Mirrors
// http/cnc_displays.go's displayListItem.
type DisplayListItem struct {
	settings.Display
	LastSeen *time.Time `json:"lastSeen,omitempty"`
}

// DisplaysList returns the configured displays with liveness info.
// Mirrors cncDisplaysListHandler.
func (d Deps) DisplaysList() []DisplayListItem {
	cfg := d.Store.Snapshot()
	out := make([]DisplayListItem, 0, len(cfg.Displays))
	for _, disp := range cfg.Displays {
		item := DisplayListItem{Display: disp}
		if seen, ok := d.Registry.DisplayLastSeen(disp.ID); ok {
			t := seen
			item.LastSeen = &t
		}
		out = append(out, item)
	}
	return out
}

// FindDisplayIndex returns the index of the display whose ID matches
// in cfg, or -1. Mirrors http/cnc_displays.go's findDisplayIndex /
// cncd/display.go's copy of it.
func FindDisplayIndex(cfg settings.Cnc, id string) int {
	for i := range cfg.Displays {
		if cfg.Displays[i].ID == id {
			return i
		}
	}
	return -1
}

// DisplaysCreate validates and appends a new display. Mirrors
// cncDisplaysCreateHandler. newID is provided by the caller (random
// ID minting is host-agnostic but each host has its own generator —
// http/cnc_displays.go's newDisplayID vs. one this package could
// offer; passing it in keeps this function pure and easy to test).
func (d Deps) DisplaysCreate(disp settings.Display, newID string) (settings.Display, int, error) {
	if disp.MachineID == "" {
		return settings.Display{}, 400, errors.New("machineId required")
	}
	cfg := d.Store.Snapshot()
	if FindMachine(cfg, disp.MachineID) == nil {
		return settings.Display{}, 400, errors.New("unknown machineId")
	}
	disp.ID = newID
	if err := d.Store.Update(func(c *settings.Cnc) error {
		c.Displays = append(c.Displays, disp)
		return nil
	}); err != nil {
		return settings.Display{}, errToStatusDefault(err), err
	}
	return disp, 0, nil
}

// DisplaysUpdate validates and replaces the display at id. Mirrors
// cncDisplaysUpdateHandler.
func (d Deps) DisplaysUpdate(id string, disp settings.Display) (settings.Display, int, error) {
	if id == "" {
		return settings.Display{}, 400, errors.New("display id required")
	}
	if disp.MachineID == "" {
		return settings.Display{}, 400, errors.New("machineId required")
	}
	cfg := d.Store.Snapshot()
	if FindMachine(cfg, disp.MachineID) == nil {
		return settings.Display{}, 400, errors.New("unknown machineId")
	}
	disp.ID = id
	idx := FindDisplayIndex(cfg, id)
	if idx < 0 {
		return settings.Display{}, 404, errors.New("display not found")
	}
	if err := d.Store.Update(func(c *settings.Cnc) error {
		i := FindDisplayIndex(*c, id)
		if i < 0 {
			return errors.New("display not found")
		}
		c.Displays[i] = disp
		return nil
	}); err != nil {
		return settings.Display{}, errToStatusDefault(err), err
	}
	return disp, 0, nil
}

// DisplaysDelete removes the display at id. Mirrors
// cncDisplaysDeleteHandler.
func (d Deps) DisplaysDelete(id string) (int, error) {
	if id == "" {
		return 400, errors.New("display id required")
	}
	cfg := d.Store.Snapshot()
	if FindDisplayIndex(cfg, id) < 0 {
		return 404, errors.New("display not found")
	}
	if err := d.Store.Update(func(c *settings.Cnc) error {
		i := FindDisplayIndex(*c, id)
		if i < 0 {
			return errors.New("display not found")
		}
		c.Displays = append(c.Displays[:i], c.Displays[i+1:]...)
		return nil
	}); err != nil {
		return errToStatusDefault(err), err
	}
	return 0, nil
}
