package cncd

// Router-level tests for the MCP mount (router_mcp.go): the bearer
// gate registerMCP wraps the streamable-HTTP handler in, and that a
// real MCP client can complete the initialize/list-tools/call-tool
// round trip through it. Per-tool behavior (happy paths, validation
// errors, the check_tools summary) is covered in cncd/mcp's own
// tests against the handlers directly — these tests only prove the
// transport + auth wiring cncd itself is responsible for.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/filebrowser/filebrowser/v2/settings"
)

func TestMCP_BearerRequired(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) {
		c.MachineToken = "s3cret"
	})
	srv := httptest.NewServer(NewRouter(deps))
	defer srv.Close()

	post := func(t *testing.T, bearer string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := post(t, ""); got != http.StatusUnauthorized {
		t.Errorf("no bearer: got %d, want 401", got)
	}
	if got := post(t, "wrong"); got != http.StatusUnauthorized {
		t.Errorf("wrong bearer: got %d, want 401", got)
	}
}

func TestMCP_CorrectBearer_FullRoundTrip(t *testing.T) {
	deps, _ := newTestDeps(t, func(c *settings.Cnc) {
		c.MachineToken = "s3cret"
	})
	srv := httptest.NewServer(NewRouter(deps))
	defer srv.Close()

	mcpClient, err := client.NewStreamableHttpClient(srv.URL+"/mcp",
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer s3cret"}))
	if err != nil {
		t.Fatalf("NewStreamableHttpClient: %v", err)
	}
	defer mcpClient.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := mcpClient.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var initReq mcp.InitializeRequest
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "cncd-router-test", Version: "0.0.0"}
	if _, err := mcpClient.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	toolsRes, err := mcpClient.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	want := map[string]bool{
		"machine_state": false, "tool_table": false, "check_tools": false,
		"preflight_program": false, "list_programs": false,
	}
	for _, tool := range toolsRes.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("tools/list did not include %q", name)
		}
	}

	var callReq mcp.CallToolRequest
	callReq.Params.Name = "machine_state"
	callReq.Params.Arguments = map[string]any{}
	callRes, err := mcpClient.CallTool(ctx, callReq)
	if err != nil {
		t.Fatalf("CallTool(machine_state): %v", err)
	}
	if callRes.IsError {
		t.Fatalf("machine_state returned an error result: %+v", callRes)
	}
}
