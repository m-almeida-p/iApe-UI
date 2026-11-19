// Command yape-agent runs an ADK REST agent that answers with the models of the
// Yape AI gateway and reads the conversation history through the history
// service's MCP server. Start it like any ADK agent:
//
//	yape-agent web --port=9095 api
//
// Its settings are the ones the Yape agent service reads (the gateway, its
// headers and the credentials of the anonymous token), plus YAPE_AGENT_MODEL and
// YAPE_MCP_URL. scripts/yape-agent.sh takes them from that service's .env and
// hands them to this program in its environment; they are not copied anywhere.
//
// "yape-agent check" asks for a token, lists the MCP server's tools and exits 0
// when both work, without sending anything to a model.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/cmd/launcher"
	"google.golang.org/adk/v2/cmd/launcher/console"
	"google.golang.org/adk/v2/cmd/launcher/universal"
	"google.golang.org/adk/v2/cmd/launcher/web"
	"google.golang.org/adk/v2/cmd/launcher/web/api"
	"google.golang.org/adk/v2/cmd/launcher/web/webui"

	"iape-ui/internal/platform/envcfg"
	"iape-ui/internal/yapeagent"
)

func main() {
	lookup, err := envcfg.EnvLookup(".env")
	if err != nil {
		log.Fatalf("yape-agent: %v", err)
	}
	cfg, err := yapeagent.LoadConfig(lookup)
	if err != nil {
		log.Fatalf("yape-agent: %v", err)
	}
	ctx := context.Background()
	tokens := yapeagent.NewTokenSource(cfg.Token, nil)

	if len(os.Args) > 1 && os.Args[1] == "check" {
		os.Exit(check(ctx, cfg, tokens))
	}

	llm, err := yapeagent.NewModel(ctx, cfg, tokens, nil)
	if err != nil {
		log.Fatalf("yape-agent: %v", err)
	}
	root, err := yapeagent.BuildAgent(llm, cfg.MCPURL)
	if err != nil {
		log.Fatalf("building agent: %v", err)
	}
	fmt.Printf("yape-agent: model %s through %s, tools from %s\n", cfg.Model, hostOf(cfg.GatewayURL), cfg.MCPURL)

	launcherConfig := &launcher.Config{AgentLoader: agent.NewSingleLoader(root)}
	l := universal.NewLauncher(console.NewLauncher(), web.NewLauncher(webui.NewLauncher(), api.NewLauncher()))
	if err := l.Execute(ctx, launcherConfig, os.Args[1:]); err != nil {
		log.Fatalf("run failed: %v\n\n%s", err, l.CommandLineSyntax())
	}
}

// check reports whether the credentials get a token and the MCP server answers.
func check(ctx context.Context, cfg yapeagent.Config, tokens yapeagent.TokenProvider) int {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	code := 0
	if _, err := tokens.Token(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "token: FAIL: %v\n", err)
		code = 1
	} else {
		fmt.Printf("token: ok (the gateway is %s, the model %s)\n", hostOf(cfg.GatewayURL), cfg.Model)
	}
	if names, err := yapeagent.MCPTools(ctx, cfg.MCPURL); err != nil {
		fmt.Fprintf(os.Stderr, "mcp: FAIL: %v\n", err)
		code = 1
	} else {
		fmt.Printf("mcp: ok, %d tools at %s: %s\n", len(names), cfg.MCPURL, strings.Join(names, ", "))
	}
	return code
}

// hostOf names the host of a URL for the log, without its path.
func hostOf(raw string) string {
	raw = strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
	host, _, _ := strings.Cut(raw, "/")
	return host
}
