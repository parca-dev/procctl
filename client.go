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

// Identity is one agent instance's owner identity (SPEC §8.1).
type Identity struct {
	WriterID uint64 // random, non-zero
	PID      uint32 // this agent's pid in its own pid namespace
	Start    uint64 // field 22 of /proc/self/stat
}

// NewIdentity creates a fresh identity for this agent process.
func NewIdentity() (Identity, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Identity{}, err
	}
	id := binary.LittleEndian.Uint64(b[:]) | 1
	st, err := startTime("/proc", os.Getpid())
	if err != nil {
		return Identity{}, err
	}
	return Identity{WriterID: id, PID: uint32(os.Getpid()), Start: st}, nil
}

// AttachOptions configures Attach.
type AttachOptions struct {
	ProcRoot          string        // default "/proc"
	MaxRegionSize     int64         // default 4 MiB
	ForceProcFallback bool          // skip pidfd_getfd; open /proc/<pid>/fd/<n>
	Identity          Identity      // required
	Lease             time.Duration // lease_ns written on claim; 0 = no lease
	ReadTimeout       time.Duration // remote state reads; default 1s
}

// Info describes an attached region.
type Info struct {
	Key           Key
	HostPID       int
	NSPid         []int
	Header        Header
	AttachPath    string // "pidfd_getfd" or "proc"
	PayloadCap    int
	ResourceSlots int
}

// Client is an attached region. Methods are safe for concurrent use.
type Client struct {
	mu       sync.Mutex
	info     Info
	area     area
	pidfd    int
	opt      AttachOptions
	owned    bool
	closed   bool
	done     chan struct{}
	doneOnce sync.Once

	// Claim rule 4 bookkeeping: heartbeat value and when we first saw it.
	hbSeen   uint64
	hbSeenAt time.Time

	// Last publication, for republish on CRC drift (SPEC §7.4).
	pubValid   bool
	pubGen     uint64
	pubPayload []byte
	pubGrants  []Grant
	pubFlags   uint32
	pubCRC     uint32
	pubLen     uint32
}

var (
	// ErrExited: the process has exited.
	ErrExited = errors.New("procctl: process exited")
	// ErrClosed: the client was closed.
	ErrClosed = errors.New("procctl: client closed")
)

// Attach validates and maps the region behind fd in process pid (SPEC §10.2).
func Attach(pid, fd int, opt AttachOptions) (*Client, error) {
	if opt.ProcRoot == "" {
		opt.ProcRoot = "/proc"
	}
	if opt.MaxRegionSize == 0 {
		opt.MaxRegionSize = DefaultMaxRegion
	}
	if opt.ReadTimeout == 0 {
		opt.ReadTimeout = time.Second
	}
	if opt.Identity.WriterID == 0 {
		return nil, errors.New("procctl: AttachOptions.Identity is required")
	}
	fdPath := fmt.Sprintf("%s/%d/fd/%d", opt.ProcRoot, pid, fd)
	link, err := os.Readlink(fdPath)
	if err != nil {
		return nil, err
	}
	consumer, major, ok := parseMemfdName(link)
	if !ok {
		return nil, invalid("not a procctl memfd: %q", link)
	}
	if major != SpecMajor {
		return nil, invalid("spec major %d in name", major)
	}

	// 1. Pin the process.
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

	// 2. Obtain the fd.
	path := "proc"
	var rfd int
	if !opt.ForceProcFallback {
		rfd, err = unix.PidfdGetfd(pidfd, fd, 0)
		if err == nil {
			path = "pidfd_getfd"
		}
	}
	if path == "proc" {
		rfd, err = unix.Open(fdPath, unix.O_RDWR|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", fdPath, err)
		}
	}
	defer unix.Close(rfd)
	if err := unix.PidfdSendSignal(pidfd, 0, nil, 0); err != nil {
		return nil, ErrExited
	}

	// 3. File checks.
	var st unix.Stat_t
	if err := unix.Fstat(rfd, &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, invalid("not a regular file")
	}
	if st.Size <= 0 || st.Size > opt.MaxRegionSize {
		return nil, invalid("size %d outside (0, %d]", st.Size, opt.MaxRegionSize)
	}
	var sfs unix.Statfs_t
	if err := unix.Fstatfs(rfd, &sfs); err != nil {
		return nil, err
	}
	if sfs.Type != unix.TMPFS_MAGIC {
		return nil, invalid("not on tmpfs (fs magic %#x)", sfs.Type)
	}

	// 4. Seals.
	seals, err := unix.FcntlInt(uintptr(rfd), unix.F_GET_SEALS, 0)
	if err != nil {
		return nil, fmt.Errorf("F_GET_SEALS: %w", err)
	}
	need := unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_SEAL
	if seals&need != need {
		return nil, invalid("missing seals (have %#x)", seals)
	}
	if seals&(unix.F_SEAL_WRITE|unix.F_SEAL_FUTURE_WRITE) != 0 {
		return nil, invalid("write-sealed")
	}

	// 5–7. Map, snapshot and validate the header.
	mem, err := unix.Mmap(rfd, 0, int(st.Size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap: %w", err)
	}
	snap := append([]byte(nil), mem[:HeaderSize]...)
	h, err := ParseHeader(snap, uint64(st.Size))
	if err == nil && h.Consumer != consumer {
		err = invalid("consumer %q does not match name %q", h.Consumer, consumer)
	}
	if err != nil {
		unix.Munmap(mem)
		return nil, err
	}
	_ = unix.Mprotect(mem[:h.PageSize], unix.PROT_READ)

	ns, err := nsPIDs(opt.ProcRoot, pid)
	if err != nil {
		unix.Munmap(mem)
		return nil, err
	}
	c := &Client{
		info: Info{
			Key:           Key{Dev: uint64(st.Dev), Ino: st.Ino},
			HostPID:       pid,
			NSPid:         ns,
			Header:        h,
			AttachPath:    path,
			PayloadCap:    h.PayloadCapacity(),
			ResourceSlots: int(h.ResourceSlots),
		},
		area:  area{mem: mem, body: mem[h.AreaOff:], slots: int(h.ResourceSlots)},
		pidfd: pidfd,
		opt:   opt,
		done:  make(chan struct{}),
	}
	ok = true
	return c, nil
}

// parseMemfdName parses "/memfd:procctl:<consumer>-v<major>[ (deleted)]".
func parseMemfdName(link string) (consumer string, major int, ok bool) {
	if !strings.HasPrefix(link, memfdPrefix) {
		return "", 0, false
	}
	s := strings.TrimSuffix(link[len(memfdPrefix):], " (deleted)")
	i := strings.LastIndex(s, "-v")
	if i <= 0 {
		return "", 0, false
	}
	m, err := strconv.Atoi(s[i+2:])
	if err != nil {
		return "", 0, false
	}
	return s[:i], m, true
}

// Info returns the region's identity and layout.
func (c *Client) Info() Info { return c.info }

// Done is closed when the process exits or the client is closed.
func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) markDone() { c.doneOnce.Do(func() { close(c.done) }) }

