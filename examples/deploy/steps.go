package main

import (
	"context"
	"fmt"
	"time"

	"github.com/ruffel/pipeline"
)

// HealthCheck verifies that a remote endpoint is reachable.
type HealthCheck struct {
	Endpoint string
}

func (h *HealthCheck) Run(ctx context.Context) error {
	pipeline.EmitInfof(ctx, "checking %s", h.Endpoint)
	sleep(200)

	pipeline.EmitInfo(ctx, "cluster healthy")

	return nil
}

// ValidateConfig checks that the deployment configuration is complete.
type ValidateConfig struct {
	Config Config
}

func (v *ValidateConfig) Run(ctx context.Context) error {
	pipeline.EmitInfof(ctx, "validating config for %s/%s", v.Config.Region, v.Config.Namespace)
	sleep(150)

	if v.Config.Replicas < 1 {
		return fmt.Errorf("replicas must be >= 1, got %d", v.Config.Replicas)
	}

	return nil
}

// PushImage tags and pushes a container image to a registry.
// Writes the resolved image ref to State for downstream steps.
type PushImage struct {
	Image string
	Tag   string
	State *State
}

func (p *PushImage) Run(ctx context.Context) error {
	ref := fmt.Sprintf("%s:%s", p.Image, p.Tag)
	total := 4

	for i := 1; i <= total; i++ {
		pipeline.EmitProgress(ctx, "push", fmt.Sprintf("pushing %s", ref), i, total)
		sleep(200)
	}

	p.State.ImageRef = ref

	return nil
}

// ApplyManifest renders and applies Kubernetes manifests.
// Reads the image ref from State (set by PushImage).
type ApplyManifest struct {
	Namespace string
	Replicas  int
	State     *State
}

func (a *ApplyManifest) Run(ctx context.Context) error {
	pipeline.EmitInfof(ctx, "deploying %s to %s (%d replicas)", a.State.ImageRef, a.Namespace, a.Replicas)

	w := pipeline.OutputWriter(ctx, pipeline.Stdout)

	fmt.Fprintf(w, "namespace/%s configured\n", a.Namespace)
	sleep(150)
	fmt.Fprintf(w, "deployment.apps/%s configured\n", "app")
	sleep(100)
	fmt.Fprintf(w, "service/%s unchanged\n", "app")

	return w.Close()
}

type SmokeTest struct {
	Endpoint string
}

func (s *SmokeTest) Run(ctx context.Context) error {
	pipeline.EmitInfof(ctx, "hitting %s", s.Endpoint)
	sleep(300)

	return nil
}

type Compile struct {
	Service string
}

func (c *Compile) Run(ctx context.Context) error {
	total := 3

	for i := 1; i <= total; i++ {
		pipeline.EmitProgress(ctx, "compile", fmt.Sprintf("compiling %s", c.Service), i, total)
		sleep(200)
	}

	return nil
}

type CheckMetrics struct {
	Namespace string
}

func (c *CheckMetrics) Run(ctx context.Context) error {
	pipeline.EmitInfof(ctx, "checking error rates in %s", c.Namespace)
	sleep(250)

	pipeline.EmitInfo(ctx, "error rate 0.02% — within threshold")

	return nil
}

// NotifySlack posts a deployment notification.
// Reads the image ref from State (set by PushImage).
type NotifySlack struct {
	Channel string
	State   *State
}

func (n *NotifySlack) Run(ctx context.Context) error {
	pipeline.EmitInfof(ctx, "posting to %s: deployed %s", n.Channel, n.State.ImageRef)
	sleep(150)

	return nil
}

func sleep(ms int) {
	time.Sleep(time.Duration(ms) * time.Millisecond) //nolint:mnd
}
