package runtime

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"recallweave/internal/agent"
	"recallweave/internal/llm"
)

type catalogDelegator struct {
	runtimeTestDelegator
	items       []DelegationAgent
	defaultName string
}

func (d catalogDelegator) AvailableAgents() []DelegationAgent { return d.items }
func (d catalogDelegator) DefaultAgent() string               { return d.defaultName }

func TestDelegationCatalogRoutesRegisteredAgentAndKeepsDefault(t *testing.T) {
	for _, selection := range []string{"", "evidence_checker"} {
		t.Run("agent="+selection, func(t *testing.T) {
			encoded, _ := json.Marshal(DelegationRequest{Agent: selection, Task: "check sources"})
			model := &delegationScriptModel{turns: []llm.Turn{delegationCall(string(encoded)), {Content: "done"}}}
			wanted := selection
			if wanted == "" {
				wanted = "researcher"
			}
			delegator := catalogDelegator{
				items:       []DelegationAgent{{Name: "researcher", Description: "Find relevant material"}, {Name: "evidence_checker", Description: "Check supplied claims"}},
				defaultName: "researcher",
				runtimeTestDelegator: func(_ context.Context, parent DelegationParent, request DelegationRequest) (RunResult, error) {
					if request.Agent != wanted {
						t.Fatalf("selected agent=%q, want %q", request.Agent, wanted)
					}
					child := delegationChild(parent, RunCompleted)
					child.State.AgentName = request.Agent
					return child, nil
				},
			}
			result, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 3), Request{Input: "check", Delegator: delegator})
			if err != nil {
				t.Fatal(err)
			}
			var toolSchemaFound bool
			for _, schema := range model.schemas[0] {
				if schema.Name != DelegateTaskToolName {
					continue
				}
				toolSchemaFound = true
				properties := schema.Parameters["properties"].(map[string]any)
				selector := properties["agent"].(map[string]any)
				if !reflect.DeepEqual(selector["enum"], []string{"researcher", "evidence_checker"}) || selector["default"] != "researcher" {
					t.Fatalf("wrong model-visible choices: %+v", selector)
				}
				if !strings.Contains(schema.Description, "Check supplied claims") {
					t.Fatal("routing description missing")
				}
			}
			if !toolSchemaFound {
				t.Fatal("delegation schema missing")
			}
			var reply DelegationResult
			if err := json.Unmarshal([]byte(result.State.Steps[0].Result), &reply); err != nil {
				t.Fatal(err)
			}
			if reply.Agent != wanted || result.State.Children[0].State.AgentName != wanted {
				t.Fatalf("selected preset attribution missing: %+v", reply)
			}
		})
	}
}

func TestUnknownPresetCanBeCorrectedWithoutSpendingDelegation(t *testing.T) {
	turn := delegationCall(`{"agent":"invented","task":"check"}`)
	turn.ToolCalls = append(turn.ToolCalls, delegationCall(`{"agent":"reviewer","task":"check"}`).ToolCalls...)
	model := &delegationScriptModel{turns: []llm.Turn{turn, {Content: "done"}}}
	starts := 0
	delegator := catalogDelegator{
		items: []DelegationAgent{{Name: "reviewer", Description: "Review claims"}}, defaultName: "reviewer",
		runtimeTestDelegator: func(_ context.Context, parent DelegationParent, request DelegationRequest) (RunResult, error) {
			starts++
			if request.Agent != "reviewer" {
				t.Fatal("unknown preset reached manager")
			}
			return delegationChild(parent, RunCompleted), nil
		},
	}
	result, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "check", Delegator: delegator})
	if err != nil {
		t.Fatal(err)
	}
	if starts != 1 || len(result.State.Children) != 1 || len(result.State.Steps) != 2 || !strings.Contains(result.State.Steps[0].Error, "unknown agent") || result.State.Steps[1].Error != "" {
		t.Fatalf("selection did not recover: starts=%d steps=%+v", starts, result.State.Steps)
	}
}

func TestChangingPresetDoesNotResetDelegationLimit(t *testing.T) {
	turn := delegationCall(`{"agent":"researcher","task":"research"}`)
	turn.ToolCalls = append(turn.ToolCalls, delegationCall(`{"agent":"reviewer","task":"review"}`).ToolCalls...)
	model := &delegationScriptModel{turns: []llm.Turn{turn, {Content: "done"}}}
	starts := 0
	delegator := catalogDelegator{items: []DelegationAgent{{Name: "researcher"}, {Name: "reviewer"}}, defaultName: "researcher", runtimeTestDelegator: func(_ context.Context, parent DelegationParent, _ DelegationRequest) (RunResult, error) {
		starts++
		return delegationChild(parent, RunCompleted), nil
	}}
	result, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "check", Delegator: delegator})
	if err != nil {
		t.Fatal(err)
	}
	if starts != 1 || len(result.State.Children) != 1 || !strings.Contains(result.State.Steps[1].Error, "one delegation") {
		t.Fatalf("changing role started extra child: starts=%d state=%+v", starts, result.State)
	}
}

func TestInvalidCatalogFailsBeforeModelRequest(t *testing.T) {
	for _, test := range []struct {
		name        string
		items       []DelegationAgent
		defaultName string
	}{
		{"empty", nil, "researcher"},
		{"missing_default", []DelegationAgent{{Name: "reviewer"}}, "researcher"},
		{"duplicate", []DelegationAgent{{Name: "researcher"}, {Name: "researcher"}}, "researcher"},
		{"empty_name", []DelegationAgent{{Name: ""}}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := &delegationScriptModel{}
			delegator := catalogDelegator{items: test.items, defaultName: test.defaultName}
			result, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "check", Delegator: delegator})
			if err == nil || model.calls != 0 || len(result.State.Calls) != 0 || result.State.Status != RunFailed {
				t.Fatalf("invalid catalog reached model: err=%v state=%+v", err, result.State)
			}
		})
	}
}