// Owned reports whether this agent currently owns the region.
func (c *Client) Owned() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.owned && c.area.cb().WriterID.Load() == c.opt.Identity.WriterID
}

// claimLocked tries to become the owner (SPEC §8.1).
func (c *Client) claimLocked(now time.Time) bool {
	if c.closed {
		return false
	}
	id := c.opt.Identity
	w := c.area.cb().WriterID.Load()
	if w == id.WriterID {
		c.owned = true
		return true
	}
	hb := c.area.cb().HeartbeatNS.Load()
	if hb != c.hbSeen || c.hbSeenAt.IsZero() {
		c.hbSeen, c.hbSeenAt = hb, now
	}
	may := w == 0 ||
		c.area.cb().WriterFlags.Load()&writerReleased != 0 ||
		c.ownerGone() ||
		(c.area.cb().LeaseNS.Load() != 0 &&
			now.Sub(c.hbSeenAt) > time.Duration(c.area.cb().LeaseNS.Load()))
	if !may || !c.area.cb().WriterID.CompareAndSwap(w, id.WriterID) {
		c.owned = false
		return false
	}
	c.area.cb().WriterPID.Store(id.PID)
	c.area.cb().WriterStart.Store(id.Start)
	c.area.cb().WriterFlags.Store(0)
	c.area.cb().LeaseNS.Store(uint64(c.opt.Lease))
	c.heartbeatLocked()
	c.owned = true
	c.adoptLocked()
	return true
}

// adoptLocked takes the region's current payload and grant table as this
// owner's publication, so later writes keep them (SPEC §9.2 A4) and drift
// checks compare against them.
func (c *Client) adoptLocked() {
	gen, payload, grants, err := c.area.readPayload()
	if err != nil || gen == 0 {
		return
	}
	c.pubValid = true
	c.pubGen = gen
	c.pubFlags = c.area.cb().Flags.Load()
	c.pubPayload = append([]byte(nil), payload...)
	c.pubGrants = grants
	c.pubLen = c.area.cb().PayloadLen.Load()
	c.pubCRC = c.area.cb().PayloadCRC32.Load()
}

// ownerGone: claim rule 3. Only meaningful when agents share a pid namespace.
func (c *Client) ownerGone() bool {
	pid := c.area.cb().WriterPID.Load()
	want := c.area.cb().WriterStart.Load()
	if pid == 0 {
		return false
	}
	got, err := startTime(c.opt.ProcRoot, int(pid))
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	return got != want
}

