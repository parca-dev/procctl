//go:build linux

package procctl

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// RegistryOptions configures a Registry.
type RegistryOptions struct {
	Consumer       string        // only this consumer; "" = all
	ProcRoot       string        // default "/proc"
	RescanInterval time.Duration // default 60s
	Lease          time.Duration // default 10s; heartbeat every Lease/4
	ReadTimeout    time.Duration // remote state reads; default 1s
	MaxRegionSize  int64
	Logger         *slog.Logger
	// ForceProcFallback skips pidfd_getfd and opens /proc/<pid>/fd/<n>.
	ForceProcFallback bool
	// OnReject, if set, is called for every region that fails attach or
	// validation (SPEC §10.2), e.g. to count rejections by reason.
	OnReject func(pid, fd int, err error)
}

// EventKind distinguishes registry events.
type EventKind int

const (
	Attached EventKind = iota
	Detached
)

// Event reports a region being attached or detached.
type Event struct {
	Kind   EventKind
	Client *Client
	Reason string
}

// Registry discovers, attaches and maintains regions: it owns them,
// heartbeats them, and detaches them when their process exits.
type Registry struct {
	opt     RegistryOptions
	id      Identity
	log     *slog.Logger
	notify  chan int
	events  chan Event
	first   chan struct{}
	mu      sync.Mutex
	clients map[Key]*Client
	byPidfd map[int]*Client
	epfd    int
	closed  bool
}

// NewRegistry creates a registry with a fresh agent identity.
func NewRegistry(opt RegistryOptions) (*Registry, error) {
	if opt.ProcRoot == "" {
		opt.ProcRoot = "/proc"
	}
	if opt.RescanInterval == 0 {
		opt.RescanInterval = 60 * time.Second
	}
	if opt.Lease == 0 {
		opt.Lease = 10 * time.Second
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	id, err := NewIdentity()
	if err != nil {
		return nil, err
	}
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}
	return &Registry{
		opt:     opt,
		id:      id,
		log:     opt.Logger,
		notify:  make(chan int, 64),
		events:  make(chan Event, 256),
		first:   make(chan struct{}),
		clients: map[Key]*Client{},
		byPidfd: map[int]*Client{},
		epfd:    epfd,
	}, nil
}

// Identity returns this agent's owner identity.
func (r *Registry) Identity() Identity { return r.id }

// Events delivers Attached/Detached events. Events are dropped if the
// channel is full.
func (r *Registry) Events() <-chan Event { return r.events }

// FirstScanDone is closed after the first full discovery pass (SPEC §9.2 A5).
func (r *Registry) FirstScanDone() <-chan struct{} { return r.first }

// Notify requests an immediate scan of one pid (hello probe, mapping event).
func (r *Registry) Notify(pid int) {
	select {
	case r.notify <- pid:
	default:
	}
}

// Clients returns the attached clients.
func (r *Registry) Clients() []*Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Client, 0, len(r.clients))
	for _, c := range r.clients {
		out = append(out, c)
	}
	return out
}

// Run discovers and maintains regions until ctx is done.
func (r *Registry) Run(ctx context.Context) error {
	go r.watchExits(ctx)
	r.scan(ctx, 0)
	close(r.first)
	rescan := time.NewTicker(r.opt.RescanInterval)
	defer rescan.Stop()
	hb := time.NewTicker(r.opt.Lease / 4)
	defer hb.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-rescan.C:
			r.scan(ctx, 0)
		case pid := <-r.notify:
			r.scan(ctx, pid)
		case <-hb.C:
			for _, c := range r.Clients() {
				if err := c.Heartbeat(); err != nil && !errors.Is(err, ErrNotOwner) {
					r.log.Debug("heartbeat", "pid", c.info.HostPID, "err", err)
				}
			}
		}
	}
}

