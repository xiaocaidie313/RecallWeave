package main

import (
	"errors"
	"testing"
	"time"

	"recallweave/internal/agent/runtime"
	"recallweave/internal/watch/useage"
)

func TestEvaluateSkillSelectionAndCost(t *testing.T) {
	spec := cases[2]
	result := runtime.RunResult{State: runtime.RunState{
		Status:       runtime.RunCompleted,
		CurrentRound: 3,
		Calls: []useage.ModelCallStats{{Usage: useage.TokenUsage{
			PromptTokens: 100, CompletionTokens: 20,
		}, LatencyMs: 1200}},
		Steps: []runtime.RunStep{
			{ToolName: "load_skill", Arguments: `{"name":"memory-audit"}`},
			{ToolName: "read_skill_reference", Arguments: `{"name":"memory-audit","path":"references/evidence-checklist.md"}`},
		},
	}}
	row := evaluate(spec, result, nil, 1300*time.Millisecond)
	if !row.Pass || row.PromptTokens != 100 || row.CompletionTokens != 20 || row.ModelLatencyMs != 1200 || row.TotalMs != 1300 {
		t.Fatalf("unexpected successful evaluation: %+v", row)
	}
	result.State.Steps[0], result.State.Steps[1] = result.State.Steps[1], result.State.Steps[0]
	if row := evaluate(spec, result, nil, time.Second); row.Pass {
		t.Fatal("reference before Skill should fail acceptance")
	}
	result.State.Steps[0], result.State.Steps[1] = result.State.Steps[1], result.State.Steps[0]
	if row := evaluate(spec, result, errors.New("model failed"), time.Second); row.Pass {
		t.Fatal("model error should fail acceptance")
	}
}

func TestEvaluateOrdinaryQuestionDoesNotLoadSkill(t *testing.T) {
	result := runtime.RunResult{State: runtime.RunState{Status: runtime.RunCompleted, CurrentRound: 1}}
	if row := evaluate(cases[0], result, nil, time.Second); !row.Pass {
		t.Fatalf("ordinary question should pass without Skill: %+v", row)
	}
	result.State.Steps = []runtime.RunStep{{ToolName: "load_skill", Arguments: `{"name":"memory-audit"}`}}
	if row := evaluate(cases[0], result, nil, time.Second); row.Pass {
		t.Fatal("ordinary question must not load Skill")
	}
}
