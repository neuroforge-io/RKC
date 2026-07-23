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
	ErrResourceBudget    = errors.New("stage resource request exceeds scheduler budget")
	// ErrCacheRejected tells Execute that a cache pointer was structurally
	// valid but its stage payload could not be restored. Execute invalidates
	// the pointer when supported and safely recomputes the stage.
	ErrCacheRejected = errors.New("cached stage result rejected")
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
	DoNotCache   bool           `json:"-"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

type Stage struct {
	ID                      string
	Version                 string
	Dependencies            []string
	Configuration           any
	InputDigests            []string
	DynamicInputDigests     func(context.Context, Inputs) ([]string, error)
	IgnoreDependencyDigests bool
	DisableCache            bool
	Restore                 func(context.Context, Inputs, Result) error
	Resources               ResourceRequest
	Run                     func(context.Context, Inputs) (Result, error)
}

type ResourceRequest struct {
	MemoryMiB int64  `json:"memory_mib"`
	CPU       int    `json:"cpu"`
	Processes int    `json:"processes"`
	OpenFiles int    `json:"open_files"`
	IOClass   string `json:"io_class,omitempty"`
}

type ResourceBudget struct {
	MemoryMiB int64 `json:"memory_mib"`
	CPU       int   `json:"cpu"`
	Processes int   `json:"processes"`
	OpenFiles int   `json:"open_files"`
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

type CacheInvalidator interface {
	Invalidate(context.Context, string) error
}

type Options struct {
	Workers int
	Budget  ResourceBudget
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
	var callbackMu sync.Mutex
	onEvent := func(event Event) {
		if options.OnEvent == nil {
			return
		}
		callbackMu.Lock()
		defer callbackMu.Unlock()
		options.OnEvent(event)
	}

	for len(completed) < len(normalized) {
		if err := ctx.Err(); err != nil {
			return Report{Results: results, Events: events, Duration: time.Since(started)}, err
		}
		ready := readyStages(normalized, completed)
		if len(ready) == 0 {
			return Report{Results: results, Events: events, Duration: time.Since(started)}, ErrDependencyCycle
		}
		pending := ready
		for len(pending) > 0 {
			batch, deferred, err := admitStages(pending, options.Workers, options.Budget)
			if err != nil {
				return Report{Results: results, Events: events, Duration: time.Since(started)}, err
			}
			pending = deferred
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
					emit(onEvent, Event{StageID: stage.ID, State: "running", StartedAt: stageStarted})
					result, runErr := executeStage(ctx, stage, results, options)
					event := Event{StageID: stage.ID, State: "complete", StartedAt: stageStarted, Duration: time.Since(stageStarted)}
					if runErr != nil {
						event.State = "failed"
						event.Error = runErr.Error()
					} else if result.CacheHit {
						event.State = "cached"
					}
					emit(onEvent, event)
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
	inputs := Inputs{Results: dependencyResults, Values: options.Values}
	inputDigests := append([]string(nil), stage.InputDigests...)
	if !stage.IgnoreDependencyDigests {
		inputDigests = append(inputDigests, dependencyDigests...)
	}
	if stage.DynamicInputDigests != nil {
		dynamicDigests, err := stage.DynamicInputDigests(ctx, inputs)
		if err != nil {
			return Result{}, fmt.Errorf("resolve dynamic input digests: %w", err)
		}
		inputDigests = append(inputDigests, dynamicDigests...)
	}
	key, err := CacheKey(stage.ID, stage.Version, inputDigests, stage.Configuration)
	if err != nil {
		return Result{}, fmt.Errorf("compute cache key: %w", err)
	}
	if options.Cache != nil && !stage.DisableCache {
		cached, ok, err := options.Cache.Load(ctx, key)
		if err != nil {
			return Result{}, fmt.Errorf("load cache: %w", err)
		}
		if ok {
			cached.StageID = stage.ID
			cached.CacheKey = key
			cached.CacheHit = true
			if stage.Restore != nil {
				if err := stage.Restore(ctx, inputs, cached); err != nil {
					if errors.Is(err, ErrCacheRejected) {
						if invalidator, supported := options.Cache.(CacheInvalidator); supported {
							if invalidateErr := invalidator.Invalidate(ctx, key); invalidateErr != nil {
								return Result{}, fmt.Errorf(
									"invalidate rejected cached result: %w",
									errors.Join(err, invalidateErr),
								)
							}
						}
					} else {
						return Result{}, fmt.Errorf("restore cached result: %w", err)
					}
				} else {
					return cached, nil
				}
			} else {
				return cached, nil
			}
		}
	}
	if stage.Run == nil {
		return Result{}, errors.New("stage has no runner")
	}
	result, err := stage.Run(ctx, inputs)
	if err != nil {
		return Result{}, err
	}
	result.StageID = stage.ID
	result.CacheKey = key
	if options.Cache != nil && !stage.DisableCache && !result.DoNotCache {
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
		if err := validateResourceRequest(stage.Resources); err != nil {
			return nil, fmt.Errorf("stage %s resources: %w", stage.ID, err)
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

func admitStages(
	pending []Stage,
	workers int,
	budget ResourceBudget,
) ([]Stage, []Stage, error) {
	if workers <= 0 {
		workers = 1
	}
	batch := make([]Stage, 0, workers)
	deferred := make([]Stage, 0, len(pending))
	used := ResourceRequest{}
	for _, stage := range pending {
		if exceedsBudget(stage.Resources, budget) {
			return nil, nil, fmt.Errorf(
				"%w: %s requests memory=%dMiB cpu=%d processes=%d open_files=%d",
				ErrResourceBudget,
				stage.ID,
				stage.Resources.MemoryMiB,
				stage.Resources.CPU,
				stage.Resources.Processes,
				stage.Resources.OpenFiles,
			)
		}
		if len(batch) >= workers || !fitsAlongside(used, stage.Resources, budget) {
			deferred = append(deferred, stage)
			continue
		}
		batch = append(batch, stage)
		used.MemoryMiB += stage.Resources.MemoryMiB
		used.CPU += stage.Resources.CPU
		used.Processes += stage.Resources.Processes
		used.OpenFiles += stage.Resources.OpenFiles
	}
	if len(batch) == 0 {
		return nil, nil, ErrResourceBudget
	}
	return batch, deferred, nil
}

func validateResourceRequest(request ResourceRequest) error {
	if request.MemoryMiB < 0 || request.CPU < 0 ||
		request.Processes < 0 || request.OpenFiles < 0 {
		return errors.New("resource values must be non-negative")
	}
	switch request.IOClass {
	case "", "latency", "normal", "bulk":
		return nil
	default:
		return fmt.Errorf("unknown I/O class %q", request.IOClass)
	}
}

func exceedsBudget(request ResourceRequest, budget ResourceBudget) bool {
	return budget.MemoryMiB > 0 && request.MemoryMiB > budget.MemoryMiB ||
		budget.CPU > 0 && request.CPU > budget.CPU ||
		budget.Processes > 0 && request.Processes > budget.Processes ||
		budget.OpenFiles > 0 && request.OpenFiles > budget.OpenFiles
}

func fitsAlongside(
	used ResourceRequest,
	request ResourceRequest,
	budget ResourceBudget,
) bool {
	return (budget.MemoryMiB <= 0 || used.MemoryMiB+request.MemoryMiB <= budget.MemoryMiB) &&
		(budget.CPU <= 0 || used.CPU+request.CPU <= budget.CPU) &&
		(budget.Processes <= 0 || used.Processes+request.Processes <= budget.Processes) &&
		(budget.OpenFiles <= 0 || used.OpenFiles+request.OpenFiles <= budget.OpenFiles)
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
