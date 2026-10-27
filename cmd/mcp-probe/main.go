// Command mcp-probe connects to an external Streamable HTTP MCP server and
// prints the tools it exposes. It does not call those tools or use a model.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"recallweave/internal/mcp"
)

func main() {
	endpoint := flag.String("url", "", "external MCP Streamable HTTP endpoint")
	tokenEnv := flag.String("token-env", "RECALLWEAVE_EXTERNAL_MCP_TOKEN", "environment variable containing the optional bearer token")
	flag.Parse()
	if *endpoint == "" {
		fmt.Fprintln(os.Stderr, "usage: go run ./cmd/mcp-probe -url https://example.com/mcp")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if *tokenEnv != "" && os.Getenv(*tokenEnv) == "" {
		fmt.Fprintf(os.Stderr, "warning: token environment variable %s is empty\n", *tokenEnv)
	}
	client, err := mcp.ConnectHTTP(ctx, *endpoint, os.Getenv(*tokenEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer client.Close()
	tools, err := client.ListTools(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	output := make([]struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
	}, 0, len(tools))
	for _, tool := range tools {
		output = append(output, struct {
			Name        string `json:"name"`
			Description string `json:"description,omitempty"`
		}{Name: tool.Name, Description: tool.Description})
	}
	if err := json.NewEncoder(os.Stdout).Encode(output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
