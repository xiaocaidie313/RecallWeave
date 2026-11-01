package runtime

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"recallweave/internal/agent"
	"recallweave/internal/delivery"
	"recallweave/internal/llm"
)

func TestDeliveryModeSchemasOnlyAdvertiseAvailableBackgroundModes(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		model := &delegationScriptModel{turns: []llm.Turn{{Content: "done"}}}
		d := groupTestDelegator{enabled: true, backgroundTestDelegator: backgroundTestDelegator{
			backgroundEnabled: enabled, continuationTestDelegator: continuationTestDelegator{enabled: true},
		}}
		_, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "hi", Delegator: d})
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		for _, schema := range model.schemas[0] {
			var modes []string
			switch schema.Name {
			case DelegateTaskToolName, FollowupTaskToolName:
				if enabled {
					modes = []string{delivery.Direct, delivery.Synthesize}
				}
			case DelegateTaskGroupToolName:
				modes = []string{delivery.Direct, delivery.Combined, delivery.Synthesize}
			default:
				continue
			}
			seen++
			properties := schema.Parameters["properties"].(map[string]any)
			field, exists := properties["delivery_mode"]
			if len(modes) == 0 {
				if exists {
					t.Fatalf("%s exposes background delivery while disabled", schema.Name)
				}
				continue
			}
			if !exists {
				t.Fatalf("%s missing delivery choice", schema.Name)
			}
			mode := field.(map[string]any)
			if mode["default"] != delivery.Direct || !reflect.DeepEqual(mode["enum"], modes) {
				t.Fatalf("%s delivery contract = %+v", schema.Name, mode)
			}
		}
		if seen != 3 {
			t.Fatalf("expected delegation, follow-up and group schemas, saw %d", seen)
		}
	}
}

func TestBackgroundDeliveryModesAreNormalizedAndForwarded(t *testing.T) {
	for _, followup := range []bool{false, true} {
		for _, mode := range []string{"", delivery.Direct, delivery.Synthesize} {
			t.Run(map[bool]string{false: "delegate", true: "followup"}[followup]+"/"+mode, func(t *testing.T) {
				want := mode
				if want == "" {
					want = delivery.Direct
				}
				starts := 0
				receipt := func(parent DelegationParent, got string) (BackgroundTask, error) {
					starts++
					if got != want {
						t.Fatalf("mode not forwarded: got %q want %q", got, want)
					}
					task := backgroundReceipt(parent)
					task.DeliveryMode = got
					return task, nil
				}
				d := backgroundTestDelegator{backgroundEnabled: true,
					continuationTestDelegator: continuationTestDelegator{enabled: true},
					submit: func(_ context.Context, parent DelegationParent, request DelegationRequest) (BackgroundTask, error) {
						return receipt(parent, request.DeliveryMode)
					},
					submitFollowup: func(_ context.Context, parent DelegationParent, request FollowupRequest) (BackgroundTask, error) {
						return receipt(parent, request.DeliveryMode)
					},
				}
				field := ""
				if mode != "" {
					field = `,"delivery_mode":"` + mode + `"`
				}
				turn := delegationCall(`{"task":"research","background":true` + field + `}`)
				if followup {
					turn = continuationCall(FollowupTaskToolName, `{"subagent_id":"`+testSubagentID+`","message":"check again","background":true`+field+`}`)
				}
				model := &delegationScriptModel{turns: []llm.Turn{turn, {Content: "started"}}}
				result, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "start", Delegator: d})
				if err != nil {
					t.Fatal(err)
				}
				var reply BackgroundTask
				if err := json.Unmarshal([]byte(result.State.Steps[0].Result), &reply); err != nil || reply.DeliveryMode != want || starts != 1 || len(result.State.Children) != 0 || len(result.State.AllCalls()) != 2 {
					t.Fatalf("delivery receipt or accounting changed: %+v starts=%d err=%v", reply, starts, err)
				}
			})
		}
	}
}

