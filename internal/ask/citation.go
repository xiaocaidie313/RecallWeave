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

// citationsFromSteps 从这次运行的检索结果里收集来源。
// 模型往往会换关键词多搜几轮，同一条记忆会重复命中，这里按 id 去重。
func citationsFromSteps(steps []runtime.RunStep) []Citation {
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
