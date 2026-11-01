package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"recallweave/internal/agent/runtime"
	"recallweave/internal/agent/subagent"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type fakeTaskGroupService struct {
	get    func(context.Context, runtime.DelegationParent, string) (runtime.TaskGroup, error)
	list   func(context.Context, uint) ([]runtime.TaskGroup, error)
	cancel func(context.Context, runtime.DelegationParent, string) (runtime.TaskGroup, error)
}

func (f fakeTaskGroupService) GetGroup(ctx context.Context, parent runtime.DelegationParent, id string) (runtime.TaskGroup, error) {
	return f.get(ctx, parent, id)
}
func (f fakeTaskGroupService) ListGroups(ctx context.Context, owner uint) ([]runtime.TaskGroup, error) {
	return f.list(ctx, owner)
}
func (f fakeTaskGroupService) CancelGroup(ctx context.Context, parent runtime.DelegationParent, id string) (runtime.TaskGroup, error) {
	return f.cancel(ctx, parent, id)
}
func taskGroupRouter(service TaskGroupService) *gin.Engine {
	router := gin.New()
	MountTaskGroups(router, NewTaskGroupHandler(service))
	return router
}

func TestTaskGroupGetUsesPathOwnerAndReturnsMemberProgress(t *testing.T) {
	service := fakeTaskGroupService{get: func(_ context.Context, parent runtime.DelegationParent, id string) (runtime.TaskGroup, error) {
		if parent.ConversationID != 7 || parent.RunID != "" || parent.Agent != nil || id != httpTestTaskID {
			t.Fatalf("scope changed: %+v %s", parent, id)
		}
		return runtime.TaskGroup{GroupID: id, ConversationID: 7, Status: "partial", TaskIDs: []string{"first", "second"}, Tasks: []runtime.BackgroundTask{{TaskID: "first", Status: "completed", Result: &runtime.DelegationResult{Summary: "verified"}}, {TaskID: "second", Status: "failed", Error: "source unavailable"}}, ReportStatus: "pending"}, nil
	}}
	response := requestSubagentTask(taskGroupRouter(service), http.MethodGet, "/api/conversations/7/task-groups/"+httpTestTaskID+"?conversation_id=99")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var group runtime.TaskGroup
	if err := json.Unmarshal(response.Body.Bytes(), &group); err != nil {
		t.Fatal(err)
	}
	if group.ConversationID != 7 || len(group.Tasks) != 2 || group.Tasks[0].Result.Summary != "verified" || group.ReportStatus != "pending" {
		t.Fatalf("lost group/member state: %+v", group)
	}
}

func TestTaskGroupListUsesPathOwnerAndReturnsEmptyArray(t *testing.T) {
	service := fakeTaskGroupService{list: func(_ context.Context, owner uint) ([]runtime.TaskGroup, error) {
		if owner != 7 {
			t.Fatalf("owner=%d", owner)
		}
		return nil, nil
	}}
	response := requestSubagentTask(taskGroupRouter(service), http.MethodGet, "/api/conversations/7/task-groups?conversation_id=99")
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"groups":[]}` {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestTaskGroupCancelReturnsRequestWithoutPretendingMembersStopped(t *testing.T) {
	calls := 0
	service := fakeTaskGroupService{cancel: func(_ context.Context, parent runtime.DelegationParent, id string) (runtime.TaskGroup, error) {
		calls++
		if parent.ConversationID != 7 || id != httpTestTaskID {
			t.Fatalf("scope changed: %+v %s", parent, id)
		}
		return runtime.TaskGroup{GroupID: id, ConversationID: 7, Status: "running", CancelRequested: true}, nil
	}}
	router := taskGroupRouter(service)
	for i := 0; i < 2; i++ {
		response := requestSubagentTask(router, http.MethodPost, "/api/conversations/7/task-groups/"+httpTestTaskID+"/cancel")
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"cancel_requested":true`) || !strings.Contains(response.Body.String(), `"status":"running"`) {
			t.Fatalf("cancel response=%s", response.Body.String())
		}
	}
	if calls != 2 {
		t.Fatalf("cancel calls=%d", calls)
	}
}