func TestGroupDeliveryModesAreNormalizedAndForwarded(t *testing.T) {
	for _, mode := range []string{"", delivery.Direct, delivery.Combined, delivery.Synthesize} {
		t.Run(mode, func(t *testing.T) {
			want := mode
			if want == "" {
				want = delivery.Direct
			}
			arguments := validGroupArgs
			if mode != "" {
				arguments = strings.TrimSuffix(arguments, "}") + `,"delivery_mode":"` + mode + `"}`
			}
			d := groupTestDelegator{enabled: true, groupSubmit: func(_ context.Context, parent DelegationParent, request GroupDelegationRequest) (TaskGroup, error) {
				if request.DeliveryMode != want || len(request.Tasks) != 2 {
					t.Fatalf("group request changed: %+v", request)
				}
				group := groupReceipt(parent)
				group.DeliveryMode = request.DeliveryMode
				return group, nil
			}}
			model := &delegationScriptModel{turns: []llm.Turn{groupCall(arguments), {Content: "started"}}}
			result, err := Run(context.Background(), agent.NewAgent(1, "main", model, "", 2), Request{Input: "start", Delegator: d})
			if err != nil {
				t.Fatal(err)
			}
			var reply TaskGroup
			if err := json.Unmarshal([]byte(result.State.Steps[0].Result), &reply); err != nil || reply.DeliveryMode != want || len(result.State.Children) != 0 || len(result.State.AllCalls()) != 2 {
				t.Fatalf("group delivery receipt or accounting changed: %+v err=%v", reply, err)
			}
		})
	}
}

func TestInvalidDeliveryModeIsRejectedBeforeSubmissionAndCanBeCorrected(t *testing.T) {
	state := &RunState{RunID: "parent", ConversationID: 7}
	budget := &delegationBudget{}
	starts, follows := 0, 0
	d := groupTestDelegator{enabled: true, backgroundTestDelegator: backgroundTestDelegator{
		backgroundEnabled: true, continuationTestDelegator: continuationTestDelegator{enabled: true},
		submit: func(_ context.Context, parent DelegationParent, _ DelegationRequest) (BackgroundTask, error) {
			starts++
			return backgroundReceipt(parent), nil
		},
		submitFollowup: func(_ context.Context, parent DelegationParent, _ FollowupRequest) (BackgroundTask, error) {
			follows++
			return backgroundReceipt(parent), nil
		},
	}, groupSubmit: func(_ context.Context, parent DelegationParent, _ GroupDelegationRequest) (TaskGroup, error) {
		starts++
		return groupReceipt(parent), nil
	}}
	single, err := delegationTool(state, nil, d, budget)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := taskGroupTools(state, nil, d, d, budget)
	if err != nil {
		t.Fatal(err)
	}
	followup := continuationTools(state, nil, d)[0]
	invalidGroup := strings.TrimSuffix(validGroupArgs, "}") + `,"delivery_mode":"automatic"}`
	for _, test := range []struct {
		tool llm.Tool
		args string
	}{
		{single, `{"task":"research","background":true,"delivery_mode":"combined"}`},
		{groups[0], invalidGroup},
		{followup, `{"subagent_id":"` + testSubagentID + `","message":"check","background":true,"delivery_mode":"automatic"}`},
	} {
		if _, err := test.tool.Run(context.Background(), test.args); err == nil || !strings.Contains(err.Error(), "mode must be") {
			t.Fatalf("%s accepted invalid mode: %v", test.tool.Schema.Name, err)
		}
	}
	if starts != 0 || follows != 0 || budget.started {
		t.Fatalf("invalid mode reached backend or consumed budget: starts=%d follows=%d budget=%+v", starts, follows, budget)
	}
	if _, err := groups[0].Run(context.Background(), validGroupArgs); err != nil {
		t.Fatalf("corrected group rejected: %v", err)
	}
	if _, err := single.Run(context.Background(), `{"task":"research","background":true}`); err == nil || !strings.Contains(err.Error(), "one delegation") {
		t.Fatalf("group/single budget bypass: %v", err)
	}
	if _, err := followup.Run(context.Background(), `{"subagent_id":"`+testSubagentID+`","message":"check","background":true,"delivery_mode":"synthesize"}`); err != nil || starts != 1 || follows != 1 {
		t.Fatalf("corrected follow-up lost independent budget: starts=%d follows=%d err=%v", starts, follows, err)
	}
}
