package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func automationTestRecord() fingerprintRecord {
	p := 0.99
	return fingerprintRecord{record: record{Model: "gpt-6.1-sol", Status: "completed"}, Attribution: &fingerprintAttribution{
		Used: 3, Decision: "completed", Prediction: "deepseek-flash", Probability: &p,
		Sequence: &fingerprintSequence{ModelLike: true},
	}}
}

func automationTestDisable(t *testing.T, fn func(credential) error) {
	t.Helper()
	previous := automationDisable
	automationDisable = fn
	t.Cleanup(func() { Quiesce(); automationDisable = previous })
}

func TestAutomationTimerRunsAndRearms(t *testing.T) {
	setupTest(t)
	hostCall = func(method string, _ any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return nil, errors.New("unexpected host call")
		}
		return json.RawMessage(`{"files":[]}`), nil
	}
	automation.Config = automationConfig{Enabled: true, Model: "gpt-6.1-sol", IntervalMinutes: 30}
	due := time.Now().UTC()
	mu.Lock()
	automation.NextRun = &due
	scheduleAutomationLocked()
	mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		finished := automation.LastFinished != nil
		rearmed := finished && !automation.Running && automationTimer != nil && automation.NextRun != nil && automation.NextRun.After(*automation.LastFinished)
		mu.Unlock()
		if finished {
			if !rearmed {
				t.Fatal("timer completed but next inspection was not scheduled")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("scheduled inspection did not run")
}

func TestAutomationValidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config automationConfig
		valid  bool
	}{
		{"minimum", automationConfig{IntervalMinutes: 1}, true},
		{"maximum", automationConfig{IntervalMinutes: 10080}, true},
		{"zero", automationConfig{}, false},
		{"negative", automationConfig{IntervalMinutes: -1}, false},
		{"too-long", automationConfig{IntervalMinutes: 10081}, false},
		{"enabled-no-model", automationConfig{IntervalMinutes: 30, Enabled: true}, false},
		{"disable-no-model", automationConfig{IntervalMinutes: 30, AutoDisable: true}, false},
		{"unknown", automationConfig{IntervalMinutes: 30, Model: "not-in-reference"}, false},
		{"effort", automationConfig{IntervalMinutes: 30, Model: "gpt-6.1-sol(high)"}, false},
		{"alias", automationConfig{IntervalMinutes: 30, Model: " openai/gpt-6.1-sol ", Enabled: true}, true},
		{"other-alias", automationConfig{IntervalMinutes: 30, Model: "deepseek/deepseek-flash", AutoDisable: true}, true},
		{"invalid-id", automationConfig{IntervalMinutes: 30, CredentialIDs: []string{"a\nb"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateAutomation(&tc.config); (err == nil) != tc.valid {
				t.Fatalf("validate = %v, valid = %v", err, tc.valid)
			}
		})
	}
	for _, model := range sharedDetector.ModelIDs {
		c := automationConfig{Model: model, IntervalMinutes: 30}
		if err := validateAutomation(&c); err != nil {
			t.Errorf("reference model %q rejected: %v", model, err)
		}
	}
	c := automationConfig{Model: " gpt-6.1-sol ", IntervalMinutes: 30, CredentialIDs: []string{"b", "a", "b"}}
	if err := validateAutomation(&c); err != nil || c.Model != "gpt-6.1-sol" || !reflect.DeepEqual(c.CredentialIDs, []string{"a", "b"}) {
		t.Fatalf("normalization: %+v, %v", c, err)
	}
}

