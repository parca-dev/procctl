// ctlthread: can PC sampling be released from a thread other than the one
// running the app's CUDA work?
//
//   --test release  : main thread enables PC sampling, opens a window and
//                     launches kernels; a control thread does Stop + Disable at
//                     --release-at-ms. With --idle-at-ms the main thread stops
//                     making CUDA calls at that time (app goes dark).
//                     --no-stop: control thread calls Disable without Stop.
//                     --sync:    control thread cuCtxSynchronize()s first.
//   --test flush    : no PC sampling; CUPTI activity API with
//                     cuptiActivityFlushPeriod. Logs every bufferCompleted call
//                     (thread, size) while the app is busy and then idle.
//
// JSON lines on --out, same envelope as pcprobe.

#include <cuda.h>
#include <cuda_runtime.h>
#include <cupti.h>
#include <cupti_pcsampling.h>

#include <getopt.h>
#include <pthread.h>
#include <signal.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>

#include <atomic>
#include <mutex>
#include <vector>

extern "C" __global__ void spin_k0(long long cycles, int *sink) {
  long long t0 = clock64();
  unsigned x = threadIdx.x + blockIdx.x * 3;
  while (clock64() - t0 < cycles) x = x * 1664525u + 1013904223u;
  if (x == 0x12345678u) *sink = (int)x;
}

static FILE *g_out = stdout;
static const char *g_name = "A";
static std::mutex g_logm;

static uint64_t now_ns() {
  struct timespec ts;
  clock_gettime(CLOCK_BOOTTIME, &ts);
  return (uint64_t)ts.tv_sec * 1000000000ull + ts.tv_nsec;
}
static long tid() { return syscall(SYS_gettid); }

static void logj(const char *ev, const char *fmt = nullptr, ...) {
  std::lock_guard<std::mutex> l(g_logm);
  fprintf(g_out, "{\"ev\":\"%s\",\"name\":\"%s\",\"pid\":%d,\"tid\":%ld,\"ts\":%llu", ev,
          g_name, getpid(), tid(), (unsigned long long)now_ns());
  if (fmt) {
    fputc(',', g_out);
    va_list ap;
    va_start(ap, fmt);
    vfprintf(g_out, fmt, ap);
    va_end(ap);
  }
  fputs("}\n", g_out);
  fflush(g_out);
}

static const char *cerr(CUptiResult r) {
  const char *s = nullptr;
  cuptiGetResultString(r, &s);
  return s ? s : "?";
}

// ------------------------------------------------------------ PC sampling setup

static CUpti_PCSamplingData alloc_pcdata(size_t npcs, size_t nstall) {
  CUpti_PCSamplingData d{};
  d.size = sizeof(d);
  d.collectNumPcs = npcs;
  d.pPcData = (CUpti_PCSamplingPCData *)calloc(npcs, sizeof(CUpti_PCSamplingPCData));
  for (size_t i = 0; i < npcs; i++) {
    d.pPcData[i].size = sizeof(CUpti_PCSamplingPCData);
    d.pPcData[i].stallReason =
        (CUpti_PCSamplingStallReason *)calloc(nstall, sizeof(CUpti_PCSamplingStallReason));
  }
  return d;
}

static CUpti_PCSamplingData g_cfgBuf, g_outBuf;

