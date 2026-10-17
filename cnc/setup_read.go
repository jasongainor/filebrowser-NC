package cnc

// Small Q-code reads the setup sheet polls with. Both go through the
// Streamer's Link like every other query, so the single RS-232 owner is
// unchanged and nothing here wakes the 3 s aggregator.

import (
	"context"
	"fmt"
)

// LoadedProgram asks the control which program is loaded (Q500) and returns
// the raw program token ("O31011", "MDI", ...).
func (s *Streamer) LoadedProgram(ctx context.Context) (string, error) {
	res, err := s.Query(ctx, 500, nil)
	if err != nil {
		return "", err
	}
	if !res.OK {
		return "", fmt.Errorf("Q500: %s", res.Error)
	}
	return extractProgram(&Metric{Parsed: res.Parsed, Value: res.Value}), nil
}

// ReadToolSlots reads length-geom, diameter-geom and diameter-wear for just
// the listed pockets (3 Q600 each) instead of the whole table.
func (s *Streamer) ReadToolSlots(ctx context.Context, pockets []int) ([]ToolTableSlot, error) {
	return ReadSlotsVia(ctx, s.Query, pockets)
}

// ReadSlotsVia is ReadToolSlots over any Q-code function (a seam for tests).
func ReadSlotsVia(ctx context.Context, query func(context.Context, int, *int) (*QueryResult, error), pockets []int) ([]ToolTableSlot, error) {
	out := make([]ToolTableSlot, 0, len(pockets))
	for _, p := range pockets {
		row := ToolTableSlot{Slot: p}
		for _, key := range [...]string{"length_geom", "diameter_geom", "diameter_wear"} {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			v := baseForKey(key) + (p - 1)
			res, qerr := query(ctx, 600, &v)
			applyBase(&row, key, res, qerr, nil)
		}
		out = append(out, row)
	}
	return out, nil
}
