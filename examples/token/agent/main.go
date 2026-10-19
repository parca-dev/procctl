//go:build linux

// token-agent: arbitrates the token resource among token clients on the host
// with procctl/exclusive, and prints who holds it.
//
//	go run ./examples/token/agent [-hold 3s] [-deadline 10s] [-lease 3s]
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/parca-dev/procctl"
	"github.com/parca-dev/procctl/exclusive"
)

func main() {
	hold := flag.Duration("hold", 3*time.Second, "per-turn hold time")
	deadline := flag.Duration("deadline", 10*time.Second, "grant duration (holder self-releases if the agent is gone)")
	lease := flag.Duration("lease", 3*time.Second, "lease written into each region")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	reg, err := procctl.NewRegistry(procctl.RegistryOptions{
		Consumer: "token", RescanInterval: time.Second, Lease: *lease, Logger: log,
	})
	if err != nil {
		log.Error("registry", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { _ = reg.Run(ctx) }()

	arb := exclusive.New(func() []exclusive.Target {
		var ts []exclusive.Target
		for _, c := range reg.Clients() {
			ts = append(ts, c)
		}
		return ts
	}, exclusive.Options{
		Kind:   1,
		Policy: exclusive.RoundRobin{Hold: *hold, Deadline: *deadline},
		Ready:  reg.FirstScanDone(),
		Logger: log,
	})
	go func() { _ = arb.Run(ctx) }()

	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = reg.Close()
			return
		case <-t.C:
			for _, s := range arb.Snapshot() {
				log.Info("resource", "holder", s.HolderPID, "grant", s.GrantID,
					"held", s.HeldFor.Round(time.Millisecond), "pending", s.Pending,
					"blocked", s.BlockedFor.Round(time.Millisecond), "grants", s.Grants, "renewals", s.Renewals)
			}
		}
	}
}