static bool pcs_enable(CUcontext ctx) {
  CUpti_PCSamplingEnableParams ep{};
  ep.size = CUpti_PCSamplingEnableParamsSize;
  ep.ctx = ctx;
  CUptiResult r = cuptiPCSamplingEnable(&ep);
  logj("enable", "\"rc\":%d,\"err\":\"%s\"", r, cerr(r));
  if (r != CUPTI_SUCCESS) return false;
  size_t n = 0;
  CUpti_PCSamplingGetNumStallReasonsParams np{};
  np.size = CUpti_PCSamplingGetNumStallReasonsParamsSize;
  np.ctx = ctx;
  np.numStallReasons = &n;
  cuptiPCSamplingGetNumStallReasons(&np);
  uint32_t *idx = (uint32_t *)calloc(n, sizeof(uint32_t));
  char **names = (char **)calloc(n, sizeof(char *));
  for (size_t i = 0; i < n; i++) names[i] = (char *)calloc(CUPTI_STALL_REASON_STRING_SIZE, 1);
  CUpti_PCSamplingGetStallReasonsParams sp{};
  sp.size = CUpti_PCSamplingGetStallReasonsParamsSize;
  sp.ctx = ctx;
  sp.numStallReasons = n;
  sp.stallReasonIndex = idx;
  sp.stallReasons = names;
  cuptiPCSamplingGetStallReasons(&sp);

  std::vector<CUpti_PCSamplingConfigurationInfo> cfg(5);
  for (auto &c : cfg) c = {};
  cfg[0].attributeType = CUPTI_PC_SAMPLING_CONFIGURATION_ATTR_TYPE_STALL_REASON;
  cfg[0].attributeData.stallReasonData.stallReasonCount = n;
  cfg[0].attributeData.stallReasonData.pStallReasonIndex = idx;
  cfg[1].attributeType = CUPTI_PC_SAMPLING_CONFIGURATION_ATTR_TYPE_COLLECTION_MODE;
  cfg[1].attributeData.collectionModeData.collectionMode =
      CUPTI_PC_SAMPLING_COLLECTION_MODE_KERNEL_SERIALIZED;
  cfg[2].attributeType = CUPTI_PC_SAMPLING_CONFIGURATION_ATTR_TYPE_ENABLE_START_STOP_CONTROL;
  cfg[2].attributeData.enableStartStopControlData.enableStartStopControl = 1;
  g_cfgBuf = alloc_pcdata(1024, n);
  cfg[3].attributeType = CUPTI_PC_SAMPLING_CONFIGURATION_ATTR_TYPE_SAMPLING_DATA_BUFFER;
  cfg[3].attributeData.samplingDataBufferData.samplingDataBuffer = &g_cfgBuf;
  cfg[4].attributeType = CUPTI_PC_SAMPLING_CONFIGURATION_ATTR_TYPE_SCRATCH_BUFFER_SIZE;
  cfg[4].attributeData.scratchBufferSizeData.scratchBufferSize = 16u << 20;
  CUpti_PCSamplingConfigurationInfoParams cp{};
  cp.size = CUpti_PCSamplingConfigurationInfoParamsSize;
  cp.ctx = ctx;
  cp.numAttributes = cfg.size();
  cp.pPCSamplingConfigurationInfo = cfg.data();
  r = cuptiPCSamplingSetConfigurationAttribute(&cp);
  logj("cfg", "\"rc\":%d,\"err\":\"%s\"", r, cerr(r));
  g_outBuf = alloc_pcdata(4096, n);
  return r == CUPTI_SUCCESS;
}

static uint64_t pcs_drain(CUcontext ctx, int *err) {
  uint64_t samples = 0;
  for (int g = 0; g < 1000; g++) {
    g_outBuf.totalNumPcs = g_outBuf.remainingNumPcs = 0;
    CUpti_PCSamplingGetDataParams gp{};
    gp.size = CUpti_PCSamplingGetDataParamsSize;
    gp.ctx = ctx;
    gp.pcSamplingData = &g_outBuf;
    CUptiResult r = cuptiPCSamplingGetData(&gp);
    if (r != CUPTI_SUCCESS) { *err = r; return samples; }
    for (size_t i = 0; i < g_outBuf.totalNumPcs; i++)
      for (size_t j = 0; j < g_outBuf.pPcData[i].stallReasonCount; j++)
        samples += g_outBuf.pPcData[i].stallReason[j].samples;
    if (g_outBuf.remainingNumPcs == 0) break;
  }
  return samples;
}

// ------------------------------------------------------------ test: release

struct Opts {
  const char *test = "release";
  double duration_s = 8;
  uint64_t start_at_ns = 0;
  double release_at_ms = 2000;
  double idle_at_ms = -1;
  bool no_stop = false;
  bool sync = false;
  double kernel_us = 2000;
  double flush_ms = 100;
};

