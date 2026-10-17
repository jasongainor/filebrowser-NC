package cnc

// Small Q-code reads the setup sheet polls with. Both go through the
// Streamer's Link like every other query, so the single RS-232 owner is
// unchanged and nothing here wakes the 3 s aggregator.

import (
	"context"
	"fmt"
)

// programMacroVar is the Haas modal O number (#4115). The control answers
// Q600 for it mid-cycle, when Q500 only says "STATUS, BUSY".
const programMacroVar = 4115

// LoadedProgram asks the control which program is loaded (Q500) and returns
// the raw program token ("O31011", "MDI", ...). While a cycle runs Q500
// answers busy, so it falls back to macro #4115.
func (s *Streamer) LoadedProgram(ctx context.Context) (string, error) {
	return LoadedProgramVia(ctx, s.Query)
}

// LoadedProgramVia is LoadedProgram over any Q-code function (a seam for tests).
func LoadedProgramVia(ctx context.Context, query func(context.Context, int, *int) (*QueryResult, error)) (string, error) {
	res, err := query(ctx, 500, nil)
	if err == nil && res.OK {
		return extractProgram(&Metric{Parsed: res.Parsed, Value: res.Value}), nil
	}
	q500 := err
	if q500 == nil {
		q500 = fmt.Errorf("Q500: %s", res.Error)
	}
	v := programMacroVar
	mres, merr := query(ctx, 600, &v)
	if merr != nil {
		return "", fmt.Errorf("%v; #%d: %w", q500, v, merr)
	}
	if !mres.OK {
		return "", fmt.Errorf("%v; #%d: %s", q500, v, mres.Error)
	}
	var n float64
	switch x := mres.Parsed.(type) {
	case float64:
		n = x
	case int:
		n = float64(x)
	}
	if n <= 0 || n != float64(int(n)) {
		return "", fmt.Errorf("%v; #%d: no program number in %q", q500, v, mres.Value)
	}
	return fmt.Sprintf("O%d", int(n)), nil
}

// ReadToolSlots reads length and diameter, geometry and wear, for just the
// listed pockets (4 Q600 each) instead of the whole table.
func (s *Streamer) ReadToolSlots(ctx context.Context, pockets []int) ([]ToolTableSlot, error) {
	return ReadSlotsVia(ctx, s.Query, pockets)
}

// ReadSlotsVia is ReadToolSlots over any Q-code function (a seam for tests).
func ReadSlotsVia(ctx context.Context, query func(context.Context, int, *int) (*QueryResult, error), pockets []int) ([]ToolTableSlot, error) {
	out := make([]ToolTableSlot, 0, len(pockets))
	for _, p := range pockets {
		row := ToolTableSlot{Slot: p}
		for _, key := range [...]string{"length_geom", "length_wear", "diameter_geom", "diameter_wear"} {
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
