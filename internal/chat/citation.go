package chat

import (
	"encoding/json"
	"sort"

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

// verifiedCitations 只收被 get_messages 核对过的记忆。
// 引用范围只包含工具实际返回的消息，不使用模型请求的范围推断原文。
func verifiedCitations(steps []runtime.RunStep) []Citation {
	// step的号称的用到的session的 那些seq号的msg
	verified := verifiedMessages(steps)
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
			// 将号称的seq区间 以 真实的工具的返回seq为依据 得到真实的seq区间
			spans := citedSpans(verified[hit.SessionID], hit.SeqStart, hit.SeqEnd)
			if len(spans) == 0 {
				continue
			}
			cited[hit.MemoryID] = struct{}{}
			for _, span := range spans {
				citations = append(citations, Citation{
					MemoryID: hit.MemoryID, SessionID: hit.SessionID,
					SeqStart: span[0], SeqEnd: span[1], Title: hit.Title,
				})
			}
		}
	}
	return citations
}

func verifiedMessages(steps []runtime.RunStep) map[uint]map[int]struct{} {
	verified := make(map[uint]map[int]struct{})
	for _, step := range steps {
		if step.ToolName != ability.GetMessagesToolName || step.Error != "" {
			continue
		}
		// 解析res的数据
		var result struct {
			SessionID uint `json:"session_id"`
			Messages  []struct {
				Seq     int    `json:"seq"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(step.Result), &result); err != nil || result.SessionID == 0 {
			continue
		}
		for _, msg := range result.Messages {
			if msg.Seq <= 0 || msg.Content == "" {
				continue
			}
			if verified[result.SessionID] == nil {
				verified[result.SessionID] = make(map[int]struct{})
			}
			verified[result.SessionID][msg.Seq] = struct{}{}
		}
	}
	return verified
}

func citedSpans(seqset map[int]struct{}, start, end int) [][2]int {
	var seqs []int
	for seq := range seqset {
		if seq >= start && seq <= end {
			seqs = append(seqs, seq)
		}
	}
	sort.Ints(seqs)
	var spans [][2]int
	for _, seq := range seqs {
		// 大于前一个区间的右边界+1，则新增一个区间
		if len(spans) == 0 || seq > spans[len(spans)-1][1]+1 {
			// [2,3] ----> [2,3] [5,5]
			spans = append(spans, [2]int{seq, seq})
		} else {
			// 刚好是当前最后seq的下一个 则拓展右边区间 [2,2]--->[2,3]
			// 拓展右边区间 刚好是当前最后seq的下一个
			spans[len(spans)-1][1] = seq
		}
	}
	return spans
}
