package server

import "testing"

// webChanges names a layer of the web project's changes as the web UI does, in the namespace its workspace gives.
func webChanges(t *testing.T, baseURL, layer string) string {
	t.Helper()
	namespace, _ := getWorkspaceInfo(t, baseURL)["namespace"].(string)
	if namespace == "" {
		t.Fatal("the workspace names no namespace")
	}
	return "ahp-changeset:/" + namespace + "/" + layer
}
