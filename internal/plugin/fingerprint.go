package plugin

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Each run sends fingerprintTarget challenges, at most that many at once per credential and
// fingerprintGlobalConcurrency across credentials. A challenge holds its slot through its retries.
const (
	fingerprintTarget            = 3
	fingerprintGlobalConcurrency = 6
	fingerprintMaxText           = 64000
)

type fingerprintChallenge struct {
	Instructions  string `json:"instructions,omitempty"`
	Prompt        string `json:"prompt"`
	ExpectedCount int    `json:"expected_count"`
}

//go:embed data/lm-detector/enrollment-suite.json
var fingerprintSuiteJSON []byte

// fingerprintSuite holds the challenges the reference answers were collected with: 12 prompt environments that
// add a system prompt, a user prefix or nothing to their three challenges.
var fingerprintSuite = func() []fingerprintChallenge {
	var suite []fingerprintChallenge
	for _, c := range mustDecode[[]struct {
		System        string `json:"system"`
		UserPrefix    string `json:"user_prefix"`
		Prompt        string `json:"prompt"`
		ExpectedCount int    `json:"expected_count"`
	}](fingerprintSuiteJSON) {
		prompt := c.Prompt
		if c.UserPrefix != "" {
			prompt = c.UserPrefix + "\n\nFinal task:\n" + c.Prompt
		}
		suite = append(suite, fingerprintChallenge{c.System, prompt, c.ExpectedCount})
	}
	return suite
}()

func fingerprintChallenges() []fingerprintChallenge {
	challenges := make([]fingerprintChallenge, fingerprintTarget)
	for i, j := range rand.Perm(len(fingerprintSuite))[:fingerprintTarget] {
		challenges[i] = fingerprintSuite[j]
	}
	return challenges
}

// fingerprintAliases maps tested models to the candidate that stands for them: the bank enrolls gpt-6.1-sol
// answers under gpt-6-astra, and deepseek-flash serves the model enrolled as deepseek-v4.1-flash-1017.
// The page compares models with the same table.
var fingerprintAliases = map[string]string{"gpt-6.1-sol": "gpt-6-astra", "deepseek-flash": "deepseek-v4.1-flash-1017"}

// fingerprintModelID is the candidate a model name stands for, without a provider prefix such as "openai/".
func fingerprintModelID(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	model = model[strings.LastIndex(model, "/")+1:]
	if alias, ok := fingerprintAliases[model]; ok {
		return alias
	}
	return model
}

type fingerprintSample struct {
	fingerprintChallenge
	Attempts int    `json:"attempts,omitempty"`
	Text     string `json:"text,omitempty"`
	Error    string `json:"error,omitempty"`
	Parsed   int    `json:"parsed_numbers"`
	Valid    bool   `json:"accepted"`
}

// fingerprintRecord is one run of challenges: its status is completed, partial (only some answers valid),
// failed, cancelled or skipped.
type fingerprintRecord struct {
	record
	DetectorRevision string                  `json:"detector_revision"`
	Concurrency      int                     `json:"concurrency,omitempty"`
	Samples          []fingerprintSample     `json:"samples"`
	Attribution      *fingerprintAttribution `json:"attribution,omitempty"`
}

// summary drops the samples and rankings that only the detail dialog shows.
func (r fingerprintRecord) summary() fingerprintRecord {
	r.Samples = nil
	if r.Attribution != nil {
		attribution := *r.Attribution
		attribution.Results, attribution.Diagnostics = nil, nil
		r.Attribution = &attribution
	}
	return r
}

type fingerprintRunRequest struct {
	runRequest
	Concurrency int `json:"concurrency"`
}

var fingerprintSlots = make(chan struct{}, fingerprintGlobalConcurrency)

func fingerprintRunResponse(body []byte) managementResponse {
	var req fingerprintRunRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return jsonError(http.StatusBadRequest, "请求格式错误")
	}
	req.Model = strings.TrimSpace(req.Model)
	if !validModelName(req.Model, false) {
		return jsonError(http.StatusBadRequest, "请选择不含推理强度后缀的模型")
	}
	if req.Concurrency == 0 {
		req.Concurrency = fingerprintTarget
	}
	if req.Concurrency < 1 || req.Concurrency > fingerprintTarget {
		return jsonError(http.StatusBadRequest, fmt.Sprintf("每凭证并发须为 1–%d", fingerprintTarget))
	}
	return startRuns("fingerprint", req.runRequest, fingerprintTarget,
		func(id string) {
			records.Fingerprints.add(id, fingerprintRecord{record: skippedRecord(req.Model), DetectorRevision: detectorRevision})
		},
		func(ctx context.Context, auth credential, p *progress) { runFingerprint(ctx, auth, req, p) })
}

