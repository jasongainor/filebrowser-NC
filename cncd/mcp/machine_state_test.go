package mcp

import (
	"context"
	"testing"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

func callTool(t *testing.T, h func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error), args map[string]any) *mcpsdk.CallToolResult {
	t.Helper()
	var req mcpsdk.CallToolRequest
	req.Params.Arguments = args
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	return res
}

func structuredOf(t *testing.T, res *mcpsdk.CallToolResult) any {
	t.Helper()
	if res.StructuredContent == nil {
		t.Fatalf("result has no structured content: %+v", res)
	}
	return res.StructuredContent
}

func TestMachineState_HappyPath(t *testing.T) {
	d := newTestDeps(t, nil)
	res := callTool(t, machineStateHandler(d), map[string]any{})
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res)
	}
	structuredOf(t, res)
}

func TestMachineState_UnknownMachine(t *testing.T) {
	d := newTestDeps(t, nil)
	res := callTool(t, machineStateHandler(d), map[string]any{"machine_id": "nope"})
	if !res.IsError {
		t.Fatalf("expected an error result for an unknown machine, got %+v", res)
	}
}

func TestMachineState_NoRegistry(t *testing.T) {
	d := newTestDeps(t, nil)
	d.Registry = nil
	res := callTool(t, machineStateHandler(d), map[string]any{})
	if !res.IsError {
		t.Fatalf("expected an error result with no registry configured, got %+v", res)
	}
}
