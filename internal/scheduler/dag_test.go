package scheduler

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memoryCache struct {
	mu       sync.Mutex
	values   map[string]Result
	loadErr  error
	storeErr error
	loads    int
	stores   int
}

func (cache *memoryCache) Load(ctx context.Context, key string) (Result, bool, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, false, err
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.loads++
	if cache.loadErr != nil {
		return Result{}, false, cache.loadErr
	}
	result, ok := cache.values[key]
	return result, ok, nil
}

func (cache *memoryCache) Store(ctx context.Context, key string, result Result) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.stores++
	if cache.storeErr != nil {
		return cache.storeErr
	}
	if cache.values == nil {
		cache.values = map[string]Result{}
	}
	cache.values[key] = result
	return nil
}

func TestValidateAndNormalizeRejectsInvalidDAGs(t *testing.T) {
	tests := []struct {
		name   string
		stages []Stage
		want   error
		text   string
	}{
		{name: "missing ID", stages: []Stage{{}}, text: "stage id is required"},
		{name: "duplicate", stages: []Stage{{ID: "a"}, {ID: "a"}}, want: ErrDuplicateStage},
		{name: "missing dependency", stages: []Stage{{ID: "a", Dependencies: []string{"missing"}}}, want: ErrMissingDependency},
		{name: "self cycle", stages: []Stage{{ID: "a", Dependencies: []string{"a"}}}, want: ErrDependencyCycle},
		{name: "multi cycle", stages: []Stage{{ID: "a", Dependencies: []string{"b"}}, {ID: "b", Dependencies: []string{"c"}}, {ID: "c", Dependencies: []string{"a"}}}, want: ErrDependencyCycle},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := validateAndNormalize(test.stages)
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("validateAndNormalize() = %v, want %v", err, test.want)
			}
			if test.text != "" && (err == nil || !strings.Contains(err.Error(), test.text)) {
				t.Fatalf("validateAndNormalize() = %v, want text %q", err, test.text)
			}
		})
	}
}

func TestValidateAndNormalizeSortsAndDeduplicates(t *testing.T) {
	input := []Stage{
		{ID: "z", Dependencies: []string{"a", "a", "", "b"}, InputDigests: []string{"2", "1", "2", ""}},
		{ID: "b"},
		{ID: "a"},
	}
	got, err := validateAndNormalize(input)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{got[0].ID, got[1].ID, got[2].ID}
	if !reflect.DeepEqual(ids, []string{"a", "b", "z"}) {
		t.Fatalf("normalized IDs = %v", ids)
	}
	if !reflect.DeepEqual(got[2].Dependencies, []string{"a", "b"}) || !reflect.DeepEqual(got[2].InputDigests, []string{"1", "2"}) {
		t.Fatalf("normalized stage = %+v", got[2])
	}
	if !reflect.DeepEqual(input[0].Dependencies, []string{"a", "a", "", "b"}) {
		t.Fatal("validateAndNormalize mutated caller dependencies")
	}
	if hasCycle(map[string]Stage{"done": {ID: "done"}}) {
		t.Fatal("acyclic graph reported a cycle")
	}
	ready := readyStages(got, map[string]bool{"a": true})
	readyIDs := make([]string, len(ready))
	for index := range ready {
		readyIDs[index] = ready[index].ID
	}
	if !reflect.DeepEqual(readyIDs, []string{"b"}) {
		t.Fatalf("readyStages() = %v, want [b]", readyIDs)
	}
}

