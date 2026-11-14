package mcp

import (
	"context"

	"recallweave/internal/ingest"
	"recallweave/internal/types"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type transcriptImporter interface {
	ImportTranscript(context.Context, types.SourceTag, string, []ingest.TranscriptMessage, ...ingest.TranscriptOptions) (ingest.ImportResult, error)
}

type importTranscriptInput struct {
	SourceTag         types.SourceTag            `json:"source_tag" jsonschema:"Actual source platform, for example codex or deepseek_harness"`
	Name              string                     `json:"name" jsonschema:"Title for the saved excerpt"`
	ExternalSessionID string                     `json:"external_session_id,omitempty" jsonschema:"Stable ID for this source conversation. Reuse it to append messages and safely retry the same batch. Omit for a new independent excerpt."`
	SeqStart          int                        `json:"seq_start,omitempty" jsonschema:"1-based message number of the first supplied message within the saved transcript. Defaults to 1. Requires external_session_id when greater than 1."`
	Messages          []ingest.TranscriptMessage `json:"messages" jsonschema:"Selected original user and assistant messages in order, 1 to 50 messages and at most 64 KiB of text"`
}

type importTranscriptOutput struct {
	SessionID     uint   `json:"session_id"`
	MessageCount  int    `json:"message_count"`
	AddedMessages int    `json:"added_messages"`
	ExtractStatus string `json:"extract_status"`
}

// RegisterImportTranscript adds write access only where the caller opts in.
func RegisterImportTranscript(server *mcp.Server, importer transcriptImporter) {
	destructive := false
	mcp.AddTool(server, &mcp.Tool{
		Name: "import_transcript",
		Description: "Save a new excerpt in RecallWeave. Call only when the current user explicitly asks to save the selected messages. " +
			"Send original text and roles in order; never invent or silently summarize a message. " +
			"Supply a stable external_session_id to sync one conversation. Existing messages must match exactly; only new messages are appended. " +
			"The session is saved even if extraction fails or is skipped. Repeating the same identified batch is safe and retries unfinished extraction. " +
			"Omitting external_session_id creates a new session on each call.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructive},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input importTranscriptInput) (*mcp.CallToolResult, importTranscriptOutput, error) {
		result, err := importer.ImportTranscript(ctx, input.SourceTag, input.Name, input.Messages, ingest.TranscriptOptions{
			ExternalSessionID: input.ExternalSessionID, SeqStart: input.SeqStart,
		})
		if err != nil {
			return nil, importTranscriptOutput{}, err
		}
		return nil, importTranscriptOutput{
			SessionID:     result.SessionID,
			MessageCount:  result.MessageCount,
			AddedMessages: result.AddedMessages,
			ExtractStatus: result.ExtractStatus,
		}, nil
	})
}