func runFingerprint(ctx context.Context, auth credential, req fingerprintRunRequest, p *progress) (r fingerprintRecord) {
	started := time.Now()
	done := 0
	r = fingerprintRecord{record: newRecord(req.Model, started), DetectorRevision: detectorRevision, Concurrency: req.Concurrency}
	r.Status = "completed"
	defer func() {
		if ctx.Err() != nil && done < fingerprintTarget {
			r.Status = "cancelled"
		}
		if recovered := recover(); recovered != nil {
			r.Status, r.Error = "failed", fmt.Sprint(recovered)
		}
		r.DurationMS = time.Since(started).Milliseconds()
		mu.Lock()
		defer mu.Unlock()
		records.Fingerprints.add(auth.ID, r)
		_ = saveStateLocked()
	}()
	challenges := fingerprintChallenges()
	type event struct {
		index  int
		sample fingerprintSample
		output modelResponse
		err    error
	}
	jobs, events := make(chan int, fingerprintTarget), make(chan event, fingerprintTarget)
	for i := range challenges {
		jobs <- i
	}
	close(jobs)
	var workers sync.WaitGroup
	for range req.Concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				select {
				case fingerprintSlots <- struct{}{}:
				case <-ctx.Done():
					return
				}
				sample, output, err := collectFingerprintSample(ctx, auth, req.Model, challenges[i])
				<-fingerprintSlots
				if sample.Attempts > 0 {
					events <- event{i, sample, output, err}
				}
			}
		}()
	}
	go func() { workers.Wait(); close(events) }()
	samples := make([]*fingerprintSample, fingerprintTarget)
	for e := range events {
		samples[e.index] = &e.sample
		r.usage.add(e.output.usage)
		if !errors.Is(e.err, context.Canceled) {
			done++
		}
		mu.Lock()
		p.Done = done
		mu.Unlock()
	}
	var outputs []fingerprintOutput
	// Preserve challenge order even when requests complete out of order.
	for _, sample := range samples {
		if sample == nil {
			continue
		}
		r.Samples = append(r.Samples, *sample)
		outputs = append(outputs, fingerprintOutput{sample.Text, sample.ExpectedCount})
	}
	attr, err := analyzeFingerprint(outputs)
	switch {
	case err == nil:
		r.Attribution = &attr
		if attr.Used < fingerprintTarget {
			r.Status = "partial"
		}
	case len(r.Samples) > 0 && r.Samples[len(r.Samples)-1].Error != "":
		// No answer was valid; the last request's error tells why.
		r.Status, r.Error = "failed", r.Samples[len(r.Samples)-1].Error
	default:
		r.Status, r.Error = "failed", err.Error()
	}
	return r
}

// collectFingerprintSample asks challenge until an answer can be scored. The sample keeps the last answer.
func collectFingerprintSample(ctx context.Context, auth credential, model string, challenge fingerprintChallenge) (sample fingerprintSample, out modelResponse, err error) {
	sample.fingerprintChallenge = challenge
	out, sample.Attempts, err = askCodex(ctx, auth, model, "", challenge.Instructions, challenge.Prompt, challengeTimeout, func(answer string) (err error) {
		sample.Text, sample.Parsed, err = fingerprintText(answer, challenge.ExpectedCount)
		return err
	})
	sample.Valid = err == nil
	if err != nil {
		sample.Error = truncate(err.Error(), 500)
	}
	return
}

// fingerprintText returns the text of answer that the detector scores and the count of its numbers, or why
// the answer cannot be scored. Like the upstream detector, only the requested count is scored when an answer runs
// longer.
func fingerprintText(answer string, expected int) (text string, parsed int, err error) {
	if len(answer) > fingerprintMaxText {
		return "", 0, errors.New("回答过长")
	}
	text, numbers := answer, parseNumbers(answer)
	if len(numbers) > expected {
		numbers = numbers[:expected]
		values := make([]string, len(numbers))
		for i, n := range numbers {
			values[i] = strconv.Itoa(n)
		}
		text = strings.Join(values, ", ")
	}
	if len(numbers) < minimumNumbers(expected) {
		err = errors.New("有效数字不足")
	}
	return text, len(numbers), err
}