func (c *Client) heartbeatLocked() {
	now := uint64(bootNow())
	prev := c.area.cb().HeartbeatNS.Load()
	if now <= prev {
		now = prev + 1
	}
	c.area.cb().HeartbeatNS.Store(now)
}

// Write publishes a consumer payload and grant table as one generation.
// payload == nil publishes "no consumer configuration" (PAYLOAD_VALID clear).
// Grants with GrantID 0 are new and receive the returned generation.
func (c *Client) Write(payload []byte, grants []Grant) (uint64, []Grant, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeLocked(payload, payload != nil, grants)
}

// WritePayload publishes a new consumer payload and keeps the current grants.
func (c *Client) WritePayload(payload []byte) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	gen, _, err := c.writeLocked(payload, payload != nil, c.pubGrants)
	return gen, err
}

// WriteGrants publishes a new grant table and keeps the current payload.
// Grants with GrantID 0 are new and receive the returned generation.
func (c *Client) WriteGrants(grants []Grant) (uint64, []Grant, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeLocked(c.pubPayload, c.pubFlags&FlagPayloadValid != 0, grants)
}

// Grants returns the grant table this agent last published (or adopted),
// and that publication's generation.
func (c *Client) Grants() ([]Grant, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Grant(nil), c.pubGrants...), c.pubGen
}

func (c *Client) writeLocked(payload []byte, valid bool, grants []Grant) (uint64, []Grant, error) {
	if c.closed {
		return 0, nil, ErrClosed
	}
	if !c.owned && !c.claimLocked(time.Now()) {
		return 0, nil, ErrNotOwner
	}
	if c.area.cb().WriterID.Load() != c.opt.Identity.WriterID {
		c.owned = false
		return 0, nil, ErrPreempted
	}
	var flags uint32
	if valid {
		flags = FlagPayloadValid
	}
	gen, out, err := c.area.write(payload, grants, flags)
	if err != nil {
		if errors.Is(err, ErrPreempted) {
			c.owned = false
		}
		return 0, nil, err
	}
	c.pubValid = true
	c.pubGen = gen
	c.pubPayload = append([]byte(nil), payload...)
	c.pubGrants = out
	c.pubFlags = flags
	c.pubLen = c.area.cb().PayloadLen.Load()
	c.pubCRC = c.area.cb().PayloadCRC32.Load()
	return gen, out, nil
}

// Heartbeat claims the region if allowed, refreshes heartbeat_ns, and
// republishes if the agent payload drifted from the last publication (§7.4).
func (c *Client) Heartbeat() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	if !c.owned {
		if !c.claimLocked(time.Now()) {
			return ErrNotOwner
		}
		return nil
	}
	if c.area.cb().WriterID.Load() != c.opt.Identity.WriterID {
		c.owned = false
		return ErrPreempted
	}
	c.heartbeatLocked()
	if c.pubValid && (c.area.cb().PayloadLen.Load() != c.pubLen || c.area.cb().PayloadCRC32.Load() != c.pubCRC) {
		if gen, _, err := c.area.write(c.pubPayload, c.pubGrants, c.pubFlags); err == nil {
			c.pubGen = gen
			c.pubLen = c.area.cb().PayloadLen.Load()
			c.pubCRC = c.area.cb().PayloadCRC32.Load()
		}
	}
	return nil
}

// ReadState reads the process's state block (SPEC §7.3).
func (c *Client) ReadState(ctx context.Context) (State, error) {
	select {
	case <-c.done:
		return State{}, ErrExited
	default:
	}
	ns := c.info.NSPid
	return readState(ctx, c.info.HostPID, &c.info.Header, ns[len(ns)-1], c.opt.ReadTimeout)
}

// WaitAck waits until the process acknowledges generation gen (SPEC §8.2).
func (c *Client) WaitAck(ctx context.Context, gen uint64) error {
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		st, err := c.ReadState(ctx)
		if errors.Is(err, ErrExited) {
			return err
		}
		if err == nil && !st.Stale && st.AppliedGen >= gen &&
			st.AppliedWriterID == c.opt.Identity.WriterID {
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

// release gives up ownership: RELEASED (successor may claim at once) or, if
// clear is set, writer_id = 0 (the process reverts to defaults now).
func (c *Client) release(clear bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.owned || c.closed {
		return
	}
	if clear {
		c.area.cb().WriterID.CompareAndSwap(c.opt.Identity.WriterID, 0)
	} else if c.area.cb().WriterID.Load() == c.opt.Identity.WriterID {
		c.area.cb().WriterFlags.Store(writerReleased)
	}
	c.owned = false
}

// Close unmaps the region and releases the pidfd. It does not release
// ownership; see Registry.Close.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.markDone()
	unix.Munmap(c.area.mem)
	return unix.Close(c.pidfd)
}

func bootNow() int64 {
	var ts unix.Timespec
	_ = unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts)
	return ts.Nano()
}
