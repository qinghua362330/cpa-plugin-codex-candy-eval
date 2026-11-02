package plugin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const syntheticManagementFixture = "test-management-secret"

func configureAutomationManagementTest(t *testing.T, origin string) {
	t.Helper()
	body, _ := json.Marshal(automationManagementConfig{BaseURL: origin, APIKey: syntheticManagementFixture})
	if response := updateAutomationManagement(body); response.StatusCode != http.StatusOK {
		t.Fatal("fixture management configuration failed")
	}
}

func TestAutomationManagementURL(t *testing.T) {
	if validAutomationManagementURL("http://::1:80") {
		t.Fatal("unbracketed IPv6 origin accepted")
	}
	for _, raw := range []string{"http://127.0.0.1", "http://127.0.0.2:8317", "https://[::1]:443", "http://[::1]"} {
		if !validAutomationManagementURL(raw) {
			t.Errorf("valid origin rejected: %s", raw)
		}
	}
	for _, raw := range []string{"", "http://localhost", "http://example.com", "http://10.0.0.1", "http://0.0.0.0", "http://[::]", "http://[::2]", "ftp://127.0.0.1", "http://127.0.0.1/", "http://127.0.0.1/path", "http://127.0.0.1?", "http://127.0.0.1?q=x", "http://127.0.0.1#", "http://127.0.0.1#x", "http://user:secret@127.0.0.1", "http://127.0.0.1:", "http://127.0.0.1:0", "http://127.0.0.1:65536", "http://127.0.0.1:bad", "http://[::1%25lo0]", " http://127.0.0.1", "http://2130706433", "http://127.1", "http://127.000.000.001", "http://127.0.0.1\\@example.com", "http://127.0.0.1/%2e%2e"} {
		if validAutomationManagementURL(raw) {
			t.Errorf("unsafe origin accepted: %s", raw)
		}
	}
}

func TestAutomationManagementStorageAndPublicStatus(t *testing.T) {
	setupTest(t)
	if automationManagementReady() {
		t.Fatal("missing config is ready")
	}
	configureAutomationManagementTest(t, "http://127.0.0.1:8317")
	name := statePath + ".management.json"
	info, err := os.Stat(name)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("secret file permissions")
	}
	data, err := os.ReadFile(name)
	var stored automationManagementConfig
	if err != nil || json.Unmarshal(data, &stored) != nil || stored.APIKey != "test-management-secret" {
		t.Fatal("secret not persisted")
	}
	for _, response := range []managementResponse{
		managementStatusResponse(),
		updateAutomationManagement([]byte(`{"base_url":"http://127.0.0.1:8317","api_key":""}`)),
		updateAutomationManagement([]byte(`{"base_url":"http://127.0.0.1:8317"}`)),
	} {
		if response.StatusCode != http.StatusOK {
			t.Fatal("public response failed")
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(response.Body, &fields) != nil || len(fields) != 2 || fields["base_url"] == nil || string(fields["ready"]) != "true" || strings.Contains(string(response.Body), "secret") || strings.Contains(string(response.Body), "api_key") {
			t.Fatal("public status contains unexpected fields")
		}
	}
	if cfg, err := readAutomationManagement(); err != nil || cfg.APIKey != stored.APIKey {
		t.Fatal("blank key not preserved")
	}
	response := updateAutomationManagement([]byte(`{"base_url":"http://127.0.0.1:8318","api_key":""}`))
	if response.StatusCode != http.StatusBadRequest {
		t.Fatal("changed URL reused key")
	}
	if cfg, err := readAutomationManagement(); err != nil || cfg.BaseURL != stored.BaseURL {
		t.Fatal("failed update altered config")
	}
	configureAutomationManagementTest(t, "http://127.0.0.1:8318")
	if !automationManagementReady() || automationManagementStatus().BaseURL != "http://127.0.0.1:8318" {
		t.Fatal("replacement config not ready")
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(name), ".automation-management-*"))
	if len(files) != 0 {
		t.Fatal("temporary secret files remained")
	}
}