// scan attaches every new region; pid > 0 limits it to that process.
func (r *Registry) scan(ctx context.Context, pid int) {
	opt := ScanOptions{ProcRoot: r.opt.ProcRoot, Consumer: r.opt.Consumer}
	cands, stats, err := Scan(ctx, opt)
	if err != nil && ctx.Err() == nil {
		r.log.Warn("scan failed", "err", err)
		return
	}
	r.log.Debug("scan", "procs", stats.Procs, "fds", stats.FDs, "took", stats.Duration, "found", len(cands))
	// Several pids can hold one region (fork); prefer the lowest pid.
	sort.Slice(cands, func(i, j int) bool { return cands[i].HostPID < cands[j].HostPID })
	for _, cand := range cands {
		if pid > 0 && cand.HostPID != pid {
			continue
		}
		r.mu.Lock()
		_, known := r.clients[cand.Key]
		r.mu.Unlock()
		if known || cand.Major != SpecMajor {
			continue
		}
		c, err := Attach(cand.HostPID, cand.FD, AttachOptions{
			ProcRoot: r.opt.ProcRoot, MaxRegionSize: r.opt.MaxRegionSize,
			Identity: r.id, Lease: r.opt.Lease, ReadTimeout: r.opt.ReadTimeout,
			ForceProcFallback: r.opt.ForceProcFallback,
		})
		if err != nil {
			r.log.Info("attach rejected", "pid", cand.HostPID, "fd", cand.FD, "err", err)
			if r.opt.OnReject != nil {
				r.opt.OnReject(cand.HostPID, cand.FD, err)
			}
			continue
		}
		r.add(c)
	}
}

func (r *Registry) add(c *Client) {
	r.mu.Lock()
	if _, dup := r.clients[c.info.Key]; dup || r.closed {
		r.mu.Unlock()
		c.Close()
		return
	}
	r.clients[c.info.Key] = c
	r.byPidfd[c.pidfd] = c
	r.mu.Unlock()
	ev := unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(c.pidfd)}
	_ = unix.EpollCtl(r.epfd, unix.EPOLL_CTL_ADD, c.pidfd, &ev)
	c.mu.Lock()
	c.claimLocked(time.Now())
	c.mu.Unlock()
	r.log.Info("attached", "pid", c.info.HostPID, "consumer", c.info.Header.Consumer,
		"path", c.info.AttachPath, "owned", c.Owned())
	r.emit(Event{Kind: Attached, Client: c})
}

func (r *Registry) remove(c *Client, reason string) {
	r.mu.Lock()
	if r.clients[c.info.Key] != c {
		r.mu.Unlock()
		return
	}
	delete(r.clients, c.info.Key)
	delete(r.byPidfd, c.pidfd)
	r.mu.Unlock()
	_ = unix.EpollCtl(r.epfd, unix.EPOLL_CTL_DEL, c.pidfd, nil)
	c.markDone()
	r.emit(Event{Kind: Detached, Client: c, Reason: reason})
	c.Close()
}

// watchExits detaches clients whose pidfd reports process exit.
func (r *Registry) watchExits(ctx context.Context) {
	evs := make([]unix.EpollEvent, 32)
	for ctx.Err() == nil {
		n, err := unix.EpollWait(r.epfd, evs, 500)
		if err != nil && !errors.Is(err, unix.EINTR) {
			return
		}
		for i := 0; i < n; i++ {
			r.mu.Lock()
			c := r.byPidfd[int(evs[i].Fd)]
			r.mu.Unlock()
			if c != nil {
				r.log.Info("process exited", "pid", c.info.HostPID)
				r.remove(c, "exit")
			}
		}
	}
}

func (r *Registry) emit(e Event) {
	select {
	case r.events <- e:
	default:
	}
}

// CloseOption configures Registry.Close.
type CloseOption func(*closeOpts)

type closeOpts struct{ clear bool }

// ReleaseLeases makes Close clear writer_id, so processes revert to their
// defaults immediately (an uninstall). By default Close sets RELEASED and a
// successor agent takes over without any reversion (SPEC §8.1).
func ReleaseLeases(clear bool) CloseOption { return func(o *closeOpts) { o.clear = clear } }

// Close releases ownership of every region and detaches them.
func (r *Registry) Close(opts ...CloseOption) error {
	var o closeOpts
	for _, f := range opts {
		f(&o)
	}
	r.mu.Lock()
	r.closed = true
	cs := make([]*Client, 0, len(r.clients))
	for _, c := range r.clients {
		cs = append(cs, c)
	}
	r.clients = map[Key]*Client{}
	r.byPidfd = map[int]*Client{}
	r.mu.Unlock()
	for _, c := range cs {
		c.release(o.clear)
		c.Close()
	}
	return unix.Close(r.epfd)
}
