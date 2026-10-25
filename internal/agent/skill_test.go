package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"recallweave/internal/skill"
)

func TestSkillsAreSeparateFromAbilities(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "memory-audit")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte("---\nname: memory-audit\ndescription: Check a memory against the source\n---\nDetailed audit instructions"), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := skill.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	memory := NewAbility("memory", "检索记忆", "先用 search_memories", nil)
	agent := NewAgent(1, "recall", nil, "记忆助手", 3, memory).WithSkills(catalog)
	if len(agent.Abilities) != 1 || agent.Abilities[0] != memory {
		t.Fatalf("skills must not become abilities: %+v", agent.Abilities)
	}
	prompt := agent.Instructions()
	if !strings.Contains(prompt, "memory-audit: Check a memory against the source") || strings.Contains(prompt, "Detailed audit instructions") {
		t.Fatalf("skill metadata/body disclosure is wrong: %q", prompt)
	}
	schemas := agent.Tools().Schemas()
	if len(schemas) != 2 || schemas[0].Name != "load_skill" || schemas[1].Name != "read_skill_reference" {
		t.Fatalf("unexpected built-in tools: %+v", schemas)
	}
	result, err := agent.Tools().Run(context.Background(), "load_skill", `{"name":"memory-audit"}`)
	if err != nil {
		t.Fatal(err)
	}
	var loaded struct {
		Name         string `json:"name"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal([]byte(result), &loaded); err != nil || loaded.Name != "memory-audit" || loaded.Instructions != "Detailed audit instructions" {
		t.Fatalf("unexpected load result: %+v err=%v", loaded, err)
	}
	if _, err := agent.Tools().Run(context.Background(), "load_skill", `{"name":"../other"}`); err == nil {
		t.Fatal("unknown skill must not load")
	}
}

func TestSkillReferenceRequiresLoadedSkillInSameRun(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "memory-audit")
	if err := os.MkdirAll(filepath.Join(directory, "references"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte("---\nname: memory-audit\ndescription: Check source\n---\nRead references/checklist.md when needed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "references", "checklist.md"), []byte("Check original messages."), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := skill.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	agent := NewAgent(1, "recall", nil, "", 3).WithSkills(catalog)
	tools := agent.Tools()
	args := `{"name":"memory-audit","path":"references/checklist.md"}`
	if _, err := tools.Run(context.Background(), "read_skill_reference", args); err == nil {
		t.Fatal("reference must not load before its skill")
	}
	if _, err := tools.Run(context.Background(), "load_skill", `{"name":"memory-audit"}`); err != nil {
		t.Fatal(err)
	}
	result, err := tools.Run(context.Background(), "read_skill_reference", args)
	if err != nil {
		t.Fatal(err)
	}
	var loaded struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(result), &loaded); err != nil || loaded.Content != "Check original messages." {
		t.Fatalf("unexpected reference: %q, %v", result, err)
	}
	if _, err := agent.Tools().Run(context.Background(), "read_skill_reference", args); err == nil {
		t.Fatal("loading a skill must not carry over into a new run")
	}
}

func TestNoSkillsDoesNotExposeLoader(t *testing.T) {
	agent := NewAgent(1, "recall", nil, "", 3)
	if len(agent.Tools().Schemas()) != 0 || strings.Contains(agent.Instructions(), "load_skill") {
		t.Fatal("agent without skills must not expose skill infrastructure")
	}
}
