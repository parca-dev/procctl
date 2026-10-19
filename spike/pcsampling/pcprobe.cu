// pcprobe: one process of the multi-process CUPTI PC sampling spike.
//
// Runs a busy kernel in a loop, optionally enables CUPTI PC sampling and opens
// sampling windows on a schedule anchored to a shared CLOCK_BOOTTIME start
// time, so several pcprobe processes can be choreographed against one GPU.
// Every CUPTI call's return code and latency, every window's sample yield
// (split by kernel function name), and kernel throughput are logged as JSON
// lines.
//
// Times on the command line are milliseconds relative to --start-at-ns.

#include <cuda.h>
#include <cuda_runtime.h>
#include <cupti.h>
#include <cupti_pcsampling.h>

#include <getopt.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

#include <map>
#include <string>
#include <vector>

// ---------------------------------------------------------------------------
// Kernels: distinct extern "C" names so samples can be attributed per process.
// ---------------------------------------------------------------------------

#define SPIN_KERNEL(K, A, B)                                                   \
  extern "C" __global__ void spin_k##K(long long cycles, int *sink) {          \
    long long t0 = clock64();                                                  \
    unsigned x = threadIdx.x + blockIdx.x * (K + 3);                           \
    while (clock64() - t0 < cycles) {                                          \
      x = x * A + B;                                                           \
    }                                                                          \
    if (x == 0x12345678u) *sink = (int)x;                                      \
  }
SPIN_KERNEL(0, 1664525u, 1013904223u)
SPIN_KERNEL(1, 22695477u, 1u)
SPIN_KERNEL(2, 1103515245u, 12345u)
SPIN_KERNEL(3, 134775813u, 1u)

typedef void (*spin_fn)(long long, int *);
static spin_fn kKernels[4] = {spin_k0, spin_k1, spin_k2, spin_k3};

// ---------------------------------------------------------------------------
// Logging
// ---------------------------------------------------------------------------

static FILE *g_out = stdout;
static std::string g_name = "p";
static int g_pid = 0;

static uint64_t now_ns() {
  struct timespec ts;
  clock_gettime(CLOCK_BOOTTIME, &ts);
  return (uint64_t)ts.tv_sec * 1000000000ull + ts.tv_nsec;
}

