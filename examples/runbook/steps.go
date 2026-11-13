package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"time"

	"github.com/ruffel/pipeline/runbook"
)

func newPlan(c *cluster, approvers []runbook.Approver, fail string) (*runbook.Plan, error) {
	return runbook.NewPlan("db-failover",
		runbook.Group("preflight",
			runbook.Observe("check-cluster", report("primary db1 and replica db2 reachable", 300*time.Millisecond)).
				Description("Verifies both database nodes are reachable"),
			runbook.Observe("check-replica-lag", report("replication lag 0.4s, within threshold", 300*time.Millisecond)).
				Description("The replica must be under 5s behind before cutting over"),
		).Description("Read-only checks, safe to run again"),

		runbook.Gate("confirm-failover", runbook.GateOptions{
			Prompt:    "Proceed with failover? Traffic pauses for ~30s",
			Approvers: approvers,
		}).Description("The last safe moment to abort"),

		runbook.Group("failover",
			runbook.Effect("demote-primary", change{
				c: c, fail: fail == "demote-primary",
				done:  func(s *clusterState) bool { return slices.Contains(s.ReadOnly, "db1") },
				apply: func(s *clusterState) { s.ReadOnly = append(s.ReadOnly, "db1") },
				msg:   "db1 set read-only",
			}).Description("Sets the current primary read-only"),
			runbook.Effect("promote-replica", change{
				c: c, fail: fail == "promote-replica",
				done:  func(s *clusterState) bool { return s.Primary == "db2" },
				apply: func(s *clusterState) { s.Primary = "db2" },
				msg:   "db2 promoted to primary",
			}).Description("Promotes the replica to primary"),
			runbook.Effect("update-dns", change{
				c: c, fail: fail == "update-dns",
				done:  func(s *clusterState) bool { return s.DNS == "db2" },
				apply: func(s *clusterState) { s.DNS = "db2" },
				msg:   "db.internal points at db2",
			}).Description("Repoints db.internal at the new primary"),
		).Description("Changes the cluster; each step asks it whether its change is made"),

		runbook.Group("verify",
			runbook.Observe("smoke-test", report("round-trip write OK", 500*time.Millisecond)).
				Description("Round-trip write against the new primary"),
			runbook.Observe("check-metrics", report("error rate 0.02%, nominal", 600*time.Millisecond)).
				Description("Error rate must stay under 0.1%"),
		).Parallel(2).Description("Health checks after the failover"),

		// A gate also waits while a person does work the runbook can't.
		runbook.Gate("incident-doc", runbook.GateOptions{
			Prompt:    "Update the incident doc with the failover time, then approve",
			Approvers: approvers,
		}).Description("A manual step: the paper trail lives outside this tool"),
	)
}

// approvers answers the gates: the operator at the terminal, or, with -yes,
// an approver that approves at once. [runbook.Approve] can release a gate
// from another terminal too.
func approvers(yes bool) []runbook.Approver {
	if yes {
		return []runbook.Approver{autoApprove{}}
	}

	return []runbook.Approver{runbook.StdinApprover()}
}

// autoApprove is an Approver that approves every gate as soon as it waits.
// The journal records it by name as what released the gate.
type autoApprove struct{}

func (autoApprove) Name() string { return "-yes" }

func (autoApprove) Wait(context.Context, runbook.GateInfo) (string, error) {
	return who(), nil
}

// change is a failover step. Its Check asks the cluster whether the change
// is made, so a resumed run, or one after a crash, doesn't make it twice.
type change struct {
	c     *cluster
	done  func(*clusterState) bool
	apply func(*clusterState)
	msg   string
	fail  bool
}

func (s change) Check(context.Context, *runbook.StepContext) (bool, error) {
	st, err := s.c.load()
	if err != nil {
		return false, err
	}

	return s.done(&st), nil
}

func (s change) Run(ctx context.Context, sc *runbook.StepContext) error {
	if err := sleep(ctx, 400*time.Millisecond); err != nil {
		return err
	}

	if s.fail {
		return errors.New("the cluster refused the change (-fail)")
	}

	st, err := s.c.load()
	if err != nil {
		return err
	}

	s.apply(&st)

	if err := s.c.save(st); err != nil {
		return err
	}

	sc.Info(s.msg)

	return nil
}

// report is a read-only step that waits for d and reports msg.
func report(msg string, d time.Duration) runbook.StepFunc {
	return func(ctx context.Context, sc *runbook.StepContext) error {
		if err := sleep(ctx, d); err != nil {
			return err
		}

		sc.Info(msg)

		return nil
	}
}

// cluster is the mock cluster, kept in a file so it outlives the process.
type cluster struct{ file string }

type clusterState struct {
	Primary  string   `json:"primary"`
	ReadOnly []string `json:"readOnly,omitempty"`
	DNS      string   `json:"dns"`
}

func (c *cluster) load() (clusterState, error) {
	b, err := os.ReadFile(c.file)
	if errors.Is(err, os.ErrNotExist) {
		return clusterState{Primary: "db1", DNS: "db1"}, nil
	}

	if err != nil {
		return clusterState{}, err
	}

	var st clusterState

	return st, json.Unmarshal(b, &st)
}

func (c *cluster) save(st clusterState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}

	return os.WriteFile(c.file, b, 0o600)
}

// sleep waits for d, or until ctx is done: a cancelled run cuts steps short.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
