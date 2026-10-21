package ask

import (
	"encoding/json"

	"recallweave/internal/agent/ability"
	"recallweave/internal/agent/runtime"
)

// Citation 是一条回答依据，指回某条记忆和它在原文里的位置。
type Citation struct {
	MemoryID  uint   `json:"memory_id"`
	SessionID uint   `json:"session_id"`
	SeqStart  int    `json:"seq_start"`
	SeqEnd    int    `json:"seq_end"`
	Title     string `json:"title"`
}

type messageSpan struct {
	SessionID uint
	SeqStart  int
	SeqEnd    int
}

// citationsFromSteps 只收被 get_messages 核对过的记忆。
// 检索命中只是候选，模型真正打开过原文的才算依据。
func citationsFromSteps(steps []runtime.RunStep) []Citation {
	verified := verifiedSpans(steps)
	if len(verified) == 0 {
		return nil
	}

	var citations []Citation
	cited := make(map[uint]struct{})
	for _, step := range steps {
		if step.ToolName != ability.SearchMemoriesToolName || step.Error != "" {
			continue
		}
		var parsed ability.SearchMemoriesResult
		if err := json.Unmarshal([]byte(step.Result), &parsed); err != nil {
			continue
		}
		for _, hit := range parsed.Memories {
			if _, seen := cited[hit.MemoryID]; seen {
				continue
			}
			if !spanOverlaps(verified, hit.SessionID, hit.SeqStart, hit.SeqEnd) {
				continue
			}
			cited[hit.MemoryID] = struct{}{}
			citations = append(citations, Citation{
				MemoryID:  hit.MemoryID,
				SessionID: hit.SessionID,
				SeqStart:  hit.SeqStart,
				SeqEnd:    hit.SeqEnd,
				Title:     hit.Title,
			})
		}
	}
	return citations
}

func verifiedSpans(steps []runtime.RunStep) []messageSpan {
	var spans []messageSpan
	for _, step := range steps {
		if step.ToolName != ability.GetMessagesToolName || step.Error != "" {
			continue
		}
		var args struct {
			SessionID uint `json:"session_id"`
			SeqStart  int  `json:"seq_start"`
			SeqEnd    int  `json:"seq_end"`
		}
		if err := json.Unmarshal([]byte(step.Arguments), &args); err != nil {
			continue
		}
		if args.SessionID == 0 {
			continue
		}
		spans = append(spans, messageSpan{
			SessionID: args.SessionID,
			SeqStart:  args.SeqStart,
			SeqEnd:    args.SeqEnd,
		})
	}
	return spans
}

func spanOverlaps(spans []messageSpan, sessionID uint, seqStart, seqEnd int) bool {
	for _, span := range spans {
		if span.SessionID != sessionID {
			continue
		}
		if span.SeqStart <= seqEnd && span.SeqEnd >= seqStart {
			return true
		}
	}
	return false
}