static Opts o;
static CUcontext g_ctx;
static std::mutex g_cupti;            // serializes PC sampling calls, like parcagpu
static std::atomic<bool> g_released{false};
static uint64_t g_t0;

static void *control_main(void *) {
  // Block every signal so the app's signal handling is unaffected.
  sigset_t all;
  sigfillset(&all);
  pthread_sigmask(SIG_BLOCK, &all, nullptr);
  pthread_setname_np(pthread_self(), "procctl-ctl");

  uint64_t at = g_t0 + (uint64_t)(o.release_at_ms * 1e6);
  while (now_ns() < at) usleep(1000);

  uint64_t l0 = now_ns();
  std::lock_guard<std::mutex> l(g_cupti);
  uint64_t lock_ns = now_ns() - l0;
  CUresult cr = cuCtxSetCurrent(g_ctx);
  if (o.sync) {
    uint64_t s0 = now_ns();
    CUresult sr = cuCtxSynchronize();
    logj("ctl_sync", "\"rc\":%d,\"lat_ns\":%llu", sr, (unsigned long long)(now_ns() - s0));
  }
  CUptiResult rs = CUPTI_SUCCESS;
  uint64_t stop_ns = 0;
  if (!o.no_stop) {
    uint64_t s0 = now_ns();
    CUpti_PCSamplingStopParams sp{};
    sp.size = CUpti_PCSamplingStopParamsSize;
    sp.ctx = g_ctx;
    rs = cuptiPCSamplingStop(&sp);
    stop_ns = now_ns() - s0;
  }
  int derr = 0;
  uint64_t drained = pcs_drain(g_ctx, &derr);
  uint64_t d0 = now_ns();
  CUpti_PCSamplingDisableParams dp{};
  dp.size = CUpti_PCSamplingDisableParamsSize;
  dp.ctx = g_ctx;
  CUptiResult rd = cuptiPCSamplingDisable(&dp);
  uint64_t dis_ns = now_ns() - d0;
  g_released = true;
  logj("ctl_release",
       "\"ctx_set_rc\":%d,\"lock_wait_ns\":%llu,\"stop_rc\":%d,\"stop_err\":\"%s\","
       "\"stop_ns\":%llu,\"drain_samples\":%llu,\"drain_err\":%d,\"disable_rc\":%d,"
       "\"disable_err\":\"%s\",\"disable_ns\":%llu,\"at_ms\":%.1f",
       cr, (unsigned long long)lock_ns, rs, cerr(rs), (unsigned long long)stop_ns,
       (unsigned long long)drained, derr, rd, cerr(rd), (unsigned long long)dis_ns,
       (now_ns() - g_t0) / 1e6);
  return nullptr;
}

