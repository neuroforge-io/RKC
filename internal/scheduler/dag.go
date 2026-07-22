// Package scheduler executes a deterministic directed acyclic graph of analysis
// stages. It is deliberately independent from repository semantics so the same
// engine can schedule inventory, syntax, semantic, documentation, indexing,
// and export stages in local or worker processes.
package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var (
	ErrDuplicateStage    = errors.New("duplicate stage")
	ErrMissingDependency = errors.New("missing stage dependency")
	ErrDependencyCycle   = errors.New("stage dependency cycle")
)

type Inputs struct {
	Results map[string]Result
	Values  map[string]any
}

type Result struct {
	StageID      string         `json:"stage_id"`
	CacheKey     string         `json:"cache_key,omitempty"`
	ObjectDigest string         `json:"object_digest,omitempty"`
	CacheHit     bool           `json:"cache_hit"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

type Stage struct {
	ID            string
	Version       string
	Dependencies  []string
	Configuration any
	InputDigests  []string
	Run           func(context.Context, Inputs) (Result, error)
}

type Event struct {
	StageID   string        `json:"stage_id"`
	State     string        `json:"state"`
	StartedAt time.Time     `json:"started_at,omitempty"`
	Duration  time.Duration `json:"duration,omitempty"`
	Error     string        `json:"error,omitempty"`
}

type Report struct {
	Results  map[string]Result `json:"results"`
	Events   []Event           `json:"events"`
	Duration time.Duration     `json:"duration"`
}

type Cache interface {
	Load(context.Context, string) (Result, bool, error)
	Store(context.Context, string, Result) error
}

type Options struct {
	Workers int
	Cache   Cache
	Values  map[string]any
	OnEvent func(Event)
}

func Execute(ctx context.Context, stages []Stage, options Options) (Report, error) {
	started := time.Now()
	if ctx == nil {
		return Report{}, errors.New("scheduler context is required")
	}
	normalized, err := validateAndNormalize(stages)
	if err != nil {
		return Report{}, err
	}
	if options.Workers <= 0 {
		options.Workers = 1
	}
	results := map[string]Result{}
	var events []Event
	completed := map[string]bool{}

	for len(completed) < len(normalized) {
		if err := ctx.Err(); err != nil {
			return Report{Results: results, Events: events, Duration: time.Since(started)}, err
		}
		ready := readyStages(normalized, completed)
		if len(ready) == 0 {
			return Report{Results: results, Events: events, Duration: time.Since(started)}, ErrDependencyCycle
		}
		for offset := 0; offset < len(ready); offset += options.Workers {
			end := offset + options.Workers
			if end > len(ready) {
				end = len(ready)
			}
			batch := ready[offset:end]
			type outcome struct {
				id     string
				result Result
				event  Event
				err    error
			}
			outcomes := make(chan outcome, len(batch))
			var wg sync.WaitGroup
			for _, stage := range batch {
				stage := stage
				wg.Add(1)
				go func() {
					defer wg.Done()
					stageStarted := time.Now()
					emit(options.OnEvent, Event{StageID: stage.ID, State: "running", StartedAt: stageStarted})
					result, runErr := executeStage(ctx, stage, results, options)
					event := Event{StageID: stage.ID, State: "complete", StartedAt: stageStarted, Duration: time.Since(stageStarted)}
					if runErr != nil {
						event.State = "failed"
						event.Error = runErr.Error()
					}
					emit(options.OnEvent, event)
					outcomes <- outcome{id: stage.ID, result: result, event: event, err: runErr}
				}()
			}
			wg.Wait()
			close(outcomes)
			collected := make([]outcome, 0, len(batch))
			for item := range outcomes {
				collected = append(collected, item)
			}
			sort.Slice(collected, func(i, j int) bool { return collected[i].id < collected[j].id })
			for _, item := range collected {
				events = append(events, item.event)
				if item.err != nil {
					return Report{Results: results, Events: events, Duration: time.Since(started)}, fmt.Errorf("stage %s: %w", item.id, item.err)
				}
				results[item.id] = item.result
				completed[item.id] = true
			}
		}
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].StageID == events[j].StageID {
			return events[i].State < events[j].State
		}
		return events[i].StageID < events[j].StageID
	})
	return Report{Results: results, Events: events, Duration: time.Since(started)}, nil
}

func executeStage(ctx context.Context, stage Stage, prior map[string]Result, options Options) (Result, error) {
	dependencyResults := make(map[string]Result, len(stage.Dependencies))
	dependencyDigests := make([]string, 0, len(stage.Dependencies))
	for _, dependency := range stage.Dependencies {
		result := prior[dependency]
		dependencyResults[dependency] = result
		dependencyDigests = append(dependencyDigests, result.ObjectDigest)
	}
	key, err := CacheKey(stage.ID, stage.Version, append(stage.InputDigests, dependencyDigests...), stage.Configuration)
	if err != nil {
		return Result{}, fmt.Errorf("compute cache key: %w", err)
	}
	if options.Cache != nil {
		cached, ok, err := options.Cache.Load(ctx, key)
		if err != nil {
			return Result{}, fmt.Errorf("load cache: %w", err)
		}
		if ok {
			cached.StageID = stage.ID
			cached.CacheKey = key
			cached.CacheHit = true
			return cached, nil
		}
	}
	if stage.Run == nil {
		return Result{}, errors.New("stage has no runner")
	}
	result, err := stage.Run(ctx, Inputs{Results: dependencyResults, Values: options.Values})
	if err != nil {
		return Result{}, err
	}
	result.StageID = stage.ID
	result.CacheKey = key
	if options.Cache != nil {
		if err := options.Cache.Store(ctx, key, result); err != nil {
			return Result{}, fmt.Errorf("store cache: %w", err)
		}
	}
	return result, nil
}

func CacheKey(stageID, version string, inputDigests []string, configuration any) (string, error) {
	digests := append([]string(nil), inputDigests...)
	sort.Strings(digests)
	payload := struct {
		StageID       string   `json:"stage_id"`
		Version       string   `json:"version"`
		InputDigests  []string `json:"input_digests"`
		Configuration any      `json:"configuration,omitempty"`
	}{stageID, version, digests, configuration}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode stage cache-key inputs: %w", err)
	}
	sum := sha256.Sum256(data)
	return "stage:" + hex.EncodeToString(sum[:]), nil
}

func validateAndNormalize(stages []Stage) ([]Stage, error) {
	byID := make(map[string]Stage, len(stages))
	for _, stage := range stages {
		if stage.ID == "" {
			return nil, errors.New("stage id is required")
		}
		if _, exists := byID[stage.ID]; exists {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateStage, stage.ID)
		}
		stage.Dependencies = uniqueSorted(stage.Dependencies)
		stage.InputDigests = uniqueSorted(stage.InputDigests)
		byID[stage.ID] = stage
	}
	for _, stage := range byID {
		for _, dependency := range stage.Dependencies {
			if _, ok := byID[dependency]; !ok {
				return nil, fmt.Errorf("%w: %s depends on %s", ErrMissingDependency, stage.ID, dependency)
			}
		}
	}
	if hasCycle(byID) {
		return nil, ErrDependencyCycle
	}
	output := make([]Stage, 0, len(byID))
	for _, stage := range byID {
		output = append(output, stage)
	}
	sort.Slice(output, func(i, j int) bool { return output[i].ID < output[j].ID })
	return output, nil
}

func hasCycle(stages map[string]Stage) bool {
	state := map[string]uint8{}
	var visit func(string) bool
	visit = func(id string) bool {
		switch state[id] {
		case 1:
			return true
		case 2:
			return false
		}
		state[id] = 1
		for _, dependency := range stages[id].Dependencies {
			if visit(dependency) {
				return true
			}
		}
		state[id] = 2
		return false
	}
	ids := make([]string, 0, len(stages))
	for id := range stages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if visit(id) {
			return true
		}
	}
	return false
}

func readyStages(stages []Stage, completed map[string]bool) []Stage {
	var output []Stage
	for _, stage := range stages {
		if completed[stage.ID] {
			continue
		}
		ready := true
		for _, dependency := range stage.Dependencies {
			if !completed[dependency] {
				ready = false
				break
			}
		}
		if ready {
			output = append(output, stage)
		}
	}
	return output
}

func uniqueSorted(values []string) []string {
	seen := map[string]struct{}{}
	for _, value := range values {
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	output := make([]string, 0, len(seen))
	for value := range seen {
		output = append(output, value)
	}
	sort.Strings(output)
	return output
}

func emit(callback func(Event), event Event) {
	if callback != nil {
		callback(event)
	}
}
