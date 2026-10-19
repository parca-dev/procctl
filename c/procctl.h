/*
 * procctl.h — process side of the procctl control channel (SPEC.md 1.0-draft r2).
 *
 * Single-header library. In exactly one translation unit:
 *
 *     #define PROCCTL_IMPLEMENTATION
 *     #include "procctl.h"
 *
 * The implementation uses Linux/POSIX interfaces: compile that translation
 * unit as C++, with -std=gnu11 (or later), or with _GNU_SOURCE defined before
 * any #include.
 *
 * Guarantees: no allocation, no threads, no constructors, no exit hooks, no
 * fork hooks. The process never writes shared memory: the region is mapped
 * PROT_READ, and the process reports through a state block in caller-provided
 * private memory that agents read with process_vm_readv.
 *
 * SPDX-License-Identifier: Apache-2.0
 * Copyright The procctl Authors.
 * Bundles <sys/sdt.h> from SystemTap, dedicated to the public domain (CC0).
 */
#ifndef PROCCTL_H
#define PROCCTL_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#define PROCCTL_SPEC_MAJOR 1
#define PROCCTL_SPEC_MINOR 0
#define PROCCTL_VERSION "0.1.0-dev"

#ifndef PROCCTL_DEF
#define PROCCTL_DEF __attribute__((visibility("hidden")))
#endif

#ifdef __cplusplus
extern "C" {
#endif

/* Layout sizes (SPEC §4–§6). */
#define PROCCTL_HEADER_SIZE 128u
#define PROCCTL_CTRL_SIZE 64u
#define PROCCTL_ENTRY_SIZE 48u
#define PROCCTL_STATE_HDR_SIZE 64u
#define PROCCTL_MAX_SLOTS 64u
#define PROCCTL_MAX_STATE_SIZE 65536u

/* Resource states (SPEC §6.1). */
#define PROCCTL_RES_IDLE 0u
#define PROCCTL_RES_ACQUIRING 1u
#define PROCCTL_RES_HOLDING 2u
#define PROCCTL_RES_RELEASED 3u
#define PROCCTL_RES_FAILED 4u
#define PROCCTL_RES_F_WANT 1u

/* procctl_config.flags */
#define PROCCTL_F_NO_HELLO 1u
#define PROCCTL_F_STATE_PROBE 2u

/* Lock flags for state writes. */
#define PROCCTL_TRY 1 /* never wait for the state lock; return -EBUSY */

/* Bytes of state_storage needed: the published state block plus a private
 * tail used to time grants (16 bytes per slot). */
#define PROCCTL_STATE_BLOCK_SIZE(slots, cap)                                   \
  ((((size_t)PROCCTL_STATE_HDR_SIZE + (size_t)(slots)*PROCCTL_ENTRY_SIZE +     \
     (size_t)(cap)) + 7u) & ~(size_t)7u)
#define PROCCTL_STATE_STORAGE(slots, cap)                                      \
  (PROCCTL_STATE_BLOCK_SIZE(slots, cap) + (size_t)(slots)*16u)

#ifdef __cplusplus
#define PROCCTL__ALIGNAS(n) alignas(n)
#define PROCCTL__STATIC_ASSERT(c, m) static_assert(c, m)
#else
#define PROCCTL__ALIGNAS(n) _Alignas(n)
#define PROCCTL__STATIC_ASSERT(c, m) _Static_assert(c, m)
#endif

typedef struct procctl {
  PROCCTL__ALIGNAS(64) unsigned char opaque[512];
} procctl_t;

typedef struct procctl_config {
  const char *consumer;      /* [a-z0-9_]{1,31} */
  uint32_t schema_version;
  uint32_t payload_capacity; /* consumer payload bytes, agent → process */
  uint16_t resource_slots;   /* 0..64 */
  uint32_t state_capacity;   /* consumer state bytes, process → agent */
  void *state_storage;       /* PROCCTL_STATE_STORAGE(...) bytes, 8-aligned, static lifetime */
  uint32_t flags;            /* PROCCTL_F_* */
} procctl_config_t;

typedef struct procctl_grant {
  uint64_t grant_id;    /* 0 = no grant */
  uint64_t duration_ns;
  uint64_t seen_gen;    /* generation of the grant table this came from */
  uint32_t kind;
} procctl_grant_t;

/* Creates the region and state block. 0 or -errno. */
PROCCTL_DEF int procctl_init(procctl_t *ctx, const procctl_config_t *cfg);

/* Hot path. 0 = no change; >0 = consumer payload bytes copied (*gen set);
 * -ENODATA a new generation with no consumer payload: use defaults (*gen set);
 * -EAGAIN busy/torn (try later); -E2BIG buf too small (generation consumed);
 * -ENODEV not initialized. Exactly one concurrent caller receives each
 * generation. */
static inline int procctl_poll(procctl_t *ctx, void *buf, size_t cap, uint64_t *gen);

PROCCTL_DEF int procctl_ack(procctl_t *ctx, uint64_t gen, int lflags);
PROCCTL_DEF bool procctl_lease_expired(procctl_t *ctx, uint64_t now_ns);
PROCCTL_DEF bool procctl_agent_attached(const procctl_t *ctx);
PROCCTL_DEF int procctl_state_set(procctl_t *ctx, const void *state, size_t len, int lflags);

PROCCTL_DEF int procctl_grant_get(procctl_t *ctx, uint32_t kind, const uint8_t id[16],
                                  procctl_grant_t *out);
PROCCTL_DEF bool procctl_grant_usable(procctl_t *ctx, const procctl_grant_t *g, uint64_t now_ns);
PROCCTL_DEF int procctl_resource_report(procctl_t *ctx, uint32_t kind, const uint8_t id[16],
                                        uint32_t state, uint64_t grant_id, uint32_t flags,
                                        uint32_t error, uint64_t grants_seen_gen, int lflags);

/* Blocks (futex) until the agent writes or timeout_ns passes. 0 or -ETIMEDOUT.
 * For a consumer-owned thread; never call on a hot path. */
PROCCTL_DEF int procctl_wait(procctl_t *ctx, uint64_t timeout_ns);

PROCCTL_DEF int procctl_reinit(procctl_t *ctx);
PROCCTL_DEF void procctl_close(procctl_t *ctx);

/* CLOCK_BOOTTIME in nanoseconds. */
PROCCTL_DEF uint64_t procctl_now_ns(void);

/* ------------------------------------------------------------------------- */
/* Internal context layout (shared with the inline fast path).                */

struct procctl__ctx {
  const uint64_t *seq;       /* agent area seq, in the read-only mapping */
  uint64_t last_seen;        /* atomic */
  const uint8_t *map;
  size_t map_size;
  const uint8_t *area;       /* agent area */
  size_t area_size;
  uint8_t *state;            /* state block (caller storage) */
  uint8_t *grant_track;      /* private tail: {grant_id, first_seen_ns} per slot */
  uint64_t instance_id;
  uint64_t hb_seen;          /* atomic: last heartbeat value seen */
  uint64_t hb_local_ns;      /* atomic: local time it was first seen */
  uint32_t state_block_size;
  uint32_t state_capacity;
  uint32_t payload_capacity;
  uint32_t schema_version;
  uint32_t flags;
  int32_t fd;
  int32_t pid;               /* pid that initialized the state block */
  int32_t lease_expired;     /* atomic */
  int32_t state_lock;        /* atomic */
  uint16_t slots;
  uint8_t ready;
  char consumer[32];
  char memfd_name[48];
};

PROCCTL_DEF int procctl__poll_slow(struct procctl__ctx *c, uint64_t s1, void *buf, size_t cap,
                                   uint64_t *gen);

static inline int procctl_poll(procctl_t *ctx, void *buf, size_t cap, uint64_t *gen) {
  struct procctl__ctx *c = (struct procctl__ctx *)(void *)ctx->opaque;
  if (__builtin_expect(!c->ready, 0)) return -19; /* -ENODEV */
  uint64_t s1 = __atomic_load_n(c->seq, __ATOMIC_ACQUIRE);
  if (__builtin_expect(s1 == __atomic_load_n(&c->last_seen, __ATOMIC_RELAXED), 1)) return 0;
  return procctl__poll_slow(c, s1, buf, cap, gen);
}

#ifdef __cplusplus
}
#endif

#endif /* PROCCTL_H */

/* ========================================================================= */
#ifdef PROCCTL_IMPLEMENTATION
#ifndef PROCCTL_IMPLEMENTATION_DONE
#define PROCCTL_IMPLEMENTATION_DONE

#if !defined(__cplusplus) && defined(__STRICT_ANSI__) && !defined(_GNU_SOURCE) &&             \
    !defined(_DEFAULT_SOURCE)
#error "procctl implementation needs _GNU_SOURCE (or -std=gnu11): define it before any #include"
#endif

#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <linux/futex.h>
#include <sched.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>

/* ---- USDT probes ---------------------------------------------------------- */
#if !defined(PROCCTL_NO_PROBES)
#if defined(__has_include)
#if __has_include(<sys/sdt.h>) && !defined(PROCCTL_BUNDLED_SDT)
#include <sys/sdt.h>
#define PROCCTL__HAVE_SDT 1
#endif
#endif
#ifndef PROCCTL__HAVE_SDT
#define PROCCTL__HAVE_SDT 1
/* ---- begin bundled SystemTap <sys/sdt.h> (CC0) ----
 * procctl modifications: sdt-config.h inlined; the two _SDT_ASM_BODY calls in
 * _SDT_PROBE pass an explicit empty variadic argument (clean under -pedantic). */
/* <sys/sdt.h> - Systemtap static probe definition macros.

   This file is dedicated to the public domain, pursuant to CC0
   (https://creativecommons.org/publicdomain/zero/1.0/)
*/

#ifndef _SYS_SDT_H
#define _SYS_SDT_H    1

