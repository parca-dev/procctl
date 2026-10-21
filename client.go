//go:build linux

package procctl

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Key identifies a region independently of which process holds it.
type Key struct{ Dev, Ino uint64 }

// NewOwnerID returns a random, non-zero owner id for one agent instance.
func NewOwnerID() uint64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return binary.LittleEndian.Uint64(b[:]) | 1
}

// AttachOptions configures Attach.
type AttachOptions struct {
	OwnerID       uint64        // required: this agent instance (NewOwnerID)
	Lease         time.Duration // written on claim; processes revert after this long without a heartbeat
	ProcRoot      string        // default "/proc"
	MaxRegionSize int64         // default 4 MiB
	ReadTimeout   time.Duration // remote state reads; default 1s
}

// Info describes an attached region.
type Info struct {
	Key     Key
	HostPID int   // in the agent's pid namespace
	NSPid   []int // the process's pid in each namespace, outermost first
	Header  Header
}

// Client is one attached region. Methods are safe for concurrent use.
type Client struct {
	mu     sync.Mutex
	info   Info
	mem    []byte
	area   area
	pidfd  int
	opt    AttachOptions
	owned  bool
	closed bool
	done   chan struct{}
	once   sync.Once

	hbSeen   uint64 // the owner's heartbeat last seen...
	hbSeenAt time.Time

	pub    []byte // our last payload, republished if another writer clobbered it
	pubGen uint64
}

var (
	// ErrExited: the process has exited.
	ErrExited = errors.New("procctl: process exited")
	// ErrClosed: the client was closed.
	ErrClosed = errors.New("procctl: client closed")
)

// Attach validates and maps the region behind fd in process pid. The region
// comes from an untrusted process: every check here is a reason to refuse it.
func Attach(pid, fd int, opt AttachOptions) (*Client, error) {
	if opt.OwnerID == 0 {
		return nil, errors.New("procctl: AttachOptions.OwnerID is required")
	}
	if opt.ProcRoot == "" {
		opt.ProcRoot = "/proc"
	}
	if opt.MaxRegionSize == 0 {
		opt.MaxRegionSize = DefaultMaxRegion
	}
	if opt.ReadTimeout == 0 {
		opt.ReadTimeout = time.Second
	}
	path := fmt.Sprintf("%s/%d/fd/%d", opt.ProcRoot, pid, fd)
	link, err := os.Readlink(path)
	if err != nil {
		return nil, err
	}
	consumer, version, ok := parseMemfdName(link)
	if !ok {
		return nil, invalid("not a procctl memfd: %q", link)
	}
	if version != Version {
		return nil, invalid("version %d in name", version)
	}

	// The pidfd pins the process: reads go to it and nothing else, and it
	// tells the registry when the process exits.
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, fmt.Errorf("pidfd_open: %w", err)
	}
	ok = false
	defer func() {
		if !ok {
			unix.Close(pidfd)
		}
	}()
	rfd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer unix.Close(rfd)
	if err := unix.PidfdSendSignal(pidfd, 0, nil, 0); err != nil {
		return nil, ErrExited // pid reused between readlink and pidfd_open
	}

	var st unix.Stat_t
	var sfs unix.Statfs_t
	if err := unix.Fstat(rfd, &st); err != nil {
		return nil, err
	}
	if err := unix.Fstatfs(rfd, &sfs); err != nil {
		return nil, err
	}
	switch {
	case st.Mode&unix.S_IFMT != unix.S_IFREG || sfs.Type != unix.TMPFS_MAGIC:
		return nil, invalid("not a memfd")
	case st.Size <= 0 || st.Size > opt.MaxRegionSize:
		return nil, invalid("size %d", st.Size)
	}
	// The seals keep the size fixed (no SIGBUS) and the header final.
	seals, err := unix.FcntlInt(uintptr(rfd), unix.F_GET_SEALS, 0)
	if err != nil {
		return nil, fmt.Errorf("F_GET_SEALS: %w", err)
	}
	need := unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_SEAL
	if seals&need != need || seals&(unix.F_SEAL_WRITE|unix.F_SEAL_FUTURE_WRITE) != 0 {
		return nil, invalid("seals %#x", seals)
	}

	mem, err := unix.Mmap(rfd, 0, int(st.Size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap: %w", err)
	}
	h, err := ParseHeader(append([]byte(nil), mem[:HeaderSize]...), uint64(st.Size))
	if err == nil && h.Consumer != consumer {
		err = invalid("consumer %q, name says %q", h.Consumer, consumer)
	}
	var ns []int
	if err == nil {
		ns, err = nsPIDs(opt.ProcRoot, pid)
	}
	if err != nil {
		unix.Munmap(mem)
		return nil, err
	}
	_ = unix.Mprotect(mem[:h.PageSize], unix.PROT_READ) // we only ever read the header copy
	ok = true
	return &Client{
		info:  Info{Key: Key{Dev: uint64(st.Dev), Ino: st.Ino}, HostPID: pid, NSPid: ns, Header: h},
		mem:   mem,
		area:  area{body: mem[h.PageSize:]},
		pidfd: pidfd,
		opt:   opt,
		done:  make(chan struct{}),
	}, nil
}

