package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

type automationConfig struct {
	Enabled         bool     `json:"enabled"`
	Model           string   `json:"model"`
	IntervalMinutes int      `json:"interval_minutes"`
	AutoDisable     bool     `json:"auto_disable"`
	CredentialIDs   []string `json:"credential_ids"`
}

type automationEvent struct {
	Time         time.Time `json:"time"`
	CredentialID string    `json:"credential_id"`
	Model        string    `json:"model"`
	Prediction   string    `json:"prediction,omitempty"`
	Probability  *float64  `json:"probability,omitempty"`
	Status       string    `json:"status"`
	Message      string    `json:"message,omitempty"`
}

type automationState struct {
	Config       automationConfig  `json:"config"`
	Running      bool              `json:"running"`
	NextRun      *time.Time        `json:"next_run"`
	LastStarted  *time.Time        `json:"last_started"`
	LastFinished *time.Time        `json:"last_finished"`
	LastError    string            `json:"last_error"`
	Events       []automationEvent `json:"events"`
}

func defaultAutomation() automationState {
	return automationState{Config: automationConfig{IntervalMinutes: 30, CredentialIDs: []string{}}, Events: []automationEvent{}}
}

// mu protects settings, timers and audit history. actionMu serializes configuration/cancellation with
// credential changes: once a save or cancel returns, an older run cannot disable another credential.
var (
	automation           = defaultAutomation()
	automationLoaded     bool
	automationLoadError  error
	automationTimer      *time.Timer
	automationCancel     context.CancelFunc
	automationGeneration uint64
	automationActionMu   sync.Mutex
	automationDisable    = disableAutomaticCredential
)

