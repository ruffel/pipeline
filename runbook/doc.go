// Package runbook runs durable, resumable plans: trees of groups and steps
// whose outcomes are recorded in a journal, so a run can be stopped, resumed
// after a crash, and watched from another process.
//
// The package is being built in stages and its API is not yet stable. It will
// replace the root pipeline package's executor once complete.
//
// A plan is data. It is built once, validated by [NewPlan], and does not change
// during a run:
//
//	plan, err := runbook.NewPlan("upgrade",
//	    runbook.Group("prechecks", checks...).Parallel(8).ContinueOnError(),
//	    runbook.Effect("backup", backup{}),
//	    runbook.Group("node1",
//	        runbook.Effect("stop", stop{n}),
//	        runbook.Effect("packages", packages{n}),
//	        runbook.Observe("verify", verify{n}),
//	    ).Labels(runbook.L{"node": "node1"}),
//	)
//
// Every element has a path built from its name and its parents' names, such
// as "upgrade/node1/packages". The path is the element's identity.
package runbook