func TestExecuteDependenciesEventsValuesAndDeterministicReport(t *testing.T) {
	var callbackMu sync.Mutex
	var callbacks []Event
	values := map[string]any{"request": "value"}
	stages := []Stage{
		{ID: "b", Version: "1", InputDigests: []string{"b-input"}, Run: func(_ context.Context, inputs Inputs) (Result, error) {
			if len(inputs.Results) != 0 || inputs.Values["request"] != "value" {
				t.Errorf("stage b inputs = %+v", inputs)
			}
			return Result{ObjectDigest: "digest-b", Metadata: map[string]any{"stage": "b"}}, nil
		}},
		{ID: "a", Version: "1", InputDigests: []string{"a-input"}, Run: func(_ context.Context, inputs Inputs) (Result, error) {
			if len(inputs.Results) != 0 || inputs.Values["request"] != "value" {
				t.Errorf("stage a inputs = %+v", inputs)
			}
			return Result{ObjectDigest: "digest-a"}, nil
		}},
		{ID: "c", Version: "2", Dependencies: []string{"b", "a", "a"}, Configuration: map[string]any{"mode": "strict"}, Run: func(_ context.Context, inputs Inputs) (Result, error) {
			if len(inputs.Results) != 2 || inputs.Results["a"].ObjectDigest != "digest-a" || inputs.Results["b"].ObjectDigest != "digest-b" {
				t.Errorf("stage c dependency inputs = %+v", inputs.Results)
			}
			return Result{ObjectDigest: "digest-c"}, nil
		}},
	}
	report, err := Execute(context.Background(), stages, Options{
		Workers: 2,
		Values:  values,
		OnEvent: func(event Event) {
			callbackMu.Lock()
			callbacks = append(callbacks, event)
			callbackMu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 3 || report.Duration <= 0 {
		t.Fatalf("Execute() report = %+v", report)
	}
	for id, result := range report.Results {
		if result.StageID != id || !strings.HasPrefix(result.CacheKey, "stage:") || result.CacheHit {
			t.Errorf("result %s = %+v", id, result)
		}
	}
	if got := []string{report.Events[0].StageID, report.Events[1].StageID, report.Events[2].StageID}; !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("report event order = %v", got)
	}
	for _, event := range report.Events {
		if event.State != "complete" || event.StartedAt.IsZero() || event.Duration < 0 || event.Error != "" {
			t.Errorf("completion event = %+v", event)
		}
	}
	callbackMu.Lock()
	defer callbackMu.Unlock()
	if len(callbacks) != 6 {
		t.Fatalf("OnEvent callback count = %d, want 6", len(callbacks))
	}
	states := map[string][]string{}
	for _, event := range callbacks {
		states[event.StageID] = append(states[event.StageID], event.State)
	}
	for id, got := range states {
		sort.Strings(got)
		if !reflect.DeepEqual(got, []string{"complete", "running"}) {
			t.Errorf("callback states for %s = %v", id, got)
		}
	}
}

func TestExecuteUsesCacheAndSkipsRunner(t *testing.T) {
	cache := &memoryCache{values: map[string]Result{}}
	var runs atomic.Int32
	stage := Stage{ID: "cached", Version: "v1", InputDigests: []string{"input"}, Run: func(context.Context, Inputs) (Result, error) {
		runs.Add(1)
		return Result{ObjectDigest: "object", Metadata: map[string]any{"kept": true}}, nil
	}}
	first, err := Execute(context.Background(), []Stage{stage}, Options{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if first.Results["cached"].CacheHit || cache.stores != 1 || runs.Load() != 1 {
		t.Fatalf("first cached execution: result=%+v stores=%d runs=%d", first.Results["cached"], cache.stores, runs.Load())
	}
	second, err := Execute(context.Background(), []Stage{stage}, Options{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	result := second.Results["cached"]
	if !result.CacheHit || result.StageID != "cached" || result.ObjectDigest != "object" || runs.Load() != 1 || cache.loads != 2 {
		t.Fatalf("cache hit result=%+v loads=%d runs=%d", result, cache.loads, runs.Load())
	}
}

func TestExecuteErrorPaths(t *testing.T) {
	sentinel := errors.New("runner failed")
	tests := []struct {
		name    string
		stage   Stage
		options Options
		want    string
	}{
		{name: "nil runner", stage: Stage{ID: "nil"}, want: "stage has no runner"},
		{name: "runner", stage: Stage{ID: "run", Run: func(context.Context, Inputs) (Result, error) { return Result{}, sentinel }}, want: "runner failed"},
		{name: "cache load", stage: Stage{ID: "load", Run: func(context.Context, Inputs) (Result, error) { return Result{}, nil }}, options: Options{Cache: &memoryCache{loadErr: errors.New("load failed")}}, want: "load cache: load failed"},
		{name: "cache store", stage: Stage{ID: "store", Run: func(context.Context, Inputs) (Result, error) { return Result{}, nil }}, options: Options{Cache: &memoryCache{storeErr: errors.New("store failed")}}, want: "store cache: store failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report, err := Execute(context.Background(), []Stage{test.stage}, test.options)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Execute() error = %v, want %q", err, test.want)
			}
			if len(report.Events) != 1 || report.Events[0].State != "failed" || !strings.Contains(report.Events[0].Error, test.want[strings.LastIndex(test.want, ":")+1:]) {
				t.Fatalf("failed report = %+v", report)
			}
		})
	}
}

func TestExecuteCancellationAndTimeout(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	report, err := Execute(cancelled, []Stage{{ID: "never", Run: func(context.Context, Inputs) (Result, error) {
		t.Fatal("runner called for pre-cancelled context")
		return Result{}, nil
	}}}, Options{})
	if !errors.Is(err, context.Canceled) || len(report.Results) != 0 {
		t.Fatalf("pre-cancelled Execute() = %+v, %v", report, err)
	}

	ctx, stop := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer stop()
	report, err = Execute(ctx, []Stage{{ID: "wait", Run: func(ctx context.Context, _ Inputs) (Result, error) {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}}}, Options{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed Execute() error = %v", err)
	}
	if len(report.Events) != 1 || report.Events[0].State != "failed" {
		t.Fatalf("timed Execute() report = %+v", report)
	}
}

func TestExecuteEmptyAndDefaultWorkerCount(t *testing.T) {
	report, err := Execute(context.Background(), nil, Options{Workers: -4})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 0 || len(report.Events) != 0 || report.Results == nil {
		t.Fatalf("Execute(empty) = %+v", report)
	}
}

func TestCacheKeyIsDeterministicAndDoesNotMutateInputs(t *testing.T) {
	digests := []string{"z", "a", "m"}
	configuration := map[string]any{"enabled": true, "count": float64(2)}
	first := CacheKey("stage", "1", digests, configuration)
	second := CacheKey("stage", "1", []string{"m", "z", "a"}, map[string]any{"count": float64(2), "enabled": true})
	if first != second || !strings.HasPrefix(first, "stage:") || len(first) != len("stage:")+64 {
		t.Fatalf("CacheKey() = %q and %q", first, second)
	}
	if !reflect.DeepEqual(digests, []string{"z", "a", "m"}) {
		t.Fatalf("CacheKey mutated input: %v", digests)
	}
	if first == CacheKey("stage", "2", digests, configuration) || first == CacheKey("other", "1", digests, configuration) {
		t.Fatal("CacheKey failed to bind stage identity/version")
	}
	if got := uniqueSorted([]string{" b ", "a", "a", ""}); !reflect.DeepEqual(got, []string{" b ", "a"}) {
		// Scheduler digests are opaque, so whitespace is retained while empty
		// values and exact duplicates are removed.
		t.Fatalf("uniqueSorted() = %q", got)
	}
	var events int
	emit(func(Event) { events++ }, Event{})
	emit(nil, Event{})
	if events != 1 {
		t.Fatalf("emit callback count = %d", events)
	}
}
