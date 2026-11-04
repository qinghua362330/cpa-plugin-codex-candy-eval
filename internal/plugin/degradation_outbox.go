package plugin

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	degradationConfigLimit = 64 << 10
	degradationBodyLimit   = 64 << 10
	degradationHistoryMax  = 500
)

type degradationWebhookConfig struct {
	Enabled        bool   `json:"enabled"`
	Endpoint       string `json:"endpoint"`
	Secret         string `json:"secret"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

type degradationWebhookPublic struct {
	Enabled        bool   `json:"enabled"`
	Endpoint       string `json:"endpoint"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Ready          bool   `json:"ready"`
}

type degradationEventPayload struct {
	EventID            string    `json:"event_id"`
	CredentialID       string    `json:"credential_id"`
	CredentialRevision string    `json:"credential_revision"`
	Email              string    `json:"email"`
	ExpectedModel      string    `json:"expected_model"`
	DetectedModel      string    `json:"detected_model"`
	Confidence         float64   `json:"confidence"`
	ObservedAt         time.Time `json:"observed_at"`
	Status             string    `json:"status"`
}

type degradationOutboxEvent struct {
	Payload     degradationEventPayload `json:"payload"`
	State       string                  `json:"state"`
	Attempts    int                     `json:"attempts"`
	NextAttempt time.Time               `json:"next_attempt,omitempty"`
	LastAttempt *time.Time              `json:"last_attempt,omitempty"`
	DeliveredAt *time.Time              `json:"delivered_at,omitempty"`
	LastError   string                  `json:"last_error,omitempty"`
}

type degradationOutboxState struct {
	Events []degradationOutboxEvent `json:"events"`
}

var (
	degradationMu        sync.Mutex
	degradationLoaded    bool
	degradationLoadError error
	degradationOutbox    = degradationOutboxState{Events: []degradationOutboxEvent{}}
	degradationWake      = make(chan struct{}, 1)
	degradationCancel    context.CancelFunc
	degradationDone      chan struct{}
	degradationRetryBase = 30 * time.Second
	degradationHTTPDo    = func(client *http.Client, request *http.Request) (*http.Response, error) { return client.Do(request) }
)

func degradationConfigPath() string { return statePath + ".degradation-webhook.json" }
func degradationOutboxPath() string { return statePath + ".degradation-outbox.json" }

func defaultDegradationConfig() degradationWebhookConfig {
	return degradationWebhookConfig{TimeoutSeconds: 10}
}

func validateDegradationEndpoint(raw string) bool {
	if raw == "" || len(raw) > 2048 || strings.TrimSpace(raw) != raw {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Opaque != "" || u.Host == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path == "" {
		return false
	}
	if u.Path != "/internal/v1/cpa/degradation-events" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateDegradationConfig(cfg *degradationWebhookConfig) error {
	cfg.Endpoint = strings.TrimSpace(cfg.Endpoint)
	if cfg.TimeoutSeconds == 0 {
		cfg.TimeoutSeconds = 10
	}
	if cfg.TimeoutSeconds < 1 || cfg.TimeoutSeconds > 60 {
		return errors.New("webhook timeout must be 1-60 seconds")
	}
	if cfg.Enabled && (!validateDegradationEndpoint(cfg.Endpoint) || len(cfg.Secret) < 16 || len(cfg.Secret) > 8192 || strings.TrimSpace(cfg.Secret) != cfg.Secret) {
		return errors.New("enabled webhook requires a valid endpoint and secret")
	}
	if cfg.Endpoint != "" && !validateDegradationEndpoint(cfg.Endpoint) {
		return errors.New("invalid webhook endpoint")
	}
	return nil
}

func readDegradationConfigLocked() (degradationWebhookConfig, error) {
	data, err := readPrivateFile(degradationConfigPath(), degradationConfigLimit)
	if errors.Is(err, os.ErrNotExist) {
		return defaultDegradationConfig(), nil
	}
	if err != nil {
		return degradationWebhookConfig{}, err
	}
	var cfg degradationWebhookConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cfg) != nil || decoder.Decode(new(any)) != io.EOF || validateDegradationConfig(&cfg) != nil {
		return degradationWebhookConfig{}, errors.New("degradation webhook configuration unavailable")
	}
	return cfg, nil
}

func readPrivateFile(name string, limit int64) ([]byte, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("private file permissions are invalid")
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("private file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("private file is invalid")
	}
	return data, nil
}

func writePrivateJSON(name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(name), ".degradation-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, name)
}

func degradationPublic(cfg degradationWebhookConfig) degradationWebhookPublic {
	return degradationWebhookPublic{Enabled: cfg.Enabled, Endpoint: cfg.Endpoint, TimeoutSeconds: cfg.TimeoutSeconds, Ready: cfg.Enabled && cfg.Secret != ""}
}

func degradationWebhookResponse() managementResponse {
	degradationMu.Lock()
	defer degradationMu.Unlock()
	cfg, err := readDegradationConfigLocked()
	if err != nil {
		return jsonResponse(http.StatusOK, degradationWebhookPublic{})
	}
	return jsonResponse(http.StatusOK, degradationPublic(cfg))
}

func updateDegradationWebhook(body []byte) managementResponse {
	if len(body) > degradationConfigLimit {
		return jsonError(http.StatusBadRequest, "webhook configuration is too large")
	}
	var cfg degradationWebhookConfig
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cfg) != nil || decoder.Decode(new(any)) != io.EOF {
		return jsonError(http.StatusBadRequest, "invalid webhook configuration")
	}
	degradationMu.Lock()
	defer degradationMu.Unlock()
	if cfg.Secret == "" {
		old, err := readDegradationConfigLocked()
		if err == nil && old.Endpoint == strings.TrimSpace(cfg.Endpoint) {
			cfg.Secret = old.Secret
		}
	}
	if err := validateDegradationConfig(&cfg); err != nil {
		return jsonError(http.StatusBadRequest, err.Error())
	}
	if err := writePrivateJSON(degradationConfigPath(), cfg); err != nil {
		return jsonError(http.StatusInternalServerError, "webhook configuration could not be saved")
	}
	wakeDegradationWorker()
	return jsonResponse(http.StatusOK, degradationPublic(cfg))
}