static void logj(const char *ev, const char *fmt = nullptr, ...) {
  fprintf(g_out, "{\"ev\":\"%s\",\"name\":\"%s\",\"pid\":%d,\"ts\":%llu", ev,
          g_name.c_str(), g_pid, (unsigned long long)now_ns());
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

static const char *cupti_err(CUptiResult r) {
  const char *s = nullptr;
  cuptiGetResultString(r, &s);
  return s ? s : "?";
}

#define CUDA_CHECK(x)                                                          \
  do {                                                                         \
    cudaError_t e_ = (x);                                                      \
    if (e_ != cudaSuccess) {                                                   \
      logj("fatal", "\"where\":\"%s\",\"err\":\"%s\"", #x,                     \
           cudaGetErrorString(e_));                                            \
      exit(2);                                                                 \
    }                                                                          \
  } while (0)

// ---------------------------------------------------------------------------
// Options
// ---------------------------------------------------------------------------

struct Opts {
  int kernel = 0;
  double duration_s = 10;
  uint64_t start_at_ns = 0;   // 0 = now
  double enable_at_ms = -1;   // <0 = never enable PC sampling (baseline)
  double disable_at_ms = -1;  // <0 = never disable
  double reenable_at_ms = -1; // <0 = never; else Disable then Enable again at this time
  double win_offset_ms = 0;
  double win_len_ms = 0;      // 0 = enable only, never Start; <0 = one window to the end
  double win_period_ms = 100;
  double kernel_us = 2000;
  int blocks = 0;             // 0 = SM count
  int threads = 256;
  bool continuous = false;    // collection mode (default KERNEL_SERIALIZED)
  bool set_scratch = true;
  double tput_interval_ms = 250;
};

static void usage() {
  fprintf(stderr,
          "pcprobe --name N --kernel 0..3 --duration-s S --start-at-ns T\n"
          "        [--enable-at-ms M] [--disable-at-ms M]\n"
          "        [--win-offset-ms M --win-len-ms L --win-period-ms P]\n"
          "        [--kernel-us U] [--blocks B] [--threads T]\n"
          "        [--continuous] [--no-scratch] [--out FILE]\n"
          "  --win-len-ms 0: enable only (never Start); <0: single window to end\n");
}

// ---------------------------------------------------------------------------
// CUPTI PC sampling helpers
// ---------------------------------------------------------------------------

struct PCS {
  CUcontext ctx = nullptr;
  bool enabled = false;
  size_t nStall = 0;
  uint32_t *stallIdx = nullptr;
  char **stallNames = nullptr;
  CUpti_PCSamplingData cfgBuf{};  // owned by CUPTI once configured
  CUpti_PCSamplingData outBuf{};  // passed to GetData
};

static CUpti_PCSamplingData alloc_pcdata(size_t npcs, size_t nstall) {
  CUpti_PCSamplingData d{};
  d.size = sizeof(CUpti_PCSamplingData);
  d.collectNumPcs = npcs;
  d.pPcData = (CUpti_PCSamplingPCData *)calloc(npcs, sizeof(CUpti_PCSamplingPCData));
  for (size_t i = 0; i < npcs; i++) {
    d.pPcData[i].size = sizeof(CUpti_PCSamplingPCData);
    d.pPcData[i].stallReason =
        (CUpti_PCSamplingStallReason *)calloc(nstall, sizeof(CUpti_PCSamplingStallReason));
  }
  return d;
}

static CUptiResult pcs_enable(PCS &p, const Opts &o) {
  uint64_t t0 = now_ns();
  CUpti_PCSamplingEnableParams ep{};
  ep.size = CUpti_PCSamplingEnableParamsSize;
  ep.ctx = p.ctx;
  CUptiResult r = cuptiPCSamplingEnable(&ep);
  logj("enable", "\"rc\":%d,\"err\":\"%s\",\"lat_ns\":%llu", r, cupti_err(r),
       (unsigned long long)(now_ns() - t0));
  if (r != CUPTI_SUCCESS) return r;
  p.enabled = true;

  CUpti_PCSamplingGetNumStallReasonsParams np{};
  np.size = CUpti_PCSamplingGetNumStallReasonsParamsSize;
  np.ctx = p.ctx;
  np.numStallReasons = &p.nStall;
  r = cuptiPCSamplingGetNumStallReasons(&np);
  if (r != CUPTI_SUCCESS) {
    logj("cfg", "\"step\":\"num_stall\",\"rc\":%d,\"err\":\"%s\"", r, cupti_err(r));
    return r;
  }
  p.stallIdx = (uint32_t *)calloc(p.nStall, sizeof(uint32_t));
  p.stallNames = (char **)calloc(p.nStall, sizeof(char *));
  for (size_t i = 0; i < p.nStall; i++)
    p.stallNames[i] = (char *)calloc(CUPTI_STALL_REASON_STRING_SIZE, 1);
  CUpti_PCSamplingGetStallReasonsParams sp{};
  sp.size = CUpti_PCSamplingGetStallReasonsParamsSize;
  sp.ctx = p.ctx;
  sp.numStallReasons = p.nStall;
  sp.stallReasonIndex = p.stallIdx;
  sp.stallReasons = p.stallNames;
  r = cuptiPCSamplingGetStallReasons(&sp);
  if (r != CUPTI_SUCCESS) {
    logj("cfg", "\"step\":\"stall_names\",\"rc\":%d,\"err\":\"%s\"", r, cupti_err(r));
    return r;
  }

  // Same attribute set as parcagpu, minus SAMPLING_PERIOD and
  // HARDWARE_BUFFER_SIZE which misbehave on Blackwell / GB10.
  std::vector<CUpti_PCSamplingConfigurationInfo> cfg;
  CUpti_PCSamplingConfigurationInfo a{};
  a.attributeType = CUPTI_PC_SAMPLING_CONFIGURATION_ATTR_TYPE_STALL_REASON;
  a.attributeData.stallReasonData.stallReasonCount = p.nStall;
  a.attributeData.stallReasonData.pStallReasonIndex = p.stallIdx;
  cfg.push_back(a);

  a = {};
  a.attributeType = CUPTI_PC_SAMPLING_CONFIGURATION_ATTR_TYPE_COLLECTION_MODE;
  a.attributeData.collectionModeData.collectionMode =
      o.continuous ? CUPTI_PC_SAMPLING_COLLECTION_MODE_CONTINUOUS
                   : CUPTI_PC_SAMPLING_COLLECTION_MODE_KERNEL_SERIALIZED;
  cfg.push_back(a);

  a = {};
  a.attributeType = CUPTI_PC_SAMPLING_CONFIGURATION_ATTR_TYPE_ENABLE_START_STOP_CONTROL;
  a.attributeData.enableStartStopControlData.enableStartStopControl = 1;
  cfg.push_back(a);

  p.cfgBuf = alloc_pcdata(1024, p.nStall);
  a = {};
  a.attributeType = CUPTI_PC_SAMPLING_CONFIGURATION_ATTR_TYPE_SAMPLING_DATA_BUFFER;
  a.attributeData.samplingDataBufferData.samplingDataBuffer = &p.cfgBuf;
  cfg.push_back(a);

  if (o.set_scratch) {
    a = {};
    a.attributeType = CUPTI_PC_SAMPLING_CONFIGURATION_ATTR_TYPE_SCRATCH_BUFFER_SIZE;
    a.attributeData.scratchBufferSizeData.scratchBufferSize = 16u << 20;
    cfg.push_back(a);
  }

  CUpti_PCSamplingConfigurationInfoParams cp{};
  cp.size = CUpti_PCSamplingConfigurationInfoParamsSize;
  cp.ctx = p.ctx;
  cp.numAttributes = cfg.size();
  cp.pPCSamplingConfigurationInfo = cfg.data();
  t0 = now_ns();
  r = cuptiPCSamplingSetConfigurationAttribute(&cp);
  logj("cfg", "\"step\":\"set_config\",\"rc\":%d,\"err\":\"%s\",\"lat_ns\":%llu,"
              "\"n_stall\":%zu,\"mode\":\"%s\"",
       r, cupti_err(r), (unsigned long long)(now_ns() - t0), p.nStall,
       o.continuous ? "continuous" : "serialized");
  p.outBuf = alloc_pcdata(4096, p.nStall);
  return r;
}

struct Drain {
  uint64_t calls = 0, pcs = 0, samples = 0, dropped = 0, hw_total = 0;
  int last_err = 0;
  uint64_t errs = 0;
  std::map<std::string, uint64_t> by_func;
};

static void pcs_drain(PCS &p, Drain &d) {
  for (int guard = 0; guard < 1000; guard++) {
    p.outBuf.totalNumPcs = 0;
    p.outBuf.remainingNumPcs = 0;
    CUpti_PCSamplingGetDataParams gp{};
    gp.size = CUpti_PCSamplingGetDataParamsSize;
    gp.ctx = p.ctx;
    gp.pcSamplingData = &p.outBuf;
    CUptiResult r = cuptiPCSamplingGetData(&gp);
    d.calls++;
    if (r != CUPTI_SUCCESS) {
      d.errs++;
      if (d.last_err != r)
        logj("getdata_err", "\"rc\":%d,\"err\":\"%s\"", r, cupti_err(r));
      d.last_err = r;
      return;
    }
    d.hw_total += p.outBuf.totalSamples;
    d.dropped += p.outBuf.droppedSamples;
    for (size_t i = 0; i < p.outBuf.totalNumPcs; i++) {
      CUpti_PCSamplingPCData *pc = &p.outBuf.pPcData[i];
      uint64_t n = 0;
      for (size_t j = 0; j < pc->stallReasonCount; j++) n += pc->stallReason[j].samples;
      d.pcs++;
      d.samples += n;
      d.by_func[pc->functionName ? pc->functionName : "(null)"] += n;
    }
    if (p.outBuf.remainingNumPcs == 0) return;
  }
}

static std::string by_func_json(const Drain &d) {
  std::string s = "{";
  bool first = true;
  for (auto &kv : d.by_func) {
    if (!first) s += ",";
    first = false;
    s += "\"" + kv.first + "\":" + std::to_string(kv.second);
  }
  return s + "}";
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

int main(int argc, char **argv) {
  Opts o;
  static struct option lo[] = {
      {"name", 1, 0, 'n'},          {"kernel", 1, 0, 'k'},
      {"duration-s", 1, 0, 'd'},    {"start-at-ns", 1, 0, 's'},
      {"enable-at-ms", 1, 0, 'e'},  {"disable-at-ms", 1, 0, 'x'},
      {"reenable-at-ms", 1, 0, 'R'},
      {"win-offset-ms", 1, 0, 'O'}, {"win-len-ms", 1, 0, 'L'},
      {"win-period-ms", 1, 0, 'P'}, {"kernel-us", 1, 0, 'u'},
      {"blocks", 1, 0, 'b'},        {"threads", 1, 0, 't'},
      {"continuous", 0, 0, 'c'},    {"no-scratch", 0, 0, 'S'},
      {"out", 1, 0, 'o'},           {"help", 0, 0, 'h'},
      {0, 0, 0, 0}};
  int c;
  while ((c = getopt_long(argc, argv, "", lo, nullptr)) != -1) {
    switch (c) {
    case 'n': g_name = optarg; break;
    case 'k': o.kernel = atoi(optarg) & 3; break;
    case 'd': o.duration_s = atof(optarg); break;
    case 's': o.start_at_ns = strtoull(optarg, nullptr, 10); break;
    case 'e': o.enable_at_ms = atof(optarg); break;
    case 'x': o.disable_at_ms = atof(optarg); break;
    case 'R': o.reenable_at_ms = atof(optarg); break;
    case 'O': o.win_offset_ms = atof(optarg); break;
    case 'L': o.win_len_ms = atof(optarg); break;
    case 'P': o.win_period_ms = atof(optarg); break;
    case 'u': o.kernel_us = atof(optarg); break;
    case 'b': o.blocks = atoi(optarg); break;
    case 't': o.threads = atoi(optarg); break;
    case 'c': o.continuous = true; break;
    case 'S': o.set_scratch = false; break;
    case 'o':
      g_out = fopen(optarg, "w");
      if (!g_out) { perror(optarg); return 1; }
      break;
    default: usage(); return 1;
    }
  }
  g_pid = getpid();

  // Bring up CUDA before the start barrier so init cost is outside the run.
  CUDA_CHECK(cudaSetDevice(0));
  CUDA_CHECK(cudaFree(0));
  cudaDeviceProp prop;
  CUDA_CHECK(cudaGetDeviceProperties(&prop, 0));
  int clockKHz = 0;
  cudaDeviceGetAttribute(&clockKHz, cudaDevAttrClockRate, 0);
  if (o.blocks <= 0) o.blocks = prop.multiProcessorCount;
  int drv = 0, rt = 0;
  cudaDriverGetVersion(&drv);
  cudaRuntimeGetVersion(&rt);
  uint32_t cuptiVer = 0;
  cuptiGetVersion(&cuptiVer);
  char uuid[64];
  {
    const unsigned char *u = (const unsigned char *)prop.uuid.bytes;
    snprintf(uuid, sizeof uuid,
             "%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
             u[0], u[1], u[2], u[3], u[4], u[5], u[6], u[7], u[8], u[9], u[10],
             u[11], u[12], u[13], u[14], u[15]);
  }
  long long cycles = (long long)(o.kernel_us * clockKHz / 1000.0);
  int *sink;
  CUDA_CHECK(cudaMalloc(&sink, sizeof(int)));
  cudaEvent_t ev0, ev1;
  CUDA_CHECK(cudaEventCreate(&ev0));
  CUDA_CHECK(cudaEventCreate(&ev1));

  PCS pcs;
  cuCtxGetCurrent(&pcs.ctx);

  // Warm up the kernel (module load) before the barrier.
  kKernels[o.kernel]<<<o.blocks, o.threads>>>(1000, sink);
  CUDA_CHECK(cudaDeviceSynchronize());

  logj("start",
       "\"gpu\":\"%s\",\"uuid\":\"%s\",\"cc\":\"%d.%d\",\"sms\":%d,\"clock_khz\":%d,"
       "\"driver\":%d,\"runtime\":%d,\"cupti\":%u,\"kernel\":\"spin_k%d\","
       "\"kernel_us\":%.1f,\"blocks\":%d,\"threads\":%d,\"enable_at_ms\":%.1f,"
       "\"disable_at_ms\":%.1f,\"win_offset_ms\":%.1f,\"win_len_ms\":%.1f,"
       "\"win_period_ms\":%.1f,\"mode\":\"%s\",\"duration_s\":%.1f",
       prop.name, uuid, prop.major, prop.minor, prop.multiProcessorCount, clockKHz,
       drv, rt, cuptiVer, o.kernel, o.kernel_us, o.blocks, o.threads, o.enable_at_ms,
       o.disable_at_ms, o.win_offset_ms, o.win_len_ms, o.win_period_ms,
       o.continuous ? "continuous" : "serialized", o.duration_s);

  // Start barrier.
  uint64_t t0 = o.start_at_ns ? o.start_at_ns : now_ns();
  for (uint64_t n = now_ns(); n < t0; n = now_ns()) {
    uint64_t left = t0 - n;
    if (left > 2000000) usleep((useconds_t)((left - 1000000) / 1000));
  }
  auto at = [&](double ms) { return t0 + (uint64_t)(ms * 1e6); };
  uint64_t tend = at(o.duration_s * 1000.0);

  bool want_sampling = o.enable_at_ms >= 0;
  bool enable_done = false, disable_done = false, enable_failed = false, reenable_done = false;
  bool active = false;
  uint64_t win_end = 0;
  uint64_t next_win = at(o.win_offset_ms);
  int win_no = 0;
  Drain win;      // current window
  Drain total;    // whole run
  uint64_t starts_ok = 0, starts_fail = 0, stops_ok = 0, stops_fail = 0;
  std::map<int, uint64_t> start_rc;

  // Throughput accounting.
  uint64_t iv_start = now_ns(), iv_launches = 0, iv_win_launches = 0;
  double iv_kern_ms = 0, iv_win_kern_ms = 0;
  uint64_t all_launches = 0;
  double all_kern_ms = 0;

  auto flush_tput = [&](uint64_t now) {
    double secs = (now - iv_start) / 1e9;
    logj("tput",
         "\"secs\":%.4f,\"launches\":%llu,\"lps\":%.1f,\"kern_ms_mean\":%.4f,"
         "\"win_launches\":%llu,\"win_kern_ms_mean\":%.4f,\"sampling\":%d",
         secs, (unsigned long long)iv_launches, iv_launches / (secs > 0 ? secs : 1),
         iv_launches ? iv_kern_ms / iv_launches : 0.0,
         (unsigned long long)iv_win_launches,
         iv_win_launches ? iv_win_kern_ms / iv_win_launches : 0.0, active ? 1 : 0);
    iv_start = now;
    iv_launches = iv_win_launches = 0;
    iv_kern_ms = iv_win_kern_ms = 0;
  };

  auto stop_window = [&](const char *why) {
    uint64_t s0 = now_ns();
    CUpti_PCSamplingStopParams sp{};
    sp.size = CUpti_PCSamplingStopParamsSize;
    sp.ctx = pcs.ctx;
    CUptiResult r = cuptiPCSamplingStop(&sp);
    uint64_t lat = now_ns() - s0;
    r == CUPTI_SUCCESS ? stops_ok++ : stops_fail++;
    pcs_drain(pcs, win);
    std::string bf = by_func_json(win);
    logj("win_stop",
         "\"win\":%d,\"why\":\"%s\",\"rc\":%d,\"err\":\"%s\",\"lat_ns\":%llu,"
         "\"samples\":%llu,\"pcs\":%llu,\"hw_total\":%llu,\"dropped\":%llu,"
         "\"getdata_calls\":%llu,\"getdata_errs\":%llu,\"by_func\":%s",
         win_no, why, r, cupti_err(r), (unsigned long long)lat,
         (unsigned long long)win.samples, (unsigned long long)win.pcs,
         (unsigned long long)win.hw_total, (unsigned long long)win.dropped,
         (unsigned long long)win.calls, (unsigned long long)win.errs, bf.c_str());
    total.calls += win.calls; total.pcs += win.pcs; total.samples += win.samples;
    total.dropped += win.dropped; total.hw_total += win.hw_total; total.errs += win.errs;
    for (auto &kv : win.by_func) total.by_func[kv.first] += kv.second;
    win = Drain();
    active = false;
  };

  for (;;) {
    uint64_t now = now_ns();
    if (now >= tend) break;

    if (want_sampling && !enable_done && now >= at(o.enable_at_ms)) {
      enable_done = true;
      CUDA_CHECK(cudaDeviceSynchronize());
      if (pcs_enable(pcs, o) != CUPTI_SUCCESS) enable_failed = true;
    }

    if (o.disable_at_ms >= 0 && !disable_done && now >= at(o.disable_at_ms) && pcs.enabled) {
      disable_done = true;
      if (active) stop_window("disable");
      CUDA_CHECK(cudaDeviceSynchronize());
      uint64_t s0 = now_ns();
      CUpti_PCSamplingDisableParams dp{};
      dp.size = CUpti_PCSamplingDisableParamsSize;
      dp.ctx = pcs.ctx;
      CUptiResult r = cuptiPCSamplingDisable(&dp);
      logj("disable", "\"rc\":%d,\"err\":\"%s\",\"lat_ns\":%llu", r, cupti_err(r),
           (unsigned long long)(now_ns() - s0));
      pcs.enabled = false;
    }

    if (o.reenable_at_ms >= 0 && !reenable_done && now >= at(o.reenable_at_ms)) {
      reenable_done = true;
      if (active) stop_window("reenable");
      CUDA_CHECK(cudaDeviceSynchronize());
      uint64_t s0 = now_ns();
      CUptiResult r = CUPTI_SUCCESS;
      if (pcs.enabled) {
        CUpti_PCSamplingDisableParams dp{};
        dp.size = CUpti_PCSamplingDisableParamsSize;
        dp.ctx = pcs.ctx;
        r = cuptiPCSamplingDisable(&dp);
        pcs.enabled = false;
      }
      logj("reenable_disable", "\"rc\":%d,\"err\":\"%s\",\"lat_ns\":%llu", r, cupti_err(r),
           (unsigned long long)(now_ns() - s0));
      enable_failed = pcs_enable(pcs, o) != CUPTI_SUCCESS;
      // Resume the window schedule from now.
      next_win = now_ns();
    }

    // Window scheduling.
    if (pcs.enabled && !enable_failed && o.win_len_ms != 0) {
      if (active && o.win_len_ms > 0 && now >= win_end) stop_window("end");
      if (!active) {
        // Skip windows that are already over.
        while (o.win_len_ms > 0 && now >= next_win + (uint64_t)(o.win_len_ms * 1e6))
          next_win += (uint64_t)(o.win_period_ms * 1e6);
        if (now >= next_win) {
          win_end = o.win_len_ms > 0 ? next_win + (uint64_t)(o.win_len_ms * 1e6) : tend;
          next_win = o.win_len_ms > 0 ? next_win + (uint64_t)(o.win_period_ms * 1e6) : UINT64_MAX;
          win_no++;
          CUDA_CHECK(cudaDeviceSynchronize());  // CUPTI wants an idle context before Start
          uint64_t s0 = now_ns();
          CUpti_PCSamplingStartParams sp{};
          sp.size = CUpti_PCSamplingStartParamsSize;
          sp.ctx = pcs.ctx;
          CUptiResult r = cuptiPCSamplingStart(&sp);
          uint64_t lat = now_ns() - s0;
          start_rc[(int)r]++;
          logj("win_start", "\"win\":%d,\"rc\":%d,\"err\":\"%s\",\"lat_ns\":%llu",
               win_no, r, cupti_err(r), (unsigned long long)lat);
          if (r == CUPTI_SUCCESS) {
            starts_ok++;
            active = true;
          } else {
            starts_fail++;
            // A failed open-ended window retries every period.
            if (o.win_len_ms < 0) next_win = now_ns() + (uint64_t)(o.win_period_ms * 1e6);
          }
        }
      }
    }

    // One kernel launch, timed.
    CUDA_CHECK(cudaEventRecord(ev0));
    kKernels[o.kernel]<<<o.blocks, o.threads>>>(cycles, sink);
    CUDA_CHECK(cudaEventRecord(ev1));
    CUDA_CHECK(cudaEventSynchronize(ev1));
    float ms = 0;
    cudaEventElapsedTime(&ms, ev0, ev1);
    iv_launches++; iv_kern_ms += ms; all_launches++; all_kern_ms += ms;
    if (active) { iv_win_launches++; iv_win_kern_ms += ms; }

    if (active) pcs_drain(pcs, win);

    now = now_ns();
    if (now - iv_start >= (uint64_t)(o.tput_interval_ms * 1e6)) flush_tput(now);
  }

  if (active) stop_window("run_end");
  flush_tput(now_ns());

  std::string rcs = "{";
  for (auto &kv : start_rc) {
    if (rcs.size() > 1) rcs += ",";
    rcs += "\"" + std::to_string(kv.first) + "\":" + std::to_string(kv.second);
  }
  rcs += "}";
  std::string bf = by_func_json(total);
  logj("end",
       "\"launches\":%llu,\"kern_ms_mean\":%.4f,\"starts_ok\":%llu,\"starts_fail\":%llu,"
       "\"start_rc\":%s,\"stops_ok\":%llu,\"stops_fail\":%llu,\"samples\":%llu,"
       "\"pcs\":%llu,\"dropped\":%llu,\"getdata_errs\":%llu,\"by_func\":%s,"
       "\"own_kernel\":\"spin_k%d\",\"enable_failed\":%d",
       (unsigned long long)all_launches, all_launches ? all_kern_ms / all_launches : 0.0,
       (unsigned long long)starts_ok, (unsigned long long)starts_fail, rcs.c_str(),
       (unsigned long long)stops_ok, (unsigned long long)stops_fail,
       (unsigned long long)total.samples, (unsigned long long)total.pcs,
       (unsigned long long)total.dropped, (unsigned long long)total.errs, bf.c_str(),
       o.kernel, enable_failed ? 1 : 0);

  if (pcs.enabled) {
    CUpti_PCSamplingDisableParams dp{};
    dp.size = CUpti_PCSamplingDisableParamsSize;
    dp.ctx = pcs.ctx;
    cuptiPCSamplingDisable(&dp);
  }
  return 0;
}
