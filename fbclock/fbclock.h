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

#pragma once

#include <stdint.h> /* for proper fixed width types */

// Only the Go package's cgo preambles define FBCLOCK_CGO. cgo
// builds its Go types from DWARF, and Go's debug/dwarf has no model for
// DW_TAG_atomic_type, which clang 21 emits where clang 19 folded the atomic
// into its base integer; every struct reachable from fbclock_lib then fails to
// convert. Go needs the size and field offsets of the shm structs; it must
// never touch seq or crc itself, so a plain base type serves it. fbclock.c
// static_asserts that the substitution is layout-identical. The Rust bindings
// solve the same problem with --opaque-type; see this directory's BUCK.
#if defined(FBCLOCK_CGO)
typedef uint_fast64_t atomic_uint64;
#elif defined(__cplusplus) && !defined(__clang__)
#include <atomic>
typedef std::atomic_uint_fast64_t atomic_uint64;
#else
#include <stdatomic.h>
typedef atomic_uint_fast64_t atomic_uint64;
#endif

#ifndef __cplusplus
#include <stdalign.h> /* for alignas in C; alignas is a keyword in C++ */
#endif

// _Nonnull is a Clang-only nullability qualifier. The open-source build
// compiles this header with gcc (via cgo and the fbclock-bin RPM), which
// doesn't know it, so make it a no-op there. Clang keeps the real keyword for
// nullsafeclang.
#if !defined(__clang__)
#define _Nonnull
#endif

// error codes
#define FBCLOCK_E_NO_ERROR 0
#define FBCLOCK_E_SHMEM_MAP_FAILED -1
#define FBCLOCK_E_SHMEM_OPEN -2
#define FBCLOCK_E_PTP_READ_OFFSET -3
#define FBCLOCK_E_PTP_OPEN -4
#define FBCLOCK_E_NO_DATA -5
#define FBCLOCK_E_WOU_TOO_BIG -6
#define FBCLOCK_E_PHC_IN_THE_PAST -7
#define FBCLOCK_E_CRC_MISMATCH -8
#define FBCLOCK_E_DATA_STALE -9
#define FBCLOCK_E_NOTSUP -10

// Fixed UTC-TAI offset - used when data not present in shared memory
#define UTC_TAI_OFFSET_NS (int64_t)(-37e9)
// Maximum gap (ns) between cached daemon sysclock sample and current sysclock
// before the V2 client refuses to extrapolate. Daemon updates every 10 ms; this
// is a 1000x failsafe — if the daemon is dead or the snapshot is corrupt,
// extrapolation error is unbounded so we'd rather return an error.
#define FBCLOCK_MAX_EXTRAPOLATION_NS (int64_t)(10000000000)

