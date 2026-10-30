package runtime

import (
	"encoding/json"
	"net/url"
	"sort"
)

// Source identifies retrieved material, not a model-authored citation or a
// claim that the material proves the answer. Only known read tools qualify.
type Source struct {
	SourceRunID string `json:"source_run_id,omitempty"`
	Kind        string `json:"kind"`
	Tool        string `json:"tool"`
	SessionID   uint   `json:"session_id,omitempty"`
	SeqStart    int    `json:"seq_start,omitempty"`
	SeqEnd      int    `json:"seq_end,omitempty"`
	URL         string `json:"url,omitempty"`
	Title       string `json:"title,omitempty"`
}

// SourcesForRun preserves where inherited material was actually fetched while
// leaving model-call accounting local to the new Run. Fresh sources have
// priority within the bounded result so old history cannot hide new evidence.
func SourcesForRun(state RunState) []Source {
	sources := make([]Source, 0)
	seen := make(map[Source]bool)
	add := func(source Source) {
		if !seen[source] && len(sources) < 32 {
			seen[source] = true
			sources = append(sources, source)
		}
	}
	for _, source := range SourcesFromSteps(state.Steps) {
		source.SourceRunID = state.RunID
		add(source)
	}
	for _, source := range state.InheritedSources {
		add(source)
	}
	return sources
}

func SourcesFromSteps(steps []RunStep) []Source {
	sources := make([]Source, 0)
	seen := make(map[Source]bool)
	add := func(source Source) {
		if !seen[source] && len(sources) < 32 {
			seen[source] = true
			sources = append(sources, source)
		}
	}
	for _, step := range steps {
		if step.Error != "" {
			continue
		}
		switch step.ToolName {
		case "get_messages":
			var result struct {
				SessionID uint `json:"session_id"`
				Messages  []struct {
					Seq     int    `json:"seq"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			if json.Unmarshal([]byte(step.Result), &result) != nil || result.SessionID == 0 {
				continue
			}
			seqs := make(map[int]bool)
			var ordered []int
			for _, message := range result.Messages {
				if message.Seq > 0 && message.Content != "" && !seqs[message.Seq] {
					seqs[message.Seq] = true
					ordered = append(ordered, message.Seq)
				}
			}
			sort.Ints(ordered)
			for i := 0; i < len(ordered); {
				// 连续的seq 合并成一个区间  [1,2,3,4,5] -> [1,5]
				start, end := ordered[i], ordered[i]
				i++
				for i < len(ordered) && ordered[i] == end+1 {
					end = ordered[i]
					i++
				}
				add(Source{Kind: "message", Tool: step.ToolName, SessionID: result.SessionID, SeqStart: start, SeqEnd: end})
			}
		case "mcp_tavily_search", "mcp_tavily_extract":
			var result struct {
				Results []struct {
					URL        string `json:"url"`
					Title      string `json:"title"`
					Content    string `json:"content"`
					RawContent string `json:"raw_content"`
				} `json:"results"`
			}
			if json.Unmarshal([]byte(step.Result), &result) != nil {
				continue
			}
			for _, item := range result.Results {
				parsed, err := url.Parse(item.URL)
				if err != nil || parsed.Hostname() == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || len(item.URL) > 2048 || (item.Content == "" && item.RawContent == "") {
					continue
				}
				add(Source{Kind: "web", Tool: step.ToolName, URL: item.URL, Title: boundedText(item.Title, 200)})
			}
		}
	}
	return sources
}
