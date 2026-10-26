package ability

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"recallweave/internal/agent"
	"recallweave/internal/artifact"
)

func TestSaveArtifactTool(t *testing.T) {
	dir := t.TempDir()
	store, err := artifact.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a := agent.NewAgent(1, "test", nil, "", 3, NewArtifactAbility(store))
	tools := a.Tools()
	if len(tools.Schemas()) != 1 || tools.Schemas()[0].Name != SaveArtifactToolName {
		t.Fatalf("save tool not registered: %+v", tools.Schemas())
	}
	for _, args := range []string{
		`{"title":"test","markdown":"body","path":"../outside.md"}`,
		`{"title":"test","markdown":"body"} {}`,
		`{"title":"test"}`, `null`, `not-json`,
	} {
		if _, err := tools.Run(context.Background(), SaveArtifactToolName, args); err == nil {
			t.Fatalf("invalid arguments accepted: %s", args)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid arguments created files: %v, %v", entries, err)
	}
	result, err := tools.Run(context.Background(), SaveArtifactToolName, `{"title":"偏好","markdown":"# 偏好\n来源：会话 1，消息 23。"}`)
	if err != nil {
		t.Fatal(err)
	}
	var saved artifact.Saved
	if err := json.Unmarshal([]byte(result), &saved); err != nil || saved.ID == "" || filepath.Dir(saved.Path) != dir {
		t.Fatalf("invalid save result: %s, %v", result, err)
	}
	data, err := os.ReadFile(saved.Path)
	if err != nil || string(data) != "# 偏好\n来源：会话 1，消息 23。" {
		t.Fatalf("saved content mismatch: %q, %v", data, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.Run(context.Background(), SaveArtifactToolName, `{"title":"失败","markdown":"正文"}`); err == nil {
		t.Fatal("closed store must not report successful save")
	}
}