static int test_release() {
  long long cycles;
  int clockKHz = 0;
  cudaDeviceGetAttribute(&clockKHz, cudaDevAttrClockRate, 0);
  cycles = (long long)(o.kernel_us * clockKHz / 1000.0);
  int sms = 0;
  cudaDeviceGetAttribute(&sms, cudaDevAttrMultiProcessorCount, 0);
  int *sink;
  cudaMalloc(&sink, sizeof(int));
  cudaEvent_t e0, e1;
  cudaEventCreate(&e0);
  cudaEventCreate(&e1);
  spin_k0<<<sms, 256>>>(1000, sink);
  cudaDeviceSynchronize();
  logj("start", "\"test\":\"release\",\"release_at_ms\":%.1f,\"idle_at_ms\":%.1f,"
                "\"no_stop\":%d,\"sync\":%d", o.release_at_ms, o.idle_at_ms, o.no_stop, o.sync);

  g_t0 = o.start_at_ns ? o.start_at_ns : now_ns();
  while (now_ns() < g_t0) usleep(500);
  uint64_t tend = g_t0 + (uint64_t)(o.duration_s * 1e9);

  if (!pcs_enable(g_ctx)) return 1;
  cudaDeviceSynchronize();
  {
    CUpti_PCSamplingStartParams sp{};
    sp.size = CUpti_PCSamplingStartParamsSize;
    sp.ctx = g_ctx;
    CUptiResult r = cuptiPCSamplingStart(&sp);
    logj("win_start", "\"rc\":%d,\"err\":\"%s\"", r, cerr(r));
  }

  pthread_t th;
  pthread_create(&th, nullptr, control_main, nullptr);

  uint64_t launches = 0, launch_errs = 0, drained = 0, drain_errs = 0;
  uint64_t pre_l = 0, post_l = 0;
  double pre_ms = 0, post_ms = 0, max_post_ms = 0;
  bool idle_logged = false;
  uint64_t iv = now_ns();
  uint64_t iv_l = 0;
  double iv_ms = 0;
  for (;;) {
    uint64_t now = now_ns();
    if (now >= tend) break;
    if (o.idle_at_ms >= 0 && now >= g_t0 + (uint64_t)(o.idle_at_ms * 1e6)) {
      if (!idle_logged) { logj("app_idle"); idle_logged = true; }
      usleep(10000);  // no CUDA calls at all
      continue;
    }
    cudaEventRecord(e0);
    spin_k0<<<sms, 256>>>(cycles, sink);
    cudaEventRecord(e1);
    cudaError_t ce = cudaEventSynchronize(e1);
    if (ce != cudaSuccess || cudaGetLastError() != cudaSuccess) launch_errs++;
    float ms = 0;
    cudaEventElapsedTime(&ms, e0, e1);
    launches++;
    iv_l++;
    iv_ms += ms;
    if (g_released) { post_l++; post_ms += ms; if (ms > max_post_ms) max_post_ms = ms; }
    else { pre_l++; pre_ms += ms; }
    // The app thread only try-locks, so it never waits behind the control thread.
    if (!g_released && g_cupti.try_lock()) {
      if (!g_released) {
        int err = 0;
        drained += pcs_drain(g_ctx, &err);
        if (err) drain_errs++;
      }
      g_cupti.unlock();
    }
    if (now_ns() - iv > 250000000ull) {
      logj("tput", "\"launches\":%llu,\"kern_ms_mean\":%.3f,\"released\":%d",
           (unsigned long long)iv_l, iv_l ? iv_ms / iv_l : 0.0, g_released.load() ? 1 : 0);
      iv = now_ns(); iv_l = 0; iv_ms = 0;
    }
  }
  pthread_join(th, nullptr);
  logj("end",
       "\"launches\":%llu,\"launch_errs\":%llu,\"pre_release_launches\":%llu,"
       "\"pre_kern_ms\":%.3f,\"post_release_launches\":%llu,\"post_kern_ms\":%.3f,"
       "\"post_kern_ms_max\":%.3f,\"main_drained_samples\":%llu,\"main_drain_errs\":%llu",
       (unsigned long long)launches, (unsigned long long)launch_errs,
       (unsigned long long)pre_l, pre_l ? pre_ms / pre_l : 0.0, (unsigned long long)post_l,
       post_l ? post_ms / post_l : 0.0, max_post_ms, (unsigned long long)drained,
       (unsigned long long)drain_errs);
  return 0;
}

// ------------------------------------------------------------ test: flush

static void CUPTIAPI buf_requested(uint8_t **buf, size_t *size, size_t *maxRecords) {
  *size = 1 << 20;
  *buf = (uint8_t *)aligned_alloc(8, *size);
  *maxRecords = 0;
}

static std::atomic<uint64_t> g_completed{0};
static void CUPTIAPI buf_completed(CUcontext, uint32_t, uint8_t *buf, size_t, size_t valid) {
  size_t nrec = 0;
  CUpti_Activity *rec = nullptr;
  while (cuptiActivityGetNextRecord(buf, valid, &rec) == CUPTI_SUCCESS) nrec++;
  g_completed++;
  logj("buf_completed", "\"valid\":%zu,\"records\":%zu,\"at_ms\":%.1f", valid, nrec,
       (now_ns() - g_t0) / 1e6);
  free(buf);
}

