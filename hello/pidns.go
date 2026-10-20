//go:build linux

package hello

import (
	"fmt"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/btf"
	"golang.org/x/sys/unix"
)

// The program reports the firing process's tgid as the agent sees it.
// bpf_get_current_pid_tgid() returns it in the root pid namespace, which is
// only right for an agent running there (hostPID on a real node). In a nested
// setup (kind: the node is itself a container) the agent's /proc uses another
// pid namespace, so the program translates: it reads
// current->group_leader->thread_pid->numbers[L].nr, L being the level of the
// agent's pid namespace, with field offsets taken from the kernel's BTF.
// (bpf_get_ns_current_pid_tgid doesn't help: it only answers for processes
// whose own pid namespace is the given one.)

// pidnsInfo describes the agent's pid namespace.
type pidnsInfo struct {
	root bool   // the initial pid namespace
	inum uint32 // inode of /proc/self/ns/pid
}

// procPidInitIno is the fixed inode of the initial pid namespace
// (PROC_PID_INIT_INO). NSpid in /proc/self/status can't tell: it is relative
// to the procfs mount's namespace, and NS_GET_PARENT can't climb above the
// caller's own namespace.
const procPidInitIno = 0xEFFFFFFC

func selfPidns(procRoot string) (pidnsInfo, error) {
	var st unix.Stat_t
	if err := unix.Stat(procRoot+"/self/ns/pid", &st); err != nil {
		return pidnsInfo{}, err
	}
	return pidnsInfo{root: st.Ino == procPidInitIno, inum: uint32(st.Ino)}, nil
}

// taskOffsets are the kernel struct offsets the translation needs.
type taskOffsets struct {
	groupLeader, threadPid uint32 // task_struct
	pidLevel, pidNumbers   uint32 // struct pid
	upidSize, upidNr       uint32 // struct upid
	upidNs                 uint32
	nsInum                 uint32 // pid_namespace.ns.inum
}

func kernelTaskOffsets() (taskOffsets, error) {
	var o taskOffsets
	spec, err := btf.LoadKernelSpec()
	if err != nil {
		return o, fmt.Errorf("kernel BTF: %w", err)
	}
	var task, pid, upid *btf.Struct
	for name, p := range map[string]**btf.Struct{"task_struct": &task, "pid": &pid, "upid": &upid} {
		if err := spec.TypeByName(name, p); err != nil {
			return o, fmt.Errorf("BTF struct %s: %w", name, err)
		}
	}
	fields := []struct {
		s    *btf.Struct
		name string
		out  *uint32
	}{
		{task, "group_leader", &o.groupLeader}, {task, "thread_pid", &o.threadPid},
		{pid, "level", &o.pidLevel}, {pid, "numbers", &o.pidNumbers},
		{upid, "nr", &o.upidNr}, {upid, "ns", &o.upidNs},
	}
	for _, f := range fields {
		off, ok := memberOffset(f.s, f.name)
		if !ok {
			return o, fmt.Errorf("BTF: no %s.%s", f.s.Name, f.name)
		}
		*f.out = off
	}
	o.upidSize = upid.Size
	var pidns *btf.Struct
	if err := spec.TypeByName("pid_namespace", &pidns); err != nil {
		return o, fmt.Errorf("BTF struct pid_namespace: %w", err)
	}
	for _, m := range pidns.Members {
		if m.Name != "ns" {
			continue
		}
		if common, ok := btf.UnderlyingType(m.Type).(*btf.Struct); ok {
			if inumOff, ok := memberOffset(common, "inum"); ok {
				o.nsInum = m.Offset.Bytes() + inumOff
				return o, nil
			}
		}
	}
	return o, fmt.Errorf("BTF: no pid_namespace.ns.inum")
}

// memberOffset finds a member by name, descending into anonymous structs and
// unions (e.g. randomized_struct_fields_start), and returns its byte offset.
func memberOffset(t btf.Type, name string) (uint32, bool) {
	var members []btf.Member
	switch t := t.(type) {
	case *btf.Struct:
		members = t.Members
	case *btf.Union:
		members = t.Members
	default:
		return 0, false
	}
	for _, m := range members {
		if m.Name == name {
			return m.Offset.Bytes(), true
		}
		if m.Name == "" {
			if off, ok := memberOffset(btf.UnderlyingType(m.Type), name); ok {
				return m.Offset.Bytes() + off, true
			}
		}
	}
	return 0, false
}

