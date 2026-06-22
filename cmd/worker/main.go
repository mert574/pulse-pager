// Command worker is the regional check executor: consume check.jobs.<region>,
// run the HTTP check by reusing internal/checker (SSRF guard toggled by config),
// write the result, and emit check.results (RFC-005).
package main

import (
	"context"
	"fmt"
	"os"

	"pulse/internal/bus"
	"pulse/internal/checker"
	"pulse/internal/config"
	"pulse/internal/entitlements"
	"pulse/internal/runtime"
	"pulse/internal/worker"
)

func main() {
	svc, err := runtime.Setup(config.ServiceWorker)
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		os.Exit(1)
	}

	ctx := context.Background()
	pg, err := svc.ConnectPostgres(ctx)
	if err != nil {
		svc.Log.Error("connect postgres", "err", err)
		os.Exit(1)
	}
	rd, err := svc.ConnectRedis(ctx) // used for the check-now per-region live state
	if err != nil {
		svc.Log.Error("connect redis", "err", err)
		os.Exit(1)
	}
	prod, err := svc.ConnectBusProducer()
	if err != nil {
		svc.Log.Error("connect kafka producer", "err", err)
		os.Exit(1)
	}
	region := svc.Cfg.Region
	cons, err := svc.ConnectBusConsumer("worker-"+region, bus.CheckJobsTopic(region))
	if err != nil {
		svc.Log.Error("connect kafka consumer", "err", err, "region", region)
		os.Exit(1)
	}

	chk := checker.New(checker.Config{BlockPrivateNetworks: svc.Cfg.BlockPrivateNetworks})
	// AllOn until per-plan gating ships (RFC-009); swap in the real resolver here.
	runner := worker.New(pg, cons, prod, chk, entitlements.AllOn{}, rd, region, svc.Log)
	loopCtx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := runner.Run(loopCtx); err != nil {
			svc.Log.Error("worker loop", "err", err)
		}
	}()
	svc.AddCloser(func(context.Context) error { cancel(); return nil })

	if err := svc.Run(ctx); err != nil {
		svc.Log.Error("run", "err", err)
		os.Exit(1)
	}
}
