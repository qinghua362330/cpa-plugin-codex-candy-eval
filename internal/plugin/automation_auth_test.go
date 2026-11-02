package plugin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const automaticAuthTestJSON = `{"type":"codex","refresh_token":"secret-refresh-token","access_token":"secret-access-token","nested":{"number":900719925474099312345,"token":"secret-nested-token"},"disabled":false}`

type automaticAuthTestHost struct {
	mu          sync.Mutex
	entry       hostAuthFile
	aliases     []hostAuthFile
	disk        json.RawMessage
	calls       map[string]int
	patches     int
	patchBody   string
	beforePatch func(*automaticAuthTestHost)
	change      func(string, int, *automaticAuthTestHost)
	fail        string
}

func newAutomaticAuthTestHost(t *testing.T) (*automaticAuthTestHost, credential) {
	t.Helper()
	setupTest(t)
	h := &automaticAuthTestHost{
		entry: hostAuthFile{ID: "credential-id", AuthIndex: "index-1", Name: "codex.json", Path: "/auth/codex.json", Source: "file", Provider: "codex", Type: "codex"},
		disk:  json.RawMessage(automaticAuthTestJSON), calls: make(map[string]int),
	}
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.calls[method]++
		if h.change != nil {
			h.change(method, h.calls[method], h)
		}
		if h.fail == method {
			return nil, errors.New("secret-host-error " + automaticAuthTestJSON)
		}
		switch method {
		case "host.auth.list":
			return json.Marshal(map[string]any{"files": append([]hostAuthFile{h.entry}, h.aliases...)})
		case "host.auth.get":
			b, _ := json.Marshal(payload)
			if string(b) != `{"auth_index":"index-1"}` {
				t.Fatal("incorrect get index")
			}
			return json.Marshal(automaticAuthFile{AuthIndex: h.entry.AuthIndex, Name: h.entry.Name, Path: h.entry.Path, JSON: h.disk})
		default:
			t.Fatal("unexpected host mutation or method")
			return nil, nil
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.patches++
		body, _ := io.ReadAll(r.Body)
		h.patchBody = string(body)
		if r.Method != http.MethodPatch || r.URL.Path != "/v0/management/auth-files/status" || r.Header.Get("Authorization") != "Bearer test-management-secret" ||
			h.patchBody != `{"auth_index":"index-1","disabled":true,"name":"codex.json"}` {
			t.Error("incorrect management request")
			w.WriteHeader(400)
			return
		}
		if h.beforePatch != nil {
			h.beforePatch(h)
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(h.disk, &fields) != nil {
			t.Error("invalid fixture")
		}
		fields["disabled"] = json.RawMessage("true")
		h.disk, _ = json.Marshal(fields)
		h.entry.Disabled = true
		_, _ = io.WriteString(w, `{"status":"ok","disabled":true}`)
	}))
	t.Cleanup(server.Close)
	configureAutomationManagementTest(t, server.URL)
	auth := credential{ID: h.entry.ID, AuthIndex: h.entry.AuthIndex, Name: h.entry.Name, Source: credentialSourceFile, Provider: "codex"}
	var err error
	auth.AutomationRevision, err = automaticCredentialRevision(auth)
	if err != nil {
		t.Fatal(err)
	}
	h.calls = make(map[string]int)
	return h, auth
}

func TestDisableAutomaticCredentialNarrowPatch(t *testing.T) {
	h, auth := newAutomaticAuthTestHost(t)
	// A refresh after the revision check must not be replaced with old tokens.
	h.beforePatch = func(h *automaticAuthTestHost) {
		h.disk = json.RawMessage(strings.ReplaceAll(automaticAuthTestJSON, "secret-refresh-token", "rotated-refresh-token"))
	}
	if err := disableAutomaticCredential(auth); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.patches != 1 || h.calls["host.auth.list"] != 2 || h.calls["host.auth.get"] != 2 {
		t.Fatal("missing PATCH or verification")
	}
	if strings.Contains(h.patchBody, "token") || !strings.Contains(string(h.disk), "rotated-refresh-token") || !strings.Contains(string(h.disk), "900719925474099312345") {
		t.Fatal("credential data was sent or replaced")
	}
}

