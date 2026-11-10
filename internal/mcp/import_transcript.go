package mcp

import (
	"context"

	"recallweave/internal/ingest"
	"recallweave/internal/types"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type transcriptImporter interface {
	ImportTranscript(context.Context, types.SourceTag, string, []ingest.TranscriptMessage) (ingest.ImportResult, error)
}

type importTranscriptInput struct {
	SourceTag types.SourceTag            `json:"source_tag" jsonschema:"Actual source platform, for example codex or deepseek_harness"`
	Name      string                     `json:"name" jsonschema:"Title for the saved excerpt"`
	Messages  []ingest.TranscriptMessage `json:"messages" jsonschema:"Selected original user and assistant messages in order, 1 to 50 messages and at most 64 KiB of text"`
}

type importTranscriptOutput struct {
	SessionID     uint   `json:"session_id"`
	MessageCount  int    `json:"message_count"`
	ExtractStatus string `json:"extract_status"`
}

// RegisterImportTranscript adds write access only where the caller opts in.
func RegisterImportTranscript(server *mcp.Server, importer transcriptImporter) {
	destructive := false
	mcp.AddTool(server, &mcp.Tool{
		Name: "import_transcript",
		Description: "Save a new excerpt in RecallWeave. Call only when the current user explicitly asks to save the selected messages. " +
			"Send original text and roles in order; never invent or silently summarize a message. " +
			"The session is saved even if extraction fails or is skipped. Each call creates a new session; do not retry automatically.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructive},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input importTranscriptInput) (*mcp.CallToolResult, importTranscriptOutput, error) {
		result, err := importer.ImportTranscript(ctx, input.SourceTag, input.Name, input.Messages)
		if err != nil {
			return nil, importTranscriptOutput{}, err
		}
		return nil, importTranscriptOutput{
			SessionID:     result.SessionID,
			MessageCount:  result.MessageCount,
			ExtractStatus: result.ExtractStatus,
		}, nil
	})
}
