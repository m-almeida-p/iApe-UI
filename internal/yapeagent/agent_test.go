package yapeagent_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/yapeagent"
)

type noArgs struct{}

type accounts struct {
	IDs []string `json:"ids"`
}

// fakeMCP serves two read-only tools over streamable HTTP, as the history service's MCP server does.
func fakeMCP(t *testing.T) string {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-history", Version: "1"}, nil)
	for _, name := range []string{"list_accounts", "get_transcript"} {
		mcp.AddTool(server, &mcp.Tool{Name: name, Description: "a tool"},
			func(context.Context, *mcp.CallToolRequest, noArgs) (*mcp.CallToolResult, accounts, error) {
				return nil, accounts{IDs: []string{"acc-1"}}, nil
			})
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestMCPToolsListsWhatTheServerOffers(t *testing.T) {
	names, err := yapeagent.MCPTools(context.Background(), fakeMCP(t))

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"list_accounts", "get_transcript"}, names)
}

func TestMCPToolsWhenTheServerIsDown(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	url := ts.URL
	ts.Close()

	_, err := yapeagent.MCPTools(context.Background(), url)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "connect to the MCP server")
}

func TestBuildAgentIsNamedForTheWebUIAndDoesNotDialTheServer(t *testing.T) {
	llm, err := yapeagent.NewModel(context.Background(), yapeagent.Config{GatewayURL: "http://127.0.0.1:1", Model: "m"}, &fakeTokens{token: "t"}, nil)
	require.NoError(t, err)

	root, err := yapeagent.BuildAgent(llm, "http://127.0.0.1:1/mcp")

	require.NoError(t, err, "building never connects: the session starts with the first request")
	assert.Equal(t, yapeagent.AppName, root.Name())
}