func TestTaskGroupRoutesRejectInvalidOwnerAndIDsBeforeService(t *testing.T) {
	router := taskGroupRouter(fakeTaskGroupService{})
	for _, owner := range []string{"0", "-1", "not-a-number", "18446744073709551616"} {
		for _, suffix := range []string{"", "/" + httpTestTaskID} {
			response := requestSubagentTask(router, http.MethodGet, "/api/conversations/"+owner+"/task-groups"+suffix)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("invalid owner %q status=%d", owner, response.Code)
			}
		}
	}
	for _, id := range []string{"short", strings.ToUpper(httpTestTaskID), strings.Repeat("g", 32)} {
		for _, endpoint := range []struct{ method, suffix string }{{http.MethodGet, ""}, {http.MethodPost, "/cancel"}} {
			response := requestSubagentTask(router, endpoint.method, "/api/conversations/7/task-groups/"+id+endpoint.suffix)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("invalid id %q status=%d", id, response.Code)
			}
		}
	}
}

func TestTaskGroupRoutesMapErrorsWithoutLeakingDetails(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{{fmt.Errorf("private detail: %w", gorm.ErrRecordNotFound), http.StatusNotFound}, {fmt.Errorf("private detail: %w", subagent.ErrBackgroundUnavailable), http.StatusServiceUnavailable}, {errors.New("private database password"), http.StatusInternalServerError}} {
		service := fakeTaskGroupService{get: func(context.Context, runtime.DelegationParent, string) (runtime.TaskGroup, error) {
			return runtime.TaskGroup{}, tc.err
		}, cancel: func(context.Context, runtime.DelegationParent, string) (runtime.TaskGroup, error) {
			return runtime.TaskGroup{}, tc.err
		}, list: func(context.Context, uint) ([]runtime.TaskGroup, error) { return nil, tc.err }}
		router := taskGroupRouter(service)
		for _, endpoint := range []struct{ method, suffix string }{{http.MethodGet, ""}, {http.MethodGet, "/" + httpTestTaskID}, {http.MethodPost, "/" + httpTestTaskID + "/cancel"}} {
			response := requestSubagentTask(router, endpoint.method, "/api/conversations/7/task-groups"+endpoint.suffix)
			if response.Code != tc.status || strings.Contains(response.Body.String(), "private") {
				t.Fatalf("status=%d expected=%d body=%s", response.Code, tc.status, response.Body.String())
			}
		}
	}
}

func TestTaskGroupRoutesRejectForeignServiceResults(t *testing.T) {
	service := fakeTaskGroupService{get: func(context.Context, runtime.DelegationParent, string) (runtime.TaskGroup, error) {
		return runtime.TaskGroup{GroupID: httpTestTaskID, ConversationID: 99, Goal: "PRIVATE_GROUP"}, nil
	}, cancel: func(context.Context, runtime.DelegationParent, string) (runtime.TaskGroup, error) {
		return runtime.TaskGroup{GroupID: "different", ConversationID: 7}, nil
	}, list: func(context.Context, uint) ([]runtime.TaskGroup, error) {
		return []runtime.TaskGroup{{ConversationID: 99, Goal: "PRIVATE_GROUP"}}, nil
	}}
	router := taskGroupRouter(service)
	for _, endpoint := range []struct{ method, suffix string }{{http.MethodGet, ""}, {http.MethodGet, "/" + httpTestTaskID}, {http.MethodPost, "/" + httpTestTaskID + "/cancel"}} {
		response := requestSubagentTask(router, endpoint.method, "/api/conversations/7/task-groups"+endpoint.suffix)
		if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "PRIVATE_GROUP") {
			t.Fatalf("foreign group exposed: %s", response.Body.String())
		}
	}
}

func TestTaskGroupRoutesReturnUnavailableWithoutService(t *testing.T) {
	router := taskGroupRouter(nil)
	for _, endpoint := range []struct{ method, suffix string }{{http.MethodGet, ""}, {http.MethodGet, "/" + httpTestTaskID}, {http.MethodPost, "/" + httpTestTaskID + "/cancel"}} {
		response := requestSubagentTask(router, endpoint.method, "/api/conversations/7/task-groups"+endpoint.suffix)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
}
