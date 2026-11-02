package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const automationManagementLimit = 64 << 10

type automationManagementConfig struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

type automationManagementPublic struct {
	BaseURL string `json:"base_url"`
	Ready   bool   `json:"ready"`
}

var automationManagementMu sync.Mutex

func validAutomationManagementURL(raw string) bool {
	if raw == "" || len(raw) > 2048 || strings.TrimSpace(raw) != raw {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Opaque != "" ||
		u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return false
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || ip.Zone() != "" || !ip.IsLoopback() {
		return false
	}
	// IPv6 origins require brackets; do not accept ambiguous unbracketed hosts.
	if ip.Is6() && !strings.HasPrefix(u.Host, "[") {
		return false
	}
	if strings.HasSuffix(u.Host, ":") {
		return false
	}
	if port := u.Port(); port != "" {
		for _, c := range port {
			if c < '0' || c > '9' {
				return false
			}
		}
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return false
		}
	}
	return true
}

func validAutomationManagementKey(key string) bool {
	if key == "" || len(key) > 8192 || strings.TrimSpace(key) != key {
		return false
	}
	for _, c := range key {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

func decodeAutomationManagement(data []byte, out any) error {
	if len(data) > automationManagementLimit {
		return errors.New("management configuration is invalid")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return errors.New("management configuration is invalid")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("management configuration is invalid")
	}
	return nil
}

// No configuration cache: readiness always reflects the separately persisted secret.
func readAutomationManagementLocked() (automationManagementConfig, error) {
	invalid := errors.New("management configuration unavailable")
	name := statePath + ".management.json"
	info, err := os.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return automationManagementConfig{}, invalid
	}
	f, err := os.Open(name)
	if err != nil {
		return automationManagementConfig{}, invalid
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return automationManagementConfig{}, invalid
	}
	data, err := io.ReadAll(io.LimitReader(f, automationManagementLimit+1))
	var cfg automationManagementConfig
	if err != nil || decodeAutomationManagement(data, &cfg) != nil || !validAutomationManagementURL(cfg.BaseURL) || !validAutomationManagementKey(cfg.APIKey) {
		return automationManagementConfig{}, invalid
	}
	return cfg, nil
}

func readAutomationManagement() (automationManagementConfig, error) {
	automationManagementMu.Lock()
	defer automationManagementMu.Unlock()
	return readAutomationManagementLocked()
}

func automationManagementStatus() automationManagementPublic {
	cfg, err := readAutomationManagement()
	if err != nil {
		return automationManagementPublic{}
	}
	return automationManagementPublic{BaseURL: cfg.BaseURL, Ready: true}
}

func automationManagementReady() bool { return automationManagementStatus().Ready }

func managementStatusResponse() managementResponse {
	return jsonResponse(http.StatusOK, automationManagementStatus())
}

func updateAutomationManagement(body []byte) managementResponse {
	var cfg automationManagementConfig
	if decodeAutomationManagement(body, &cfg) != nil || !validAutomationManagementURL(cfg.BaseURL) {
		return jsonError(http.StatusBadRequest, "invalid loopback management configuration")
	}
	automationActionMu.Lock()
	defer automationActionMu.Unlock()
	automationManagementMu.Lock()
	defer automationManagementMu.Unlock()
	if cfg.APIKey == "" {
		old, err := readAutomationManagementLocked()
		if err != nil || old.BaseURL != cfg.BaseURL {
			return jsonError(http.StatusBadRequest, "management key required for this origin")
		}
		cfg.APIKey = old.APIKey
	}
	if !validAutomationManagementKey(cfg.APIKey) {
		return jsonError(http.StatusBadRequest, "invalid management key")
	}
	if err := writeAutomationManagement(cfg); err != nil {
		return jsonError(http.StatusInternalServerError, "management configuration could not be saved")
	}
	return jsonResponse(http.StatusOK, automationManagementPublic{BaseURL: cfg.BaseURL, Ready: true})
}

func writeAutomationManagement(cfg automationManagementConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	name := statePath + ".management.json"
	f, err := os.CreateTemp(filepath.Dir(name), ".automation-management-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), name)
}

func newAutomationManagementClient() *http.Client {
	return &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			DisableKeepAlives:     true,
		},
	}
}

func patchAutomaticCredential(cfg automationManagementConfig, entry hostAuthFile) error {
	if !validAutomationManagementURL(cfg.BaseURL) || !validAutomationManagementKey(cfg.APIKey) {
		return errors.New("automatic disable skipped: management configuration unavailable")
	}
	// Only the host mutates current metadata under its per-auth mutation gate.
	body, _ := json.Marshal(map[string]any{"name": entry.Name, "auth_index": entry.AuthIndex, "disabled": true})
	req, err := http.NewRequest(http.MethodPatch, cfg.BaseURL+"/v0/management/auth-files/status", bytes.NewReader(body))
	if err != nil {
		return errors.New("automatic disable skipped: invalid management request")
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	client := newAutomationManagementClient()
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	uncertain := errors.New("automatic disable uncertain: management PATCH failed")
	if err != nil {
		return uncertain
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, automationManagementLimit+1))
	if err != nil || len(data) > automationManagementLimit || resp.StatusCode != http.StatusOK {
		return uncertain
	}
	var result struct {
		Status   string `json:"status"`
		Disabled *bool  `json:"disabled"`
	}
	if json.Unmarshal(data, &result) != nil || result.Status != "ok" || result.Disabled == nil || !*result.Disabled {
		return uncertain
	}
	return nil
}
