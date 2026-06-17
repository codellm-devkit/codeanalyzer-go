// Package worker runs tasks concurrently.
package worker

import (
	"fmt"
	"sync"
)

// Task is a unit of work.
type Task struct {
	ID      int    `json:"id"`
	Payload string `json:"payload"`
}

// Result holds the outcome of processing a Task.
type Result struct {
	TaskID int    `json:"task_id"`
	Output string `json:"output"`
}

// Processor is the processing interface.
// Exercises: interface type, is_interface=true.
type Processor interface {
	Process(t Task) (Result, error)
}

// Worker runs tasks in background goroutines.
type Worker struct {
	mu   sync.Mutex
	done bool
}

// New creates a Worker.
func New() *Worker {
	return &Worker{}
}

// Run launches a goroutine to process t.
// Exercises: goroutine launch — GoCallsite.is_goroutine=true for the w.execute call.
func (w *Worker) Run(p Processor, t Task) {
	go w.execute(p, t)
}

// Combine merges multiple results into a single Result.
// Exercises: variadic parameter (results ...Result), is_variadic=true.
func Combine(results ...Result) Result {
	out := Result{}
	for _, r := range results {
		out.Output += r.Output
	}
	return out
}

// execute processes a task under the mutex.
// Exercises: unexported method (is_exported=false), cyclomatic_complexity > 1 (if branch).
func (w *Worker) execute(p Processor, t Task) {
	w.mu.Lock()
	defer w.mu.Unlock()
	r, err := p.Process(t)
	if err != nil {
		_ = fmt.Errorf("task %d: %w", t.ID, err)
		return
	}
	_ = r
}
