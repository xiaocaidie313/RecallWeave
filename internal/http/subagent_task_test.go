package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"recallweave/internal/agent/runtime"
	"recallweave/internal/agent/subagent"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const httpTestTaskID = "0123456789abcdef0123456789abcdef"

type fakeSubagentTaskService struct {
	get    func(context.Context, runtime.DelegationParent, string) (runtime.BackgroundTask, error)
	list   func(context.Context, uint) ([]runtime.BackgroundTask, error)
	cancel func(context.Context, runtime.DelegationParent, string) (runtime.BackgroundTask, error)
}

func (f fakeSubagentTaskService) GetTask(ctx context.Context, owner runtime.DelegationParent, id string) (runtime.BackgroundTask, error) {
	return f.get(ctx, owner, id)
}
func (f fakeSubagentTaskService) ListTasks(ctx context.Context, owner uint) ([]runtime.BackgroundTask, error) {
	return f.list(ctx, owner)
}
func (f fakeSubagentTaskService) CancelTask(ctx context.Context, owner runtime.DelegationParent, id string) (runtime.BackgroundTask, error) {
	return f.cancel(ctx, owner, id)
}

func subagentTaskRouter(service SubagentTaskService) *gin.Engine {
	router := gin.New()
	MountSubagentTasks(router, NewSubagentTaskHandler(service))
	return router
}
func requestSubagentTask(router *gin.Engine, method, path string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(method, path, nil))
	return response
}

func TestSubagentTaskGetUsesPathOwnerAndReturnsResult(t *testing.T) {
	calls := 0
	service := fakeSubagentTaskService{get: func(_ context.Context, owner runtime.DelegationParent, id string) (runtime.BackgroundTask, error) {
		calls++
		if owner.ConversationID != 7 || owner.RunID != "" || owner.Agent != nil || id != httpTestTaskID {
			t.Fatalf("owner=%+v id=%q", owner, id)
		}
		return runtime.BackgroundTask{TaskID: id, ConversationID: 7, Status: "completed", Result: &runtime.DelegationResult{Summary: "核查完成", Status: runtime.RunCompleted}}, nil
	}}
	response := requestSubagentTask(subagentTaskRouter(service), http.MethodGet, "/api/conversations/7/subagent-tasks/"+httpTestTaskID+"?conversation_id=99")
	if response.Code != http.StatusOK || calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, calls, response.Body.String())
	}
	var task runtime.BackgroundTask
	if err := json.Unmarshal(response.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if task.TaskID != httpTestTaskID || task.ConversationID != 7 || task.Result == nil || task.Result.Summary != "核查完成" {
		t.Fatalf("task=%+v", task)
	}
}

