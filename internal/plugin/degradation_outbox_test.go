package plugin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDegradationWebhookConfigurationDoesNotExposeSecret(t *testing.T) {
	setupTest(t)
	body := []byte(`{"enabled":true,"endpoint":"https://control.example/internal/v1/cpa/degradation-events","secret":"0123456789abcdef","timeout_seconds":7}`)
	response := updateDegradationWebhook(body)
	if response.StatusCode != http.StatusOK || strings.Contains(string(response.Body), "0123456789abcdef") {
		t.Fatalf("save response=%d %s", response.StatusCode, response.Body)
	}
	response = degradationWebhookResponse()
	if response.StatusCode != http.StatusOK || strings.Contains(string(response.Body), "secret") || strings.Contains(string(response.Body), "0123456789abcdef") {
		t.Fatalf("status leaked secret: %s", response.Body)
	}
	info, err := os.Stat(degradationConfigPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions=%v err=%v", info, err)
	}
}

func TestDegradationDeliverySignsRawBodyAndKeepsEventID(t *testing.T) {
	setupTest(t)
	secret := "0123456789abcdef"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload degradationEventPayload
		if json.Unmarshal(body, &payload) != nil || payload.EventID == "" || r.Header.Get("X-CPA-Event-ID") != payload.EventID {
			t.Fatalf("invalid payload/header: %s %q", body, r.Header.Get("X-CPA-Event-ID"))
		}
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte(r.Header.Get("X-CPA-Timestamp") + "\n"))
		_, _ = mac.Write(body)
		if !hmac.Equal([]byte(r.Header.Get("X-CPA-Signature")), []byte(hex.EncodeToString(mac.Sum(nil)))) {
			t.Fatal("invalid signature")
		}
		if calls.Add(1) == 1 {
			http.Error(w, "retry", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	response := updateDegradationWebhook([]byte(`{"enabled":true,"endpoint":"` + server.URL + `/internal/v1/cpa/degradation-events","secret":"` + secret + `","timeout_seconds":2}`))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("configure=%d %s", response.StatusCode, response.Body)
	}
	if err := queueDegradationEvent(credential{ID: "credential-1", Email: "User@Example.com", AutomationRevision: "revision-hash"}, "gpt-6.1-sol", "gpt-6-luna", 0.97, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	deliverOneDegradationEvent(t.Context())
	degradationMu.Lock()
	firstID := degradationOutbox.Events[0].Payload.EventID
	degradationOutbox.Events[0].NextAttempt = time.Now().Add(-time.Second)
	degradationMu.Unlock()
	deliverOneDegradationEvent(t.Context())
	degradationMu.Lock()
	defer degradationMu.Unlock()
	event := degradationOutbox.Events[0]
	if event.Payload.EventID != firstID || event.State != "delivered" || event.Attempts != 2 || event.Payload.Email != "user@example.com" || event.Payload.Status != "quarantined" {
		t.Fatalf("event=%+v", event)
	}
}

func TestDegradationHistoryNeverDropsPendingEvents(t *testing.T) {
	setupTest(t)
	for i := 0; i < degradationHistoryMax+5; i++ {
		state := "delivered"
		if i == 0 {
			state = "pending"
		}
		degradationOutbox.Events = append(degradationOutbox.Events, degradationOutboxEvent{Payload: degradationEventPayload{EventID: string(rune(i + 1))}, State: state})
	}
	trimDegradationHistoryLocked()
	if len(degradationOutbox.Events) != degradationHistoryMax || degradationOutbox.Events[0].State != "pending" {
		t.Fatalf("history length=%d first=%+v", len(degradationOutbox.Events), degradationOutbox.Events[0])
	}
}

func TestDegradationQueueRejectsIncompleteIdentityAndRollsBackFailedPersistence(t *testing.T) {
	setupTest(t)
	if response := updateDegradationWebhook([]byte(`{"enabled":true,"endpoint":"https://control.example/internal/v1/cpa/degradation-events","secret":"0123456789abcdef","timeout_seconds":7}`)); response.StatusCode != http.StatusOK {
		t.Fatalf("configure=%d %s", response.StatusCode, response.Body)
	}
	for _, auth := range []credential{
		{ID: "credential-1", AutomationRevision: "revision-1"},
		{ID: "credential-1", Email: "user@example.com"},
	} {
		if err := queueDegradationEvent(auth, "gpt-6.1-sol", "gpt-6-luna", 0.9, time.Now()); err == nil {
			t.Fatal("incomplete event identity was accepted")
		}
	}
	degradationLoadError = io.ErrUnexpectedEOF
	if err := queueDegradationEvent(credential{ID: "credential-1", Email: "user@example.com", AutomationRevision: "revision-1"}, "gpt-6.1-sol", "gpt-6-luna", 0.9, time.Now()); err == nil {
		t.Fatal("outbox persistence failure was ignored")
	}
	if len(degradationOutbox.Events) != 0 {
		t.Fatalf("failed event remained in memory: %+v", degradationOutbox.Events)
	}
}

func TestDegradationQueueReportsInvalidPrivateConfiguration(t *testing.T) {
	setupTest(t)
	if err := os.WriteFile(degradationConfigPath(), []byte(`{"enabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := queueDegradationEvent(credential{ID: "credential-1", Email: "user@example.com", AutomationRevision: "revision-1"}, "gpt-6.1-sol", "gpt-6-luna", 0.9, time.Now()); err == nil {
		t.Fatal("invalid private configuration was silently ignored")
	}
}

func TestDegradationQueueRejectsWhenPendingOutboxIsFull(t *testing.T) {
	setupTest(t)
	if response := updateDegradationWebhook([]byte(`{"enabled":true,"endpoint":"https://control.example/internal/v1/cpa/degradation-events","secret":"0123456789abcdef","timeout_seconds":7}`)); response.StatusCode != http.StatusOK {
		t.Fatalf("configure=%d %s", response.StatusCode, response.Body)
	}
	for i := 0; i < degradationHistoryMax; i++ {
		degradationOutbox.Events = append(degradationOutbox.Events, degradationOutboxEvent{Payload: degradationEventPayload{EventID: string(rune(i + 1))}, State: "pending"})
	}
	if err := queueDegradationEvent(credential{ID: "credential-1", Email: "user@example.com", AutomationRevision: "revision-1"}, "gpt-6.1-sol", "gpt-6-luna", 0.9, time.Now()); err == nil {
		t.Fatal("full pending outbox accepted another event")
	}
	if len(degradationOutbox.Events) != degradationHistoryMax {
		t.Fatalf("outbox length=%d want %d", len(degradationOutbox.Events), degradationHistoryMax)
	}
}
