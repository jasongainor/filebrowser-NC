package cnc

import (
	"context"
	"errors"
	"testing"
)

func fakeQuery(replies map[int]string) func(context.Context, int, *int) (*QueryResult, error) {
	return func(_ context.Context, q int, v *int) (*QueryResult, error) {
		value, ok := replies[q]
		if !ok {
			return nil, errors.New("no reply")
		}
		if q == 500 && value == "STATUS, BUSY" {
			return &QueryResult{Q: q, Value: value, Error: `Q500 expected PROGRAM prefix, got "STATUS, BUSY"`}, nil
		}
		return &QueryResult{Q: q, Var: v, Value: value, Parsed: parseValue(value, q, v), OK: true}, nil
	}
}

func TestLoadedProgramIdleUsesQ500(t *testing.T) {
	got, err := LoadedProgramVia(context.Background(), fakeQuery(map[int]string{500: "PROGRAM, O03104, IDLE, PARTS, 15"}))
	if err != nil || LoadedProgramNumber(got) != "3104" {
		t.Fatalf("got %q, %v; want O03104", got, err)
	}
}

// Mid-cycle the control answers Q500 with STATUS, BUSY; macro #4115 still
// carries the running program (seen on the TM-2P: "MACRO, 4115,    3104.000000").
func TestLoadedProgramBusyFallsBackToMacro4115(t *testing.T) {
	got, err := LoadedProgramVia(context.Background(), fakeQuery(map[int]string{
		500: "STATUS, BUSY",
		600: "MACRO, 4115,    3104.000000",
	}))
	if err != nil || LoadedProgramNumber(got) != "3104" {
		t.Fatalf("got %q, %v; want O3104", got, err)
	}
}

func TestLoadedProgramBusyAndNoMacroIsAnError(t *testing.T) {
	if _, err := LoadedProgramVia(context.Background(), fakeQuery(map[int]string{500: "STATUS, BUSY"})); err == nil {
		t.Fatal("want an error when neither Q500 nor #4115 answers")
	}
}