// maxPidnsDepth bounds the unrolled walk over struct pid's numbers[]: the
// agent's pid namespace must be at most this deep (kind: 1).
const maxPidnsDepth = 8

// helloProgram puts the firing process's tgid, as seen in the agent's pid
// namespace, into the ring buffer.
func helloProgram(rb *ebpf.Map, ns pidnsInfo, o *taskOffsets) asm.Instructions {
	output := asm.Instructions{
		asm.LoadMapPtr(asm.R1, rb.FD()).WithSymbol("emit"),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, -8),
		asm.Mov.Imm(asm.R3, 4),
		asm.Mov.Imm(asm.R4, 0),
		asm.FnRingbufOutput.Call(),
		asm.Mov.Imm(asm.R0, 0).WithSymbol("out"),
		asm.Return(),
	}
	if ns.root {
		//	u32 tgid = bpf_get_current_pid_tgid() >> 32;
		return append(asm.Instructions{
			asm.FnGetCurrentPidTgid.Call(),
			asm.RSh.Imm(asm.R0, 32),
			asm.StoreMem(asm.RFP, -8, asm.R0, asm.Word),
		}, output...)
	}
	// bpf_probe_read_kernel(fp+dst, size, src+off); bail out on failure.
	read := func(dst int16, size int32, src asm.Register, off uint32) asm.Instructions {
		return asm.Instructions{
			asm.Mov.Reg(asm.R1, asm.RFP),
			asm.Add.Imm(asm.R1, int32(dst)),
			asm.Mov.Imm(asm.R2, size),
			asm.Mov.Reg(asm.R3, src),
			asm.Add.Imm(asm.R3, int32(off)),
			asm.FnProbeReadKernel.Call(),
			asm.JNE.Imm(asm.R0, 0, "out"),
		}
	}
	//	struct pid *p = current->group_leader->thread_pid;
	//	for (i = 0; i <= p->level && i < maxPidnsDepth; i++)
	//		if (p->numbers[i].ns->ns.inum == agent_inum)
	//			emit(p->numbers[i].nr);
	var p asm.Instructions
	p = append(p, asm.FnGetCurrentTask.Call(), asm.Mov.Reg(asm.R6, asm.R0))
	p = append(p, read(-16, 8, asm.R6, o.groupLeader)...)
	p = append(p, asm.LoadMem(asm.R6, asm.RFP, -16, asm.DWord))
	p = append(p, read(-16, 8, asm.R6, o.threadPid)...)
	p = append(p, asm.LoadMem(asm.R6, asm.RFP, -16, asm.DWord))
	p = append(p, read(-16, 4, asm.R6, o.pidLevel)...)
	p = append(p, asm.LoadMem(asm.R8, asm.RFP, -16, asm.Word))
	for i := uint32(0); i < maxPidnsDepth; i++ {
		upid := o.pidNumbers + i*o.upidSize
		next := fmt.Sprintf("level%d", i+1)
		if i == maxPidnsDepth-1 {
			next = "out"
		}
		blk := asm.Instructions{asm.JLT.Imm(asm.R8, int32(i), "out")} // past the process's level
		blk = append(blk, read(-16, 8, asm.R6, upid+o.upidNs)...)
		blk = append(blk, asm.LoadMem(asm.R7, asm.RFP, -16, asm.DWord))
		blk = append(blk, read(-16, 4, asm.R7, o.nsInum)...)
		blk = append(blk, asm.LoadMem(asm.R7, asm.RFP, -16, asm.Word))
		// 32-bit compare: namespace inodes have the high bit set.
		blk = append(blk, asm.JNE.Imm32(asm.R7, int32(ns.inum), next))
		blk = append(blk, read(-8, 4, asm.R6, upid+o.upidNr)...)
		blk = append(blk, asm.Ja.Label("emit"))
		if i > 0 {
			blk[0] = blk[0].WithSymbol(fmt.Sprintf("level%d", i))
		}
		p = append(p, blk...)
	}
	return append(p, output...)
}
