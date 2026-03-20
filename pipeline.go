package pipeline

import "context"

type Pipeline struct {
	ID     string
	Name   string
	Stages []Stage
}

type Stage struct {
	ID    string
	Name  string
	Group string // UI labelling or parallel execution.
	Steps []Step
}

type Step struct {
	ID   string
	Name string
	Run  StepFn
}

type StepFn func(ctx context.Context) error