func TestDisableAutomaticCredentialRejectsUnsafeIdentity(t *testing.T) {
	cases := map[string]func(*automaticAuthTestHost, *credential){
		"source":            func(h *automaticAuthTestHost, a *credential) { a.Source = credentialSourceProvider },
		"provider":          func(h *automaticAuthTestHost, a *credential) { a.Provider = "claude" },
		"empty ID":          func(h *automaticAuthTestHost, a *credential) { a.ID = "" },
		"empty index":       func(h *automaticAuthTestHost, a *credential) { a.AuthIndex = "" },
		"wrong index":       func(h *automaticAuthTestHost, a *credential) { a.AuthIndex = "other" },
		"empty revision":    func(h *automaticAuthTestHost, a *credential) { a.AutomationRevision = "" },
		"index whitespace":  func(h *automaticAuthTestHost, a *credential) { a.AuthIndex = " index-1" },
		"traversal name":    func(h *automaticAuthTestHost, a *credential) { a.Name = "../codex.json" },
		"backslash name":    func(h *automaticAuthTestHost, a *credential) { a.Name = `a\codex.json` },
		"wrong name":        func(h *automaticAuthTestHost, a *credential) { a.Name = "other.json" },
		"extension":         func(h *automaticAuthTestHost, a *credential) { a.Name = "codex" },
		"no path":           func(h *automaticAuthTestHost, a *credential) { h.entry.Path = "" },
		"relative path":     func(h *automaticAuthTestHost, a *credential) { h.entry.Path = "auth/codex.json" },
		"unclean path":      func(h *automaticAuthTestHost, a *credential) { h.entry.Path = "/auth/../auth/codex.json" },
		"path basename":     func(h *automaticAuthTestHost, a *credential) { h.entry.Path = "/auth/other.json" },
		"runtime":           func(h *automaticAuthTestHost, a *credential) { h.entry.RuntimeOnly = true },
		"memory":            func(h *automaticAuthTestHost, a *credential) { h.entry.Source = "memory" },
		"inventory type":    func(h *automaticAuthTestHost, a *credential) { h.entry.Type = "claude" },
		"JSON type":         func(h *automaticAuthTestHost, a *credential) { h.disk = json.RawMessage(`{"type":"claude"}`) },
		"JSON missing type": func(h *automaticAuthTestHost, a *credential) { h.disk = json.RawMessage(`{}`) },
		"JSON null":         func(h *automaticAuthTestHost, a *credential) { h.disk = json.RawMessage(`null`) },
		"duplicate ID":      func(h *automaticAuthTestHost, a *credential) { h.aliases = []hostAuthFile{h.entry} },
		"alias name": func(h *automaticAuthTestHost, a *credential) {
			h.aliases = []hostAuthFile{{ID: "child", Name: h.entry.Name}}
		},
		"alias path": func(h *automaticAuthTestHost, a *credential) {
			h.aliases = []hostAuthFile{{ID: "child", Path: "/auth/./codex.json"}}
		},
		"alias index": func(h *automaticAuthTestHost, a *credential) {
			h.aliases = []hostAuthFile{{ID: "child", AuthIndex: h.entry.AuthIndex}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h, auth := newAutomaticAuthTestHost(t)
			mutate(h, &auth)
			if err := disableAutomaticCredential(auth); err == nil {
				t.Fatal("unsafe credential accepted")
			}
			if h.patches != 0 {
				t.Fatal("unsafe PATCH attempted")
			}
		})
	}
}

func TestDisableAutomaticCredentialRevisionChanges(t *testing.T) {
	for _, kind := range []string{"replacement", "refresh", "path"} {
		t.Run(kind, func(t *testing.T) {
			h, auth := newAutomaticAuthTestHost(t)
			switch kind {
			case "replacement":
				h.disk = json.RawMessage(`{"type":"codex","refresh_token":"other-account"}`)
			case "refresh":
				h.disk = json.RawMessage(strings.ReplaceAll(automaticAuthTestJSON, "secret-refresh-token", "rotated"))
			case "path":
				h.entry.Path = "/other/codex.json"
			}
			if err := disableAutomaticCredential(auth); err == nil {
				t.Fatal("changed revision accepted")
			}
			if h.patches != 0 {
				t.Fatal("PATCH after revision change")
			}
		})
	}
}

func TestDisableAutomaticCredentialFailuresAreSanitized(t *testing.T) {
	for _, stage := range []string{"list", "get", "verify list error", "verify list enabled", "verify path", "verify get error", "verify get enabled"} {
		t.Run(stage, func(t *testing.T) {
			h, auth := newAutomaticAuthTestHost(t)
			if !strings.HasPrefix(stage, "verify") {
				h.fail = "host.auth." + stage
			}
			h.change = func(method string, count int, h *automaticAuthTestHost) {
				if count != 2 {
					return
				}
				if method == "host.auth.list" && stage == "verify list error" {
					h.fail = method
				}
				if method == "host.auth.list" && stage == "verify list enabled" {
					h.entry.Disabled = false
				}
				if method == "host.auth.list" && stage == "verify path" {
					h.entry.Path = "/other/codex.json"
				}
				if method == "host.auth.get" && stage == "verify get error" {
					h.fail = method
				}
				if method == "host.auth.get" && stage == "verify get enabled" {
					h.disk = json.RawMessage(automaticAuthTestJSON)
				}
			}
			err := disableAutomaticCredential(auth)
			if err == nil {
				t.Fatal("failure not reported")
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "refresh_token") {
				t.Fatal("sensitive error leaked")
			}
			if strings.HasPrefix(stage, "verify") && !strings.Contains(err.Error(), "uncertain") {
				t.Fatal("uncertainty not reported")
			}
		})
	}
}

func TestDisableAutomaticCredentialGetIdentityMismatch(t *testing.T) {
	for _, field := range []string{"name", "path", "auth_index"} {
		t.Run(field, func(t *testing.T) {
			h, auth := newAutomaticAuthTestHost(t)
			original := hostCall
			hostCall = func(method string, payload any) (json.RawMessage, error) {
				raw, err := original(method, payload)
				if method == "host.auth.get" {
					var result map[string]json.RawMessage
					if json.Unmarshal(raw, &result) != nil {
						t.Fatal("invalid fixture")
					}
					result[field] = json.RawMessage(`"mismatch"`)
					return json.Marshal(result)
				}
				return raw, err
			}
			if disableAutomaticCredential(auth) == nil || h.patches != 0 {
				t.Fatal("mismatched get accepted")
			}
		})
	}
}