static int test_flush() {
  int clockKHz = 0, sms = 0;
  cudaDeviceGetAttribute(&clockKHz, cudaDevAttrClockRate, 0);
  cudaDeviceGetAttribute(&sms, cudaDevAttrMultiProcessorCount, 0);
  long long cycles = (long long)(o.kernel_us * clockKHz / 1000.0);
  int *sink;
  cudaMalloc(&sink, sizeof(int));
  CUptiResult r = cuptiActivityRegisterCallbacks(buf_requested, buf_completed);
  logj("register", "\"rc\":%d,\"err\":\"%s\"", r, cerr(r));
  r = cuptiActivityEnable(CUPTI_ACTIVITY_KIND_CONCURRENT_KERNEL);
  logj("activity_enable", "\"rc\":%d,\"err\":\"%s\"", r, cerr(r));
  r = cuptiActivityFlushPeriod((uint32_t)o.flush_ms);
  logj("flush_period", "\"rc\":%d,\"err\":\"%s\",\"ms\":%.0f", r, cerr(r), o.flush_ms);
  g_t0 = now_ns();
  logj("start", "\"test\":\"flush\",\"busy_ms\":%.0f,\"duration_s\":%.1f", o.idle_at_ms,
       o.duration_s);
  uint64_t tend = g_t0 + (uint64_t)(o.duration_s * 1e9);
  uint64_t idle_at = g_t0 + (uint64_t)(o.idle_at_ms * 1e6);
  bool idle_logged = false;
  while (now_ns() < tend) {
    if (now_ns() >= idle_at) {
      if (!idle_logged) { logj("app_idle", "\"completed_so_far\":%llu", (unsigned long long)g_completed.load()); idle_logged = true; }
      usleep(10000);
      continue;
    }
    spin_k0<<<sms, 256>>>(cycles, sink);
    cudaDeviceSynchronize();
  }
  logj("end", "\"completed\":%llu", (unsigned long long)g_completed.load());
  cuptiActivityFlushAll(0);
  return 0;
}

int main(int argc, char **argv) {
  static struct option lo[] = {
      {"test", 1, 0, 'T'},       {"name", 1, 0, 'n'},          {"duration-s", 1, 0, 'd'},
      {"start-at-ns", 1, 0, 's'}, {"release-at-ms", 1, 0, 'r'}, {"idle-at-ms", 1, 0, 'i'},
      {"no-stop", 0, 0, 'N'},    {"sync", 0, 0, 'Y'},           {"kernel-us", 1, 0, 'u'},
      {"flush-ms", 1, 0, 'f'},   {"out", 1, 0, 'o'},            {0, 0, 0, 0}};
  int c;
  while ((c = getopt_long(argc, argv, "", lo, nullptr)) != -1) {
    switch (c) {
    case 'T': o.test = optarg; break;
    case 'n': g_name = optarg; break;
    case 'd': o.duration_s = atof(optarg); break;
    case 's': o.start_at_ns = strtoull(optarg, nullptr, 10); break;
    case 'r': o.release_at_ms = atof(optarg); break;
    case 'i': o.idle_at_ms = atof(optarg); break;
    case 'N': o.no_stop = true; break;
    case 'Y': o.sync = true; break;
    case 'u': o.kernel_us = atof(optarg); break;
    case 'f': o.flush_ms = atof(optarg); break;
    case 'o': g_out = fopen(optarg, "w"); if (!g_out) { perror(optarg); return 1; } break;
    default: return 1;
    }
  }
  cudaSetDevice(0);
  cudaFree(0);
  cuCtxGetCurrent(&g_ctx);
  if (!strcmp(o.test, "release")) return test_release();
  if (!strcmp(o.test, "flush")) return test_flush();
  fprintf(stderr, "unknown --test %s\n", o.test);
  return 1;
}