func TestAutomationManagementInvalidConfiguration(t *testing.T) {
	setupTest(t)
	for _, body := range []string{`null`, `{}`, `{"base_url":"http://user:secret@127.0.0.1","api_key":"secret"}`, `{"base_url":"http://127.0.0.1","api_key":"secret\nheader"}`, `{"base_url":"http://127.0.0.1","api_key":"secret","unknown":"secret"}`, `{"base_url":"http://127.0.0.1","api_key":"secret"} {}`, strings.Repeat("secret", automationManagementLimit)} {
		response := updateAutomationManagement([]byte(body))
		if response.StatusCode != http.StatusBadRequest || strings.Contains(string(response.Body), "secret") {
			t.Fatal("invalid configuration accepted or leaked")
		}
	}
	statePath = filepath.Join(t.TempDir(), "missing", "state.json")
	response := updateAutomationManagement([]byte(`{"base_url":"http://127.0.0.1","api_key":"secret"}`))
	if response.StatusCode != http.StatusInternalServerError || strings.Contains(string(response.Body), "secret") || automationManagementReady() {
		t.Fatal("storage failure not sanitized")
	}
}

func TestAutomationManagementReadFailsClosed(t *testing.T) {
	for _, kind := range []string{"corrupt", "permissions", "symlink", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			setupTest(t)
			configureAutomationManagementTest(t, "http://127.0.0.1")
			name := statePath + ".management.json"
			var err error
			switch kind {
			case "corrupt":
				err = os.WriteFile(name, []byte(`{"api_key":"secret"}`), 0600)
			case "permissions":
				err = os.Chmod(name, 0644)
			case "oversized":
				err = os.WriteFile(name, []byte(strings.Repeat("secret", automationManagementLimit)), 0600)
			case "symlink":
				if err = os.Rename(name, name+".target"); err == nil {
					err = os.Symlink(name+".target", name)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			response := managementStatusResponse()
			if automationManagementReady() || strings.Contains(string(response.Body), "secret") {
				t.Fatal("unsafe secret store accepted or exposed")
			}
		})
	}
}

func TestAutomationManagementUpdateWaitsForAction(t *testing.T) {
	setupTest(t)
	automationActionMu.Lock()
	started := make(chan struct{})
	done := make(chan managementResponse, 1)
	go func() {
		close(started)
		done <- updateAutomationManagement([]byte(`{"base_url":"http://127.0.0.1","api_key":"test-secret"}`))
	}()
	<-started
	select {
	case <-done:
		automationActionMu.Unlock()
		t.Fatal("configuration update bypassed action lock")
	case <-time.After(20 * time.Millisecond):
	}
	automationActionMu.Unlock()
	select {
	case response := <-done:
		if response.StatusCode != http.StatusOK {
			t.Fatal("configuration update failed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("configuration update deadlocked")
	}
}

func TestAutomationManagementHTTPResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		ok     bool
	}{
		{"success", 200, `{"status":"ok","disabled":true}`, true},
		{"unauthorized", 401, `{"error":"secret-admin-token"}`, false},
		{"forbidden", 403, `secret`, false},
		{"not found", 404, `secret`, false},
		{"conflict", 409, `secret`, false},
		{"hook failure", 500, `secret`, false},
		{"no content", 204, ``, false},
		{"malformed", 200, `secret`, false},
		{"wrong state", 200, `{"status":"ok","disabled":false}`, false},
		{"missing state", 200, `{"status":"ok"}`, false},
		{"wrong status", 200, `{"status":"error","disabled":true}`, false},
		{"oversized", 200, strings.Repeat("secret", automationManagementLimit), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			err := patchAutomaticCredential(automationManagementConfig{BaseURL: server.URL, APIKey: syntheticManagementFixture}, hostAuthFile{Name: "codex.json", AuthIndex: "index-1"})
			if (err == nil) != tc.ok {
				t.Fatal("unexpected PATCH result")
			}
			if err != nil && (strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), server.URL)) {
				t.Fatal("HTTP error leaked details")
			}
		})
	}
}

func TestAutomationManagementHTTPTransport(t *testing.T) {
	client := newAutomationManagementClient()
	defer client.CloseIdleConnections()
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || client.Timeout != 10*time.Second {
		t.Fatal("transport is not bounded and direct")
	}
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	t.Setenv("HTTP_PROXY", target.URL)
	t.Setenv("HTTPS_PROXY", target.URL)
	if err := patchAutomaticCredential(automationManagementConfig{BaseURL: redirect.URL, APIKey: "test-secret"}, hostAuthFile{Name: "codex.json", AuthIndex: "index-1"}); err == nil {
		t.Fatal("redirect accepted")
	}
	if targetCalls.Load() != 0 {
		t.Fatal("credential sent to redirect or proxy")
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	err := patchAutomaticCredential(automationManagementConfig{BaseURL: closed.URL, APIKey: "test-secret"}, hostAuthFile{Name: "codex.json", AuthIndex: "index-1"})
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), closed.URL) {
		t.Fatal("transport error not sanitized")
	}
}