/*
  This file defines a family of macros

       STAP_PROBEn(op1, ..., opn)

  that emit a nop into the instruction stream, and some data into an auxiliary
  note section.  The data in the note section describes the operands, in terms
  of size and location.  Each location is encoded as assembler operand string.
  Consumer tools such as gdb or systemtap insert breakpoints on top of
  the nop, and decode the location operand-strings, like an assembler,
  to find the values being passed.

  The operand strings are selected by the compiler for each operand.
  They are constrained by gcc inline-assembler codes.  The default is:

  #define STAP_SDT_ARG_CONSTRAINT nor

  This is a good default if the operands tend to be integral and
  moderate in number (smaller than number of registers).  In other
  cases, the compiler may report "'asm' requires impossible reload" or
  similar.  In this case, consider simplifying the macro call (fewer
  and simpler operands), reduce optimization, or override the default
  constraints string via:

  #define STAP_SDT_ARG_CONSTRAINT g
  #include <sys/sdt.h>

  See also:
  https://sourceware.org/systemtap/wiki/UserSpaceProbeImplementation
  https://gcc.gnu.org/onlinedocs/gcc/Constraints.html
 */



#ifdef __ASSEMBLER__
# define _SDT_PROBE(provider, name, n, arglist)	\
  _SDT_ASM_BODY(provider, name, _SDT_ASM_SUBSTR_1, (_SDT_DEPAREN_##n arglist), ) \
  _SDT_ASM_BASE
# define _SDT_ASM_1(x)			x;
# define _SDT_ASM_2(a, b)		a,b;
# define _SDT_ASM_3(a, b, c)		a,b,c;
# define _SDT_ASM_5(a, b, c, d, e)	a,b,c,d,e;
# define _SDT_ASM_STRING_1(x)		.asciz #x;
# define _SDT_ASM_SUBSTR_1(x)		.ascii #x;
# define _SDT_DEPAREN_0()				/* empty */
# define _SDT_DEPAREN_1(a)				a
# define _SDT_DEPAREN_2(a,b)				a b
# define _SDT_DEPAREN_3(a,b,c)				a b c
# define _SDT_DEPAREN_4(a,b,c,d)			a b c d
# define _SDT_DEPAREN_5(a,b,c,d,e)			a b c d e
# define _SDT_DEPAREN_6(a,b,c,d,e,f)			a b c d e f
# define _SDT_DEPAREN_7(a,b,c,d,e,f,g)			a b c d e f g
# define _SDT_DEPAREN_8(a,b,c,d,e,f,g,h)		a b c d e f g h
# define _SDT_DEPAREN_9(a,b,c,d,e,f,g,h,i)		a b c d e f g h i
# define _SDT_DEPAREN_10(a,b,c,d,e,f,g,h,i,j)		a b c d e f g h i j
# define _SDT_DEPAREN_11(a,b,c,d,e,f,g,h,i,j,k)		a b c d e f g h i j k
# define _SDT_DEPAREN_12(a,b,c,d,e,f,g,h,i,j,k,l)	a b c d e f g h i j k l
#else
#if defined _SDT_HAS_SEMAPHORES
#define _SDT_NOTE_SEMAPHORE_USE(provider, name) \
  __asm__ __volatile__ ("" :: "m" (provider##_##name##_semaphore));
#else
#define _SDT_NOTE_SEMAPHORE_USE(provider, name)
#endif

# define _SDT_PROBE(provider, name, n, arglist) \
  do {									    \
    _SDT_NOTE_SEMAPHORE_USE(provider, name); \
    __asm__ __volatile__ (_SDT_ASM_BODY(provider, name, _SDT_ASM_ARGS, (n), ) \
			  :: _SDT_ASM_OPERANDS_##n arglist);		    \
    __asm__ __volatile__ (_SDT_ASM_BASE);				    \
  } while (0)
# define _SDT_S(x)			#x
# define _SDT_ASM_1(x)			_SDT_S(x) "\n"
# define _SDT_ASM_2(a, b)		_SDT_S(a) "," _SDT_S(b) "\n"
# define _SDT_ASM_3(a, b, c)		_SDT_S(a) "," _SDT_S(b) "," \
					_SDT_S(c) "\n"
# define _SDT_ASM_5(a, b, c, d, e)	_SDT_S(a) "," _SDT_S(b) "," \
					_SDT_S(c) "," _SDT_S(d) "," \
					_SDT_S(e) "\n"
# define _SDT_ASM_ARGS(n)		_SDT_ASM_TEMPLATE_##n
# define _SDT_ASM_STRING_1(x)		_SDT_ASM_1(.asciz #x)
# define _SDT_ASM_SUBSTR_1(x)		_SDT_ASM_1(.ascii #x)

# define _SDT_ARGFMT(no)                _SDT_ASM_1(_SDT_SIGN %n[_SDT_S##no]) \
                                        _SDT_ASM_1(_SDT_SIZE %n[_SDT_S##no]) \
                                        _SDT_ASM_1(_SDT_TYPE %n[_SDT_S##no]) \
                                        _SDT_ASM_SUBSTR(_SDT_ARGTMPL(_SDT_A##no))


# ifndef STAP_SDT_ARG_CONSTRAINT
# if defined __powerpc__
# define STAP_SDT_ARG_CONSTRAINT        nZr
# elif defined __arm__
# define STAP_SDT_ARG_CONSTRAINT        g
# elif defined __loongarch__
# define STAP_SDT_ARG_CONSTRAINT        nmr
# else
# define STAP_SDT_ARG_CONSTRAINT        nor
# endif
# endif

# define _SDT_STRINGIFY(x)              #x
# define _SDT_ARG_CONSTRAINT_STRING(x)  _SDT_STRINGIFY(x)
/* _SDT_S encodes the size and type as 0xSSTT which is decoded by the assembler
   macros _SDT_SIZE and _SDT_TYPE */
# define _SDT_ARG(n, x)				    \
  [_SDT_S##n] "n" ((_SDT_ARGSIGNED (x) ? (int)-1 : 1) * (-(((int) _SDT_ARGSIZE (x)) << 8) + (-(0x7f & __builtin_classify_type (x))))), \
  [_SDT_A##n] _SDT_ARG_CONSTRAINT_STRING (STAP_SDT_ARG_CONSTRAINT) (_SDT_ARGVAL (x))
#endif
#define _SDT_ASM_STRING(x)		_SDT_ASM_STRING_1(x)
#define _SDT_ASM_SUBSTR(x)		_SDT_ASM_SUBSTR_1(x)

#define _SDT_ARGARRAY(x)	(__builtin_classify_type (x) == 14	\
				 || __builtin_classify_type (x) == 5)

#ifdef __cplusplus
# define _SDT_ARGSIGNED(x)	(!_SDT_ARGARRAY (x) \
				 && __sdt_type<__typeof (x)>::__sdt_signed)
# define _SDT_ARGSIZE(x)	(_SDT_ARGARRAY (x) \
				 ? sizeof (void *) : sizeof (x))
# define _SDT_ARGVAL(x)		(x)

# include <cstddef>

template<typename __sdt_T>
struct __sdt_type
{
  static const bool __sdt_signed = false;
};
  
#define __SDT_ALWAYS_SIGNED(T) \
template<> struct __sdt_type<T> { static const bool __sdt_signed = true; };
#define __SDT_COND_SIGNED(T,CT)						\
template<> struct __sdt_type<T> { static const bool __sdt_signed = ((CT)(-1) < 1); };
__SDT_ALWAYS_SIGNED(signed char)
__SDT_ALWAYS_SIGNED(short)
__SDT_ALWAYS_SIGNED(int)
__SDT_ALWAYS_SIGNED(long)
__SDT_ALWAYS_SIGNED(long long)
__SDT_ALWAYS_SIGNED(volatile signed char)
__SDT_ALWAYS_SIGNED(volatile short)
__SDT_ALWAYS_SIGNED(volatile int)
__SDT_ALWAYS_SIGNED(volatile long)
__SDT_ALWAYS_SIGNED(volatile long long)
__SDT_ALWAYS_SIGNED(const signed char)
__SDT_ALWAYS_SIGNED(const short)
__SDT_ALWAYS_SIGNED(const int)
__SDT_ALWAYS_SIGNED(const long)
__SDT_ALWAYS_SIGNED(const long long)
__SDT_ALWAYS_SIGNED(const volatile signed char)
__SDT_ALWAYS_SIGNED(const volatile short)
__SDT_ALWAYS_SIGNED(const volatile int)
__SDT_ALWAYS_SIGNED(const volatile long)
__SDT_ALWAYS_SIGNED(const volatile long long)
__SDT_COND_SIGNED(char, char)
__SDT_COND_SIGNED(wchar_t, wchar_t)
__SDT_COND_SIGNED(volatile char, char)
__SDT_COND_SIGNED(volatile wchar_t, wchar_t)
__SDT_COND_SIGNED(const char, char)
__SDT_COND_SIGNED(const wchar_t, wchar_t)
__SDT_COND_SIGNED(const volatile char, char)
__SDT_COND_SIGNED(const volatile wchar_t, wchar_t)
#if defined (__GNUC__) && (__GNUC__ > 4 || (__GNUC__ == 4 && __GNUC_MINOR__ >= 4))
/* __SDT_COND_SIGNED(char16_t) */
/* __SDT_COND_SIGNED(char32_t) */
#endif

template<typename __sdt_E>
struct __sdt_type<__sdt_E[]> : public __sdt_type<__sdt_E *> {};

template<typename __sdt_E, size_t __sdt_N>
struct __sdt_type<__sdt_E[__sdt_N]> : public __sdt_type<__sdt_E *> {};

#elif !defined(__ASSEMBLER__)
__extension__ extern unsigned long long __sdt_unsp;
# define _SDT_ARGINTTYPE(x)						\
  __typeof (__builtin_choose_expr (((__builtin_classify_type (x)	\
				     + 3) & -4) == 4, (x), 0U))
# define _SDT_ARGSIGNED(x)						\
  (!__extension__							\
   (__builtin_constant_p ((((unsigned long long)			\
			    (_SDT_ARGINTTYPE (x)) __sdt_unsp)		\
			   & ((unsigned long long)1 << (sizeof (unsigned long long)	\
				       * __CHAR_BIT__ - 1))) == 0)	\
    || (_SDT_ARGINTTYPE (x)) -1 > (_SDT_ARGINTTYPE (x)) 0))
# define _SDT_ARGSIZE(x)	\
  (_SDT_ARGARRAY (x) ? sizeof (void *) : sizeof (x))
# define _SDT_ARGVAL(x)		(x)
#endif

#if defined __powerpc__ || defined __powerpc64__
# define _SDT_ARGTMPL(id)	%I[id]%[id]
#elif defined __i386__
# define _SDT_ARGTMPL(id)	%k[id]  /* gcc.gnu.org/PR80115 sourceware.org/PR24541 */
#else
# define _SDT_ARGTMPL(id)	%[id]
#endif

/* NB: gdb PR24541 highlighted an unspecified corner of the sdt.h
   operand note format.

   The named register may be a longer or shorter (!) alias for the
   storage where the value in question is found.  For example, on
   i386, 64-bit value may be put in register pairs, and the register
   name stored would identify just one of them.  Previously, gcc was
   asked to emit the %w[id] (16-bit alias of some registers holding
   operands), even when a wider 32-bit value was used.

   Bottom line: the byte-width given before the @ sign governs.  If
   there is a mismatch between that width and that of the named
   register, then a sys/sdt.h note consumer may need to employ
   architecture-specific heuristics to figure out where the compiler
   has actually put the complete value.
*/

#ifdef __LP64__
# define _SDT_ASM_ADDR	.8byte
#else
# define _SDT_ASM_ADDR	.4byte
#endif

/* The ia64 and s390 nop instructions take an argument. */
#if defined(__ia64__) || defined(__s390__) || defined(__s390x__)
#define _SDT_NOP	nop 0
#else
#define _SDT_NOP	nop
#endif

#define _SDT_NOTE_NAME	"stapsdt"
#define _SDT_NOTE_TYPE	3

/* If the assembler supports the necessary feature, then we can play
   nice with code in COMDAT sections, which comes up in C++ code.
   Without that assembler support, some combinations of probe placements
   in certain kinds of C++ code may produce link-time errors.  */
/* procctl: sdt-config.h inlined; GNU as and LLVM support section autogroup. */
#ifndef _SDT_ASM_SECTION_AUTOGROUP_SUPPORT
#define _SDT_ASM_SECTION_AUTOGROUP_SUPPORT 1
#endif
#if _SDT_ASM_SECTION_AUTOGROUP_SUPPORT
# define _SDT_ASM_AUTOGROUP "?"
#else
# define _SDT_ASM_AUTOGROUP ""
#endif

#define _SDT_DEF_MACROS							     \
	_SDT_ASM_1(.altmacro)						     \
	_SDT_ASM_1(.macro _SDT_SIGN x)				     	     \
	_SDT_ASM_3(.pushsection .note.stapsdt,"","note")		     \
	_SDT_ASM_1(.iflt \\x)						     \
	_SDT_ASM_1(.ascii "-")						     \
	_SDT_ASM_1(.endif)						     \
	_SDT_ASM_1(.popsection)						     \
	_SDT_ASM_1(.endm)						     \
	_SDT_ASM_1(.macro _SDT_SIZE_ x)					     \
	_SDT_ASM_3(.pushsection .note.stapsdt,"","note")		     \
	_SDT_ASM_1(.ascii "\x")						     \
	_SDT_ASM_1(.popsection)						     \
	_SDT_ASM_1(.endm)						     \
	_SDT_ASM_1(.macro _SDT_SIZE x)					     \
	_SDT_ASM_1(_SDT_SIZE_ %%((-(-\\x*((-\\x>0)-(-\\x<0))))>>8))	     \
	_SDT_ASM_1(.endm)						     \
	_SDT_ASM_1(.macro _SDT_TYPE_ x)				             \
	_SDT_ASM_3(.pushsection .note.stapsdt,"","note")		     \
	_SDT_ASM_2(.ifc 8,\\x)					     	     \
	_SDT_ASM_1(.ascii "f")						     \
	_SDT_ASM_1(.endif)						     \
	_SDT_ASM_1(.ascii "@")						     \
	_SDT_ASM_1(.popsection)						     \
	_SDT_ASM_1(.endm)						     \
	_SDT_ASM_1(.macro _SDT_TYPE x)				     	     \
	_SDT_ASM_1(_SDT_TYPE_ %%((\\x)&(0xff)))			     \
	_SDT_ASM_1(.endm)

#define _SDT_UNDEF_MACROS						      \
  _SDT_ASM_1(.purgem _SDT_SIGN)						      \
  _SDT_ASM_1(.purgem _SDT_SIZE_)					      \
  _SDT_ASM_1(.purgem _SDT_SIZE)						      \
  _SDT_ASM_1(.purgem _SDT_TYPE_)					      \
  _SDT_ASM_1(.purgem _SDT_TYPE)

#define _SDT_ASM_BODY(provider, name, pack_args, args, ...)		      \
  _SDT_DEF_MACROS							      \
  _SDT_ASM_1(990:	_SDT_NOP)					      \
  _SDT_ASM_3(		.pushsection .note.stapsdt,_SDT_ASM_AUTOGROUP,"note") \
  _SDT_ASM_1(		.balign 4)					      \
  _SDT_ASM_3(		.4byte 992f-991f, 994f-993f, _SDT_NOTE_TYPE)	      \
  _SDT_ASM_1(991:	.asciz _SDT_NOTE_NAME)				      \
  _SDT_ASM_1(992:	.balign 4)					      \
  _SDT_ASM_1(993:	_SDT_ASM_ADDR 990b)				      \
  _SDT_ASM_1(		_SDT_ASM_ADDR _.stapsdt.base)			      \
  _SDT_SEMAPHORE(provider,name)						      \
  _SDT_ASM_STRING(provider)						      \
  _SDT_ASM_STRING(name)							      \
  pack_args args							      \
  _SDT_ASM_SUBSTR(\x00)							      \
  _SDT_UNDEF_MACROS							      \
  _SDT_ASM_1(994:	.balign 4)					      \
  _SDT_ASM_1(		.popsection)

#define _SDT_ASM_BASE							      \
  _SDT_ASM_1(.ifndef _.stapsdt.base)					      \
  _SDT_ASM_5(		.pushsection .stapsdt.base,"aG","progbits",	      \
							.stapsdt.base,comdat) \
  _SDT_ASM_1(		.weak _.stapsdt.base)				      \
  _SDT_ASM_1(		.hidden _.stapsdt.base)				      \
  _SDT_ASM_1(	_.stapsdt.base: .space 1)				      \
  _SDT_ASM_2(		.size _.stapsdt.base, 1)			      \
  _SDT_ASM_1(		.popsection)					      \
  _SDT_ASM_1(.endif)

#if defined _SDT_HAS_SEMAPHORES
#define _SDT_SEMAPHORE(p,n) \
	_SDT_ASM_1(		_SDT_ASM_ADDR p##_##n##_semaphore)
#else
#define _SDT_SEMAPHORE(p,n) _SDT_ASM_1(		_SDT_ASM_ADDR 0)
#endif

#define _SDT_ASM_BLANK _SDT_ASM_SUBSTR(\x20)
#define _SDT_ASM_TEMPLATE_0		/* no arguments */
#define _SDT_ASM_TEMPLATE_1		_SDT_ARGFMT(1)
#define _SDT_ASM_TEMPLATE_2		_SDT_ASM_TEMPLATE_1 _SDT_ASM_BLANK _SDT_ARGFMT(2)
#define _SDT_ASM_TEMPLATE_3		_SDT_ASM_TEMPLATE_2 _SDT_ASM_BLANK _SDT_ARGFMT(3)
#define _SDT_ASM_TEMPLATE_4		_SDT_ASM_TEMPLATE_3 _SDT_ASM_BLANK _SDT_ARGFMT(4)
#define _SDT_ASM_TEMPLATE_5		_SDT_ASM_TEMPLATE_4 _SDT_ASM_BLANK _SDT_ARGFMT(5)
#define _SDT_ASM_TEMPLATE_6		_SDT_ASM_TEMPLATE_5 _SDT_ASM_BLANK _SDT_ARGFMT(6)
#define _SDT_ASM_TEMPLATE_7		_SDT_ASM_TEMPLATE_6 _SDT_ASM_BLANK _SDT_ARGFMT(7)
#define _SDT_ASM_TEMPLATE_8		_SDT_ASM_TEMPLATE_7 _SDT_ASM_BLANK _SDT_ARGFMT(8)
#define _SDT_ASM_TEMPLATE_9		_SDT_ASM_TEMPLATE_8 _SDT_ASM_BLANK _SDT_ARGFMT(9)
#define _SDT_ASM_TEMPLATE_10		_SDT_ASM_TEMPLATE_9 _SDT_ASM_BLANK _SDT_ARGFMT(10)
#define _SDT_ASM_TEMPLATE_11		_SDT_ASM_TEMPLATE_10 _SDT_ASM_BLANK _SDT_ARGFMT(11)
#define _SDT_ASM_TEMPLATE_12		_SDT_ASM_TEMPLATE_11 _SDT_ASM_BLANK _SDT_ARGFMT(12)
#define _SDT_ASM_OPERANDS_0()		[__sdt_dummy] "g" (0)
#define _SDT_ASM_OPERANDS_1(arg1)	_SDT_ARG(1, arg1)
#define _SDT_ASM_OPERANDS_2(arg1, arg2) \
  _SDT_ASM_OPERANDS_1(arg1), _SDT_ARG(2, arg2)
#define _SDT_ASM_OPERANDS_3(arg1, arg2, arg3) \
  _SDT_ASM_OPERANDS_2(arg1, arg2), _SDT_ARG(3, arg3)
#define _SDT_ASM_OPERANDS_4(arg1, arg2, arg3, arg4) \
  _SDT_ASM_OPERANDS_3(arg1, arg2, arg3), _SDT_ARG(4, arg4)
#define _SDT_ASM_OPERANDS_5(arg1, arg2, arg3, arg4, arg5) \
  _SDT_ASM_OPERANDS_4(arg1, arg2, arg3, arg4), _SDT_ARG(5, arg5)
#define _SDT_ASM_OPERANDS_6(arg1, arg2, arg3, arg4, arg5, arg6) \
  _SDT_ASM_OPERANDS_5(arg1, arg2, arg3, arg4, arg5), _SDT_ARG(6, arg6)
#define _SDT_ASM_OPERANDS_7(arg1, arg2, arg3, arg4, arg5, arg6, arg7) \
  _SDT_ASM_OPERANDS_6(arg1, arg2, arg3, arg4, arg5, arg6), _SDT_ARG(7, arg7)
#define _SDT_ASM_OPERANDS_8(arg1, arg2, arg3, arg4, arg5, arg6, arg7, arg8) \
  _SDT_ASM_OPERANDS_7(arg1, arg2, arg3, arg4, arg5, arg6, arg7), \
    _SDT_ARG(8, arg8)
#define _SDT_ASM_OPERANDS_9(arg1,arg2,arg3,arg4,arg5,arg6,arg7,arg8,arg9) \
  _SDT_ASM_OPERANDS_8(arg1, arg2, arg3, arg4, arg5, arg6, arg7, arg8), \
    _SDT_ARG(9, arg9)
#define _SDT_ASM_OPERANDS_10(arg1,arg2,arg3,arg4,arg5,arg6,arg7,arg8,arg9,arg10) \
  _SDT_ASM_OPERANDS_9(arg1, arg2, arg3, arg4, arg5, arg6, arg7, arg8, arg9), \
    _SDT_ARG(10, arg10)
#define _SDT_ASM_OPERANDS_11(arg1,arg2,arg3,arg4,arg5,arg6,arg7,arg8,arg9,arg10,arg11) \
  _SDT_ASM_OPERANDS_10(arg1, arg2, arg3, arg4, arg5, arg6, arg7, arg8, arg9, arg10), \
    _SDT_ARG(11, arg11)
#define _SDT_ASM_OPERANDS_12(arg1,arg2,arg3,arg4,arg5,arg6,arg7,arg8,arg9,arg10,arg11,arg12) \
  _SDT_ASM_OPERANDS_11(arg1, arg2, arg3, arg4, arg5, arg6, arg7, arg8, arg9, arg10, arg11), \
    _SDT_ARG(12, arg12)

/* These macros can be used in C, C++, or assembly code.
   In assembly code the arguments should use normal assembly operand syntax.  */

#define STAP_PROBE(provider, name) \
  _SDT_PROBE(provider, name, 0, ())
#define STAP_PROBE1(provider, name, arg1) \
  _SDT_PROBE(provider, name, 1, (arg1))
#define STAP_PROBE2(provider, name, arg1, arg2) \
  _SDT_PROBE(provider, name, 2, (arg1, arg2))
#define STAP_PROBE3(provider, name, arg1, arg2, arg3) \
  _SDT_PROBE(provider, name, 3, (arg1, arg2, arg3))
#define STAP_PROBE4(provider, name, arg1, arg2, arg3, arg4) \
  _SDT_PROBE(provider, name, 4, (arg1, arg2, arg3, arg4))
#define STAP_PROBE5(provider, name, arg1, arg2, arg3, arg4, arg5) \
  _SDT_PROBE(provider, name, 5, (arg1, arg2, arg3, arg4, arg5))
#define STAP_PROBE6(provider, name, arg1, arg2, arg3, arg4, arg5, arg6)	\
  _SDT_PROBE(provider, name, 6, (arg1, arg2, arg3, arg4, arg5, arg6))
#define STAP_PROBE7(provider, name, arg1, arg2, arg3, arg4, arg5, arg6, arg7) \
  _SDT_PROBE(provider, name, 7, (arg1, arg2, arg3, arg4, arg5, arg6, arg7))
#define STAP_PROBE8(provider,name,arg1,arg2,arg3,arg4,arg5,arg6,arg7,arg8) \
  _SDT_PROBE(provider, name, 8, (arg1,arg2,arg3,arg4,arg5,arg6,arg7,arg8))
#define STAP_PROBE9(provider,name,arg1,arg2,arg3,arg4,arg5,arg6,arg7,arg8,arg9)\
  _SDT_PROBE(provider, name, 9, (arg1,arg2,arg3,arg4,arg5,arg6,arg7,arg8,arg9))
#define STAP_PROBE10(provider,name,arg1,arg2,arg3,arg4,arg5,arg6,arg7,arg8,arg9,arg10) \
  _SDT_PROBE(provider, name, 10, \
	     (arg1,arg2,arg3,arg4,arg5,arg6,arg7,arg8,arg9,arg10))
#define STAP_PROBE11(provider,name,arg1,arg2,arg3,arg4,arg5,arg6,arg7,arg8,arg9,arg10,arg11) \
  _SDT_PROBE(provider, name, 11, \
	     (arg1,arg2,arg3,arg4,arg5,arg6,arg7,arg8,arg9,arg10,arg11))
#define STAP_PROBE12(provider,name,arg1,arg2,arg3,arg4,arg5,arg6,arg7,arg8,arg9,arg10,arg11,arg12) \
  _SDT_PROBE(provider, name, 12, \
	     (arg1,arg2,arg3,arg4,arg5,arg6,arg7,arg8,arg9,arg10,arg11,arg12))

/* This STAP_PROBEV macro can be used in variadic scenarios, where the
   number of probe arguments is not known until compile time.  Since
   variadic macro support may vary with compiler options, you must
   pre-#define SDT_USE_VARIADIC to enable this type of probe.

   The trick to count __VA_ARGS__ was inspired by this post by
   Laurent Deniau <laurent.deniau@cern.ch>:
       http://groups.google.com/group/comp.std.c/msg/346fc464319b1ee5

   Note that our _SDT_NARG is called with an extra 0 arg that's not
   counted, so we don't have to worry about the behavior of macros
   called without any arguments.  */

#define _SDT_NARG(...) __SDT_NARG(__VA_ARGS__, 12,11,10,9,8,7,6,5,4,3,2,1,0)
#define __SDT_NARG(_0,_1,_2,_3,_4,_5,_6,_7,_8,_9,_10,_11,_12, N, ...) N
#ifdef SDT_USE_VARIADIC
#define _SDT_PROBE_N(provider, name, N, ...) \
  _SDT_PROBE(provider, name, N, (__VA_ARGS__))
#define STAP_PROBEV(provider, name, ...) \
  _SDT_PROBE_N(provider, name, _SDT_NARG(0, ##__VA_ARGS__), ##__VA_ARGS__)
#endif

/* These macros are for use in asm statements.  You must compile
   with -std=gnu99 or -std=c99 to use the STAP_PROBE_ASM macro.

   The STAP_PROBE_ASM macro generates a quoted string to be used in the
   template portion of the asm statement, concatenated with strings that
   contain the actual assembly code around the probe site.

   For example:

	asm ("before\n"
	     STAP_PROBE_ASM(provider, fooprobe, %eax 4(%esi))
	     "after");

   emits the assembly code for "before\nafter", with a probe in between.
   The probe arguments are the %eax register, and the value of the memory
   word located 4 bytes past the address in the %esi register.  Note that
   because this is a simple asm, not a GNU C extended asm statement, these
   % characters do not need to be doubled to generate literal %reg names.

   In a GNU C extended asm statement, the probe arguments can be specified
   using the macro STAP_PROBE_ASM_TEMPLATE(n) for n arguments.  The paired
   macro STAP_PROBE_ASM_OPERANDS gives the C values of these probe arguments,
   and appears in the input operand list of the asm statement.  For example:

	asm ("someinsn %0,%1\n" // %0 is output operand, %1 is input operand
	     STAP_PROBE_ASM(provider, fooprobe, STAP_PROBE_ASM_TEMPLATE(3))
	     "otherinsn %[namedarg]"
	     : "r" (outvar)
	     : "g" (some_value), [namedarg] "i" (1234),
	       STAP_PROBE_ASM_OPERANDS(3, some_value, some_ptr->field, 1234));

    This is just like writing:

	STAP_PROBE3(provider, fooprobe, some_value, some_ptr->field, 1234));

    but the probe site is right between "someinsn" and "otherinsn".

    The probe arguments in STAP_PROBE_ASM can be given as assembly
    operands instead, even inside a GNU C extended asm statement.
    Note that these can use operand templates like %0 or %[name],
    and likewise they must write %%reg for a literal operand of %reg.  */

#define _SDT_ASM_BODY_1(p,n,...) _SDT_ASM_BODY(p,n,_SDT_ASM_SUBSTR,(__VA_ARGS__))
#define _SDT_ASM_BODY_2(p,n,...) _SDT_ASM_BODY(p,n,/*_SDT_ASM_STRING */,__VA_ARGS__)
#define _SDT_ASM_BODY_N2(p,n,no,...) _SDT_ASM_BODY_ ## no(p,n,__VA_ARGS__)
#define _SDT_ASM_BODY_N1(p,n,no,...) _SDT_ASM_BODY_N2(p,n,no,__VA_ARGS__)
#define _SDT_ASM_BODY_N(p,n,...) _SDT_ASM_BODY_N1(p,n,_SDT_NARG(0, __VA_ARGS__),__VA_ARGS__)

#if __STDC_VERSION__ >= 199901L
# define STAP_PROBE_ASM(provider, name, ...)		\
  _SDT_ASM_BODY_N(provider, name, __VA_ARGS__)					\
  _SDT_ASM_BASE
# define STAP_PROBE_ASM_OPERANDS(n, ...) _SDT_ASM_OPERANDS_##n(__VA_ARGS__)
#else
# define STAP_PROBE_ASM(provider, name, args)	\
  _SDT_ASM_BODY(provider, name, /* _SDT_ASM_STRING */, (args))	\
  _SDT_ASM_BASE
#endif
#define STAP_PROBE_ASM_TEMPLATE(n) _SDT_ASM_TEMPLATE_##n,"use _SDT_ASM_TEMPLATE_"


/* DTrace compatible macro names.  */
#define DTRACE_PROBE(provider,probe)		\
  STAP_PROBE(provider,probe)
#define DTRACE_PROBE1(provider,probe,parm1)	\
  STAP_PROBE1(provider,probe,parm1)
#define DTRACE_PROBE2(provider,probe,parm1,parm2)	\
  STAP_PROBE2(provider,probe,parm1,parm2)
#define DTRACE_PROBE3(provider,probe,parm1,parm2,parm3) \
  STAP_PROBE3(provider,probe,parm1,parm2,parm3)
#define DTRACE_PROBE4(provider,probe,parm1,parm2,parm3,parm4)	\
  STAP_PROBE4(provider,probe,parm1,parm2,parm3,parm4)
#define DTRACE_PROBE5(provider,probe,parm1,parm2,parm3,parm4,parm5)	\
  STAP_PROBE5(provider,probe,parm1,parm2,parm3,parm4,parm5)
#define DTRACE_PROBE6(provider,probe,parm1,parm2,parm3,parm4,parm5,parm6) \
  STAP_PROBE6(provider,probe,parm1,parm2,parm3,parm4,parm5,parm6)
#define DTRACE_PROBE7(provider,probe,parm1,parm2,parm3,parm4,parm5,parm6,parm7) \
  STAP_PROBE7(provider,probe,parm1,parm2,parm3,parm4,parm5,parm6,parm7)
#define DTRACE_PROBE8(provider,probe,parm1,parm2,parm3,parm4,parm5,parm6,parm7,parm8) \
  STAP_PROBE8(provider,probe,parm1,parm2,parm3,parm4,parm5,parm6,parm7,parm8)
#define DTRACE_PROBE9(provider,probe,parm1,parm2,parm3,parm4,parm5,parm6,parm7,parm8,parm9) \
  STAP_PROBE9(provider,probe,parm1,parm2,parm3,parm4,parm5,parm6,parm7,parm8,parm9)
#define DTRACE_PROBE10(provider,probe,parm1,parm2,parm3,parm4,parm5,parm6,parm7,parm8,parm9,parm10) \
  STAP_PROBE10(provider,probe,parm1,parm2,parm3,parm4,parm5,parm6,parm7,parm8,parm9,parm10)
#define DTRACE_PROBE11(provider,probe,parm1,parm2,parm3,parm4,parm5,parm6,parm7,parm8,parm9,parm10,parm11) \
  STAP_PROBE11(provider,probe,parm1,parm2,parm3,parm4,parm5,parm6,parm7,parm8,parm9,parm10,parm11)
#define DTRACE_PROBE12(provider,probe,parm1,parm2,parm3,parm4,parm5,parm6,parm7,parm8,parm9,parm10,parm11,parm12) \
  STAP_PROBE12(provider,probe,parm1,parm2,parm3,parm4,parm5,parm6,parm7,parm8,parm9,parm10,parm11,parm12)


#endif /* sys/sdt.h */
/* ---- end bundled <sys/sdt.h> ---- */
#endif
#endif

#if defined(__clang__)
#define PROCCTL__DIAG_OFF                                                                       \
  _Pragma("clang diagnostic push")                                                              \
  _Pragma("clang diagnostic ignored \"-Wunknown-warning-option\"")                             \
  _Pragma("clang diagnostic ignored \"-Wvariadic-macro-arguments-omitted\"")                   \
  _Pragma("clang diagnostic ignored \"-Wgnu-zero-variadic-macro-arguments\"")
#define PROCCTL__DIAG_ON _Pragma("clang diagnostic pop")
#elif defined(__GNUC__)
#define PROCCTL__DIAG_OFF                                                                       \
  _Pragma("GCC diagnostic push") _Pragma("GCC diagnostic ignored \"-Wpedantic\"")
#define PROCCTL__DIAG_ON _Pragma("GCC diagnostic pop")
#else
#define PROCCTL__DIAG_OFF
#define PROCCTL__DIAG_ON
#endif

#if !defined(PROCCTL_NO_PROBES)
#define PROCCTL__PROBE_HELLO(fd, name, pid)                                                     \
  do {                                                                                          \
    PROCCTL__DIAG_OFF STAP_PROBE3(procctl, hello, fd, name, pid);                               \
    PROCCTL__DIAG_ON                                                                            \
  } while (0)
#define PROCCTL__PROBE_STATE(seq, seen)                                                         \
  do {                                                                                          \
    PROCCTL__DIAG_OFF STAP_PROBE2(procctl, state, seq, seen);                                   \
    PROCCTL__DIAG_ON                                                                            \
  } while (0)
#else
#define PROCCTL__PROBE_HELLO(fd, name, pid) ((void)0)
#define PROCCTL__PROBE_STATE(seq, seen) ((void)0)
#endif

#ifndef MFD_CLOEXEC
#define MFD_CLOEXEC 0x0001u
#endif
#ifndef MFD_ALLOW_SEALING
#define MFD_ALLOW_SEALING 0x0002u
#endif
#ifndef MFD_NOEXEC_SEAL
#define MFD_NOEXEC_SEAL 0x0008u
#endif
#ifndef F_ADD_SEALS
#define F_ADD_SEALS 1033
#endif
#ifndef F_SEAL_SEAL
#define F_SEAL_SEAL 0x0001
#define F_SEAL_SHRINK 0x0002
#define F_SEAL_GROW 0x0004
#endif
#ifndef SYS_getrandom
#define SYS_getrandom __NR_getrandom
#endif

/* Field offsets. */
#define PC__H_SIG 0
#define PC__H_MAJOR 8
#define PC__H_MINOR 10
#define PC__H_HSIZE 12
#define PC__H_TOTAL 16
#define PC__H_PAGE 24
#define PC__H_CONSUMER 32
#define PC__H_SCHEMA 64
#define PC__H_CREATOR 68
#define PC__H_AREA_OFF 72
#define PC__H_AREA_SIZE 80
#define PC__H_STATE_ADDR 88
#define PC__H_STATE_SIZE 96
#define PC__H_SLOTS 100
#define PC__H_CREATED 104
#define PC__H_INSTANCE 112
#define PC__H_CRC 120

#define PC__C_SEQ 0
#define PC__C_LEN 8
#define PC__C_FLAGS 12
#define PC__C_WRITER 16
#define PC__C_HB 24
#define PC__C_LEASE 32
#define PC__C_CRC 56

#define PC__S_MAGIC 0
#define PC__S_SEQ 8
#define PC__S_INSTANCE 16
#define PC__S_PID 24
#define PC__S_LEN 28
#define PC__S_APPLIED 32
#define PC__S_APPLIED_W 40
#define PC__S_SEEN 48
#define PC__S_CRC 56

#define PC__E_ID 0
#define PC__E_GRANT 16
#define PC__E_DUR 24       /* grant entry */
#define PC__E_ACTIVITY 24  /* resource entry */
#define PC__E_KIND 32
#define PC__E_STATE 36
#define PC__E_FLAGS 40
#define PC__E_ERROR 44

static const uint32_t procctl__crc_tab[256] = {
    0x00000000u, 0xf26b8303u, 0xe13b70f7u, 0x1350f3f4u, 0xc79a971fu, 0x35f1141cu,
    0x26a1e7e8u, 0xd4ca64ebu, 0x8ad958cfu, 0x78b2dbccu, 0x6be22838u, 0x9989ab3bu,
    0x4d43cfd0u, 0xbf284cd3u, 0xac78bf27u, 0x5e133c24u, 0x105ec76fu, 0xe235446cu,
    0xf165b798u, 0x030e349bu, 0xd7c45070u, 0x25afd373u, 0x36ff2087u, 0xc494a384u,
    0x9a879fa0u, 0x68ec1ca3u, 0x7bbcef57u, 0x89d76c54u, 0x5d1d08bfu, 0xaf768bbcu,
    0xbc267848u, 0x4e4dfb4bu, 0x20bd8edeu, 0xd2d60dddu, 0xc186fe29u, 0x33ed7d2au,
    0xe72719c1u, 0x154c9ac2u, 0x061c6936u, 0xf477ea35u, 0xaa64d611u, 0x580f5512u,
    0x4b5fa6e6u, 0xb93425e5u, 0x6dfe410eu, 0x9f95c20du, 0x8cc531f9u, 0x7eaeb2fau,
    0x30e349b1u, 0xc288cab2u, 0xd1d83946u, 0x23b3ba45u, 0xf779deaeu, 0x05125dadu,
    0x1642ae59u, 0xe4292d5au, 0xba3a117eu, 0x4851927du, 0x5b016189u, 0xa96ae28au,
    0x7da08661u, 0x8fcb0562u, 0x9c9bf696u, 0x6ef07595u, 0x417b1dbcu, 0xb3109ebfu,
    0xa0406d4bu, 0x522bee48u, 0x86e18aa3u, 0x748a09a0u, 0x67dafa54u, 0x95b17957u,
    0xcba24573u, 0x39c9c670u, 0x2a993584u, 0xd8f2b687u, 0x0c38d26cu, 0xfe53516fu,
    0xed03a29bu, 0x1f682198u, 0x5125dad3u, 0xa34e59d0u, 0xb01eaa24u, 0x42752927u,
    0x96bf4dccu, 0x64d4cecfu, 0x77843d3bu, 0x85efbe38u, 0xdbfc821cu, 0x2997011fu,
    0x3ac7f2ebu, 0xc8ac71e8u, 0x1c661503u, 0xee0d9600u, 0xfd5d65f4u, 0x0f36e6f7u,
    0x61c69362u, 0x93ad1061u, 0x80fde395u, 0x72966096u, 0xa65c047du, 0x5437877eu,
    0x4767748au, 0xb50cf789u, 0xeb1fcbadu, 0x197448aeu, 0x0a24bb5au, 0xf84f3859u,
    0x2c855cb2u, 0xdeeedfb1u, 0xcdbe2c45u, 0x3fd5af46u, 0x7198540du, 0x83f3d70eu,
    0x90a324fau, 0x62c8a7f9u, 0xb602c312u, 0x44694011u, 0x5739b3e5u, 0xa55230e6u,
    0xfb410cc2u, 0x092a8fc1u, 0x1a7a7c35u, 0xe811ff36u, 0x3cdb9bddu, 0xceb018deu,
    0xdde0eb2au, 0x2f8b6829u, 0x82f63b78u, 0x709db87bu, 0x63cd4b8fu, 0x91a6c88cu,
    0x456cac67u, 0xb7072f64u, 0xa457dc90u, 0x563c5f93u, 0x082f63b7u, 0xfa44e0b4u,
    0xe9141340u, 0x1b7f9043u, 0xcfb5f4a8u, 0x3dde77abu, 0x2e8e845fu, 0xdce5075cu,
    0x92a8fc17u, 0x60c37f14u, 0x73938ce0u, 0x81f80fe3u, 0x55326b08u, 0xa759e80bu,
    0xb4091bffu, 0x466298fcu, 0x1871a4d8u, 0xea1a27dbu, 0xf94ad42fu, 0x0b21572cu,
    0xdfeb33c7u, 0x2d80b0c4u, 0x3ed04330u, 0xccbbc033u, 0xa24bb5a6u, 0x502036a5u,
    0x4370c551u, 0xb11b4652u, 0x65d122b9u, 0x97baa1bau, 0x84ea524eu, 0x7681d14du,
    0x2892ed69u, 0xdaf96e6au, 0xc9a99d9eu, 0x3bc21e9du, 0xef087a76u, 0x1d63f975u,
    0x0e330a81u, 0xfc588982u, 0xb21572c9u, 0x407ef1cau, 0x532e023eu, 0xa145813du,
    0x758fe5d6u, 0x87e466d5u, 0x94b49521u, 0x66df1622u, 0x38cc2a06u, 0xcaa7a905u,
    0xd9f75af1u, 0x2b9cd9f2u, 0xff56bd19u, 0x0d3d3e1au, 0x1e6dcdeeu, 0xec064eedu,
    0xc38d26c4u, 0x31e6a5c7u, 0x22b65633u, 0xd0ddd530u, 0x0417b1dbu, 0xf67c32d8u,
    0xe52cc12cu, 0x1747422fu, 0x49547e0bu, 0xbb3ffd08u, 0xa86f0efcu, 0x5a048dffu,
    0x8ecee914u, 0x7ca56a17u, 0x6ff599e3u, 0x9d9e1ae0u, 0xd3d3e1abu, 0x21b862a8u,
    0x32e8915cu, 0xc083125fu, 0x144976b4u, 0xe622f5b7u, 0xf5720643u, 0x07198540u,
    0x590ab964u, 0xab613a67u, 0xb831c993u, 0x4a5a4a90u, 0x9e902e7bu, 0x6cfbad78u,
    0x7fab5e8cu, 0x8dc0dd8fu, 0xe330a81au, 0x115b2b19u, 0x020bd8edu, 0xf0605beeu,
    0x24aa3f05u, 0xd6c1bc06u, 0xc5914ff2u, 0x37faccf1u, 0x69e9f0d5u, 0x9b8273d6u,
    0x88d28022u, 0x7ab90321u, 0xae7367cau, 0x5c18e4c9u, 0x4f48173du, 0xbd23943eu,
    0xf36e6f75u, 0x0105ec76u, 0x12551f82u, 0xe03e9c81u, 0x34f4f86au, 0xc69f7b69u,
    0xd5cf889du, 0x27a40b9eu, 0x79b737bau, 0x8bdcb4b9u, 0x988c474du, 0x6ae7c44eu,
    0xbe2da0a5u, 0x4c4623a6u, 0x5f16d052u, 0xad7d5351u,
};

static uint32_t procctl__crc_update(uint32_t crc, const void *p, size_t n) {
  const uint8_t *b = (const uint8_t *)p;
  while (n--) crc = procctl__crc_tab[(crc ^ *b++) & 0xffu] ^ (crc >> 8);
  return crc;
}
static uint32_t procctl__crc32c(const void *p, size_t n) {
  return ~procctl__crc_update(~0u, p, n);
}

static inline uint64_t pc__ld64(const uint8_t *p) {
  uint64_t v;
  memcpy(&v, p, 8);
  return v;
}
static inline uint32_t pc__ld32(const uint8_t *p) {
  uint32_t v;
  memcpy(&v, p, 4);
  return v;
}
static inline void pc__st64(uint8_t *p, uint64_t v) { memcpy(p, &v, 8); }
static inline void pc__st32(uint8_t *p, uint32_t v) { memcpy(p, &v, 4); }
static inline void pc__st16(uint8_t *p, uint16_t v) { memcpy(p, &v, 2); }

/* Atomic loads on the read-only mapping (8-byte aligned addresses). */
static inline uint64_t pc__aload64(const uint8_t *p, int mo) {
  return __atomic_load_n((const uint64_t *)(const void *)p, mo);
}
static inline uint32_t pc__aload32(const uint8_t *p, int mo) {
  return __atomic_load_n((const uint32_t *)(const void *)p, mo);
}

static struct procctl__ctx *pc__c(procctl_t *ctx) {
  return (struct procctl__ctx *)(void *)ctx->opaque;
}

PROCCTL_DEF uint64_t procctl_now_ns(void) {
  struct timespec ts;
  clock_gettime(CLOCK_BOOTTIME, &ts);
  return (uint64_t)ts.tv_sec * 1000000000ull + (uint64_t)ts.tv_nsec;
}

static uint64_t pc__random64(void) {
  uint64_t v = 0;
  if (syscall(SYS_getrandom, &v, sizeof v, 1 /* GRND_NONBLOCK */) != (long)sizeof v || v == 0) {
    v = procctl_now_ns() ^ ((uint64_t)getpid() << 32) ^ (uint64_t)(uintptr_t)&v;
    v ^= v >> 29;
    v *= 0xbf58476d1ce4e5b9ull;
    v ^= v >> 32;
  }
  return v ? v : 1;
}

static int pc__valid_consumer(const char *s) {
  size_t n = 0;
  if (!s) return 0;
  for (; s[n]; n++) {
    char ch = s[n];
    if (!((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_')) return 0;
    if (n >= 31) return 0;
  }
  return n > 0;
}

/* ---- state block ------------------------------------------------------- */

static size_t pc__state_used(struct procctl__ctx *c) {
  return PROCCTL_STATE_HDR_SIZE + (size_t)c->slots * PROCCTL_ENTRY_SIZE +
         pc__ld32(c->state + PC__S_LEN);
}

/* CRC32C of the used block with seq and crc32c taken as zero (SPEC §6). */
static uint32_t pc__state_crc(struct procctl__ctx *c) {
  static const uint8_t zero8[8] = {0};
  uint32_t crc = ~0u;
  crc = procctl__crc_update(crc, c->state, PC__S_SEQ);
  crc = procctl__crc_update(crc, zero8, 8);
  crc = procctl__crc_update(crc, c->state + 16, PC__S_CRC - 16);
  crc = procctl__crc_update(crc, zero8, 4);
  crc = procctl__crc_update(crc, c->state + PC__S_CRC + 4, pc__state_used(c) - (PC__S_CRC + 4));
  return ~crc;
}

static int pc__state_lock(struct procctl__ctx *c, int lflags) {
  if (getpid() != c->pid) return -ECHILD; /* SPEC §12: never write an inherited block */
  for (unsigned spins = 0;; spins++) {
    int exp = 0;
    if (__atomic_compare_exchange_n(&c->state_lock, &exp, 1, 0, __ATOMIC_ACQUIRE,
                                    __ATOMIC_RELAXED))
      return 0;
    if (lflags & PROCCTL_TRY) return -EBUSY;
    if (spins > 64) sched_yield();
  }
}

/* Begin a state update: seq → odd (SPEC §7.2). Caller holds the lock. */
static void pc__state_begin(struct procctl__ctx *c) {
  uint64_t *seq = (uint64_t *)(void *)(c->state + PC__S_SEQ);
  __atomic_store_n(seq, __atomic_load_n(seq, __ATOMIC_RELAXED) + 1, __ATOMIC_RELAXED);
  __atomic_thread_fence(__ATOMIC_RELEASE);
}

static void pc__state_end(struct procctl__ctx *c) {
  uint64_t *seq = (uint64_t *)(void *)(c->state + PC__S_SEQ);
  pc__st32(c->state + PC__S_PID, (uint32_t)c->pid);
  pc__st32(c->state + PC__S_CRC, pc__state_crc(c));
  uint64_t s = __atomic_load_n(seq, __ATOMIC_RELAXED) + 1;
  __atomic_store_n(seq, s, __ATOMIC_RELEASE);
  __atomic_store_n(&c->state_lock, 0, __ATOMIC_RELEASE);
  if (c->flags & PROCCTL_F_STATE_PROBE) {
    uint64_t seen = pc__ld64(c->state + PC__S_SEEN);
    PROCCTL__PROBE_STATE(s, seen);
  }
}

static void pc__state_reset(struct procctl__ctx *c) {
  memset(c->state, 0, c->state_block_size);
  memcpy(c->state + PC__S_MAGIC, "PROCSTAT", 8);
  pc__st64(c->state + PC__S_INSTANCE, c->instance_id);
  pc__st32(c->state + PC__S_PID, (uint32_t)c->pid);
  pc__st64(c->state + PC__S_SEQ, 2);
  pc__st32(c->state + PC__S_CRC, pc__state_crc(c));
  memset(c->grant_track, 0, (size_t)c->slots * 16u);
}

/* ---- region ---------------------------------------------------------------- */

static int pc__create_region(struct procctl__ctx *c) {
  long page = sysconf(_SC_PAGESIZE);
  if (page < 4096 || page > 65536 || (page & (page - 1))) return -EINVAL;
  size_t P = (size_t)page;
  size_t need = PROCCTL_CTRL_SIZE + (size_t)c->slots * PROCCTL_ENTRY_SIZE + c->payload_capacity;
  size_t area = (need + P - 1) / P * P;
  size_t total = P + area;
  void *m;

  int fd = (int)syscall(SYS_memfd_create, c->memfd_name,
                        MFD_CLOEXEC | MFD_ALLOW_SEALING | MFD_NOEXEC_SEAL);
  if (fd < 0 && errno == EINVAL)
    fd = (int)syscall(SYS_memfd_create, c->memfd_name, MFD_CLOEXEC | MFD_ALLOW_SEALING);
  if (fd < 0) return -errno;
  if (ftruncate(fd, (off_t)total) != 0) goto fail;

  c->instance_id = pc__random64();
  pc__state_reset(c);

  {
    uint8_t h[PROCCTL_HEADER_SIZE];
    memset(h, 0, sizeof h);
    memcpy(h + PC__H_SIG, "PROC_CTL", 8);
    pc__st16(h + PC__H_MAJOR, PROCCTL_SPEC_MAJOR);
    pc__st16(h + PC__H_MINOR, PROCCTL_SPEC_MINOR);
    pc__st32(h + PC__H_HSIZE, PROCCTL_HEADER_SIZE);
    pc__st64(h + PC__H_TOTAL, total);
    pc__st32(h + PC__H_PAGE, (uint32_t)P);
    memcpy(h + PC__H_CONSUMER, c->consumer, strlen(c->consumer));
    pc__st32(h + PC__H_SCHEMA, c->schema_version);
    pc__st32(h + PC__H_CREATOR, (uint32_t)getpid());
    pc__st64(h + PC__H_AREA_OFF, P);
    pc__st64(h + PC__H_AREA_SIZE, area);
    pc__st64(h + PC__H_STATE_ADDR, (uint64_t)(uintptr_t)c->state);
    pc__st32(h + PC__H_STATE_SIZE, c->state_block_size);
    pc__st16(h + PC__H_SLOTS, c->slots);
    pc__st64(h + PC__H_CREATED, procctl_now_ns());
    pc__st64(h + PC__H_INSTANCE, c->instance_id);
    pc__st32(h + PC__H_CRC, procctl__crc32c(h, PC__H_CRC));
    if (pwrite(fd, h, sizeof h, 0) != (ssize_t)sizeof h) goto fail;
  }
  if (fcntl(fd, F_ADD_SEALS, F_SEAL_SHRINK | F_SEAL_GROW | F_SEAL_SEAL) != 0) goto fail;

  m = mmap(NULL, total, PROT_READ, MAP_SHARED, fd, 0);
  if (m == MAP_FAILED) goto fail;

  c->fd = fd;
  c->map = (const uint8_t *)m;
  c->map_size = total;
  c->area = c->map + P;
  c->area_size = area;
  c->seq = (const uint64_t *)(const void *)(c->area + PC__C_SEQ);
  __atomic_store_n(&c->last_seen, 0, __ATOMIC_RELAXED);
  __atomic_store_n(&c->hb_seen, 0, __ATOMIC_RELAXED);
  __atomic_store_n(&c->hb_local_ns, procctl_now_ns(), __ATOMIC_RELAXED);
  __atomic_store_n(&c->lease_expired, 1, __ATOMIC_RELAXED);
  return 0;
fail : {
  int e = errno;
  close(fd);
  return -(e ? e : EIO);
}
}

PROCCTL_DEF int procctl_init(procctl_t *ctx, const procctl_config_t *cfg) {
  PROCCTL__STATIC_ASSERT(sizeof(struct procctl__ctx) <= sizeof(((procctl_t *)0)->opaque),
                         "procctl_t too small");
  struct procctl__ctx *c = pc__c(ctx);
  memset(ctx, 0, sizeof *ctx);
  if (!cfg || !pc__valid_consumer(cfg->consumer)) return -EINVAL;
  if (cfg->resource_slots > PROCCTL_MAX_SLOTS) return -EINVAL;
  size_t block = PROCCTL_STATE_BLOCK_SIZE(cfg->resource_slots, cfg->state_capacity);
  if (block > PROCCTL_MAX_STATE_SIZE) return -E2BIG;
  if (!cfg->state_storage || ((uintptr_t)cfg->state_storage & 7u)) return -EINVAL;

  c->slots = cfg->resource_slots;
  c->state = (uint8_t *)cfg->state_storage;
  c->state_block_size = (uint32_t)block;
  c->grant_track = c->state + block;
  c->state_capacity = cfg->state_capacity;
  c->payload_capacity = cfg->payload_capacity;
  c->schema_version = cfg->schema_version;
  c->flags = cfg->flags;
  c->pid = getpid();
  c->fd = -1;
  strncpy(c->consumer, cfg->consumer, sizeof c->consumer - 1);
  {
    /* "procctl:<consumer>-v<major>" */
    char *p = c->memfd_name;
    memcpy(p, "procctl:", 8);
    p += 8;
    size_t n = strlen(c->consumer);
    memcpy(p, c->consumer, n);
    p += n;
    *p++ = '-';
    *p++ = 'v';
    *p++ = (char)('0' + PROCCTL_SPEC_MAJOR);
    *p = 0;
  }
  int r = pc__create_region(c);
  if (r) return r;
  c->ready = 1;
  if (!(c->flags & PROCCTL_F_NO_HELLO)) {
    int fd = c->fd, pid = c->pid;
    const char *name = c->memfd_name;
    PROCCTL__PROBE_HELLO(fd, name, pid);
  }
  return 0;
}

PROCCTL_DEF void procctl_close(procctl_t *ctx) {
  struct procctl__ctx *c = pc__c(ctx);
  if (!c->ready) return;
  c->ready = 0;
  munmap((void *)c->map, c->map_size);
  close(c->fd);
}

PROCCTL_DEF int procctl_reinit(procctl_t *ctx) {
  struct procctl__ctx *c = pc__c(ctx);
  if (!c->ready) return -ENODEV;
  c->ready = 0;
  munmap((void *)c->map, c->map_size);
  close(c->fd);
  c->pid = getpid();
  __atomic_store_n(&c->state_lock, 0, __ATOMIC_RELAXED);
  int r = pc__create_region(c);
  if (r) return r;
  c->ready = 1;
  if (!(c->flags & PROCCTL_F_NO_HELLO)) {
    int fd = c->fd, pid = c->pid;
    const char *name = c->memfd_name;
    PROCCTL__PROBE_HELLO(fd, name, pid);
  }
  return 0;
}

/* ---- agent area reads (SPEC §7.1) ------------------------------------------- */

/* Streams the agent payload [64, 64 + slots*48 + n) of the area through the
 * seqlock reader. grants (may be NULL) receives the grant table; payload (may
 * be NULL) receives the consumer payload. Returns 0, -EAGAIN (busy/torn). */
static int pc__read_payload(struct procctl__ctx *c, uint64_t s1, uint8_t *grants, uint8_t *payload,
                            size_t payload_cap, size_t *len_out, uint32_t *flags_out) {
  const uint8_t *a = c->area;
  uint32_t n = pc__aload32(a + PC__C_LEN, __ATOMIC_RELAXED);
  uint32_t want_crc = pc__aload32(a + PC__C_CRC, __ATOMIC_RELAXED);
  uint32_t flags = pc__aload32(a + PC__C_FLAGS, __ATOMIC_RELAXED);
  size_t gt = (size_t)c->slots * PROCCTL_ENTRY_SIZE;
  if (PROCCTL_CTRL_SIZE + gt + (size_t)n > c->area_size) return -EAGAIN;
  if (payload && n > payload_cap) return -E2BIG;
  uint32_t crc = ~0u;
  const uint8_t *src = a + PROCCTL_CTRL_SIZE;
  size_t total = gt + n;
  for (size_t off = 0; off < total; off += 8) {
    uint64_t w = pc__aload64(src + off, __ATOMIC_RELAXED);
    size_t take = total - off < 8 ? total - off : 8;
    crc = procctl__crc_update(crc, &w, take);
    if (off < gt) {
      if (grants) memcpy(grants + off, &w, take); /* gt is a multiple of 8 */
    } else if (payload) {
      memcpy(payload + (off - gt), &w, take);
    }
  }
  __atomic_thread_fence(__ATOMIC_ACQUIRE);
  uint64_t s2 = pc__aload64(a + PC__C_SEQ, __ATOMIC_RELAXED);
  if (s2 != s1 || ~crc != want_crc) return -EAGAIN;
  if (len_out) *len_out = n;
  if (flags_out) *flags_out = flags;
  return 0;
}

PROCCTL_DEF int procctl__poll_slow(struct procctl__ctx *c, uint64_t s1, void *buf, size_t cap,
                                   uint64_t *gen) {
  uint64_t old = __atomic_load_n(&c->last_seen, __ATOMIC_RELAXED);
  if (s1 == old) return 0;
  if ((s1 & 1u) || s1 == 0) return -EAGAIN;
  if (pc__aload32(c->area + PC__C_LEN, __ATOMIC_RELAXED) > cap) {
    if (__atomic_compare_exchange_n(&c->last_seen, &old, s1, 0, __ATOMIC_RELAXED,
                                    __ATOMIC_RELAXED))
      return -E2BIG;
    return 0;
  }
  /* Claim the generation first so exactly one caller proceeds. A torn read
   * means seq has moved on, so the next poll sees a newer generation. */
  if (!__atomic_compare_exchange_n(&c->last_seen, &old, s1, 0, __ATOMIC_RELAXED,
                                   __ATOMIC_RELAXED))
    return 0;
  size_t n = 0;
  uint32_t flags = 0;
  for (int tries = 0; tries < 3; tries++) {
    int r = pc__read_payload(c, s1, NULL, (uint8_t *)buf, cap, &n, &flags);
    if (r == 0) {
      if (gen) *gen = s1;
      if (!(flags & 1u) || n == 0) return -ENODATA; /* PAYLOAD_VALID clear: use defaults */
      return (int)n;
    }
    if (r == -E2BIG) return -E2BIG;
    if (pc__aload64(c->area + PC__C_SEQ, __ATOMIC_ACQUIRE) != s1) break;
  }
  return -EAGAIN;
}

PROCCTL_DEF bool procctl_agent_attached(const procctl_t *ctx) {
  const struct procctl__ctx *c = (const struct procctl__ctx *)(const void *)ctx->opaque;
  return c->ready && pc__aload64(c->area + PC__C_WRITER, __ATOMIC_ACQUIRE) != 0;
}

/* ---- lease (SPEC §8.3) ---------------------------------------------------------- */

PROCCTL_DEF bool procctl_lease_expired(procctl_t *ctx, uint64_t now_ns) {
  struct procctl__ctx *c = pc__c(ctx);
  if (!c->ready) return true;
  if (!now_ns) now_ns = procctl_now_ns();
  bool expired;
  uint64_t writer = pc__aload64(c->area + PC__C_WRITER, __ATOMIC_ACQUIRE);
  uint64_t lease = pc__aload64(c->area + PC__C_LEASE, __ATOMIC_ACQUIRE);
  uint64_t hb = pc__aload64(c->area + PC__C_HB, __ATOMIC_ACQUIRE);
  if (writer == 0) {
    expired = true;
  } else {
    if (hb != __atomic_load_n(&c->hb_seen, __ATOMIC_RELAXED)) {
      __atomic_store_n(&c->hb_seen, hb, __ATOMIC_RELAXED);
      __atomic_store_n(&c->hb_local_ns, now_ns, __ATOMIC_RELAXED);
    }
    expired = lease != 0 && now_ns - __atomic_load_n(&c->hb_local_ns, __ATOMIC_RELAXED) > lease;
  }
  int was = __atomic_exchange_n(&c->lease_expired, expired ? 1 : 0, __ATOMIC_RELAXED);
  if (was && !expired) /* recovered: re-deliver the current payload */
    __atomic_store_n(&c->last_seen, 0, __ATOMIC_RELAXED);
  return expired;
}

/* ---- state writes ------------------------------------------------------------- */

PROCCTL_DEF int procctl_ack(procctl_t *ctx, uint64_t gen, int lflags) {
  struct procctl__ctx *c = pc__c(ctx);
  if (!c->ready) return -ENODEV;
  int r = pc__state_lock(c, lflags);
  if (r) return r;
  uint64_t w = pc__aload64(c->area + PC__C_WRITER, __ATOMIC_ACQUIRE);
  pc__state_begin(c);
  if (gen > pc__ld64(c->state + PC__S_APPLIED)) pc__st64(c->state + PC__S_APPLIED, gen);
  pc__st64(c->state + PC__S_APPLIED_W, w);
  pc__state_end(c);
  return 0;
}

PROCCTL_DEF int procctl_state_set(procctl_t *ctx, const void *state, size_t len, int lflags) {
  struct procctl__ctx *c = pc__c(ctx);
  if (!c->ready) return -ENODEV;
  if (len > c->state_capacity) return -E2BIG;
  int r = pc__state_lock(c, lflags);
  if (r) return r;
  pc__state_begin(c);
  uint8_t *dst = c->state + PROCCTL_STATE_HDR_SIZE + (size_t)c->slots * PROCCTL_ENTRY_SIZE;
  memcpy(dst, state, len);
  pc__st32(c->state + PC__S_LEN, (uint32_t)len);
  pc__state_end(c);
  return 0;
}

/* ---- exclusive resources (SPEC §9) ------------------------------------------- */

PROCCTL_DEF int procctl_grant_get(procctl_t *ctx, uint32_t kind, const uint8_t id[16],
                                  procctl_grant_t *out) {
  struct procctl__ctx *c = pc__c(ctx);
  memset(out, 0, sizeof *out);
  if (!c->ready) return -ENODEV;
  if (c->slots == 0) return -ENOENT;
  uint8_t table[PROCCTL_MAX_SLOTS * PROCCTL_ENTRY_SIZE];
  for (int tries = 0; tries < 8; tries++) {
    uint64_t s1 = pc__aload64(c->area + PC__C_SEQ, __ATOMIC_ACQUIRE);
    if (s1 & 1u) continue;
    if (s1 == 0) return 0; /* never written: no grants */
    if (pc__read_payload(c, s1, table, NULL, 0, NULL, NULL) != 0) continue;
    out->seen_gen = s1;
    for (unsigned i = 0; i < c->slots; i++) {
      const uint8_t *e = table + (size_t)i * PROCCTL_ENTRY_SIZE;
      uint64_t g = pc__ld64(e + PC__E_GRANT);
      if (g && pc__ld32(e + PC__E_KIND) == kind && memcmp(e + PC__E_ID, id, 16) == 0) {
        out->grant_id = g;
        out->duration_ns = pc__ld64(e + PC__E_DUR);
        out->kind = kind;
        break;
      }
    }
    return 0;
  }
  return -EAGAIN;
}

PROCCTL_DEF bool procctl_grant_usable(procctl_t *ctx, const procctl_grant_t *g, uint64_t now_ns) {
  struct procctl__ctx *c = pc__c(ctx);
  if (!c->ready || !g->grant_id) return false;
  if (!now_ns) now_ns = procctl_now_ns();
  /* Track the first time each grant_id was observed (SPEC §9.1 H1/H3). */
  uint8_t *t = c->grant_track;
  int slot = -1, oldest = 0;
  uint64_t oldest_ns = UINT64_MAX;
  for (unsigned i = 0; i < c->slots; i++) {
    uint64_t gid = pc__ld64(t + i * 16u);
    uint64_t ts = pc__ld64(t + i * 16u + 8);
    if (gid == g->grant_id) { slot = (int)i; break; }
    if (ts < oldest_ns) { oldest_ns = ts; oldest = (int)i; }
  }
  if (slot < 0) {
    slot = oldest;
    pc__st64(t + (unsigned)slot * 16u, g->grant_id);
    pc__st64(t + (unsigned)slot * 16u + 8, now_ns);
  }
  uint64_t first = pc__ld64(t + (unsigned)slot * 16u + 8);
  return now_ns - first < g->duration_ns;
}

PROCCTL_DEF int procctl_resource_report(procctl_t *ctx, uint32_t kind, const uint8_t id[16],
                                        uint32_t state, uint64_t grant_id, uint32_t flags,
                                        uint32_t error, uint64_t grants_seen_gen, int lflags) {
  struct procctl__ctx *c = pc__c(ctx);
  if (!c->ready) return -ENODEV;
  if (c->slots == 0) return -ENOENT;
  int r = pc__state_lock(c, lflags);
  if (r) return r;
  uint8_t *tab = c->state + PROCCTL_STATE_HDR_SIZE;
  uint8_t *e = NULL, *empty = NULL;
  for (unsigned i = 0; i < c->slots; i++) {
    uint8_t *x = tab + (size_t)i * PROCCTL_ENTRY_SIZE;
    uint8_t z[16] = {0};
    if (pc__ld32(x + PC__E_KIND) == kind && memcmp(x + PC__E_ID, id, 16) == 0) { e = x; break; }
    if (!empty && pc__ld32(x + PC__E_KIND) == 0 && memcmp(x + PC__E_ID, z, 16) == 0) empty = x;
  }
  if (!e) e = empty;
  if (!e) {
    __atomic_store_n(&c->state_lock, 0, __ATOMIC_RELEASE);
    return -ENOSPC;
  }
  pc__state_begin(c);
  memcpy(e + PC__E_ID, id, 16);
  pc__st64(e + PC__E_GRANT, grant_id);
  if ((flags & PROCCTL_RES_F_WANT) || state == PROCCTL_RES_ACQUIRING ||
      state == PROCCTL_RES_HOLDING)
    pc__st64(e + PC__E_ACTIVITY, procctl_now_ns());
  pc__st32(e + PC__E_KIND, kind);
  pc__st32(e + PC__E_STATE, state);
  pc__st32(e + PC__E_FLAGS, flags);
  pc__st32(e + PC__E_ERROR, error);
  if (grants_seen_gen > pc__ld64(c->state + PC__S_SEEN))
    pc__st64(c->state + PC__S_SEEN, grants_seen_gen);
  pc__state_end(c);
  return 0;
}

/* ---- wait (SPEC §8.4) --------------------------------------------------------------- */

PROCCTL_DEF int procctl_wait(procctl_t *ctx, uint64_t timeout_ns) {
  struct procctl__ctx *c = pc__c(ctx);
  if (!c->ready) return -ENODEV;
  const uint32_t *word = (const uint32_t *)(const void *)(c->area + PC__C_SEQ);
  uint32_t v = __atomic_load_n(word, __ATOMIC_ACQUIRE);
  struct timespec ts;
  ts.tv_sec = (time_t)(timeout_ns / 1000000000ull);
  ts.tv_nsec = (long)(timeout_ns % 1000000000ull);
  long r = syscall(SYS_futex, word, FUTEX_WAIT, v, &ts, NULL, 0);
  if (r == -1 && errno == ETIMEDOUT) return -ETIMEDOUT;
  return 0;
}

#endif /* PROCCTL_IMPLEMENTATION_DONE */
#endif /* PROCCTL_IMPLEMENTATION */