func TestSubagentTaskListUsesPathOwnerAndReturnsEmptyArray(t *testing.T) {
	service := fakeSubagentTaskService{list: func(_ context.Context, owner uint) ([]runtime.BackgroundTask, error) {
		if owner != 7 {
			t.Fatalf("owner=%d", owner)
		}
		return nil, nil
	}}
	response := requestSubagentTask(subagentTaskRouter(service), http.MethodGet, "/api/conversations/7/subagent-tasks?conversation_id=99")
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"tasks":[]}` {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestSubagentTaskCancelReturnsIdempotentState(t *testing.T) {
	calls := 0
	service := fakeSubagentTaskService{cancel: func(_ context.Context, owner runtime.DelegationParent, id string) (runtime.BackgroundTask, error) {
		calls++
		if owner.ConversationID != 7 || owner.RunID != "" || owner.Agent != nil || id != httpTestTaskID {
			t.Fatalf("owner=%+v id=%q", owner, id)
		}
		return runtime.BackgroundTask{TaskID: id, ConversationID: 7, Status: "cancelled", CancelRequested: true}, nil
	}}
	router := subagentTaskRouter(service)
	first := requestSubagentTask(router, http.MethodPost, "/api/conversations/7/subagent-tasks/"+httpTestTaskID+"/cancel?conversation_id=99")
	second := requestSubagentTask(router, http.MethodPost, "/api/conversations/7/subagent-tasks/"+httpTestTaskID+"/cancel")
	if first.Code != http.StatusOK || second.Code != http.StatusOK || calls != 2 || first.Body.String() != second.Body.String() {
		t.Fatalf("first=%d %s second=%d %s calls=%d", first.Code, first.Body.String(), second.Code, second.Body.String(), calls)
	}
}

func TestSubagentTaskRejectsInvalidPathBeforeCallingService(t *testing.T) {
	// Missing fake functions panic if invalid input reaches the service.
	router := subagentTaskRouter(fakeSubagentTaskService{})
	for _, owner := range []string{"0", "-1", "invalid", "18446744073709551616"} {
		for _, endpoint := range taskHTTPEndpoints() {
			response := requestSubagentTask(router, endpoint.method, "/api/conversations/"+owner+"/subagent-tasks"+endpoint.suffix)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("owner=%s status=%d body=%s", owner, response.Code, response.Body.String())
			}
		}
	}
	for _, id := range []string{"short", strings.Repeat("a", 31), strings.Repeat("a", 33), strings.ToUpper(httpTestTaskID), strings.Repeat("g", 32), "%20" + httpTestTaskID} {
		for _, cancel := range []bool{false, true} {
			method, suffix := http.MethodGet, ""
			if cancel {
				method, suffix = http.MethodPost, "/cancel"
			}
			response := requestSubagentTask(router, method, "/api/conversations/7/subagent-tasks/"+id+suffix)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("id=%s status=%d body=%s", id, response.Code, response.Body.String())
			}
		}
	}
}

func taskHTTPEndpoints() []struct{ method, suffix string } {
	return []struct{ method, suffix string }{{http.MethodGet, ""}, {http.MethodGet, "/" + httpTestTaskID}, {http.MethodPost, "/" + httpTestTaskID + "/cancel"}}
}

func TestSubagentTaskErrorsAreMappedWithoutInternalDetails(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
	}{
		{"not found", fmt.Errorf("private detail: %w", gorm.ErrRecordNotFound), http.StatusNotFound},
		{"disabled", fmt.Errorf("private detail: %w", subagent.ErrBackgroundUnavailable), http.StatusServiceUnavailable},
		{"internal", errors.New("private password=secret failure"), http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := fakeSubagentTaskService{
				get: func(context.Context, runtime.DelegationParent, string) (runtime.BackgroundTask, error) {
					return runtime.BackgroundTask{}, test.err
				},
				list: func(context.Context, uint) ([]runtime.BackgroundTask, error) { return nil, test.err },
				cancel: func(context.Context, runtime.DelegationParent, string) (runtime.BackgroundTask, error) {
					return runtime.BackgroundTask{}, test.err
				},
			}
			router := subagentTaskRouter(service)
			for _, endpoint := range taskHTTPEndpoints() {
				response := requestSubagentTask(router, endpoint.method, "/api/conversations/7/subagent-tasks"+endpoint.suffix)
				if response.Code != test.status || strings.Contains(response.Body.String(), "private") || strings.Contains(response.Body.String(), "secret") {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
			}
		})
	}
}

func TestSubagentTaskNilServiceIsUnavailable(t *testing.T) {
	router := subagentTaskRouter(nil)
	for _, endpoint := range taskHTTPEndpoints() {
		response := requestSubagentTask(router, endpoint.method, "/api/conversations/7/subagent-tasks"+endpoint.suffix)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestSubagentTaskRejectsForeignServiceResults(t *testing.T) {
	foreign := runtime.BackgroundTask{TaskID: httpTestTaskID, ConversationID: 99, Task: "foreign private task"}
	service := fakeSubagentTaskService{
		get: func(context.Context, runtime.DelegationParent, string) (runtime.BackgroundTask, error) {
			return foreign, nil
		},
		list: func(context.Context, uint) ([]runtime.BackgroundTask, error) {
			return []runtime.BackgroundTask{foreign}, nil
		},
		cancel: func(context.Context, runtime.DelegationParent, string) (runtime.BackgroundTask, error) {
			return foreign, nil
		},
	}
	router := subagentTaskRouter(service)
	for _, endpoint := range taskHTTPEndpoints() {
		response := requestSubagentTask(router, endpoint.method, "/api/conversations/7/subagent-tasks"+endpoint.suffix)
		if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "foreign") {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
}
