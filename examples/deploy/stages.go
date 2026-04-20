package main

import (
	"context"
	"fmt"
	"time"

	"github.com/ruffel/pipeline"
)

// Each stage builder wires step structs via bound method values:
// (&Struct{...}).Run passes the method as a [pipeline.StepFn].

func preflight(cfg Config) pipeline.Stage {
	return pipeline.NewStage("preflight",
		pipeline.NewStep("check-cluster", (&HealthCheck{
			Endpoint: "https://k8s.example.com/healthz",
		}).Run),
		pipeline.NewStep("validate-config", (&ValidateConfig{
			Config: cfg,
		}).Run),
	)
}

// build compiles services and pushes the image concurrently.
func build(cfg Config, state *State) pipeline.Stage {
	return pipeline.NewParallelStage("build",
		pipeline.NewStep("compile-api", (&Compile{Service: "api"}).Run),
		pipeline.NewStep("compile-worker", (&Compile{Service: "worker"}).Run),
		pipeline.NewStep("push-image", (&PushImage{
			Image: cfg.Image,
			Tag:   cfg.Tag,
			State: state,
		}).Run),
	)
}

func deploy(cfg Config, state *State) pipeline.Stage {
	return pipeline.NewStage("deploy",
		pipeline.NewStep("apply-manifest", (&ApplyManifest{
			Namespace: cfg.Namespace,
			Replicas:  cfg.Replicas,
			State:     state,
		}).Run),
	)
}

// verify runs checks in parallel. ContinueOnError ensures all checks
// complete even if one fails — errors are joined and reported together.
func verify(cfg Config) pipeline.Stage {
	return pipeline.NewParallelStage("verify",
		pipeline.NewStep("smoke-test", (&SmokeTest{
			Endpoint: fmt.Sprintf("https://%s.example.com/health", cfg.Namespace),
		}).Run).WithRetry(3, time.Second).WithTimeout(2*time.Second),
		pipeline.NewStep("check-metrics", (&CheckMetrics{
			Namespace: cfg.Namespace,
		}).Run),
	).WithContinueOnError(true)
}

// notify is skipped entirely in dry-run mode.
func notify(cfg Config, state *State) pipeline.Stage {
	return pipeline.NewStage("notify",
		pipeline.NewStep("post-slack", (&NotifySlack{
			Channel: "#deployments",
			State:   state,
		}).Run),
	).WithCondition(func(_ context.Context) string {
		if cfg.DryRun {
			return "dry-run mode"
		}

		return ""
	})
}