func validateAutomation(c *automationConfig) error {
	c.Model = strings.TrimSpace(c.Model)
	if c.IntervalMinutes < 1 || c.IntervalMinutes > 10080 {
		return errors.New("巡检间隔须为 1–10080 分钟")
	}
	if c.Model != "" && (!validModelName(c.Model, false) || !slices.Contains(sharedDetector.ModelIDs, fingerprintModelID(c.Model))) {
		return errors.New("请选择指纹参考库支持的模型（支持已有模型别名）")
	}
	if (c.Enabled || c.AutoDisable) && c.Model == "" {
		return errors.New("请先选择默认检测模型")
	}
	if len(c.CredentialIDs) > 10000 {
		return errors.New("凭证数量超限")
	}
	ids := make([]string, 0, len(c.CredentialIDs))
	for _, id := range c.CredentialIDs {
		if strings.TrimSpace(id) == "" || len(id) > 512 || strings.ContainsAny(id, "\x00\r\n") {
			return errors.New("凭证标识无效")
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	c.CredentialIDs = slices.Compact(ids)
	return nil
}

// Settings live beside, rather than inside, the upstream history, so clearing test results or rolling
// back the plugin cannot silently discard the automatic-disable policy or its audit trail.
func loadAutomation() {
	mu.Lock()
	defer mu.Unlock()
	if automationLoaded {
		return
	}
	automationLoaded = true
	b, err := os.ReadFile(statePath + ".automation.json")
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	s := defaultAutomation()
	if err == nil {
		err = json.Unmarshal(b, &s)
	}
	if err == nil {
		err = validateAutomation(&s.Config)
	}
	if err != nil {
		automationLoadError = err
		automation.LastError = "巡检配置读取失败，自动巡检已暂停"
		return
	}
	s.Running = false
	s.Events = append([]automationEvent{}, s.Events[max(0, len(s.Events)-200):]...)
	automation = s
}

func saveAutomationLocked() error {
	if automationLoadError != nil {
		return automationLoadError
	}
	b, err := json.Marshal(automation)
	if err != nil {
		return err
	}
	p := statePath + ".automation.json"
	if err = os.WriteFile(p+".tmp", b, 0o600); err != nil {
		return err
	}
	defer os.Remove(p + ".tmp")
	return os.Rename(p+".tmp", p)
}

func automationResponse() managementResponse {
	mu.Lock()
	defer mu.Unlock()
	return jsonResponse(http.StatusOK, automation)
}

func updateAutomation(body []byte) managementResponse {
	var c automationConfig
	if json.Unmarshal(body, &c) != nil {
		return jsonError(400, "巡检配置格式错误")
	}
	if err := validateAutomation(&c); err != nil {
		return jsonError(400, err.Error())
	}
	automationActionMu.Lock()
	defer automationActionMu.Unlock()
	if c.AutoDisable && !automationManagementReady() {
		return jsonError(400, "请先配置本机 CPA 管理地址和密钥")
	}
	mu.Lock()
	defer mu.Unlock()
	previous := automation
	automation.Config, automation.LastError, automation.NextRun = c, "", nil
	if c.Enabled {
		next := time.Now().UTC().Add(time.Duration(c.IntervalMinutes) * time.Minute)
		automation.NextRun = &next
	}
	if err := saveAutomationLocked(); err != nil {
		automation = previous
		return jsonError(500, "巡检配置保存失败，原设置保持不变")
	}
	automationGeneration++
	if automationCancel != nil {
		automationCancel()
	}
	scheduleAutomationLocked()
	return jsonResponse(200, automation)
}

func scheduleAutomationLocked() {
	if automationTimer != nil {
		automationTimer.Stop()
		automationTimer = nil
	}
	if !automation.Config.Enabled || automation.Running || runCtx.Err() != nil || automationLoadError != nil {
		return
	}
	if automation.NextRun == nil {
		next := time.Now().UTC().Add(time.Duration(automation.Config.IntervalMinutes) * time.Minute)
		automation.NextRun = &next
	}
	generation := automationGeneration
	automationTimer = time.AfterFunc(max(time.Second, time.Until(*automation.NextRun)), func() {
		mu.Lock()
		defer mu.Unlock()
		if generation != automationGeneration || !automation.Config.Enabled || runCtx.Err() != nil {
			return
		}
		_ = startAutomationLocked()
	})
}

func runAutomationResponse() managementResponse {
	mu.Lock()
	defer mu.Unlock()
	if err := startAutomationLocked(); err != nil {
		return jsonError(409, err.Error())
	}
	return jsonResponse(200, automation)
}

func cancelAutomationResponse() managementResponse {
	automationActionMu.Lock()
	defer automationActionMu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	automationGeneration++
	if automationCancel != nil {
		automationCancel()
	}
	scheduleAutomationLocked()
	return jsonResponse(200, automation)
}

func startAutomationLocked() error {
	if automation.Running {
		return errors.New("巡检正在运行，不会重复启动")
	}
	if runCtx.Err() != nil {
		return errors.New("插件正在停止")
	}
	if automation.Config.Model == "" {
		return errors.New("请先保存默认检测模型")
	}
	if automationLoadError != nil {
		return errors.New("巡检配置无法读取，已暂停执行")
	}
	if automationTimer != nil {
		automationTimer.Stop()
		automationTimer = nil
	}
	previous := automation
	at := time.Now().UTC()
	automation.Running, automation.LastStarted, automation.NextRun, automation.LastError = true, &at, nil, ""
	if err := saveAutomationLocked(); err != nil {
		automation = previous
		automation.LastError = "无法保存巡检状态，已暂停执行"
		return errors.New(automation.LastError)
	}
	ctx, cancel := context.WithCancel(runCtx)
	automationCancel = cancel
	c, generation := automation.Config, automationGeneration
	tasks.Add(1)
	go func() {
		defer tasks.Done()
		defer cancel()
		defer func() {
			mu.Lock()
			defer mu.Unlock()
			if recover() != nil {
				automation.LastError = "巡检执行异常，已停止本轮"
			}
			at := time.Now().UTC()
			automation.Running, automation.LastFinished, automationCancel = false, &at, nil
			if automation.Config.Enabled {
				interval := time.Duration(automation.Config.IntervalMinutes) * time.Minute
				if generation == automationGeneration || automation.NextRun == nil {
					next := automation.LastStarted.Add(interval)
					if !next.After(at) {
						next = next.Add((at.Sub(next)/interval + 1) * interval)
					}
					automation.NextRun = &next
				}
			} else {
				automation.NextRun = nil
			}
			if err := saveAutomationLocked(); err != nil {
				automation.LastError = "无法保存巡检结果，已暂停自动巡检"
				return
			}
			scheduleAutomationLocked()
		}()
		executeAutomation(ctx, c, generation)
	}()
	return nil
}

func automaticCredential(a credential, c automationConfig) bool {
	return a.Provider == "codex" && a.Source == credentialSourceFile && !a.Disabled && !a.Unavailable &&
		(len(c.CredentialIDs) == 0 || slices.Contains(c.CredentialIDs, a.ID))
}

func executeAutomation(ctx context.Context, c automationConfig, generation uint64) {
	auths, err := credentials()
	if err != nil {
		mu.Lock()
		automation.LastError = "读取凭证列表失败"
		mu.Unlock()
		return
	}
	for _, auth := range auths {
		if ctx.Err() != nil {
			return
		}
		if !automaticCredential(auth, c) {
			continue
		}
		// Refresh eligibility before spending quota; a previous credential may have taken minutes.
		fresh, err := credentials()
		if err != nil {
			mu.Lock()
			automation.LastError = "刷新凭证状态失败"
			mu.Unlock()
			return
		}
		i := slices.IndexFunc(fresh, func(a credential) bool { return a.ID == auth.ID })
		if i < 0 || !automaticCredential(fresh[i], c) {
			continue
		}
		auth = fresh[i]
		if c.AutoDisable {
			auth.AutomationRevision, err = automaticCredentialRevision(auth)
			if err != nil {
				mu.Lock()
				if ctx.Err() != nil || generation != automationGeneration {
					mu.Unlock()
					return
				}
				ok := appendAutomationEventLocked(automationEvent{Time: time.Now().UTC(), CredentialID: auth.ID, Model: c.Model, Status: "skipped", Message: "无法确认测试前凭证版本，已跳过"})
				mu.Unlock()
				if !ok {
					return
				}
				continue
			}
		}
		mu.Lock()
		if ctx.Err() != nil {
			mu.Unlock()
			return
		}
		if running[auth.ID] != nil {
			mu.Unlock()
			continue
		}
		testCtx, cancel := context.WithCancel(ctx)
		p := &progress{Test: "fingerprint", Model: c.Model, Total: fingerprintTarget, cancel: cancel}
		running[auth.ID] = p
		mu.Unlock()
		r := runFingerprint(testCtx, auth, fingerprintRunRequest{runRequest: runRequest{Model: c.Model}, Concurrency: 1}, p)
		cancel()
		mu.Lock()
		delete(running, auth.ID)
		mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if !applyAutomationResult(ctx, c, generation, auth, r) {
			return
		}
	}
}

func automaticVerdict(r fingerprintRecord) string {
	a := r.Attribution
	if r.Status != "completed" || a == nil || a.Used != fingerprintTarget || a.Prediction == "" || a.Probability == nil ||
		(a.Decision != "" && a.Decision != "completed") || (a.Sequence != nil && !a.Sequence.ModelLike) {
		return "inconclusive"
	}
	if fingerprintModelID(r.Model) == fingerprintModelID(a.Prediction) {
		return "match"
	}
	return "mismatch"
}

func applyAutomationResult(ctx context.Context, c automationConfig, generation uint64, auth credential, r fingerprintRecord) bool {
	automationActionMu.Lock()
	defer automationActionMu.Unlock()
	e := automationEvent{Time: time.Now().UTC(), CredentialID: auth.ID, Model: c.Model, Status: automaticVerdict(r)}
	if fingerprintModelID(r.Model) != fingerprintModelID(c.Model) {
		e.Status = "inconclusive"
	}
	if r.Attribution != nil {
		e.Prediction = r.Attribution.Prediction
		e.Probability = r.Attribution.Probability
	}
	if e.Status == "inconclusive" {
		e.Message = "检测未得到完整有效结论，未禁用凭证"
	}
	mu.Lock()
	if ctx.Err() != nil || generation != automationGeneration {
		mu.Unlock()
		return false
	}
	// Persist the verdict before applying a destructive policy; if storage fails, do not disable.
	if !appendAutomationEventLocked(e) {
		mu.Unlock()
		return false
	}
	disable := e.Status == "mismatch" && c.AutoDisable && automation.Config.AutoDisable
	mu.Unlock()
	if !disable {
		return true
	}
	auths, err := credentials()
	if err != nil {
		e.Status, e.Message = "disable_failed", "无法核对凭证状态，未执行禁用"
	} else {
		i := slices.IndexFunc(auths, func(a credential) bool { return a.ID == auth.ID })
		if i < 0 || !automaticCredential(auths[i], c) || ctx.Err() != nil ||
			auths[i].AuthIndex != auth.AuthIndex || auths[i].Name != auth.Name ||
			auths[i].Provider != auth.Provider || auths[i].Source != auth.Source {
			e.Status, e.Message = "skipped", "凭证已变更、删除、禁用、冷却或巡检已取消"
		} else if err := automationDisable(auth); err != nil {
			e.Status, e.Message = "disable_failed", "CPA 禁用凭证失败，请在凭证列表核对状态"
		} else {
			e.Status, e.Message = "disabled", "检测模型不匹配，已禁用此凭证（影响该凭证的所有模型）"
		}
	}
	mu.Lock()
	defer mu.Unlock()
	return appendAutomationEventLocked(e)
}

func appendAutomationEventLocked(e automationEvent) bool {
	automation.Events = append(automation.Events, e)
	if len(automation.Events) > 200 {
		automation.Events = append([]automationEvent{}, automation.Events[len(automation.Events)-200:]...)
	}
	if err := saveAutomationLocked(); err != nil {
		automation.LastError = "巡检记录保存失败，已停止本轮"
		return false
	}
	return true
}
