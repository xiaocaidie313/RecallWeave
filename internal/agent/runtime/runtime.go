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
	"recallweave/internal/plan"
	"recallweave/internal/watch/useage"
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
		RunID:            runID,
		ParentRunID:      req.ParentRunID,
		SubagentID:       req.SubagentID,
		PreviousRunID:    req.PreviousRunID,
		AgentID:          a.ID,
		AgentName:        a.Name,
		ConversationID:   req.ConversationID,
		Input:            req.Input,
		Status:           RunRunning,
		StartedAt:        time.Now(),
		InheritedSources: append([]Source(nil), req.InheritedSources...),
	}

	var conversation *llm.Conversation
	finish := func(status RunStatus) {
		finishedAt := time.Now()
		state.Status = status
		state.FinishedAt = &finishedAt
		if req.CaptureContext && conversation != nil {
			// 保存对话切片
			snapshot, err := conversation.Snapshot()
			if err != nil {
				state.ContextSnapshot = nil
				state.ContextError = err.Error()
			} else {
				state.ContextSnapshot = snapshot
			}
		}
	}
	fail := func(round int, err error, status RunStatus) (RunResult, error) {
		state.Errors = append(state.Errors, RunError{
			Round:   round,
			Message: err.Error(),
			Fatal:   true,
		})
		// 保存对话切片
		finish(status)
		return RunResult{State: state}, err
	}

	tools := a.Tools()
	for _, schema := range tools.Schemas() {
		if schema.Name == plan.ToolName || schema.Name == DelegateTaskToolName || schema.Name == FollowupTaskToolName || schema.Name == ListSubagentsToolName || schema.Name == GetSubagentTaskToolName || schema.Name == CancelSubagentTaskToolName {
			return fail(0, fmt.Errorf("runtime: tool name %q is reserved for runtime", schema.Name), RunFailed)
		}
	}
	tools.Register(planTool(&state))
	if req.Delegator != nil && req.ParentRunID == "" {
		tool, err := delegationTool(&state, a, req.Delegator)
		if err != nil {
			return fail(0, err, RunFailed)
		}
		tools.Register(tool)
		req.SystemPrompt += "\n" + delegationInstructions
		if background, ok := enabledBackground(req.Delegator); ok {
			for _, tool := range backgroundTools(&state, a, background) {
				tools.Register(tool)
			}
			req.SystemPrompt += "\n" + backgroundInstructions
		}
		// 如果可以续期 注册续期工具  判断是不是 continuationDelegator 接口
		if continuation, ok := req.Delegator.(ContinuationDelegator); ok && continuation.ContinuationsEnabled() {
			for _, tool := range continuationTools(&state, a, continuation) {
				tools.Register(tool)
			}
			req.SystemPrompt += "\n" + continuationInstructions
		}
	}
	maxRounds := a.Rounds()
	if req.ContextSnapshot == nil {
		conversation = buildConversation(a, req)
	} else {
		// 如果有切片 说明是续期 复活原对话
		conversation, err = resumeConversation(a, req)
		if err != nil {
			state.ContextError = err.Error()
			return fail(0, fmt.Errorf("runtime: restore context: %w", err), RunFailed)
		}
	}

	for round := 1; round <= maxRounds; round++ {
		if err := ctx.Err(); err != nil {
			return fail(round, err, RunCancelled)
		}
		state.CurrentRound = round

		started := time.Now()
		turn, err := a.Model.Next(ctx, conversation, tools.Schemas())
		call := useage.ModelCallStats{
			RunID:     state.RunID,
			Model:     modelName(a.Model),
			Round:     round,
			Usage:     useage.FromLLM(turn.Usage),
			LatencyMs: time.Since(started).Milliseconds(),
		}
		state.Calls = append(state.Calls, call)
		useage.Record(call)
		if err != nil {
			wrapped := fmt.Errorf("runtime: chat generation failed at round %d: %w", round, err)
			status := RunFailed
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				status = RunCancelled
			}
			return fail(round, wrapped, status)
		}
		if err := ctx.Err(); err != nil {
			return fail(round, err, RunCancelled)
		}

		if len(turn.ToolCalls) == 0 {
			if strings.TrimSpace(turn.Content) == "" {
				return fail(round, errors.New("runtime: model returned empty response"), RunFailed)
			}
			if state.Plan != nil && state.Plan.HasOpenSteps() {
				if round == maxRounds {
					return fail(round, errors.New("runtime: cannot finish with pending or in_progress plan steps"), RunFailed)
				}
				conversation.AddSystemMessage("计划仍有 pending 或 in_progress 步骤。继续执行并更新计划；若无法继续，将对应步骤标为 blocked/skipped 并填写原因，然后给出包含未完成部分的回答。")
				continue
			}
			status := RunCompleted
			if state.Plan != nil && state.Plan.HasUncompletedSteps() {
				status = RunPartial
			}
			finish(status)
			return RunResult{Content: turn.Content, State: state}, nil
		}

		for _, call := range turn.ToolCalls {
			if err := ctx.Err(); err != nil {
				return fail(round, err, RunCancelled)
			}
			step := RunStep{
				Round:     round,
				ToolName:  call.Name,
				Arguments: call.Arguments,
			}
			if state.Plan != nil && call.Name != plan.ToolName {
				step.PlanStep = state.Plan.ActiveStep()
			}

			toolStarted := time.Now()
			output, err := tools.Run(ctx, call.Name, call.Arguments)
			step.LatencyMs = time.Since(toolStarted).Milliseconds()
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
		if err := ctx.Err(); err != nil {
			return fail(round, err, RunCancelled)
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

func modelName(model llm.ChatGenerator) string {
	if named, ok := model.(interface{ ModelName() string }); ok {
		return named.ModelName()
	}
	return ""
}

func encodeToolError(err error) string {
	encoded, marshalErr := json.Marshal(map[string]string{"error": err.Error()})
	if marshalErr != nil {
		return `{"error":"tool execution failed"}`
	}
	return string(encoded)
}
