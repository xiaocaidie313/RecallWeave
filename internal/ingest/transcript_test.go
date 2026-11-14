package ingest

import (
	"context"
	"math"
	"strings"
	"testing"

	"recallweave/internal/types"
)

func TestTranscriptSyncRejectsInvalidOptionsBeforeWriting(t *testing.T) {
	for _, opt := range []TranscriptOptions{
		{SeqStart: -1},
		{SeqStart: 2},
		{ExternalSessionID: "  "},
		{ExternalSessionID: strings.Repeat("x", 192)},
		{ExternalSessionID: "thread", SeqStart: math.MaxInt},
	} {
		_, err := NewService(nil, nil).ImportTranscript(context.Background(), types.SourceCodex, "title",
			[]TranscriptMessage{{Role: "user", Content: "original"}}, opt)
		if err == nil {
			t.Fatalf("invalid options accepted: %+v", opt)
		}
	}
}
