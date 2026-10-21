//go:build linux

// echo-agent: pushes a message to every echo client on the host, waits for
// the acknowledgement, and prints each client's reported state.
//
//	go run ./examples/echo/agent [-interval 2s] [-lease 3s] [-rescan 1s] [-uninstall]
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/parca-dev/procctl"
)

func main() {
	interval := flag.Duration("interval", 2*time.Second, "how often to push a new message")
	lease := flag.Duration("lease", 3*time.Second, "lease written into each region")
	rescan := flag.Duration("rescan", time.Second, "discovery rescan interval")
	uninstall := flag.Bool("uninstall", false, "on exit, clear ownership so clients revert immediately")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	reg, err := procctl.NewRegistry(procctl.RegistryOptions{
		Consumer: "echo", RescanInterval: *rescan, Lease: *lease, Logger: log,
	})
	if err != nil {
		log.Error("registry", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { _ = reg.Run(ctx) }()

	n := 0
	push := func(c *procctl.Client) {
		n++
		msg := fmt.Sprintf("hello #%d from agent pid %d at %s", n, os.Getpid(), time.Now().Format("15:04:05.000"))
		t0 := time.Now()
		gen, err := c.Write([]byte(msg))
		if err != nil {
			log.Warn("write", "pid", c.Info().HostPID, "err", err)
			return
		}
		actx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := c.WaitAck(actx, gen); err != nil {
			log.Warn("no ack", "pid", c.Info().HostPID, "gen", gen, "err", err)
			return
		}
		st, err := c.ReadState(ctx)
		if err != nil {
			log.Warn("read state", "pid", c.Info().HostPID, "err", err)
			return
		}
		log.Info("acked", "pid", c.Info().HostPID, "gen", gen, "latency", time.Since(t0).Round(time.Microsecond),
			"state", string(st.Consumer))
	}

	t := time.NewTicker(*interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if *uninstall {
				_ = reg.Uninstall()
			} else {
				_ = reg.Close()
			}
			log.Info("closed", "uninstall", *uninstall)
			return
		case ev := <-reg.Events():
			if ev.Kind == procctl.Attached {
				push(ev.Client)
			} else {
				log.Info("detached", "pid", ev.Client.Info().HostPID, "reason", ev.Reason)
			}
		case <-t.C:
			for _, c := range reg.Clients() {
				push(c)
			}
		}
	}
}