#ifdef __cplusplus
extern "C" {
#endif

typedef struct fbclock_clockdata {
  // PHC time when ptp client last time received sync message
  int64_t ingress_time_ns;
  // error bound calculated based on PTP client GM offset, path delay and
  // frequency adjustment
  uint32_t error_bound_ns;
  // multiplier we use to adjust error bound when clock in holdover mode
  uint32_t holdover_multiplier_ns;
  // start time (TAI) to begin smearing clock
  uint64_t clock_smearing_start_s;
  // end time (TAI) to stop smearing clock
  uint64_t clock_smearing_end_s;
  // UTC offset before latest published leap second (tzdata)
  int32_t utc_offset_pre_s;
  // UTC offset after latest published leap second (tzdata)
  int32_t utc_offset_post_s;

} fbclock_clockdata;

typedef struct fbclock_clockdata_v2 {
  // PHC time when ptp client last time received sync message
  int64_t ingress_time_ns;
  // error bound calculated based on PTP client GM offset, path delay and
  // frequency adjustment
  uint32_t error_bound_ns;
  // multiplier we use to adjust error bound when clock in holdover mode
  uint32_t holdover_multiplier_ns;
  // start time (TAI) to begin smearing clock
  uint64_t clock_smearing_start_s;
  // UTC offset before latest published leap second (tzdata)
  int16_t utc_offset_pre_s;
  // UTC offset after latest published leap second (tzdata)
  int16_t utc_offset_post_s;
  // we may have sys clock read with MONOTONIC_RAW or REALTIME clock source
  uint32_t clockId;
  // periodically updated PHC time
  int64_t phc_time_ns;
  // system clock time received during periodical update of PHC time
  int64_t sysclock_time_ns;
  // extrapolation coefficient in PPB
  int64_t coef_ppb;
} fbclock_clockdata_v2;

// fbclock shared memory object
typedef struct fbclock_shmdata {
  atomic_uint64 crc;
  fbclock_clockdata data;
} fbclock_shmdata;

// One seqlock-protected anchor section. The v2 region holds two of these on
// separate cache lines (forced by alignas) so a write to one never invalidates
// the other reader's cache line or bumps the other's counter.
typedef struct fbclock_shmsection {
  alignas(64) atomic_uint64 seq;
  fbclock_clockdata_v2 data;
} fbclock_shmsection;

// fbclock v2 shared memory object: two homogeneous anchor sections, each a
// seqlock-protected fbclock_clockdata_v2 (see fbclock_shmsection).
//  - primary: the anchor fbclock_gettime reads. MONOTONIC_RAW, or REALTIME on
//    hosts whose kernel rejects MONOTONIC_RAW in PTP_SYS_OFFSET_EXTENDED.
//  - realtime: a dedicated REALTIME-domain anchor for fbclock_gettime_past,
//    written by the daemon only when the primary is MONOTONIC_RAW. When the
//    primary is already REALTIME it is itself the realtime anchor, so
//    gettime_past uses it directly and this section stays empty.
// Each section carries its own clockId; gettime_past selects by clock domain.
typedef struct fbclock_shmdata_v2 {
  fbclock_shmsection primary;
  fbclock_shmsection realtime;
} fbclock_shmdata_v2;

#define FBCLOCK_SHMDATA_SIZE sizeof(fbclock_shmdata)
#define FBCLOCK_SHMDATA_V2_SIZE sizeof(fbclock_shmdata_v2)
// The daemon's shmem files. Consumers can bind-mount the directory, which
// exists before the files do.
#define FBCLOCK_DIR "/run/fbclock"
#define FBCLOCK_DIR_PATH "/run/fbclock/data_v2"
// The daemon still writes v1 for binaries built before v2; fbclock_init reads
// only v2.
#define FBCLOCK_DIR_PATH_V1 "/run/fbclock/data_v1"
// Hardlinks to the files above, kept for binaries built before 1 Oct 2026.
#define FBCLOCK_LEGACY_PATH "/run/fbclock_data_v2"
#define FBCLOCK_PATH_V1 "/run/fbclock_data_v1"
// Default for clients. fbclock_init* falls back to FBCLOCK_LEGACY_PATH where
// FBCLOCK_DIR isn't mounted.
#define FBCLOCK_PATH "/run/fbclock/data_v2"
#define FBCLOCK_POW2_16 ((double)(1ULL << 16))
#define FBCLOCK_PTPPATH "/dev/fbclock/ptp"

// No version in the name: an incompatible change needs a new path. Additive
// changes bump FBCLOCK_DATA_MINOR; a field added later uses 0 for "absent".
#define FBCLOCK_DATA_PATH "/run/fbclock/data"
// The file's first 8 bytes, NUL included.
#define FBCLOCK_DATA_MAGIC "FBCLOCK"
// Readers require this major.
#define FBCLOCK_DATA_MAJOR 3
// Readers check minor >= N before using a field added in minor N.
#define FBCLOCK_DATA_MINOR 0

// Source bits, the one list of what the file holds: bit k is
// fbclock_data.sections[k] (fbclock_data_section_index). The header's sources
// and fbclock_options.sources use the same bits. The file stores them, so they
// never change; a new source takes the next free bit.
//
// PTP on MONOTONIC_RAW, or REALTIME if the kernel rejects it.
#define FBCLOCK_SOURCE_PTP (1u << 0)
// PRTC, PTP on the RTC (CLOCK_REALTIME), read only by fbclock_gettime_past for
// network monitoring. If you just want the time, use FBCLOCK_SOURCE_PTP.
#define FBCLOCK_SOURCE_PRTC (1u << 1)
// NTP on REALTIME, the clock chrony steers, so coef_ppb is 0.
#define FBCLOCK_SOURCE_NTP (1u << 2)
// Callers only, never in the header: every source, including ones added later.
#define FBCLOCK_SOURCE_ANY 0xFFFFFFFFu

// Written at daemon startup: the magic last, after every section is set up.
typedef struct fbclock_data_header {
  alignas(64) uint8_t magic[8];
  uint8_t major;
  uint8_t minor;
  // sources is 32 bits, so it starts at the next multiple of 4, byte 12
  uint8_t reserved0[2];
  // FBCLOCK_SOURCE_* bits of the sections this host offers
  uint32_t sources;
  uint8_t reserved[48];
} fbclock_data_header;

// One source's clock, copied as a whole under its section's seqlock.
typedef struct fbclock_data_clock {
  // the anchor: the source's time (TAI) and the base clock at one instant
  int64_t ref_time_ns;
  int64_t sysclock_time_ns;
  // source rate against the base clock; 0 for NTP
  int64_t coef_ppb;
  // base clock of sysclock_time_ns: CLOCK_MONOTONIC_RAW or CLOCK_REALTIME
  uint32_t clock_id;
  // the bound: error_bound_ns at ingress_time_ns (TAI), then growing at
  // holdover_multiplier_ns; UINT32_MAX in either means too big to serve
  uint32_t error_bound_ns;
  int64_t ingress_time_ns;
  // ns per second, Q16.16
  uint32_t holdover_multiplier_ns;
  uint8_t reserved[12];
} fbclock_data_clock;

typedef struct fbclock_data_section {
  alignas(64) atomic_uint64 seq;
  fbclock_data_clock clock;
} fbclock_data_section;

// The whole file, 4096 bytes: the header, one section per source bit, then
// lines no bit reaches.
typedef struct fbclock_data {
  fbclock_data_header header;
  fbclock_data_section sections[32];
  uint8_t reserved[31 * 64];
} fbclock_data;

// Returns k for source bit k, its index in fbclock_data.sections, or -1 if
// source isn't exactly one bit.
int fbclock_data_section_index(uint32_t source);

#define FBCLOCK_DATA_SIZE sizeof(fbclock_data)

// supported time standards
#define FBCLOCK_TAI 0
#define FBCLOCK_UTC 1

// response to fbclock_gettime request
typedef struct fbclock_truetime {
  uint64_t earliest_ns;
  uint64_t latest_ns;
} fbclock_truetime;

// Default for fbclock_options.max_wou_ns, and what fbclock_init (or a NULL
// options) selects: no caller cap, so only fbclock's built-in limit applies
// (error_bound/holdover saturate at UINT32_MAX, ~4.29s).
#define FBCLOCK_MAX_WOU_NS_UNSET 0

// Values for fbclock_options.version.
#define FBCLOCK_VERSION_DEFAULT 0
#define FBCLOCK_V2 2
#define FBCLOCK_V3 3

// fbclock_options and fbclock_lib have no stable binary layout: build fbclock
// from the same version as its callers (from source or libfbclock.a).

// Optional configuration passed to fbclock_init_with_options.
typedef struct fbclock_options {
  // If non-zero, reject any reading whose window (latest_ns - earliest_ns)
  // exceeds this many ns with FBCLOCK_E_WOU_TOO_BIG. FBCLOCK_MAX_WOU_NS_UNSET
  // (0) keeps the default behavior.
  uint64_t max_wou_ns;
  // Sources the caller accepts; 0 means FBCLOCK_SOURCE_PTP. Only FBCLOCK_V3
  // serves NTP, with windows orders of magnitude wider than PTP's.
  uint32_t sources;
  // FBCLOCK_V3 reads v3 at shm_path (NULL: FBCLOCK_DATA_PATH); any other value
  // reads v2 as today. 0 is the library's default, v2 for now.
  uint32_t version;
} fbclock_options;

// fbclock library
typedef struct fbclock_lib {
  int shm_fd; // file descriptor of opened shared memory object
  uint64_t max_wou_ns; // caller's WOU cap (ns); 0 = none, see fbclock_options
  fbclock_shmdata_v2* shmp_v2; // mmap-ed data
  fbclock_data* shmp_v3; // the v3 mapping, NULL on a v2 lib
  int section_v3; // k: the lib reads shmp_v3->sections[k]
} fbclock_lib;

int fbclock_clockdata_store_data(uint32_t fd, fbclock_clockdata* data);
// Writes the primary anchor section.
int fbclock_clockdata_store_data_v2(uint32_t fd, fbclock_clockdata_v2* data);
// Writes the realtime anchor section, used on hosts whose primary is
// MONOTONIC_RAW. The daemon writes it separately from (and after) the primary
// section; when the primary is already REALTIME it is skipped.
int fbclock_clockdata_store_data_realtime(
    uint32_t fd,
    fbclock_clockdata_v2* data);
int fbclock_clockdata_load_data(fbclock_shmdata* shm, fbclock_clockdata* data);
int fbclock_clockdata_load_data_v2(
    fbclock_shmdata_v2* shmp,
    fbclock_clockdata_v2* data);
int fbclock_clockdata_load_data_realtime(
    fbclock_shmdata_v2* shmp,
    fbclock_clockdata_v2* data);
// One writer per section. Copies clock as given, reserved bytes included.
void fbclock_data_section_store(
    fbclock_data_section* _Nonnull section,
    const fbclock_data_clock* _Nonnull clock);
// NO_DATA if never written (seq 0); CRC_MISMATCH if every try saw a write.
// clock is written only on success.
int fbclock_data_section_load(
    const fbclock_data_section* _Nonnull section,
    fbclock_data_clock* _Nonnull clock);
// Call after every section is set up: writes the version, sources and zero
// reserved bytes, then the magic. A valid magic is never cleared.
void fbclock_data_header_store(
    fbclock_data_header* _Nonnull header,
    uint32_t sources);
// The store's read side: copies the magic, then the rest, so a copy with the
// magic set has its version. Callers check the copy's magic and major.
void fbclock_data_header_load(
    const fbclock_data_header* _Nonnull header,
    fbclock_data_header* _Nonnull copy);
uint64_t fbclock_window_of_uncertainty(
    double seconds,
    uint64_t error_bound_ns,
    double holdover_multiplier_ns);
// Returns FBCLOCK_E_WOU_TOO_BIG when max_wou_ns is set (non-zero) and the
// window (truetime->latest_ns - truetime->earliest_ns) exceeds it, else
// FBCLOCK_E_NO_ERROR. Applied by the gettime paths; public for unit testing.
int fbclock_check_max_wou(
    uint64_t max_wou_ns,
    const fbclock_truetime* truetime);
int fbclock_calculate_time_v2(
    uint64_t error_bound_ns,
    double h_value_ns,
    fbclock_clockdata_v2* state,
    int64_t sysclock_time_now_ns,
    fbclock_truetime* truetime,
    int timezone);
int fbclock_calculate_time_past_v2(
    uint64_t error_bound_ns,
    double h_value_ns,
    fbclock_clockdata_v2* _Nonnull state,
    int64_t ts_realtime_ns,
    fbclock_truetime* _Nonnull truetime,
    int timezone);
uint64_t fbclock_apply_utc_offset_v2(
    fbclock_clockdata_v2* state,
    int64_t phctime_ns);
// Spreads one leap second over [smear_start_ns, smear_end_ns] so the result is
// continuous at both ends. multiplier is the signed leap in seconds:
// (offset_post_ns - offset_pre_ns) / 1e9.
uint64_t fbclock_apply_smear(
    uint64_t time,
    uint64_t offset_pre_ns,
    uint64_t offset_post_ns,
    uint64_t smear_start_ns,
    uint64_t smear_end_ns,
    int multiplier);

// methods we provide to end users
int fbclock_init(fbclock_lib* lib, const char* shm_path);
// Like fbclock_init, but applies caller options (see fbclock_options).
// options == NULL selects defaults (identical to fbclock_init).
int fbclock_init_with_options(
    fbclock_lib* lib,
    const char* shm_path,
    const fbclock_options* options);
// Returns 1 if this host has a PTP device (FBCLOCK_PTPPATH exists), else 0.
int fbclock_is_ptp_host(void);
int fbclock_destroy(fbclock_lib* lib);
int fbclock_gettime(fbclock_lib* lib, fbclock_truetime* truetime);
int fbclock_gettime_utc(fbclock_lib* lib, fbclock_truetime* truetime);
// fbclock_gettime_past translates ts (CLOCK_REALTIME ns since epoch) into a
// TAI [earliest, latest] window from its source. ts must be from the same host
// the lib is running on, within FBCLOCK_MAX_EXTRAPOLATION_NS of the daemon's
// most recent REALTIME snapshot (in either direction). A v3 PTP lib reads PRTC.
int fbclock_gettime_past(
    fbclock_lib* _Nonnull lib,
    int64_t ts_realtime_ns,
    fbclock_truetime* _Nonnull truetime);
int fbclock_gettime_past_utc(
    fbclock_lib* _Nonnull lib,
    int64_t ts_realtime_ns,
    fbclock_truetime* _Nonnull truetime);

/*
 * fbclock_truetime_midpoint_ns: CONVENIENCE ONLY. THIS IS NOT "THE TIME".
 *
 * fbclock never returns a single instant. Every fbclock_gettime* call returns a
 * window [earliest_ns, latest_ns], the Window of Uncertainty (WOU), and
 * guarantees only (to a very high probability) that the true time lies
 * somewhere inside that closed interval. It says nothing about where inside.
 *
 * This helper returns the arithmetic center of that window,
 * earliest_ns + (latest_ns - earliest_ns) / 2. fbclock happens to build the
 * window symmetrically around its own best estimate, so the midpoint equals
 * that estimate. That does NOT make it the true time, and it does NOT make the
 * true time any likelier to sit near the midpoint than near either edge: the
 * WOU is a bound, not a probability distribution. Every point in [earliest_ns,
 * latest_ns] is equally consistent with the guarantee, so collapsing the window
 * to its midpoint discards the uncertainty and can be wrong by up to half the
 * window in either direction.
 *
 * Use this ONLY when you need one scalar timestamp for convenience (a log
 * field, or an API that takes a single number) and can tolerate an error of up
 * to half the WOU.
 *
 * If you care about correctness (ordering two events, establishing
 * happens-before, any comparison), do NOT use this. Use the full window: event
 * A provably happens-before event B only when A.latest_ns < B.earliest_ns.
 * Reason with earliest_ns / latest_ns, not this value.
 */
uint64_t fbclock_truetime_midpoint_ns(
    const fbclock_truetime* _Nonnull truetime);

// turn error code into err msg
const char* fbclock_strerror(int err_code);

#ifdef __cplusplus
} // extern "C"
#endif
