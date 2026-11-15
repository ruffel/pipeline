// Package pipeline runs durable, resumable plans: trees of groups and steps
// whose outcomes are recorded in a journal, so a run can be stopped, resumed
// after a crash, and watched from another process.
//
// A plan is data. It is built once, validated by [NewPlan], and does not change
// during a run:
//
//	plan, err := pipeline.NewPlan("upgrade",
//	    pipeline.Group("prechecks", checks...).Parallel(8).ContinueOnError(),
//	    pipeline.Effect("backup", backup{}),
//	    pipeline.Group("node1",
//	        pipeline.Effect("stop", stop{n}),
//	        pipeline.Effect("packages", packages{n}),
//	        pipeline.Observe("verify", verify{n}),
//	    ).Labels(pipeline.L{"node": "node1"}),
//	)
//
// Every element has a path built from its name and its parents' names, such
// as "upgrade/node1/packages". The path is the element's identity.
package pipeline