func loadDegradationOutbox() {
	degradationMu.Lock()
	defer degradationMu.Unlock()
	if degradationLoaded {
		return
	}
	degradationLoaded = true
	data, err := readPrivateFile(degradationOutboxPath(), 4<<20)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err == nil {
		err = json.Unmarshal(data, &degradationOutbox)
	}
	if err != nil || degradationOutbox.Events == nil {
		degradationLoadError = errors.New("degradation outbox unavailable")
		degradationOutbox = degradationOutboxState{Events: []degradationOutboxEvent{}}
	}
}

func saveDegradationOutboxLocked() error {
	if degradationLoadError != nil {
		return degradationLoadError
	}
	return writePrivateJSON(degradationOutboxPath(), degradationOutbox)
}

func newDegradationEventID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	text := hex.EncodeToString(raw[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", text[:8], text[8:12], text[12:16], text[16:20], text[20:]), nil
}

func queueDegradationEvent(auth credential, expected, detected string, confidence float64, observedAt time.Time) error {
	degradationMu.Lock()
	cfg, err := readDegradationConfigLocked()
	if err != nil {
		degradationMu.Unlock()
		return err
	}
	if !cfg.Enabled {
		degradationMu.Unlock()
		return nil
	}
	email := strings.ToLower(strings.TrimSpace(auth.Email))
	if strings.TrimSpace(auth.ID) == "" || strings.TrimSpace(auth.AutomationRevision) == "" || email == "" {
		degradationMu.Unlock()
		return errors.New("degradation event identity is incomplete")
	}
	pending := 0
	for _, event := range degradationOutbox.Events {
		if event.State == "pending" {
			pending++
		}
	}
	if pending >= degradationHistoryMax {
		degradationMu.Unlock()
		return errors.New("degradation event outbox is full")
	}
	id, err := newDegradationEventID()
	if err != nil {
		degradationMu.Unlock()
		return err
	}
	now := time.Now().UTC()
	event := degradationOutboxEvent{Payload: degradationEventPayload{
		EventID: id, CredentialID: auth.ID, CredentialRevision: auth.AutomationRevision,
		Email: email, ExpectedModel: expected,
		DetectedModel: detected, Confidence: confidence, ObservedAt: observedAt.UTC(), Status: "quarantined",
	}, State: "pending", NextAttempt: now}
	degradationOutbox.Events = append(degradationOutbox.Events, event)
	trimDegradationHistoryLocked()
	err = saveDegradationOutboxLocked()
	if err != nil {
		for i := len(degradationOutbox.Events) - 1; i >= 0; i-- {
			if degradationOutbox.Events[i].Payload.EventID == id {
				degradationOutbox.Events = append(degradationOutbox.Events[:i], degradationOutbox.Events[i+1:]...)
				break
			}
		}
	}
	degradationMu.Unlock()
	if err == nil {
		wakeDegradationWorker()
	}
	return err
}

func trimDegradationHistoryLocked() {
	if len(degradationOutbox.Events) <= degradationHistoryMax {
		return
	}
	drop := len(degradationOutbox.Events) - degradationHistoryMax
	kept := make([]degradationOutboxEvent, 0, len(degradationOutbox.Events)-drop)
	for _, event := range degradationOutbox.Events {
		if drop > 0 && event.State == "delivered" {
			drop--
			continue
		}
		kept = append(kept, event)
	}
	degradationOutbox.Events = kept
}

func degradationEventsResponse() managementResponse {
	degradationMu.Lock()
	defer degradationMu.Unlock()
	copyEvents := append([]degradationOutboxEvent{}, degradationOutbox.Events...)
	return jsonResponse(http.StatusOK, map[string]any{"events": copyEvents, "storage_error": errorText(degradationLoadError)})
}

func retryDegradationEvent(body []byte) managementResponse {
	var request struct {
		EventID string `json:"event_id"`
	}
	if json.Unmarshal(body, &request) != nil || request.EventID == "" {
		return jsonError(http.StatusBadRequest, "event_id is required")
	}
	degradationMu.Lock()
	for i := range degradationOutbox.Events {
		if degradationOutbox.Events[i].Payload.EventID != request.EventID {
			continue
		}
		degradationOutbox.Events[i].State = "pending"
		degradationOutbox.Events[i].NextAttempt = time.Now().UTC()
		degradationOutbox.Events[i].LastError = ""
		if err := saveDegradationOutboxLocked(); err != nil {
			degradationMu.Unlock()
			return jsonError(http.StatusInternalServerError, "event retry could not be saved")
		}
		result := jsonResponse(http.StatusAccepted, degradationOutbox.Events[i])
		degradationMu.Unlock()
		wakeDegradationWorker()
		return result
	}
	degradationMu.Unlock()
	return jsonError(http.StatusNotFound, "event not found")
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func wakeDegradationWorker() {
	select {
	case degradationWake <- struct{}{}:
	default:
	}
}

func startDegradationWorker() {
	degradationMu.Lock()
	if degradationDone != nil {
		degradationMu.Unlock()
		wakeDegradationWorker()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	degradationCancel = cancel
	degradationDone = make(chan struct{})
	done := degradationDone
	degradationMu.Unlock()
	go func() {
		defer close(done)
		for {
			wait := nextDegradationWait()
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-degradationWake:
				timer.Stop()
			case <-timer.C:
			}
			deliverOneDegradationEvent(ctx)
		}
	}()
}

func stopDegradationWorker() {
	degradationMu.Lock()
	cancel, done := degradationCancel, degradationDone
	degradationCancel, degradationDone = nil, nil
	degradationMu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func nextDegradationWait() time.Duration {
	degradationMu.Lock()
	defer degradationMu.Unlock()
	now := time.Now().UTC()
	wait := time.Hour
	for _, event := range degradationOutbox.Events {
		if event.State != "pending" {
			continue
		}
		if !event.NextAttempt.After(now) {
			return 0
		}
		if candidate := time.Until(event.NextAttempt); candidate < wait {
			wait = candidate
		}
	}
	return wait
}

func deliverOneDegradationEvent(ctx context.Context) {
	degradationMu.Lock()
	cfg, cfgErr := readDegradationConfigLocked()
	now := time.Now().UTC()
	index := -1
	if cfgErr == nil && cfg.Enabled {
		for i := range degradationOutbox.Events {
			if degradationOutbox.Events[i].State == "pending" && !degradationOutbox.Events[i].NextAttempt.After(now) {
				index = i
				break
			}
		}
	}
	if index < 0 {
		degradationMu.Unlock()
		return
	}
	event := degradationOutbox.Events[index]
	degradationMu.Unlock()

	err := sendDegradationEvent(ctx, cfg, event.Payload)
	finished := time.Now().UTC()
	degradationMu.Lock()
	defer degradationMu.Unlock()
	if index >= len(degradationOutbox.Events) || degradationOutbox.Events[index].Payload.EventID != event.Payload.EventID {
		return
	}
	current := &degradationOutbox.Events[index]
	current.Attempts++
	current.LastAttempt = &finished
	if err == nil {
		current.State, current.LastError, current.DeliveredAt = "delivered", "", &finished
	} else {
		current.State = "pending"
		current.LastError = "delivery failed"
		delay := degradationRetryBase << min(current.Attempts-1, 6)
		if delay > time.Hour {
			delay = time.Hour
		}
		current.NextAttempt = finished.Add(delay)
	}
	_ = saveDegradationOutboxLocked()
}

func sendDegradationEvent(ctx context.Context, cfg degradationWebhookConfig, payload degradationEventPayload) error {
	body, err := json.Marshal(payload)
	if err != nil || len(body) > degradationBodyLimit {
		return errors.New("event encoding failed")
	}
	timestamp := fmt.Sprintf("%d", time.Now().UTC().Unix())
	mac := hmac.New(sha256.New, []byte(cfg.Secret))
	_, _ = mac.Write([]byte(timestamp + "\n"))
	_, _ = mac.Write(body)
	signature := hex.EncodeToString(mac.Sum(nil))
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CPA-Event-ID", payload.EventID)
	request.Header.Set("X-CPA-Timestamp", timestamp)
	request.Header.Set("X-CPA-Signature", signature)
	client := &http.Client{Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := degradationHTTPDo(client, request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, degradationBodyLimit))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("webhook status %d", response.StatusCode)
	}
	return nil
}