// parseMemfdName parses "/memfd:procctl:<consumer>-v<version>[ (deleted)]".
func parseMemfdName(link string) (consumer string, version int, ok bool) {
	s, found := strings.CutPrefix(link, memfdPrefix)
	if !found {
		return "", 0, false
	}
	s = strings.TrimSuffix(s, " (deleted)")
	i := strings.LastIndex(s, "-v")
	if i <= 0 {
		return "", 0, false
	}
	v, err := strconv.Atoi(s[i+2:])
	return s[:i], v, err == nil
}

// Info returns the region's identity and header.
func (c *Client) Info() Info { return c.info }

// Done is closed when the process exits or the client is closed.
func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) markDone() { c.once.Do(func() { close(c.done) }) }

// Owned reports whether this agent owns the region.
func (c *Client) Owned() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.owned && ld64(&c.area.cb().Owner_id) == c.opt.OwnerID
}

// claimLocked becomes the owner if the region has none, its owner released
// it, or the owner's heartbeat has not changed for a lease.
func (c *Client) claimLocked(now time.Time) bool {
	if c.closed {
		return false
	}
	cb := c.area.cb()
	owner := ld64(&cb.Owner_id)
	if owner == c.opt.OwnerID {
		c.owned = true
		return true
	}
	if hb := ld64(&cb.Heartbeat_ns); hb != c.hbSeen || c.hbSeenAt.IsZero() {
		c.hbSeen, c.hbSeenAt = hb, now
	}
	lease := time.Duration(ld64(&cb.Lease_ns))
	free := owner == 0 || ld32(&cb.Flags)&flagReleased != 0 || (lease > 0 && now.Sub(c.hbSeenAt) > lease)
	if !free || !cas64(&cb.Owner_id, owner, c.opt.OwnerID) {
		c.owned = false
		return false
	}
	st32(&cb.Flags, 0)
	st64(&cb.Lease_ns, uint64(c.opt.Lease))
	c.heartbeatLocked()
	c.owned = true
	// Keep the current configuration: it is ours to republish from now on.
	if gen, payload, err := c.area.read(); err == nil && gen != 0 {
		c.pub, c.pubGen = payload, gen
	}
	return true
}

func (c *Client) heartbeatLocked() {
	cb := c.area.cb()
	now := uint64(bootNow())
	if prev := ld64(&cb.Heartbeat_ns); now <= prev {
		now = prev + 1 // strictly increasing, whatever the clocks
	}
	st64(&cb.Heartbeat_ns, now)
}

// ownLocked claims the region if it can, and checks nobody took it over.
func (c *Client) ownLocked() error {
	if c.closed {
		return ErrClosed
	}
	if !c.owned && !c.claimLocked(time.Now()) {
		return ErrNotOwner
	}
	if ld64(&c.area.cb().Owner_id) != c.opt.OwnerID {
		c.owned = false
		return ErrPreempted
	}
	return nil
}

// Write publishes payload as a new configuration generation and returns it.
// An empty payload tells the process to use its defaults.
func (c *Client) Write(payload []byte) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ownLocked(); err != nil {
		return 0, err
	}
	gen, err := c.area.write(payload)
	if err != nil {
		return 0, err
	}
	c.pub, c.pubGen = append([]byte(nil), payload...), gen
	return gen, nil
}

// Heartbeat keeps the lease alive (call it every Lease/4 or so), claiming the
// region first if this agent may. If another writer clobbered our last
// payload (an old owner that resumed mid-write), it is published again.
func (c *Client) Heartbeat() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ownLocked(); err != nil {
		return err
	}
	c.heartbeatLocked()
	if c.pubGen != 0 {
		if gen, payload, err := c.area.read(); err != nil || gen != c.pubGen || string(payload) != string(c.pub) {
			if gen, err := c.area.write(c.pub); err == nil {
				c.pubGen = gen
			}
		}
	}
	return nil
}

// ReadState reads the process's state block.
func (c *Client) ReadState(ctx context.Context) (State, error) {
	select {
	case <-c.done:
		return State{}, ErrExited
	default:
	}
	ns := c.info.NSPid
	return readState(ctx, c.info.HostPID, &c.info.Header, ns[len(ns)-1], c.opt.ReadTimeout)
}

// WaitAck waits until the process has applied generation gen.
func (c *Client) WaitAck(ctx context.Context, gen uint64) error {
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		st, err := c.ReadState(ctx)
		if errors.Is(err, ErrExited) {
			return err
		}
		if err == nil && !st.Stale && st.AppliedGen >= gen {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return ErrExited
		case <-t.C:
		}
	}
}

// release gives up ownership: RELEASED (a successor takes over at once, the
// process keeps its configuration) or, with uninstall, no owner (the process
// reverts to its defaults now).
func (c *Client) release(uninstall bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.owned || c.closed {
		return
	}
	cb := c.area.cb()
	if uninstall {
		cas64(&cb.Owner_id, c.opt.OwnerID, 0)
	} else if ld64(&cb.Owner_id) == c.opt.OwnerID {
		st32(&cb.Flags, flagReleased)
	}
	c.owned = false
}

// Close unmaps the region. It does not give up ownership; Registry.Close does.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.markDone()
	unix.Munmap(c.mem)
	return unix.Close(c.pidfd)
}

func bootNow() int64 {
	var ts unix.Timespec
	_ = unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts)
	return ts.Nano()
}
