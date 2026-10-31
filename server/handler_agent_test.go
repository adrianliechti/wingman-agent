package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAgentAPIContract(t *testing.T) {
	t.Setenv("WINGMAN_URL", "http://localhost:1")
	app, err := New(t.Context(), t.TempDir(), &ServerOptions{NoBrowser: true, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost/api/workspace", nil))
	if rec.Code != 200 {
		t.Fatalf("bootstrap: %d %s", rec.Code, rec.Body)
	}
	var boot struct {
		WorkspaceScope
		Protocol int `json:"protocol"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &boot); err != nil {
		t.Fatal(err)
	}
	if boot.Protocol != 2 || boot.InstanceID == "" || boot.WorkspaceID == "" {
		t.Fatalf("bootstrap: %#v", boot)
	}
	activityRequest := httptest.NewRequest(http.MethodGet, "http://localhost/api/workspace/activity", nil)
	activityRequest.Header.Set(instanceHeader, boot.InstanceID)
	activity := httptest.NewRecorder()
	app.ServeHTTP(activity, activityRequest)
	if activity.Code != 200 || !json.Valid(activity.Body.Bytes()) {
		t.Fatalf("activity: %d %s", activity.Code, activity.Body)
	}

	var info map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	assertWorkspaceInfo(t, info)
	if info["path"] != app.workspace.RootPath {
		t.Fatalf("workspace path = %v", info["path"])
	}
	// Startup is unscoped; refreshes must detect that the server was replaced.
	for _, instance := range []string{app.scope.InstanceID, "old-instance"} {
		req := httptest.NewRequest(http.MethodGet, "http://localhost/api/workspace", nil)
		req.Header.Set(instanceHeader, instance)
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		want := http.StatusOK
		if instance != app.scope.InstanceID {
			want = http.StatusConflict
		}
		if rec.Code != want {
			t.Fatalf("discovery instance %q: %d", instance, rec.Code)
		}
	}
	for _, instance := range []string{"", "old-instance"} {
		req := httptest.NewRequest(http.MethodPost, "http://localhost/api/files", nil)
		req.Header.Set(instanceHeader, instance)
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict {
			t.Fatalf("instance %q: %d", instance, rec.Code)
		}
	}
}
