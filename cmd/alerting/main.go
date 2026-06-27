// Command alerting consumes check.results, reduces per-region results to one
// verdict, runs the reused state machine, and emits notify.events (RFC-006). In
// the barebones it boots, connects to Postgres + Redis, a consumer for
// check.results and a producer for notify.events, and serves health.
package main

import (
	"context"
	"fmt"
	"os"

	"pulse/internal/alerting"
	"pulse/internal/bus"
	"pulse/internal/config"
	"pulse/internal/runtime"
)

func main() {
	svc, err := runtime.Setup(config.ServiceAlerting)
	if err != nil {
		fmt.Fprintln(os.Stderr, "alerting:", err)
		os.Exit(1)
	}

	ctx := context.Background()
	pg, err := svc.ConnectPostgres(ctx)
	if err != nil {
		svc.Log.Error("connect postgres", "err", err)
		os.Exit(1)
	}
	if _, err := svc.ConnectRedis(ctx); err != nil { // required by config; the aggregation window lands later (RFC-006 feature 6)
		svc.Log.Error("connect redis", "err", err)
		os.Exit(1)
	}
	prod, err := svc.ConnectBusProducer()
	if err != nil {
		svc.Log.Error("connect kafka producer", "err", err)
		os.Exit(1)
	}
	cons, err := svc.ConnectBusConsumer("alerting", bus.TopicCheckResults)
	if err != nil {
		svc.Log.Error("connect kafka consumer", "err", err)
		os.Exit(1)
	}

	runner := alerting.NewRunner(pg, cons, prod, svc.Log, svc.Reg)
	loopCtx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := runner.Run(loopCtx); err != nil {
			svc.Log.Error("alerting loop", "err", err)
		}
	}()
	svc.AddCloser(func(context.Context) error { cancel(); return nil })

	if err := svc.Run(ctx); err != nil {
		svc.Log.Error("run", "err", err)
		os.Exit(1)
	}
}
