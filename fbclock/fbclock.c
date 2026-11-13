/*
Copyright (c) Facebook, Inc. and its affiliates.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

#include "fbclock.h"
#include <assert.h> // for static_assert in C11
#include <errno.h>
#include <fcntl.h> // For O_* constants
#include <stddef.h>
#include <stdint.h>
#include <stdio.h> // for printf and perror
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <time.h>
#include <unistd.h> // close

#ifdef __aarch64__
#include <arm_acle.h>
#endif

// This file implements the seqlock, so it must see the real atomics. Defining
// FBCLOCK_CGO here would also leave the assert below comparing a type with
// itself.
#ifdef FBCLOCK_CGO
#error "FBCLOCK_CGO is only for cgo's view of the header, never for fbclock.c"
#endif

// fbclock.h hands cgo a plain uint_fast64_t in place of atomic_uint64 so it can
// convert the shm structs. That is only sound while the two lay out the same.
static_assert(
    sizeof(atomic_uint64) == sizeof(uint_fast64_t) &&
        alignof(atomic_uint64) == alignof(uint_fast64_t),
    "atomic_uint64 must match its base type's layout; see FBCLOCK_CGO in fbclock.h");

// Every writer and reader of /run/fbclock/data depends on these offsets.
static_assert(sizeof(FBCLOCK_DATA_MAGIC) == 8, "magic is 8 bytes with its NUL");
static_assert(
    sizeof(fbclock_data_header) == 64,
    "the header is one cache line");
static_assert(offsetof(fbclock_data_header, major) == 8, "major");
static_assert(offsetof(fbclock_data_header, minor) == 9, "minor");
static_assert(offsetof(fbclock_data_header, sources) == 12, "sources");
static_assert(
    sizeof(((fbclock_data_header*)0)->sources) == 4,
    "sources is 32 bits");
static_assert(
    sizeof(fbclock_data_clock) == 56,
    "a clock fits a cache line after its seq");
static_assert(
    sizeof(fbclock_data_section) == 64,
    "a section is one cache line");
static_assert(
    offsetof(fbclock_data_section, clock.ref_time_ns) == 8,
    "ref_time_ns");
static_assert(
    offsetof(fbclock_data_section, clock.sysclock_time_ns) == 16,
    "sysclock_time_ns");
static_assert(offsetof(fbclock_data_section, clock.coef_ppb) == 24, "coef_ppb");
static_assert(offsetof(fbclock_data_section, clock.clock_id) == 32, "clock_id");
static_assert(
    offsetof(fbclock_data_section, clock.error_bound_ns) == 36,
    "error_bound_ns");
static_assert(
    offsetof(fbclock_data_section, clock.ingress_time_ns) == 40,
    "ingress_time_ns");
static_assert(
    offsetof(fbclock_data_section, clock.holdover_multiplier_ns) == 48,
    "holdover");
static_assert(offsetof(fbclock_data_section, clock.reserved) == 52, "reserved");
// The file stores the source bits, and bit k is sections[k], line k + 1.
static_assert(FBCLOCK_SOURCE_PTP == 1u << 0, "PTP is bit 0");
static_assert(FBCLOCK_SOURCE_PRTC == 1u << 1, "PRTC is bit 1");
static_assert(FBCLOCK_SOURCE_NTP == 1u << 2, "NTP is bit 2");
static_assert(offsetof(fbclock_data, sections) == 64, "sections[0] is line 1");
static_assert(
    sizeof(((fbclock_data*)0)->sections) / sizeof(fbclock_data_section) ==
        8 * sizeof(((fbclock_data_header*)0)->sources),
    "one section per source bit");
static_assert(FBCLOCK_DATA_SIZE == 4096, "the file is 4096 bytes");

int fbclock_data_section_index(uint32_t source) {
  if (source == 0 || (source & (source - 1)) != 0) {
    return -1;
  }
  return __builtin_ctz(source);
}

#if defined(__GNUC__) && !defined(__OPTIMIZE__)
#define fbclock_debug_print(fmt, ...)  \
  do {                                 \
    fprintf(stderr, fmt, __VA_ARGS__); \
  } while (0)
#else
#define fbclock_debug_print(fmt, ...)
#endif

#define FBCLOCK_CLOCKDATA_SIZE sizeof(fbclock_clockdata)
#define FBCLOCK_CLOCKDATA_V2_SIZE sizeof(fbclock_clockdata_v2)
#define FBCLOCK_MAX_READ_TRIES 1000
#define NANOSECONDS_IN_SECONDS 1000000000ULL
// Smear window in seconds. v2 shared memory carries only the start, so the
// client rebuilds the end from this; keep it equal to leapDurationS in
// fbclock/daemon/daemon.go, which is what v1 publishes directly.
#define SMEAR_DURATION 62500

#ifdef __x86_64__
#define fbclock_crc64 __builtin_ia32_crc32di
#endif

#ifdef __aarch64__
#define fbclock_crc64 __crc32cd
#if defined(__GNUC__) && !defined(__clang__)
#pragma GCC target("+crc")
#endif
#endif

// dumb replacement for platforms we don't fully support
#ifndef fbclock_crc64
#define fbclock_crc64(a, b) ({ a ^ b; })
#endif

static inline uint64_t fbclock_clockdata_crc(fbclock_clockdata* value) {
  uint64_t counter = fbclock_crc64(0xFFFFFFFF, value->ingress_time_ns);
  counter = fbclock_crc64(counter, value->error_bound_ns);
  counter = fbclock_crc64(counter, value->holdover_multiplier_ns);
  return counter ^ 0xFFFFFFFF;
}

int fbclock_clockdata_store_data(uint32_t fd, fbclock_clockdata* data) {
  fbclock_shmdata* shmp = mmap(
      NULL, FBCLOCK_SHMDATA_SIZE, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
  if (shmp == MAP_FAILED) {
    return FBCLOCK_E_SHMEM_MAP_FAILED;
  }
  uint64_t crc = fbclock_clockdata_crc(data);
  memcpy(&shmp->data, data, FBCLOCK_CLOCKDATA_SIZE);
  atomic_store(&shmp->crc, crc);
  munmap(shmp, FBCLOCK_SHMDATA_SIZE);
  return FBCLOCK_E_NO_ERROR;
}

// fbclock_seq_write_begin marks the seqlock at seqp as "writing" (odd value)
// and returns the even value to publish once the write completes. A single
// writer is assumed; concurrent readers detect a torn read via the counter.
static uint64_t fbclock_seq_write_begin(atomic_uint64* _Nonnull seqp) {
  uint64_t seq = 0;
  for (int i = 0; i < FBCLOCK_MAX_READ_TRIES && (seq = atomic_load(seqp)) & 1;
       i++) {
    // LSB set means another writer is mid-write, but there should be only one
    // writer so we just wait for it to finish (make seq even)
    usleep(1);
  }
  seq = (seq & ~1) + 1;
  atomic_store(seqp, seq);
  // The store only orders what comes before it; this keeps the section's
  // writes from becoming visible ahead of the odd value.
  atomic_thread_fence(memory_order_release);
  return seq + 1;
}

// fbclock_seq_write_end publishes the post-write (even) seqlock value.
static void fbclock_seq_write_end(atomic_uint64* _Nonnull seqp, uint64_t seq) {
  if (!seq) {
    seq += 2; // avoid 0 value on wraparound
  }
  atomic_store(seqp, seq);
}

// Starts a seqlock read: the sequence to check after the copy. Odd means a
// write is in progress, 0 that the section was never written.
static uint64_t fbclock_seq_read_begin(const atomic_uint64* _Nonnull seqp) {
  return atomic_load(seqp);
}

// Ends a seqlock read: non-zero if a write overlapped the copy.
static int fbclock_seq_read_retry(
    const atomic_uint64* _Nonnull seqp,
    uint64_t seq) {
  // The load only orders what comes after it; this keeps the copy from
  // finishing after the re-check.
  atomic_thread_fence(memory_order_acquire);
  return seq != atomic_load(seqp);
}

// fbclock_section_store writes one section's data under its seqlock. A single
// writer is assumed; concurrent readers detect a torn read via the counter.
// Each section has its own seqlock on its own cache line, so writing one never
// bumps the other's counter or disturbs the other reader's cache line.
static void fbclock_section_store(
    fbclock_shmsection* _Nonnull section,
    const fbclock_clockdata_v2* _Nonnull data) {
  uint64_t seq = fbclock_seq_write_begin(&section->seq);
  section->data = *data;
  fbclock_seq_write_end(&section->seq, seq);
}

int fbclock_clockdata_store_data_v2(uint32_t fd, fbclock_clockdata_v2* data) {
  if (data == NULL) {
    return FBCLOCK_E_NO_DATA;
  }
  fbclock_shmdata_v2* shmp = mmap(
      NULL, FBCLOCK_SHMDATA_V2_SIZE, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
  if (shmp == MAP_FAILED || shmp == NULL) {
    return FBCLOCK_E_SHMEM_MAP_FAILED;
  }
  fbclock_section_store(&shmp->primary, data);
  munmap(shmp, FBCLOCK_SHMDATA_V2_SIZE);
  return FBCLOCK_E_NO_ERROR;
}

// Writes the realtime section. The daemon calls this separately from (and
// after) the primary store so the primary anchor is published without waiting
// on the slower REALTIME PHC read. Only called on hosts whose primary is
// MONOTONIC_RAW.
int fbclock_clockdata_store_data_realtime(
    uint32_t fd,
    fbclock_clockdata_v2* data) {
  if (data == NULL) {
    return FBCLOCK_E_NO_DATA;
  }
  fbclock_shmdata_v2* shmp = mmap(
      NULL, FBCLOCK_SHMDATA_V2_SIZE, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
  if (shmp == MAP_FAILED || shmp == NULL) {
    return FBCLOCK_E_SHMEM_MAP_FAILED;
  }
  fbclock_section_store(&shmp->realtime, data);
  munmap(shmp, FBCLOCK_SHMDATA_V2_SIZE);
  return FBCLOCK_E_NO_ERROR;
}

int fbclock_clockdata_load_data(
    fbclock_shmdata* shmp,
    fbclock_clockdata* data) {
  for (int i = 0; i < FBCLOCK_MAX_READ_TRIES; i++) {
    memcpy(data, &shmp->data, FBCLOCK_CLOCKDATA_SIZE);
    uint64_t crc = atomic_load(&shmp->crc);
    uint64_t our_crc = fbclock_clockdata_crc(data);
    if (our_crc == crc) {
      fbclock_debug_print("reading clock data took %d tries\n", i + 1);
      return FBCLOCK_E_NO_ERROR;
    }
  }
  fbclock_debug_print(
      "failed to read clock data after %d tries\n", FBCLOCK_MAX_READ_TRIES);
  // TODO: Enable mismatch error.
  // return FBCLOCK_E_CRC_MISMATCH;
  return FBCLOCK_E_NO_ERROR;
}

// Reads one anchor section under its seqlock, retrying on torn reads. Primary
// reads also retry uninitialized data for cold-start compatibility.
static int fbclock_section_load(
    fbclock_shmsection* section,
    fbclock_clockdata_v2* data,
    int fail_fast_if_uninitialized) {
  if (section == NULL || data == NULL) {
    return FBCLOCK_E_NO_DATA;
  }
  for (int i = 0; i < FBCLOCK_MAX_READ_TRIES; i++) {
    uint64_t seq = fbclock_seq_read_begin(&section->seq);
    if (!seq) { // 0 value means uninitialized
      if (fail_fast_if_uninitialized) {
        return FBCLOCK_E_NO_DATA;
      }
      usleep(1);
      continue;
    }
    if (seq & 1) {
      continue;
    }
    *data = section->data;
    if (!fbclock_seq_read_retry(&section->seq, seq)) {
      fbclock_debug_print("reading clock data took %d tries\n", i + 1);
      return FBCLOCK_E_NO_ERROR;
    }
  }
  fbclock_debug_print(
      "failed to read clock data after %d tries\n", FBCLOCK_MAX_READ_TRIES);
  return FBCLOCK_E_CRC_MISMATCH;
}

int fbclock_clockdata_load_data_v2(
    fbclock_shmdata_v2* shmp,
    fbclock_clockdata_v2* data) {
  if (shmp == NULL) {
    return FBCLOCK_E_NO_DATA;
  }
  return fbclock_section_load(&shmp->primary, data, 0);
}

int fbclock_clockdata_load_data_realtime(
    fbclock_shmdata_v2* shmp,
    fbclock_clockdata_v2* data) {
  if (shmp == NULL) {
    return FBCLOCK_E_NO_DATA;
  }
  return fbclock_section_load(&shmp->realtime, data, 1);
}

void fbclock_data_section_store(
    fbclock_data_section* _Nonnull section,
    const fbclock_data_clock* _Nonnull clock) {
  uint64_t seq = fbclock_seq_write_begin(&section->seq);
  section->clock = *clock;
  fbclock_seq_write_end(&section->seq, seq);
}

int fbclock_data_section_load(
    const fbclock_data_section* _Nonnull section,
    fbclock_data_clock* _Nonnull clock) {
  for (int i = 0; i < FBCLOCK_MAX_READ_TRIES; i++) {
    uint64_t seq = fbclock_seq_read_begin(&section->seq);
    if (!seq) {
      return FBCLOCK_E_NO_DATA;
    }
    if (seq & 1) {
      continue;
    }
    fbclock_data_clock copy = section->clock;
    if (!fbclock_seq_read_retry(&section->seq, seq)) {
      *clock = copy;
      return FBCLOCK_E_NO_ERROR;
    }
  }
  return FBCLOCK_E_CRC_MISMATCH;
}

enum {
  FBCLOCK_DATA_HEADER_WORDS = sizeof(fbclock_data_header) / sizeof(uint64_t)
};

// Copied as atomic 8-byte words, the magic's stored last with release and
// loaded first with acquire; minor and sources share one word.
void fbclock_data_header_store(
    fbclock_data_header* _Nonnull header,
    uint32_t sources) {
  fbclock_data_header value = {
      .major = FBCLOCK_DATA_MAJOR,
      .minor = FBCLOCK_DATA_MINOR,
      .sources = sources,
  };
  memcpy(value.magic, FBCLOCK_DATA_MAGIC, sizeof(value.magic));
  uint64_t words[FBCLOCK_DATA_HEADER_WORDS];
  memcpy(words, &value, sizeof(words));
  uint64_t* _Nonnull to = (uint64_t*)header;
  for (size_t i = 1; i < FBCLOCK_DATA_HEADER_WORDS; i++) {
    __atomic_store_n(&to[i], words[i], __ATOMIC_RELAXED);
  }
  __atomic_store_n(&to[0], words[0], __ATOMIC_RELEASE);
}

void fbclock_data_header_load(
    const fbclock_data_header* _Nonnull header,
    fbclock_data_header* _Nonnull copy) {
  const uint64_t* _Nonnull from = (const uint64_t*)header;
  uint64_t words[FBCLOCK_DATA_HEADER_WORDS];
  words[0] = __atomic_load_n(&from[0], __ATOMIC_ACQUIRE);
  for (size_t i = 1; i < FBCLOCK_DATA_HEADER_WORDS; i++) {
    words[i] = __atomic_load_n(&from[i], __ATOMIC_RELAXED);
  }
  memcpy(copy, words, sizeof(words));
}

static void fbclock_close_fds(fbclock_lib* lib) {
  if (lib == NULL) {
    return;
  }
  if (lib->shm_fd >= 0) {
    close(lib->shm_fd);
    lib->shm_fd = -1;
  }
}

static int fbclock_init_v3(
    fbclock_lib* _Nonnull lib,
    const char* shm_path,
    const fbclock_options* _Nonnull options) {
  // destroy treats a lib whose v3 init failed as v2
  lib->shm_fd = -1;
  lib->shmp_v2 = NULL;
  lib->max_wou_ns = options->max_wou_ns;
  const char* path = (shm_path != NULL) ? shm_path : FBCLOCK_DATA_PATH;
  const uint32_t sources =
      (options->sources != 0) ? options->sources : FBCLOCK_SOURCE_ANY;

  int fd = open(path, O_RDONLY, 0);
  if (fd == -1) {
    perror("open fbclock data");
    return FBCLOCK_E_SHMEM_OPEN;
  }
  struct stat st;
  if (fstat(fd, &st) != 0) {
    perror("fstat fbclock data");
    close(fd);
    return FBCLOCK_E_SHMEM_OPEN;
  }
  // Still empty while the daemon creates it: reading it would SIGBUS.
  if (st.st_size < (off_t)FBCLOCK_DATA_SIZE) {
    fprintf(
        stderr,
        "fbclock: %s is %lld bytes, v3 needs %zu\n",
        path,
        (long long)st.st_size,
        (size_t)FBCLOCK_DATA_SIZE);
    close(fd);
    return FBCLOCK_E_SHMEM_OPEN;
  }
  fbclock_data* data =
      mmap(NULL, FBCLOCK_DATA_SIZE, PROT_READ, MAP_SHARED, fd, 0);
  close(fd);
  if (data == MAP_FAILED || data == NULL) {
    return FBCLOCK_E_SHMEM_MAP_FAILED;
  }

  fbclock_data_header header;
  fbclock_data_header_load(&data->header, &header);
  const uint32_t both = sources & header.sources;
  int rcode = FBCLOCK_E_NO_ERROR;
  if (memcmp(header.magic, FBCLOCK_DATA_MAGIC, sizeof(header.magic)) != 0) {
    rcode = FBCLOCK_E_NO_DATA; // the daemon is still starting
  } else if (header.major != FBCLOCK_DATA_MAJOR) {
    rcode = FBCLOCK_E_NOTSUP;
  } else if (both & FBCLOCK_SOURCE_PTP) {
    lib->section_v3 = fbclock_data_section_index(FBCLOCK_SOURCE_PTP);
  } else if (both & FBCLOCK_SOURCE_NTP) {
    lib->section_v3 = fbclock_data_section_index(FBCLOCK_SOURCE_NTP);
  } else {
    rcode = FBCLOCK_E_NOTSUP;
  }
  if (rcode != FBCLOCK_E_NO_ERROR) {
    munmap(data, FBCLOCK_DATA_SIZE);
    return rcode;
  }
  lib->shmp_v3 = data;
  return FBCLOCK_E_NO_ERROR;
}

int fbclock_init_with_options(
    fbclock_lib* _Nonnull lib,
    const char* shm_path,
    const fbclock_options* options) {
  lib->shmp_v3 = NULL;
  if (options != NULL && options->version == FBCLOCK_V3) {
    return fbclock_init_v3(lib, shm_path, options);
  }
  // Where /run/fbclock isn't mounted, read the same file under its old name.
  if (shm_path != NULL && strcmp(shm_path, FBCLOCK_PATH) == 0 &&
      access(shm_path, F_OK) != 0) {
    shm_path = FBCLOCK_LEGACY_PATH;
  }
  lib->shm_fd = -1;
  lib->shmp_v2 = NULL;
  lib->max_wou_ns =
      (options != NULL) ? options->max_wou_ns : FBCLOCK_MAX_WOU_NS_UNSET;
  const uint32_t sources = (options != NULL && options->sources != 0)
      ? options->sources
      : FBCLOCK_SOURCE_PTP;
  uint32_t host_source = FBCLOCK_SOURCE_PTP;
  if (access(FBCLOCK_PTPPATH, F_OK) != 0) {
    // only "not found" means no PTP device; any other error leaves it unknown
    if (errno != ENOENT) {
      perror("checking " FBCLOCK_PTPPATH);
      return FBCLOCK_E_PTP_OPEN;
    }
    host_source = FBCLOCK_SOURCE_NTP;
  }
  // NOTSUP unless the caller accepts what this host serves (see the
  // FBCLOCK_SOURCE_* flags)
  if (!(sources & host_source)) {
    return FBCLOCK_E_NOTSUP;
  }

  int sfd = open(shm_path, O_RDONLY, 0);
  if (sfd == -1) {
    perror("open shmem device");
    return FBCLOCK_E_SHMEM_OPEN;
  }
  lib->shm_fd = sfd;

  struct stat st;
  if (fstat(lib->shm_fd, &st) != 0) {
    perror("fstat shmem device");
    fbclock_close_fds(lib);
    return FBCLOCK_E_SHMEM_OPEN;
  }
  // v1 or still-empty segment: reading it as v2 would SIGBUS.
  if (st.st_size < (off_t)FBCLOCK_SHMDATA_V2_SIZE) {
    fprintf(
        stderr,
        "fbclock: %s is %lld bytes, v2 needs %zu\n",
        shm_path,
        (long long)st.st_size,
        (size_t)FBCLOCK_SHMDATA_V2_SIZE);
    fbclock_close_fds(lib);
    return FBCLOCK_E_SHMEM_OPEN;
  }

  fbclock_shmdata_v2* shmp = mmap(
      NULL, FBCLOCK_SHMDATA_V2_SIZE, PROT_READ, MAP_SHARED, lib->shm_fd, 0);
  if (shmp == MAP_FAILED) {
    fbclock_close_fds(lib);
    return FBCLOCK_E_SHMEM_MAP_FAILED;
  }
  lib->shmp_v2 = shmp;
  return FBCLOCK_E_NO_ERROR;
}

int fbclock_init(fbclock_lib* lib, const char* shm_path) {
  return fbclock_init_with_options(lib, shm_path, NULL);
}

int fbclock_is_ptp_host(void) {
  return access(FBCLOCK_PTPPATH, F_OK) == 0 ? 1 : 0;
}

int fbclock_destroy(fbclock_lib* _Nonnull lib) {
  if (lib->shmp_v3 != NULL) {
    munmap(lib->shmp_v3, FBCLOCK_DATA_SIZE);
    return FBCLOCK_E_NO_ERROR;
  }
  munmap(lib->shmp_v2, FBCLOCK_SHMDATA_V2_SIZE);
  fbclock_close_fds(lib);
  return FBCLOCK_E_NO_ERROR;
  // we don't want to unlink it, others might still use it
}

uint64_t fbclock_window_of_uncertainty(
    double seconds,
    uint64_t error_bound_ns,
    double holdover_multiplier_ns) {
  uint64_t h = (uint64_t)(holdover_multiplier_ns * seconds);
  uint64_t w = error_bound_ns + h;
  fbclock_debug_print("error_bound=%lu\n", error_bound_ns);
  fbclock_debug_print("holdover_multiplier=%f\n", holdover_multiplier_ns);
  fbclock_debug_print("%.3f seconds holdover, h=%lu\n", seconds, h);
  fbclock_debug_print("w = %lu ns\n", w);
  fbclock_debug_print("w = %lu ms\n", w / 1000000);
  return w;
}

uint64_t fbclock_truetime_midpoint_ns(
    const fbclock_truetime* _Nonnull truetime) {
  return truetime->earliest_ns +
      (truetime->latest_ns - truetime->earliest_ns) / 2;
}

int fbclock_check_max_wou(
    uint64_t max_wou_ns,
    const fbclock_truetime* truetime) {
  if (max_wou_ns != FBCLOCK_MAX_WOU_NS_UNSET &&
      (truetime->latest_ns - truetime->earliest_ns) > max_wou_ns) {
    return FBCLOCK_E_WOU_TOO_BIG;
  }
  return FBCLOCK_E_NO_ERROR;
}

int fbclock_calculate_time_v2(
    uint64_t error_bound_ns,
    double h_value_ns,
    fbclock_clockdata_v2* state,
    int64_t sysclock_time_now_ns,
    fbclock_truetime* truetime,
    int time_standard) {
  int64_t phc_time_ns = state->phc_time_ns;
  if (state->ingress_time_ns > phc_time_ns) {
    return FBCLOCK_E_PHC_IN_THE_PAST;
  }
  int64_t diff_ns = sysclock_time_now_ns - state->sysclock_time_ns;
  // Refuse to extrapolate past a sane bound, or backwards in time. Either case
  // means the daemon stopped updating shared memory or there's a clock-source
  // mismatch, so the extrapolated PHC time would have unbounded error.
  if (diff_ns < 0 || diff_ns > FBCLOCK_MAX_EXTRAPOLATION_NS) {
    return FBCLOCK_E_DATA_STALE;
  }
  // NANOSECONDS_IN_SECONDS is uint64_t, which would otherwise convert the
  // signed dividend to unsigned. When coef_ppb is negative the product is
  // negative, and that conversion turns it into a value near 2^64, making
  // the quotient ~2^64/1e9 ≈ 18.45 s instead of the intended small ns count.
  phc_time_ns +=
      diff_ns + diff_ns * state->coef_ppb / (int64_t)NANOSECONDS_IN_SECONDS;

  // Time since the last SYNC from the GM, at the PHC instant this call
  // reports. Clamped like the past_v2 path below: a coef_ppb under -1e9
  // extrapolates back past ingress, and fbclock_window_of_uncertainty casts
  // the holdover product to uint64_t.
  int64_t holdover_ns = phc_time_ns - state->ingress_time_ns;
  double seconds =
      holdover_ns > 0 ? (double)holdover_ns / NANOSECONDS_IN_SECONDS : 0.0;

  // UTC offset applied if time standard used is UTC (and not TAI)
  if (time_standard == FBCLOCK_UTC) {
    phc_time_ns = fbclock_apply_utc_offset_v2(state, phc_time_ns);
  }

  // calculate the Window of Uncertainty (WOU) (in nanoseconds)
  uint64_t wou_ns =
      fbclock_window_of_uncertainty(seconds, error_bound_ns, h_value_ns);
  truetime->earliest_ns = phc_time_ns - wou_ns;
  truetime->latest_ns = phc_time_ns + wou_ns;
  return FBCLOCK_E_NO_ERROR;
}

// Like fbclock_calculate_time_v2 but for the REALTIME-domain anchor section,
// with a caller-provided ts (CLOCK_REALTIME ns). The caller's ts is typically
// in the past (e.g. a kernel SW TX timestamp captured before sendmsg), so
// diff_ns may be negative and the bound is symmetric. If the ts predates the
// last GM sync, the holdover term is dropped (seconds clamped to 0).
int fbclock_calculate_time_past_v2(
    uint64_t error_bound_ns,
    double h_value_ns,
    fbclock_clockdata_v2* _Nonnull state,
    int64_t ts_realtime_ns,
    fbclock_truetime* _Nonnull truetime,
    int time_standard) {
  int64_t phc_anchor_ns = state->phc_time_ns;

  int64_t diff_ns = ts_realtime_ns - state->sysclock_time_ns;
  if (diff_ns > FBCLOCK_MAX_EXTRAPOLATION_NS ||
      diff_ns < -FBCLOCK_MAX_EXTRAPOLATION_NS) {
    return FBCLOCK_E_DATA_STALE;
  }

  // Same signed-arithmetic care as v2: keep the divisor signed so a negative
  // (coef_ppb * diff_ns) product divides correctly.
  int64_t phc_at_ts_ns = phc_anchor_ns + diff_ns +
      diff_ns * state->coef_ppb / (int64_t)NANOSECONDS_IN_SECONDS;

  // Holdover term grows with time since last GM sync. For a past ts, measure
  // from the past PHC moment, not from "now". If the past moment predates the
  // last sync (ts older than ingress), clamp seconds to 0 so the WOU collapses
  // to error_bound_ns alone.
  double seconds = 0.0;
  if (phc_at_ts_ns > state->ingress_time_ns) {
    seconds = (double)(phc_at_ts_ns - state->ingress_time_ns) /
        NANOSECONDS_IN_SECONDS;
  }

  if (time_standard == FBCLOCK_UTC) {
    phc_at_ts_ns = fbclock_apply_utc_offset_v2(state, phc_at_ts_ns);
  }

  uint64_t wou_ns =
      fbclock_window_of_uncertainty(seconds, error_bound_ns, h_value_ns);
  truetime->earliest_ns = phc_at_ts_ns - wou_ns;
  truetime->latest_ns = phc_at_ts_ns + wou_ns;
  return FBCLOCK_E_NO_ERROR;
}

int fbclock_gettime_tz_v2(
    fbclock_lib* lib,
    fbclock_truetime* truetime,
    int time_standard) {
  fbclock_clockdata_v2 state = {};
  int rcode = fbclock_clockdata_load_data_v2(lib->shmp_v2, &state);
  if (rcode != FBCLOCK_E_NO_ERROR) {
    return rcode;
  }

  // cannot determine Truetime without these values
  if (state.error_bound_ns == 0 || state.ingress_time_ns == 0) {
    return FBCLOCK_E_NO_DATA;
  }
  if (state.phc_time_ns == 0 || state.sysclock_time_ns == 0) {
    return FBCLOCK_E_NO_DATA;
  }

  // if the value is stored as UINT32_MAX then it's too big
  if (state.error_bound_ns == UINT32_MAX ||
      state.holdover_multiplier_ns == UINT32_MAX) {
    return FBCLOCK_E_WOU_TOO_BIG;
  }

  uint64_t error_bound =
      state.error_bound_ns; // FIXME add sys clock error bound here
  double h_value = (double)state.holdover_multiplier_ns / FBCLOCK_POW2_16;

  struct timespec ts;
  if (clock_gettime(state.clockId, &ts) == -1) {
    return FBCLOCK_E_PTP_READ_OFFSET;
  }
  int64_t sysclock_time_now_ns =
      ts.tv_sec * NANOSECONDS_IN_SECONDS + ts.tv_nsec;

  // Compute the window, then enforce the caller's optional max-WOU policy.
  rcode = fbclock_calculate_time_v2(
      error_bound,
      h_value,
      &state,
      sysclock_time_now_ns,
      truetime,
      time_standard);
  if (rcode != FBCLOCK_E_NO_ERROR) {
    return rcode;
  }
  return fbclock_check_max_wou(lib->max_wou_ns, truetime);
}

static int fbclock_gettime_v3(
    fbclock_lib* _Nonnull lib,
    fbclock_truetime* _Nonnull truetime,
    int time_standard) {
  if (lib->shmp_v3 == NULL) {
    return FBCLOCK_E_NO_DATA;
  }
  fbclock_data_clock clock;
  int rcode = fbclock_data_section_load(
      &lib->shmp_v3->sections[lib->section_v3], &clock);
  if (rcode != FBCLOCK_E_NO_ERROR) {
    return rcode;
  }
  if (clock.error_bound_ns == UINT32_MAX ||
      clock.holdover_multiplier_ns == UINT32_MAX) {
    return FBCLOCK_E_WOU_TOO_BIG;
  }
  struct timespec ts;
  if (clock_gettime(clock.clock_id, &ts) == -1) {
    return FBCLOCK_E_PTP_READ_OFFSET;
  }
  int64_t sysclock_time_now_ns =
      ts.tv_sec * NANOSECONDS_IN_SECONDS + ts.tv_nsec;

  // as in fbclock_calculate_time_v2: the clock stepped back since the sample
  if (clock.ingress_time_ns > clock.ref_time_ns) {
    return FBCLOCK_E_PHC_IN_THE_PAST;
  }
  int64_t diff_ns = sysclock_time_now_ns - clock.sysclock_time_ns;
  if (diff_ns < 0 || diff_ns > FBCLOCK_MAX_EXTRAPOLATION_NS) {
    return FBCLOCK_E_DATA_STALE;
  }
  // signed divisor, as in fbclock_calculate_time_v2
  int64_t time_ns = clock.ref_time_ns + diff_ns +
      diff_ns * clock.coef_ppb / (int64_t)NANOSECONDS_IN_SECONDS;
  int64_t holdover_ns = time_ns - clock.ingress_time_ns;
  double seconds =
      holdover_ns > 0 ? (double)holdover_ns / NANOSECONDS_IN_SECONDS : 0.0;
  // The anchor is TAI, and v3 never smears.
  if (time_standard == FBCLOCK_UTC) {
    time_ns += UTC_TAI_OFFSET_NS;
  }
  uint64_t wou_ns = fbclock_window_of_uncertainty(
      seconds,
      clock.error_bound_ns,
      clock.holdover_multiplier_ns / FBCLOCK_POW2_16);
  truetime->earliest_ns = time_ns - wou_ns;
  truetime->latest_ns = time_ns + wou_ns;
  return fbclock_check_max_wou(lib->max_wou_ns, truetime);
}

int fbclock_gettime(
    fbclock_lib* _Nonnull lib,
    fbclock_truetime* _Nonnull truetime) {
  if (lib->shmp_v3 != NULL) {
    return fbclock_gettime_v3(lib, truetime, FBCLOCK_TAI);
  }
  return fbclock_gettime_tz_v2(lib, truetime, FBCLOCK_TAI);
}

int fbclock_gettime_utc(
    fbclock_lib* _Nonnull lib,
    fbclock_truetime* _Nonnull truetime) {
  if (lib->shmp_v3 != NULL) {
    return fbclock_gettime_v3(lib, truetime, FBCLOCK_UTC);
  }
  return fbclock_gettime_tz_v2(lib, truetime, FBCLOCK_UTC);
}

static int fbclock_gettime_past_tz_v2(
    fbclock_lib* _Nonnull lib,
    int64_t ts_realtime_ns,
    fbclock_truetime* _Nonnull truetime,
    int time_standard) {
  // Select the REALTIME-domain section. If the primary is already REALTIME
  // (older hosts) we use it directly; otherwise the primary is MONOTONIC_RAW
  // and the daemon publishes a dedicated REALTIME section we read instead.
  fbclock_clockdata_v2 state = {};
  if (lib->shmp_v2 == NULL) {
    return FBCLOCK_E_NO_DATA;
  }
  int rcode = fbclock_section_load(&lib->shmp_v2->primary, &state, 1);
  if (rcode != FBCLOCK_E_NO_ERROR) {
    return rcode;
  }
  if (state.clockId != CLOCK_REALTIME) {
    rcode = fbclock_clockdata_load_data_realtime(lib->shmp_v2, &state);
    if (rcode != FBCLOCK_E_NO_ERROR) {
      return rcode;
    }
  }

  // The selected section is self-contained; make sure it actually has data.
  if (state.error_bound_ns == 0 || state.ingress_time_ns == 0 ||
      state.phc_time_ns == 0 || state.sysclock_time_ns == 0) {
    return FBCLOCK_E_NO_DATA;
  }

  if (state.error_bound_ns == UINT32_MAX ||
      state.holdover_multiplier_ns == UINT32_MAX) {
    return FBCLOCK_E_WOU_TOO_BIG;
  }

  uint64_t error_bound = state.error_bound_ns;
  double h_value = (double)state.holdover_multiplier_ns / FBCLOCK_POW2_16;

  // Compute the window, then enforce the caller's optional max-WOU policy.
  rcode = fbclock_calculate_time_past_v2(
      error_bound, h_value, &state, ts_realtime_ns, truetime, time_standard);
  if (rcode != FBCLOCK_E_NO_ERROR) {
    return rcode;
  }
  return fbclock_check_max_wou(lib->max_wou_ns, truetime);
}

static int fbclock_gettime_past_v3(
    fbclock_lib* _Nonnull lib,
    int64_t ts_realtime_ns,
    fbclock_truetime* _Nonnull truetime,
    int time_standard) {
  if (lib->shmp_v3 == NULL) {
    return FBCLOCK_E_NO_DATA;
  }
  // PTP's anchor isn't always on CLOCK_REALTIME; PRTC's is.
  int k = lib->section_v3;
  if (k == fbclock_data_section_index(FBCLOCK_SOURCE_PTP)) {
    k = fbclock_data_section_index(FBCLOCK_SOURCE_PRTC);
  }
  fbclock_data_clock clock;
  int rcode = fbclock_data_section_load(&lib->shmp_v3->sections[k], &clock);
  if (rcode != FBCLOCK_E_NO_ERROR) {
    return rcode;
  }
  if (clock.error_bound_ns == UINT32_MAX ||
      clock.holdover_multiplier_ns == UINT32_MAX) {
    return FBCLOCK_E_WOU_TOO_BIG;
  }
  int64_t diff_ns = ts_realtime_ns - clock.sysclock_time_ns;
  if (diff_ns > FBCLOCK_MAX_EXTRAPOLATION_NS ||
      diff_ns < -FBCLOCK_MAX_EXTRAPOLATION_NS) {
    return FBCLOCK_E_DATA_STALE;
  }
  int64_t time_ns = clock.ref_time_ns + diff_ns +
      diff_ns * clock.coef_ppb / (int64_t)NANOSECONDS_IN_SECONDS;
  // as in fbclock_calculate_time_past_v2: no holdover for a ts before ingress
  double seconds = 0.0;
  if (time_ns > clock.ingress_time_ns) {
    seconds =
        (double)(time_ns - clock.ingress_time_ns) / NANOSECONDS_IN_SECONDS;
  }
  if (time_standard == FBCLOCK_UTC) {
    time_ns += UTC_TAI_OFFSET_NS;
  }
  uint64_t wou_ns = fbclock_window_of_uncertainty(
      seconds,
      clock.error_bound_ns,
      clock.holdover_multiplier_ns / FBCLOCK_POW2_16);
  truetime->earliest_ns = time_ns - wou_ns;
  truetime->latest_ns = time_ns + wou_ns;
  return fbclock_check_max_wou(lib->max_wou_ns, truetime);
}

int fbclock_gettime_past(
    fbclock_lib* _Nonnull lib,
    int64_t ts_realtime_ns,
    fbclock_truetime* _Nonnull truetime) {
  if (lib->shmp_v3 != NULL) {
    return fbclock_gettime_past_v3(lib, ts_realtime_ns, truetime, FBCLOCK_TAI);
  }
  return fbclock_gettime_past_tz_v2(lib, ts_realtime_ns, truetime, FBCLOCK_TAI);
}

int fbclock_gettime_past_utc(
    fbclock_lib* _Nonnull lib,
    int64_t ts_realtime_ns,
    fbclock_truetime* _Nonnull truetime) {
  if (lib->shmp_v3 != NULL) {
    return fbclock_gettime_past_v3(lib, ts_realtime_ns, truetime, FBCLOCK_UTC);
  }
  return fbclock_gettime_past_tz_v2(lib, ts_realtime_ns, truetime, FBCLOCK_UTC);
}

uint64_t fbclock_apply_smear(
    uint64_t time,
    uint64_t offset_pre_ns,
    uint64_t offset_post_ns,
    uint64_t smear_start_ns,
    uint64_t smear_end_ns,
    int multiplier) {
  if (time > smear_end_ns) {
    time -= offset_post_ns;
  } else if (time < smear_start_ns) {
    time -= offset_pre_ns;
  } else {
    // Spread the whole leap second across the window the daemon published, so
    // the ramp reaches offset_post_ns exactly at smear_end_ns. A window of N
    // seconds smears 1ns every N ns.
    uint64_t window_ns = smear_end_ns - smear_start_ns;
    uint64_t elapsed_ns = time - smear_start_ns;
    uint64_t ramp_ns = window_ns
        ? (uint64_t)(((__uint128_t)elapsed_ns * NANOSECONDS_IN_SECONDS) /
                     window_ns)
        : NANOSECONDS_IN_SECONDS;
    time -= (uint64_t)((int64_t)offset_pre_ns +
                       (int64_t)multiplier * (int64_t)ramp_ns);
  }
  return time;
}

uint64_t fbclock_apply_utc_offset_v2(
    fbclock_clockdata_v2* state,
    int64_t phctime_ns) {
  // Fixed offset is applied if tzdata information not in shared memory
  if (state->utc_offset_pre_s == 0 && state->utc_offset_post_s == 0) {
    phctime_ns += UTC_TAI_OFFSET_NS;
    return (uint64_t)phctime_ns;
  }

  fbclock_debug_print(
      "UTC-TAI Offset Before Leap Second Event: %d\n", state->utc_offset_pre_s);
  fbclock_debug_print(
      "UTC-TAI Offset After Leap Second Event: %d\n", state->utc_offset_post_s);
  fbclock_debug_print(
      "Clock Smearing Start Time (TAI): %lu\n", state->clock_smearing_start_s);
  fbclock_debug_print(
      "Clock Smearing End Time (TAI): %lu\n",
      state->clock_smearing_start_s + SMEAR_DURATION);

  // Multipler may be negative (if a negative leap second is applied)
  int multiplier = state->utc_offset_post_s - state->utc_offset_pre_s;

  // Switch to nanoseconds
  uint64_t smear_end_ns =
      (state->clock_smearing_start_s + SMEAR_DURATION) * NANOSECONDS_IN_SECONDS;
  uint64_t smear_start_ns =
      state->clock_smearing_start_s * NANOSECONDS_IN_SECONDS;
  uint64_t offset_post_ns = state->utc_offset_post_s * NANOSECONDS_IN_SECONDS;
  uint64_t offset_pre_ns = state->utc_offset_pre_s * NANOSECONDS_IN_SECONDS;

  return fbclock_apply_smear(
      phctime_ns,
      offset_pre_ns,
      offset_post_ns,
      smear_start_ns,
      smear_end_ns,
      multiplier);
}

const char* fbclock_strerror(int err_code) {
  const char* err_info;
  switch (err_code) {
    case FBCLOCK_E_SHMEM_MAP_FAILED:
      err_info = "shmem map error";
      break;
    case FBCLOCK_E_SHMEM_OPEN:
      err_info = "shmem open error";
      break;
    case FBCLOCK_E_PTP_READ_OFFSET:
      err_info = "system clock read error";
      break;
    case FBCLOCK_E_PTP_OPEN:
      err_info = "PTP device open error";
      break;
    case FBCLOCK_E_NO_DATA:
      err_info = "no data from daemon error";
      break;
    case FBCLOCK_E_WOU_TOO_BIG:
      err_info = "WOU is too big";
      break;
    case FBCLOCK_E_PHC_IN_THE_PAST:
      err_info = "PHC jumped back in time";
      break;
    case FBCLOCK_E_CRC_MISMATCH:
      err_info = "CRC check failed all tries";
      break;
    case FBCLOCK_E_DATA_STALE:
      err_info = "shared memory data too old to extrapolate";
      break;
    case FBCLOCK_E_NOTSUP:
      err_info = "this host has no time source the caller accepts";
      break;
    case FBCLOCK_E_NO_ERROR:
      err_info = "no error";
      break;
    default:
      err_info = "unknown error";
      break;
  }
  return err_info;
}