func TestAutomationVerdict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*fingerprintRecord)
		want   string
	}{
		{"mismatch", func(*fingerprintRecord) {}, "mismatch"},
		{"alias", func(r *fingerprintRecord) { r.Attribution.Prediction = "openai/gpt-6-astra" }, "match"},
		{"reverse-alias", func(r *fingerprintRecord) { r.Model = "gpt-6-astra"; r.Attribution.Prediction = "gpt-6.1-sol" }, "match"},
		{"legacy-decision", func(r *fingerprintRecord) { r.Attribution.Decision = "" }, "mismatch"},
		{"failed", func(r *fingerprintRecord) { r.Status = "failed"; r.Error = "request failed" }, "inconclusive"},
		{"partial", func(r *fingerprintRecord) { r.Status = "partial" }, "inconclusive"},
		{"cancelled", func(r *fingerprintRecord) { r.Status = "cancelled" }, "inconclusive"},
		{"skipped", func(r *fingerprintRecord) { r.Status = "skipped" }, "inconclusive"},
		{"two-valid", func(r *fingerprintRecord) { r.Attribution.Used = 2 }, "inconclusive"},
		{"four-valid", func(r *fingerprintRecord) { r.Attribution.Used = 4 }, "inconclusive"},
		{"no-attribution", func(r *fingerprintRecord) { r.Attribution = nil }, "inconclusive"},
		{"no-probability", func(r *fingerprintRecord) { r.Attribution.Probability = nil }, "inconclusive"},
		{"no-prediction", func(r *fingerprintRecord) { r.Attribution.Prediction = "" }, "inconclusive"},
		{"partial-decision", func(r *fingerprintRecord) { r.Attribution.Decision = "partial" }, "inconclusive"},
		{"non-model", func(r *fingerprintRecord) { r.Attribution.Sequence.ModelLike = false }, "inconclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := automationTestRecord()
			tc.change(&r)
			if got := automaticVerdict(r); got != tc.want {
				t.Fatalf("verdict = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAutomationApplyPolicy(t *testing.T) {
	for _, mode := range []string{"observe", "disable", "failed", "partial", "request-error", "match", "stale", "cancelled", "storage-failure", "policy-off", "cooldown", "disable-error", "changed-index", "changed-name", "changed-provider", "changed-source"} {
		t.Run(mode, func(t *testing.T) {
			setupTest(t)
			c := automationConfig{Model: "gpt-6.1-sol", IntervalMinutes: 30, AutoDisable: true, CredentialIDs: []string{"selected"}}
			if mode == "observe" {
				c.AutoDisable = false
			}
			automation.Config = c
			automationGeneration = 7
			auth := credential{ID: "selected", AuthIndex: "selected-index", Name: "selected.json", Provider: "codex", Source: credentialSourceFile}
			current := hostAuthFile{ID: auth.ID, AuthIndex: auth.AuthIndex, Name: auth.Name, Provider: auth.Provider, Source: "file", Unavailable: mode == "cooldown"}
			switch mode {
			case "changed-index":
				current.AuthIndex = "replacement-index"
			case "changed-name":
				current.Name = "replacement.json"
			case "changed-provider":
				current.Provider = "claude"
			case "changed-source":
				current.Source = "config"
			}
			var disabled []credential
			automationTestDisable(t, func(a credential) error {
				disabled = append(disabled, a)
				if mode == "disable-error" {
					return errors.New("host rejected save")
				}
				return nil
			})
			hostCall = func(method string, _ any) (json.RawMessage, error) {
				if method != "host.auth.list" {
					t.Errorf("unexpected host mutation/call %s", method)
					return nil, errors.New("unexpected host call")
				}
				return json.Marshal(map[string]any{"files": []hostAuthFile{
					{ID: "other", Provider: "codex", AuthIndex: "other-index"},
					current,
				}})
			}
			r := automationTestRecord()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			generation, wantContinue, wantStatus := uint64(7), true, "inconclusive"
			switch mode {
			case "observe", "policy-off":
				wantStatus = "mismatch"
			case "disable":
				wantStatus = "disabled"
			case "disable-error":
				wantStatus = "disable_failed"
			case "cooldown", "changed-index", "changed-name", "changed-provider", "changed-source":
				wantStatus = "skipped"
			case "failed", "partial":
				r.Status = mode
			case "request-error":
				r.Status, r.Error, r.Attribution = "failed", "request failed", nil
			case "match":
				r.Attribution.Prediction = "gpt-6-astra"
				wantStatus = "match"
			case "stale":
				generation--
				wantContinue = false
			case "cancelled":
				cancel()
				wantContinue = false
			case "storage-failure":
				statePath = filepath.Join(t.TempDir(), "missing", "state.json")
				wantContinue = false
			}
			if mode == "policy-off" {
				automation.Config.AutoDisable = false
			}
			if got := applyAutomationResult(ctx, c, generation, auth, r); got != wantContinue {
				t.Fatalf("continue = %v, want %v", got, wantContinue)
			}
			wantCalls := 0
			if mode == "disable" || mode == "disable-error" {
				wantCalls = 1
			}
			if len(disabled) != wantCalls {
				t.Fatalf("disable calls = %+v, want %d", disabled, wantCalls)
			}
			if wantCalls == 1 && (disabled[0].ID != auth.ID || disabled[0].AuthIndex != auth.AuthIndex || disabled[0].Name != auth.Name || disabled[0].Provider != auth.Provider || disabled[0].Source != auth.Source) {
				t.Fatalf("disabled wrong/stale credential: %+v", disabled)
			}
			if wantContinue {
				if len(automation.Events) == 0 || automation.Events[len(automation.Events)-1].Status != wantStatus {
					t.Fatalf("events = %+v, want final %s", automation.Events, wantStatus)
				}
			} else if mode != "storage-failure" && len(automation.Events) != 0 {
				t.Fatalf("obsolete result recorded: %+v", automation.Events)
			}
			if mode == "storage-failure" && automation.LastError == "" {
				t.Fatal("storage failure not surfaced")
			}
		})
	}
}

func TestAutomationRejectsResultForDifferentModel(t *testing.T) {
	setupTest(t)
	c := automationConfig{Model: "gpt-6.1-sol", IntervalMinutes: 30, AutoDisable: true}
	automation.Config = c
	auth := credential{ID: "selected", AuthIndex: "selected-index", Name: "selected.json", Provider: "codex", Source: credentialSourceFile}
	automationTestDisable(t, func(credential) error {
		t.Error("result for a different requested model must not disable credential")
		return nil
	})
	hostCall = func(method string, _ any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			t.Errorf("unexpected host call %s", method)
			return nil, errors.New("unexpected host call")
		}
		return json.Marshal(map[string]any{"files": []hostAuthFile{{ID: auth.ID, AuthIndex: auth.AuthIndex, Name: auth.Name, Provider: auth.Provider, Source: "file"}}})
	}
	r := automationTestRecord()
	r.Model = "deepseek-flash"
	r.Attribution.Prediction = "gpt-6-astra"
	// The result itself is complete and mismatched, but belongs to another model.
	if automaticVerdict(r) != "mismatch" {
		t.Fatal("fixture must exercise the model-policy guard, not an inconclusive result")
	}
	applyAutomationResult(context.Background(), c, automationGeneration, auth, r)
	if len(automation.Events) != 1 || automation.Events[0].Status != "inconclusive" {
		t.Fatalf("result for another model must be inconclusive: %+v", automation.Events)
	}
	for _, event := range automation.Events {
		if event.Status == "disabled" {
			t.Fatalf("unrelated result recorded a disable: %+v", event)
		}
	}
}

func TestAutomationPersistenceAndRearm(t *testing.T) {
	setupTest(t)
	if res := updateAutomationManagement([]byte(`{"base_url":"http://127.0.0.1:8317","api_key":"test-only-key"}`)); res.StatusCode != 200 {
		t.Fatalf("management fixture: %s", res.Body)
	}
	c := automationConfig{Enabled: true, AutoDisable: true, Model: "gpt-6.1-sol", IntervalMinutes: 17, CredentialIDs: []string{"selected"}}
	body, _ := json.Marshal(c)
	if res := updateAutomation(body); res.StatusCode != 200 {
		t.Fatalf("update: %s", res.Body)
	}
	mu.Lock()
	automation.Running = true
	automation.Events = []automationEvent{{CredentialID: "selected", Status: "mismatch"}}
	err := saveAutomationLocked()
	if automationTimer != nil {
		automationTimer.Stop()
		automationTimer = nil
	}
	automation, automationLoaded = defaultAutomation(), false
	mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	HandleMethod("plugin.register", nil)
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(automation.Config, c) || automation.Running || len(automation.Events) != 1 || automation.Events[0].Status != "mismatch" {
		t.Fatalf("roundtrip = %+v", automation)
	}
	if automationTimer == nil || automation.NextRun == nil || !automation.NextRun.After(time.Now()) {
		t.Fatal("enabled persisted config was not rearmed")
	}
}

func TestAutomationAutoDisableRequiresManagement(t *testing.T) {
	setupTest(t)
	if automationManagementReady() {
		t.Fatal("fresh test unexpectedly has management configuration")
	}
	if res := updateAutomation([]byte(`{"enabled":true,"model":"gpt-6.1-sol","interval_minutes":17,"credential_ids":["selected"]}`)); res.StatusCode != 200 {
		t.Fatalf("observation policy: %s", res.Body)
	}
	before, generation, timer := automation, automationGeneration, automationTimer
	persisted, err := os.ReadFile(statePath + ".automation.json")
	if err != nil {
		t.Fatal(err)
	}
	res := updateAutomation([]byte(`{"enabled":true,"auto_disable":true,"model":"deepseek-flash","interval_minutes":30,"credential_ids":["other"]}`))
	if res.StatusCode != 400 {
		t.Fatalf("auto-disable without management returned %d: %s", res.StatusCode, res.Body)
	}
	if !reflect.DeepEqual(automation, before) || automationGeneration != generation || automationTimer != timer {
		t.Fatalf("rejected update changed policy or scheduling: %+v", automation)
	}
	after, err := os.ReadFile(statePath + ".automation.json")
	if err != nil || string(after) != string(persisted) {
		t.Fatalf("rejected update changed persisted policy: %v", err)
	}
}

func TestAutomationBadPersistenceAndUpdateRollback(t *testing.T) {
	t.Run("corrupt", func(t *testing.T) {
		setupTest(t)
		data := []byte(`{"config":`)
		if err := os.WriteFile(statePath+".automation.json", data, 0600); err != nil {
			t.Fatal(err)
		}
		loadAutomation()
		if automationLoadError == nil || automation.Config.Enabled || automation.LastError == "" {
			t.Fatal("corrupt settings did not fail closed")
		}
		if res := runAutomationResponse(); res.StatusCode != 409 {
			t.Fatalf("run status = %d", res.StatusCode)
		}
		body := []byte(`{"model":"gpt-6.1-sol","interval_minutes":30}`)
		if res := updateAutomation(body); res.StatusCode != 500 {
			t.Fatalf("update status = %d", res.StatusCode)
		}
		got, err := os.ReadFile(statePath + ".automation.json")
		if err != nil || string(got) != string(data) {
			t.Fatal("corrupt settings overwritten")
		}
	})
	t.Run("rollback", func(t *testing.T) {
		setupTest(t)
		before := automation.Config
		statePath = filepath.Join(t.TempDir(), "missing", "state.json")
		res := updateAutomation([]byte(`{"model":"gpt-6.1-sol","interval_minutes":30,"enabled":true}`))
		if res.StatusCode != 500 || !reflect.DeepEqual(automation.Config, before) || automationGeneration != 0 || automationTimer != nil {
			t.Fatalf("failed update changed policy: %+v", automation)
		}
	})
}

func TestAutomationSerializedSelectionAndOverlap(t *testing.T) {
	setupTest(t)
	automation.Config = automationConfig{Model: "gpt-6.1-sol", IntervalMinutes: 30}
	automationTestDisable(t, func(credential) error { t.Error("failed requests must never disable"); return nil })
	gate, started := make(chan struct{}), make(chan string, 20)
	var release sync.Once
	defer func() { release.Do(func() { close(gate) }); tasks.Wait() }()
	var callsMu sync.Mutex
	var calls []string
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		if method == "host.auth.list" {
			return json.RawMessage(`{"files":[
			{"id":"a","provider":"codex","source":"file"},
			{"id":"b","provider":"codex","source":"file"},
			{"id":"disabled","provider":"codex","disabled":true},
			{"id":"cooling","provider":"codex","unavailable":true,"next_retry_after":"2999-01-01T00:00:00Z"},
			{"id":"configured","provider":"codex","runtime_only":true},
			{"id":"other","provider":"claude"}]}`), nil
		}
		if method != "host.model.execute_stream" {
			t.Errorf("unexpected call %s", method)
			return nil, errors.New("unexpected call")
		}
		id := payload.(map[string]any)["auth_id"].(string)
		callsMu.Lock()
		calls = append(calls, id)
		callsMu.Unlock()
		started <- id
		<-gate
		return nil, errors.New("synthetic request error")
	}
	if res := runAutomationResponse(); res.StatusCode != 200 {
		t.Fatalf("start: %s", res.Body)
	}
	select {
	case id := <-started:
		if id != "a" {
			t.Fatalf("first auth = %s", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not start")
	}
	if res := runAutomationResponse(); res.StatusCode != 409 {
		t.Fatalf("overlap status = %d", res.StatusCode)
	}
	manual := fingerprintRunResponse([]byte(`{"credential_ids":["a"],"model":"gpt-6.1-sol","concurrency":1}`))
	var summary runSummary
	if err := json.Unmarshal(manual.Body, &summary); err != nil || manual.StatusCode != 200 || summary.Started != 0 || summary.Busy != 1 {
		t.Fatalf("manual launch must report automation-owned credential busy: %s, %v", manual.Body, err)
	}
	select {
	case id := <-started:
		t.Fatalf("concurrent model request for %s", id)
	case <-time.After(30 * time.Millisecond):
	}
	release.Do(func() { close(gate) })
	tasks.Wait()
	callsMu.Lock()
	defer callsMu.Unlock()
	if !reflect.DeepEqual(calls, []string{"a", "a", "a", "b", "b", "b"}) {
		t.Fatalf("serialized eligible selection = %v", calls)
	}
	if automation.Running || len(running) != 0 || automation.LastFinished == nil || len(automation.Events) != 2 {
		t.Fatalf("run did not finish cleanly: %+v", automation)
	}
	for _, e := range automation.Events {
		if e.Status != "inconclusive" {
			t.Fatalf("failed request verdict: %+v", e)
		}
	}
}

func TestAutomationCancellationAndQuiesce(t *testing.T) {
	for _, action := range []string{"cancel", "update", "quiesce"} {
		t.Run(action, func(t *testing.T) {
			setupTest(t)
			automation.Config = automationConfig{Model: "gpt-6.1-sol", IntervalMinutes: 30}
			automationTestDisable(t, func(credential) error { t.Error("cancelled run disabled credential"); return nil })
			started := make(chan struct{}, 3)
			hostCall = streamHost(func(method string, _ any) (json.RawMessage, error) {
				if method == "host.auth.list" {
					return json.RawMessage(`{"files":[{"id":"a","provider":"codex"}]}`), nil
				}
				if method != "host.model.execute_stream" {
					t.Errorf("unexpected call %s", method)
					return nil, errors.New("unexpected call")
				}
				started <- struct{}{}
				return nil, nil
			})
			if res := runAutomationResponse(); res.StatusCode != 200 {
				t.Fatalf("start: %s", res.Body)
			}
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("run did not start")
			}
			switch action {
			case "cancel":
				if res := cancelAutomationResponse(); res.StatusCode != 200 {
					t.Fatal("cancel failed")
				}
			case "update":
				if res := updateAutomation([]byte(`{"model":"deepseek-flash","interval_minutes":30}`)); res.StatusCode != 200 {
					t.Fatal("update failed")
				}
			case "quiesce":
				Quiesce()
			}
			tasks.Wait()
			if automation.Running || len(running) != 0 || len(automation.Events) != 0 {
				t.Fatalf("cancelled run acted or leaked: %+v", automation)
			}
			if action == "quiesce" {
				if runCtx.Err() == nil || automationTimer != nil || runAutomationResponse().StatusCode != 409 {
					t.Fatal("quiesced plugin accepted automation")
				}
			}
		})
	}
}

func TestAutomationCredentialFilter(t *testing.T) {
	c := automationConfig{CredentialIDs: []string{"selected"}}
	base := credential{ID: "selected", Provider: "codex", Source: credentialSourceFile}
	for _, tc := range []struct {
		name   string
		change func(*credential)
		want   bool
	}{
		{"selected", func(*credential) {}, true},
		{"unselected", func(a *credential) { a.ID = "other" }, false},
		{"disabled", func(a *credential) { a.Disabled = true }, false},
		{"cooldown", func(a *credential) { a.Unavailable = true }, false},
		{"provider-config", func(a *credential) { a.Source = credentialSourceProvider }, false},
		{"other-provider", func(a *credential) { a.Provider = "claude" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := base
			tc.change(&a)
			if got := automaticCredential(a, c); got != tc.want {
				t.Fatalf("eligible = %v, want %v", got, tc.want)
			}
		})
	}
	if !automaticCredential(base, automationConfig{}) {
		t.Fatal("empty selection must include eligible files")
	}
}

func TestAutomationFailedStartAndScheduledStop(t *testing.T) {
	t.Run("failed-start", func(t *testing.T) {
		setupTest(t)
		automation.Config = automationConfig{Model: "gpt-6.1-sol", IntervalMinutes: 30}
		statePath = filepath.Join(t.TempDir(), "missing", "state.json")
		if res := runAutomationResponse(); res.StatusCode != 409 || automation.Running || automationCancel != nil || automation.LastError == "" {
			t.Fatalf("failed persistence launched run: %+v", automation)
		}
	})
	t.Run("scheduled-stop", func(t *testing.T) {
		setupTest(t)
		if res := updateAutomation([]byte(`{"model":"gpt-6.1-sol","interval_minutes":30,"enabled":true}`)); res.StatusCode != 200 {
			t.Fatal("update failed")
		}
		if automationTimer == nil {
			t.Fatal("enabled config has no timer")
		}
		Quiesce()
		if automationTimer != nil || runCtx.Err() == nil {
			t.Fatal("quiesce left timer armed")
		}
		HandleMethod("plugin.reconfigure", nil)
		if automationTimer == nil || runCtx.Err() != nil {
			t.Fatal("reconfigure did not rearm enabled policy")
		}
	})
}

func TestAutomationDisableReceivesTestedRevision(t *testing.T) {
	setupTest(t)
	c := automationConfig{Model: "gpt-6.1-sol", IntervalMinutes: 30, AutoDisable: true, CredentialIDs: []string{"selected-id"}}
	automation.Config = c
	auth := credential{
		ID: "selected-id", AuthIndex: "selected-index", Name: "selected.json", Provider: "codex", Source: credentialSourceFile,
		AutomationRevision: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	calls := 0
	automationTestDisable(t, func(got credential) error {
		calls++
		if !reflect.DeepEqual(got, auth) {
			t.Errorf("disable received refreshed identity instead of tested credential and revision: got %+v, want %+v", got, auth)
		}
		return nil
	})
	hostCall = func(method string, _ any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			t.Errorf("unexpected host call %s", method)
			return nil, errors.New("unexpected host call")
		}
		// Inventory refresh cannot supply the pre-test full-JSON revision.
		return json.Marshal(map[string]any{"files": []hostAuthFile{
			{ID: "other-id", AuthIndex: "other-index", Name: "other.json", Provider: "codex", Source: "file"},
			{ID: auth.ID, AuthIndex: auth.AuthIndex, Name: auth.Name, Provider: auth.Provider, Source: "file", Priority: 42},
		}})
	}
	if !applyAutomationResult(context.Background(), c, automationGeneration, auth, automationTestRecord()) {
		t.Fatal("matching tested identity stopped policy application")
	}
	if calls != 1 || len(automation.Events) == 0 || automation.Events[len(automation.Events)-1].Status != "disabled" {
		t.Fatalf("disable calls = %d, events = %+v", calls, automation.Events)
	}
}
