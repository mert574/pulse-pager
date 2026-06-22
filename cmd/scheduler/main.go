// Command scheduler owns the schedule for every org's monitors and fans out one
// check job per (monitor, region) to Kafka (RFC-004). It runs as a single
// instance; leader election is a later package (RFC-004 / ADR-0004), safe for
// now since a single scheduler cannot double-dispatch.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"pulse/internal/config"
	"pulse/internal/runtime"
	"pulse/internal/scheduler"
)

func main() {
	svc, err := runtime.Setup(config.ServiceScheduler)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scheduler:", err)
		os.Exit(1)
	}

	ctx := context.Background()
	pg, err := svc.ConnectPostgres(ctx)
	if err != nil {
		svc.Log.Error("connect postgres", "err", err)
		os.Exit(1)
	}
	rd, err := svc.ConnectRedis(ctx) // used to mark per-region "scheduled" live state
	if err != nil {
		svc.Log.Error("connect redis", "err", err)
		os.Exit(1)
	}
	prod, err := svc.ConnectKafkaProducer()
	if err != nil {
		svc.Log.Error("connect kafka producer", "err", err)
		os.Exit(1)
	}

	disp := scheduler.New(pg, prod, rd, svc.Log, time.Second)
	loopCtx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := disp.Run(loopCtx); err != nil {
			svc.Log.Error("scheduler loop", "err", err)
		}
	}()
	svc.AddCloser(func(context.Context) error { cancel(); return nil })

	if err := svc.Run(ctx); err != nil {
		svc.Log.Error("run", "err", err)
		os.Exit(1)
	}
}
