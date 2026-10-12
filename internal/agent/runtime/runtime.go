// Package runtime 执行 agent：组装对话 → 调模型 → 执行工具 → 回灌结果，直到模型给出回答。
package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"recallweave/internal/agent"
	"recallweave/internal/llm"
)

func Run(ctx context.Context, a *agent.Agent, req Request) (RunResult, error) {
	if a == nil || a.Model == nil {
		return RunResult{}, errors.New("runtime: agent model is nil")
	}
	if strings.TrimSpace(req.Input) == "" {
		return RunResult{}, errors.New("runtime: input is empty")
	}

	runID, err := newRunID()
	if err != nil {
		return RunResult{}, fmt.Errorf("runtime: generate run id: %w", err)
	}
	state := RunState{
		RunID:          runID,
		AgentID:        a.ID,
		ConversationID: req.ConversationID,
		Input:          req.Input,
		Status:         RunRunning,
		StartedAt:      time.Now(),
	}

	finish := func(status RunStatus) {
		finishedAt := time.Now()
		state.Status = status
		state.FinishedAt = &finishedAt
	}
	fail := func(round int, err error, status RunStatus) (RunResult, error) {
		state.Errors = append(state.Errors, RunError{
			Round:   round,
			Message: err.Error(),
			Fatal:   true,
		})
		finish(status)
		return RunResult{State: state}, err
	}

	tools := a.Tools()
	maxRounds := a.Rounds()

	systemPrompt := strings.TrimSpace(a.Instructions() + "\n\n" + req.SystemPrompt)
	conversation := llm.NewConversation(systemPrompt, llm.WithConversationID(req.ConversationID))
	applyPreload(conversation, req.Preload)
	conversation.AddUserMessage(req.Input)

	for round := 1; round <= maxRounds; round++ {
		if err := ctx.Err(); err != nil {
			return fail(round, err, RunCancelled)
		}
		state.CurrentRound = round

		turn, err := a.Model.Next(ctx, conversation, tools.Schemas())
		if err != nil {
			wrapped := fmt.Errorf("runtime: chat generation failed at round %d: %w", round, err)
			status := RunFailed
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				status = RunCancelled
			}
			return fail(round, wrapped, status)
		}

		if len(turn.ToolCalls) == 0 {
			if strings.TrimSpace(turn.Content) == "" {
				return fail(round, errors.New("runtime: model returned empty response"), RunFailed)
			}
			finish(RunCompleted)
			return RunResult{Content: turn.Content, State: state}, nil
		}

		for _, call := range turn.ToolCalls {
			step := RunStep{
				Round:     round,
				ToolName:  call.Name,
				Arguments: call.Arguments,
			}

			output, err := tools.Run(ctx, call.Name, call.Arguments)
			if err != nil {
				// 工具失败不中断，把原因回灌给模型，让它换个问法或换个工具。
				step.Error = err.Error()
				state.Steps = append(state.Steps, step)
				state.Errors = append(state.Errors, RunError{
					Round:    round,
					ToolName: call.Name,
					Message:  err.Error(),
				})
				conversation.AddToolResult(call.ID, encodeToolError(err))
				continue
			}

			step.Result = output
			state.Steps = append(state.Steps, step)
			conversation.AddToolResult(call.ID, output)
		}
	}

	return fail(maxRounds, fmt.Errorf("runtime: maximum tool rounds reached: %d", maxRounds), RunFailed)
}

func newRunID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

func encodeToolError(err error) string {
	encoded, marshalErr := json.Marshal(map[string]string{"error": err.Error()})
	if marshalErr != nil {
		return `{"error":"tool execution failed"}`
	}
	return string(encoded)
}
