package cncapi

// Shared logic behind /api/cnc/queue/*. Mirrors http/cnc_queue.go.

import (
	"fmt"

	"github.com/filebrowser/filebrowser/v2/cnc"
)

// QueueList returns machineID's queue (empty slice, never nil, when
// the store isn't initialized). Mirrors cncQueueListHandler.
func (d Deps) QueueList(machineID string) []cnc.QueueItem {
	qs := d.Registry.Queues()
	if qs == nil {
		return []cnc.QueueItem{}
	}
	return qs.List(machineID)
}

// QueueAddRequest is the wire shape for POST /api/cnc/queue.
type QueueAddRequest struct {
	FilePath  string `json:"file_path"`
	MachineID string `json:"machine_id,omitempty"`
}

// QueueAdd validates and resolves req.FilePath, appends it to
// resolvedMachineID's queue, and broadcasts the new snapshot. Mirrors
// cncQueueAddHandler. queryMachineID is the ?machine_id= fallback used
// when the body omits one.
func (d Deps) QueueAdd(req QueueAddRequest, queryMachineID string) (cnc.QueueItem, int, error) {
	if req.FilePath == "" {
		return cnc.QueueItem{}, 400, fmt.Errorf("file_path required")
	}
	machineID := req.MachineID
	if machineID == "" {
		machineID = queryMachineID
	}
	streamer, resolvedID, status, err := d.ResolveStreamer(machineID)
	if err != nil {
		return cnc.QueueItem{}, status, err
	}
	clean, err := CleanScopedPath(req.FilePath)
	if err != nil {
		return cnc.QueueItem{}, 400, err
	}
	absPath, err := d.Resolver.FullPath(clean)
	if err != nil {
		return cnc.QueueItem{}, 400, err
	}
	qs := d.Registry.Queues()
	if qs == nil {
		return cnc.QueueItem{}, 503, fmt.Errorf("queue persistence unavailable")
	}
	item, err := qs.Add(resolvedID, cnc.QueueItem{FilePath: clean}, absPath)
	if err != nil {
		return cnc.QueueItem{}, errToStatusDefault(err), err
	}
	streamer.EmitQueueSnapshot(qs.List(resolvedID))
	return item, 0, nil
}

// QueueRemove removes id from machineID's queue and broadcasts the
// new snapshot. Mirrors cncQueueRemoveHandler.
func (d Deps) QueueRemove(machineID, id string) (int, error) {
	if id == "" {
		return 400, fmt.Errorf("queue item id required")
	}
	qs := d.Registry.Queues()
	if qs == nil {
		return 503, fmt.Errorf("queue persistence unavailable")
	}
	if err := qs.Remove(machineID, id); err != nil {
		return errToStatusDefault(err), err
	}
	if streamer, _ := d.Registry.Streamer(machineID); streamer != nil {
		streamer.EmitQueueSnapshot(qs.List(machineID))
	}
	return 0, nil
}

// QueueReorder replaces machineID's queue ordering and broadcasts the
// new snapshot, returning it. Mirrors cncQueueReorderHandler.
func (d Deps) QueueReorder(machineID string, ids []string) ([]cnc.QueueItem, int, error) {
	qs := d.Registry.Queues()
	if qs == nil {
		return nil, 503, fmt.Errorf("queue persistence unavailable")
	}
	if err := qs.Reorder(machineID, ids); err != nil {
		return nil, errToStatusDefault(err), err
	}
	list := qs.List(machineID)
	if streamer, _ := d.Registry.Streamer(machineID); streamer != nil {
		streamer.EmitQueueSnapshot(list)
	}
	return list, 0, nil
}

// QueuePromote marks id as "running" (demoting any other in-flight
// row) and broadcasts the new snapshot. New endpoint — no filebrowser
// precedent existed at the HTTP layer (the equivalent,
// PromoteByONumber, only ever fired from the streamer's own
// auto-attach watcher, cnc/registry.go). This is its manual,
// ID-addressed counterpart: an operator confirming "this queued file
// is what's running" when the O-number auto-match hasn't fired yet.
func (d Deps) QueuePromote(machineID, id string) (*cnc.QueueItem, int, error) {
	if id == "" {
		return nil, 400, fmt.Errorf("queue item id required")
	}
	qs := d.Registry.Queues()
	if qs == nil {
		return nil, 503, fmt.Errorf("queue persistence unavailable")
	}
	item, err := qs.PromoteByID(machineID, id)
	if err != nil {
		return nil, errToStatusDefault(err), err
	}
	if streamer, _ := d.Registry.Streamer(machineID); streamer != nil {
		streamer.EmitQueueSnapshot(qs.List(machineID))
	}
	return item, 0, nil
}
