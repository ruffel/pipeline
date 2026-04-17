package pipeline

// ExecutorOption configures an [Executor] built by [NewExecutorWithOptions].
// Options are additive unless documented otherwise.
type ExecutorOption func(*executorConfig)

type executorConfig struct {
	observers []Observer
}

// NewExecutorWithOptions returns an Executor configured by the given options.
// Use [NewExecutor] for the common case where you only want to pass observers.
func NewExecutorWithOptions(options ...ExecutorOption) *Executor {
	cfg := executorConfig{}

	for _, option := range options {
		if option != nil {
			option(&cfg)
		}
	}

	return &Executor{observers: filterObservers(cfg.observers)}
}

// WithObservers appends observers to the executor configuration. Nil observers
// are ignored when the executor is built.
func WithObservers(observers ...Observer) ExecutorOption {
	copied := append([]Observer(nil), observers...)

	return func(cfg *executorConfig) {
		cfg.observers = append(cfg.observers, copied...)
	}
}

func filterObservers(observers []Observer) []Observer {
	filtered := make([]Observer, 0, len(observers))

	for _, observer := range observers {
		if observer != nil {
			filtered = append(filtered, observer)
		}
	}

	return filtered
}
