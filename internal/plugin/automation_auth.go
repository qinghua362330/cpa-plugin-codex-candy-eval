package plugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
)

type automaticAuthFile struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name"`
	Path      string          `json:"path"`
	JSON      json.RawMessage `json:"json"`
	fields    map[string]json.RawMessage
}

func automaticCredentialSnapshot(auth credential) (hostAuthFile, string, error) {
	if hostCall == nil || auth.Source != credentialSourceFile || auth.Provider != "codex" ||
		!automaticAuthIdentifier(auth.ID) || !automaticAuthIdentifier(auth.AuthIndex) || !automaticAuthName(auth.Name) {
		return hostAuthFile{}, "", errors.New("automatic disable skipped: invalid file credential")
	}
	entry, err := automaticAuthInventory(auth)
	if err != nil {
		return hostAuthFile{}, "", err
	}
	file, err := readAutomaticAuthFile(entry)
	if err != nil {
		return hostAuthFile{}, "", err
	}
	// Hash stable identity and the physical JSON, not volatile cooldown/timestamps.
	hash := sha256.New()
	_ = json.NewEncoder(hash).Encode([]string{entry.ID, entry.AuthIndex, entry.Name, entry.Path, entry.Provider})
	_, _ = hash.Write(file.JSON)
	return entry, fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func automaticCredentialRevision(auth credential) (string, error) {
	_, revision, err := automaticCredentialSnapshot(auth)
	return revision, err
}

// The caller holds automationActionMu across validation and this action, so
// explicit management configuration updates cannot race an in-flight disable.
func disableAutomaticCredential(auth credential) error {
	if auth.AutomationRevision == "" {
		return errors.New("automatic disable skipped: missing credential revision")
	}
	config, err := readAutomationManagement()
	if err != nil {
		return errors.New("automatic disable skipped: management configuration unavailable")
	}
	entry, revision, err := automaticCredentialSnapshot(auth)
	if err != nil {
		return err
	}
	if revision != auth.AutomationRevision {
		return errors.New("automatic disable skipped: credential changed")
	}
	if err := patchAutomaticCredential(config, entry); err != nil {
		return err
	}
	verified, err := automaticAuthInventory(auth)
	if err != nil || !verified.Disabled || verified.Path != entry.Path {
		return errors.New("automatic disable uncertain: runtime verification failed")
	}
	persisted, err := readAutomaticAuthFile(verified)
	if err != nil || !bytes.Equal(bytes.TrimSpace(persisted.fields["disabled"]), []byte("true")) {
		return errors.New("automatic disable uncertain: persistence verification failed")
	}
	return nil
}

func automaticAuthIdentifier(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n\t")
}

func automaticAuthName(name string) bool {
	return automaticAuthIdentifier(name) && !strings.ContainsAny(name, "/\\:") && strings.HasSuffix(name, ".json")
}

func automaticAuthInventory(auth credential) (hostAuthFile, error) {
	invalid := errors.New("automatic disable skipped: physical credential could not be confirmed")
	raw, err := hostCall("host.auth.list", map[string]any{})
	if err != nil {
		return hostAuthFile{}, invalid
	}
	var list struct {
		Files []hostAuthFile `json:"files"`
	}
	if json.Unmarshal(raw, &list) != nil || list.Files == nil {
		return hostAuthFile{}, invalid
	}
	var entry hostAuthFile
	matches := 0
	for _, candidate := range list.Files {
		if candidate.ID == auth.ID {
			entry = candidate
			matches++
		}
	}
	if matches != 1 || entry.AuthIndex != auth.AuthIndex || entry.Name != auth.Name ||
		entry.Provider != "codex" || (entry.Type != "" && entry.Type != "codex") ||
		entry.RuntimeOnly || entry.Source != "file" || !automaticAuthIdentifier(entry.Path) ||
		strings.ContainsAny(entry.Path, "\\:") || !path.IsAbs(entry.Path) || path.Clean(entry.Path) != entry.Path || path.Base(entry.Path) != entry.Name {
		return hostAuthFile{}, invalid
	}
	for _, candidate := range list.Files {
		if candidate.ID == entry.ID {
			continue
		}
		// Compare names as well as normalized paths: virtual children may not
		// expose a path, and case-insensitive hosts may alias file names.
		if strings.EqualFold(candidate.Name, entry.Name) || candidate.AuthIndex == entry.AuthIndex ||
			(candidate.Path != "" && strings.EqualFold(path.Clean(candidate.Path), entry.Path)) {
			return hostAuthFile{}, invalid
		}
	}
	return entry, nil
}

func readAutomaticAuthFile(entry hostAuthFile) (automaticAuthFile, error) {
	invalid := errors.New("automatic disable skipped: physical credential could not be read safely")
	raw, err := hostCall("host.auth.get", map[string]string{"auth_index": entry.AuthIndex})
	if err != nil {
		return automaticAuthFile{}, invalid
	}
	var file automaticAuthFile
	if json.Unmarshal(raw, &file) != nil || file.AuthIndex != entry.AuthIndex || file.Name != entry.Name || file.Path != entry.Path ||
		json.Unmarshal(file.JSON, &file.fields) != nil || file.fields == nil {
		return automaticAuthFile{}, invalid
	}
	var provider string
	if json.Unmarshal(file.fields["type"], &provider) != nil || provider != "codex" {
		return automaticAuthFile{}, invalid
	}
	return file, nil
}
