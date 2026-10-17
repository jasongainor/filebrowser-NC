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
	token, _, err := ProgramStateVia(ctx, query)
	return token, err
}

// ProgramState is LoadedProgram plus whether the control is busy (in a
// cycle): busy means the token came from #4115, which during a touch-off
// macro is the macro's own O number.
func (s *Streamer) ProgramState(ctx context.Context) (string, bool, error) {
	return ProgramStateVia(ctx, s.Query)
}

// toolInSpindleVar is the Haas "tool in spindle" macro variable.
const toolInSpindleVar = 3026

// ToolInSpindle reads #3026.
func (s *Streamer) ToolInSpindle(ctx context.Context) (int, error) {
	return ToolInSpindleVia(ctx, s.Query)
}

// ToolInSpindleVia is ToolInSpindle over any Q-code function.
func ToolInSpindleVia(ctx context.Context, query func(context.Context, int, *int) (*QueryResult, error)) (int, error) {
	v := toolInSpindleVar
	res, err := query(ctx, 600, &v)
	if err != nil {
		return 0, err
	}
	if !res.OK {
		return 0, fmt.Errorf("#%d: %s", v, res.Error)
	}
	switch x := res.Parsed.(type) {
	case float64:
		return int(x), nil
	case int:
		return x, nil
	}
	return 0, fmt.Errorf("#%d: no tool number in %q", v, res.Value)
}

// ProgramStateVia is ProgramState over any Q-code function.
func ProgramStateVia(ctx context.Context, query func(context.Context, int, *int) (*QueryResult, error)) (string, bool, error) {
	res, err := query(ctx, 500, nil)
	if err == nil && res.OK {
		return extractProgram(&Metric{Parsed: res.Parsed, Value: res.Value}), false, nil
	}
	q500 := err
	if q500 == nil {
		q500 = fmt.Errorf("Q500: %s", res.Error)
	}
	v := programMacroVar
	mres, merr := query(ctx, 600, &v)
	if merr != nil {
		return "", false, fmt.Errorf("%v; #%d: %w", q500, v, merr)
	}
	if !mres.OK {
		return "", false, fmt.Errorf("%v; #%d: %s", q500, v, mres.Error)
	}
	var n float64
	switch x := mres.Parsed.(type) {
	case float64:
		n = x
	case int:
		n = float64(x)
	}
	if n <= 0 || n != float64(int(n)) {
		return "", false, fmt.Errorf("%v; #%d: no program number in %q", q500, v, mres.Value)
	}
	return fmt.Sprintf("O%d", int(n)), true, nil
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
