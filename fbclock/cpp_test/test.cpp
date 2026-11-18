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

#include <gtest/gtest.h>
#include <stdio.h>
#include <string.h>
#include <sys/mman.h>
#include <time.h>
#include <unistd.h>
#include <atomic>
#include <chrono>
#include <cmath>
#include <filesystem>
#include <future>
#include <iterator>
#include <string>
#include <thread>

#include "../fbclock.h"

TEST(fbclockTest, test_write_read) {
  int err;
  char* test_shm = std::tmpnam(nullptr);

  // open file, write data into it
  FILE* f = fopen(test_shm, "wb+");
  int sfd_rw = fileno(f);
  ASSERT_NE(sfd_rw, -1);

  err = ftruncate(sfd_rw, FBCLOCK_SHMDATA_SIZE);
  ASSERT_EQ(err, 0);

  fbclock_clockdata data = {
      .ingress_time_ns = 1, .error_bound_ns = 2, .holdover_multiplier_ns = 3};
  err = fbclock_clockdata_store_data(sfd_rw, &data);
  ASSERT_EQ(err, 0);

  fclose(f);

  // read data from the file
  f = fopen(test_shm, "r");
  int sfd_ro = fileno(f);
  ASSERT_NE(sfd_ro, -1);

  fbclock_shmdata* shmp = (fbclock_shmdata*)mmap(
      nullptr, FBCLOCK_SHMDATA_SIZE, PROT_READ, MAP_SHARED, sfd_ro, 0);
  ASSERT_NE(shmp, MAP_FAILED);

  fbclock_clockdata read_data;

  err = fbclock_clockdata_load_data(shmp, &read_data);
  ASSERT_EQ(err, 0);

  munmap(shmp, FBCLOCK_SHMDATA_SIZE);
  fclose(f);

  EXPECT_EQ(data.ingress_time_ns, read_data.ingress_time_ns);
  EXPECT_EQ(data.error_bound_ns, read_data.error_bound_ns);
  EXPECT_EQ(data.holdover_multiplier_ns, read_data.holdover_multiplier_ns);

  remove(test_shm);
}

int writer_thread(int sfd_rw, int tries) {
  int err;
  fbclock_clockdata data = {
      .ingress_time_ns = 1, .error_bound_ns = 2, .holdover_multiplier_ns = 3};
  for (int i = 0; i < tries; i++) {
    err = fbclock_clockdata_store_data(sfd_rw, &data);
    if (err != 0) {
      return err;
    }
    data.ingress_time_ns = data.ingress_time_ns + 1;
    if (data.ingress_time_ns > 10000) {
      data.ingress_time_ns = 1;
    }
    data.error_bound_ns = data.ingress_time_ns * 2;
    data.holdover_multiplier_ns = data.ingress_time_ns * 3;
  }
  return 0;
}

int reader_thread(fbclock_shmdata* shmp, int tries) {
  int err;
  fbclock_clockdata data;
  for (int i = 0; i < tries; i++) {
    err = fbclock_clockdata_load_data(shmp, &data);
    if (err != 0) {
      return err;
    }
    if (data.ingress_time_ns * 2 != data.error_bound_ns) {
      printf("ingress_time_ns: %lu\n", data.ingress_time_ns);
      printf("error_bound_ns: %d\n", data.error_bound_ns);
      printf("holdover_multiplier_ns: %d\n", data.holdover_multiplier_ns);
      return -1;
    }
    if (data.ingress_time_ns * 3 != data.holdover_multiplier_ns) {
      printf("ingress_time_ns: %lu\n", data.ingress_time_ns);
      printf("error_bound_ns: %d\n", data.error_bound_ns);
      printf("holdover_multiplier_ns: %d\n", data.holdover_multiplier_ns);
      return -1;
    }
  }
  return 0;
}

TEST(fbclockTest, test_concurrent) {
  int err;
  char* test_shm = std::tmpnam(nullptr);

  // open file, write data into it
  FILE* f_rw = fopen(test_shm, "wb+");
  int sfd_rw = fileno(f_rw);
  ASSERT_NE(sfd_rw, -1);

  err = ftruncate(sfd_rw, FBCLOCK_SHMDATA_SIZE);
  ASSERT_EQ(err, 0);

  // read data from the file
  FILE* f_ro = fopen(test_shm, "r");
  int sfd_ro = fileno(f_ro);
  ASSERT_NE(sfd_ro, -1);

  fbclock_shmdata* shmp = (fbclock_shmdata*)mmap(
      nullptr, FBCLOCK_SHMDATA_SIZE, PROT_READ, MAP_SHARED, sfd_ro, 0);
  ASSERT_NE(shmp, MAP_FAILED);

  int tries = 10000;

  // spawn two functions asynchronously, make sure there is no inconsistent data
  auto future_writer =
      std::async(std::launch::async, writer_thread, sfd_rw, tries);
  auto future_reader =
      std::async(std::launch::async, reader_thread, shmp, tries);
  err = future_writer.get();
  ASSERT_EQ(err, 0);
  err = future_reader.get();
  ASSERT_EQ(err, 0);
  munmap(shmp, FBCLOCK_SHMDATA_SIZE);
  remove(test_shm);
}

int writer_thread_v2(int sfd_rw, int tries) {
  int err;
  fbclock_clockdata_v2 data = {
      .ingress_time_ns = 1,
      .error_bound_ns = 2,
      .holdover_multiplier_ns = 3,
      .clockId = CLOCK_MONOTONIC_RAW,
      .phc_time_ns = 1748164346441310791,
      .sysclock_time_ns = 1748164309441310791,
  };
  for (int i = 0; i < tries; i++) {
    err = fbclock_clockdata_store_data_v2(sfd_rw, &data, nullptr);
    if (err != 0) {
      return err;
    }
    data.ingress_time_ns = data.ingress_time_ns + 1000;
    if (data.ingress_time_ns > 10000) {
      data.ingress_time_ns = 1;
    }
    data.error_bound_ns = data.ingress_time_ns * 2;
    data.holdover_multiplier_ns = data.ingress_time_ns * 3;
    data.phc_time_ns += 10000;
    data.sysclock_time_ns += 10000;
    usleep(10000); // sleep 10ms, more often than the daemon's 100ms
  }
  return 0;
}

int reader_thread_v2(fbclock_shmdata_v2* shmp, int tries) {
  int err;
  fbclock_clockdata_v2 data;
  for (int i = 0; i < tries; i++) {
    err = fbclock_clockdata_load_data_v2(shmp, &data);
    if (err != 0) {
      printf("load v2 data failed: %d\n", err);
      return err;
    }
    if (data.ingress_time_ns * 2 != data.error_bound_ns) {
      printf("ingress_time_ns: %lu\n", data.ingress_time_ns);
      printf("error_bound_ns: %d\n", data.error_bound_ns);
      printf("holdover_multiplier_ns: %d\n", data.holdover_multiplier_ns);
      return -1;
    }
    if (data.ingress_time_ns * 3 != data.holdover_multiplier_ns) {
      printf("ingress_time_ns: %lu\n", data.ingress_time_ns);
      printf("error_bound_ns: %d\n", data.error_bound_ns);
      printf("holdover_multiplier_ns: %d\n", data.holdover_multiplier_ns);
      return -2;
    }
    if ((data.phc_time_ns - data.sysclock_time_ns) != 37000000000) {
      printf("phc_time_ns: %lu\n", data.phc_time_ns);
      printf("sysclock_time_ns: %lu\n", data.sysclock_time_ns);
      return -3;
    }
  }
  return 0;
}

TEST(fbclockTest, test_concurrent_v2) {
  int err;
  char* test_shm = std::tmpnam(nullptr);

  // open file, write data into it
  FILE* f_rw = fopen(test_shm, "wb+");
  int sfd_rw = fileno(f_rw);
  ASSERT_NE(sfd_rw, -1);

  err = ftruncate(sfd_rw, FBCLOCK_SHMDATA_V2_SIZE);
  ASSERT_EQ(err, 0);

  // read data from the file
  FILE* f_ro = fopen(test_shm, "r");
  int sfd_ro = fileno(f_ro);
  ASSERT_NE(sfd_ro, -1);

  fbclock_shmdata_v2* shmp = (fbclock_shmdata_v2*)mmap(
      nullptr, FBCLOCK_SHMDATA_V2_SIZE, PROT_READ, MAP_SHARED, sfd_ro, 0);
  ASSERT_NE(shmp, MAP_FAILED);

  int tries = 1000;

  // spawn two functions asynchronously, make sure there is no inconsistent data
  auto future_writer =
      std::async(std::launch::async, writer_thread_v2, sfd_rw, tries);
  auto future_reader =
      std::async(std::launch::async, reader_thread_v2, shmp, tries * 10);
  err = future_writer.get();
  ASSERT_EQ(err, 0);
  err = future_reader.get();
  ASSERT_EQ(err, 0);
  munmap(shmp, FBCLOCK_SHMDATA_V2_SIZE);
  remove(test_shm);
}

TEST(fbclockTest, test_uninitialized_realtime_section_returns_no_data) {
  fbclock_shmdata_v2 shmp = {};
  atomic_store(&shmp.realtime.seq, 0);
  fbclock_clockdata_v2 data = {};

  EXPECT_EQ(
      fbclock_clockdata_load_data_realtime(&shmp, &data), FBCLOCK_E_NO_DATA);
}

TEST(fbclockTest, test_uninitialized_primary_section_returns_crc_mismatch) {
  fbclock_shmdata_v2 shmp = {};
  atomic_store(&shmp.primary.seq, 0);
  fbclock_clockdata_v2 data = {};

  const auto before = fbclock_get_read_stats();
  EXPECT_EQ(
      fbclock_clockdata_load_data_v2(&shmp, &data), FBCLOCK_E_CRC_MISMATCH);
  // Waiting for the daemon's first write isn't meeting a write.
  EXPECT_EQ(fbclock_get_read_stats().gave_up, before.gave_up);
}

// A write that never ends, as a writer stopped between its two seq stores.
TEST(fbclockTest, test_read_stats_count_a_read_that_gives_up) {
  fbclock_shmdata_v2 shmp = {};
  atomic_store(&shmp.primary.seq, 3);
  fbclock_clockdata_v2 data = {};

  const auto before = fbclock_get_read_stats();
  EXPECT_EQ(
      fbclock_clockdata_load_data_v2(&shmp, &data), FBCLOCK_E_CRC_MISMATCH);
  const auto after = fbclock_get_read_stats();
  EXPECT_EQ(after.retried, before.retried + 1);
  EXPECT_EQ(after.gave_up, before.gave_up + 1);
}

// Reads racing a writer that stores back to back meet a write sooner or later.
TEST(fbclockTest, test_read_stats_count_a_read_that_retries) {
  char path[] = "/tmp/fbclock_testXXXXXX";
  const int fd = mkstemp(path);
  ASSERT_GE(fd, 0);
  ASSERT_EQ(ftruncate(fd, FBCLOCK_SHMDATA_V2_SIZE), 0);
  fbclock_clockdata_v2 data = {.ingress_time_ns = 1, .error_bound_ns = 1};
  ASSERT_EQ(fbclock_clockdata_store_data_v2(fd, &data, nullptr), 0);
  auto* shmp = static_cast<fbclock_shmdata_v2*>(
      mmap(nullptr, FBCLOCK_SHMDATA_V2_SIZE, PROT_READ, MAP_SHARED, fd, 0));
  ASSERT_NE(shmp, MAP_FAILED);

  std::atomic<bool> done{false};
  std::thread writer([&] {
    fbclock_clockdata_v2 next = data;
    while (!done.load()) {
      next.ingress_time_ns++;
      fbclock_clockdata_store_data_v2(fd, &next, nullptr);
    }
  });
  // Only this thread reads, so the counts change with its own reads alone.
  const auto until =
      std::chrono::steady_clock::now() + std::chrono::seconds(10);
  fbclock_clockdata_v2 copy = {};
  bool retried_and_read = false;
  while (!retried_and_read && std::chrono::steady_clock::now() < until) {
    const auto before = fbclock_get_read_stats();
    const int rc = fbclock_clockdata_load_data_v2(shmp, &copy);
    const auto after = fbclock_get_read_stats();
    retried_and_read = rc == FBCLOCK_E_NO_ERROR &&
        after.retried == before.retried + 1 && after.gave_up == before.gave_up;
  }
  done.store(true);
  writer.join();
  EXPECT_TRUE(retried_and_read);

  munmap(shmp, FBCLOCK_SHMDATA_V2_SIZE);
  close(fd);
  remove(path);
}

TEST(fbclockTest, test_gettime_past_uninitialized_primary_returns_no_data) {
  fbclock_shmdata_v2 shmp = {};
  atomic_store(&shmp.primary.seq, 0);
  fbclock_lib lib = {};
  lib.shmp_v2 = &shmp;
  fbclock_truetime truetime = {};

  EXPECT_EQ(fbclock_gettime_past(&lib, 0, &truetime), FBCLOCK_E_NO_DATA);
}

static long open_fd_count() {
  return std::distance(
      std::filesystem::directory_iterator("/proc/self/fd"),
      std::filesystem::directory_iterator{});
}

// Every segment is v2, whatever its name, and init opens nothing else.
TEST(fbclockTest, test_init_maps_every_segment_as_v2) {
  char path[] = "/tmp/fbclock_testXXXXXX"; // no _v2 suffix
  const int fd = mkstemp(path);
  ASSERT_GE(fd, 0);
  ASSERT_EQ(ftruncate(fd, FBCLOCK_SHMDATA_V2_SIZE), 0);
  const long fds = open_fd_count();

  fbclock_lib lib = {};
  // ANY passes the source check on any host, PTP device or not
  const fbclock_options any = {.sources = FBCLOCK_SOURCE_ANY};
  ASSERT_EQ(fbclock_init_with_options(&lib, path, &any), FBCLOCK_E_NO_ERROR);
  EXPECT_NE(lib.shmp_v2, nullptr);
  // The segment's fd, and no PTP device.
  EXPECT_EQ(open_fd_count(), fds + 1);
  EXPECT_EQ(fbclock_destroy(&lib), FBCLOCK_E_NO_ERROR);

  close(fd);
  remove(path);
}

// An empty segment (the daemon is still creating it) or a v1 one is too short
// to read as v2.
TEST(fbclockTest, test_init_rejects_a_segment_shorter_than_v2) {
  char path[] = "/tmp/fbclock_testXXXXXX";
  const int fd = mkstemp(path);
  ASSERT_GE(fd, 0);
  const long fds = open_fd_count();
  // ANY passes the source check on any host, PTP device or not
  const fbclock_options any = {.sources = FBCLOCK_SOURCE_ANY};

  for (const off_t size : {(off_t)0, (off_t)FBCLOCK_SHMDATA_SIZE}) {
    ASSERT_EQ(ftruncate(fd, size), 0);
    fbclock_lib lib = {};
    testing::internal::CaptureStderr();
    EXPECT_EQ(fbclock_init_with_options(&lib, path, &any), FBCLOCK_E_SHMEM_OPEN)
        << "size " << size;
    EXPECT_NE(
        testing::internal::GetCapturedStderr().find(path), std::string::npos);
    EXPECT_EQ(lib.shmp_v2, nullptr);
    EXPECT_EQ(open_fd_count(), fds);
  }

  close(fd);
  remove(path);
}

TEST(fbclockTest, test_failed_init_leaves_fds_negative_and_destroy_safe) {
  fbclock_lib lib;
  memset(&lib, 0xFF, sizeof(lib));
  int err = fbclock_init(&lib, "/nonexistent/fbclock_shm_v2");
  ASSERT_NE(err, FBCLOCK_E_NO_ERROR);
  EXPECT_EQ(lib.shm_fd, -1);
  EXPECT_EQ(lib.shmp_v2, nullptr);
  EXPECT_EQ(fbclock_destroy(&lib), FBCLOCK_E_NO_ERROR);
}

// A host with no PTP device stays NOTSUP, segment or not, unless the caller
// accepts NTP.
TEST(fbclockTest, test_init_without_ptp_device_needs_the_ntp_source) {
  if (fbclock_is_ptp_host()) {
    GTEST_SKIP() << "needs a host without " << FBCLOCK_PTPPATH;
  }
  char path[] = "/tmp/fbclock_testXXXXXX";
  const int fd = mkstemp(path);
  ASSERT_GE(fd, 0);
  ASSERT_EQ(ftruncate(fd, FBCLOCK_SHMDATA_V2_SIZE), 0);

  fbclock_lib lib = {};
  EXPECT_EQ(fbclock_init(&lib, path), FBCLOCK_E_NOTSUP);
  const fbclock_options unset = {};
  EXPECT_EQ(fbclock_init_with_options(&lib, path, &unset), FBCLOCK_E_NOTSUP);
  const fbclock_options ptp = {.sources = FBCLOCK_SOURCE_PTP};
  EXPECT_EQ(fbclock_init_with_options(&lib, path, &ptp), FBCLOCK_E_NOTSUP);

  const fbclock_options ntp = {.sources = FBCLOCK_SOURCE_NTP};
  // no segment yet: retryable, as on a PTP host before the daemon's first write
  const std::string missing = std::string(path) + "_missing";
  EXPECT_EQ(
      fbclock_init_with_options(&lib, missing.c_str(), &ntp),
      FBCLOCK_E_SHMEM_OPEN);
  ASSERT_EQ(fbclock_init_with_options(&lib, path, &ntp), FBCLOCK_E_NO_ERROR);
  EXPECT_NE(lib.shmp_v2, nullptr);
  EXPECT_EQ(fbclock_destroy(&lib), FBCLOCK_E_NO_ERROR);

  const fbclock_options any = {.sources = FBCLOCK_SOURCE_ANY};
  ASSERT_EQ(fbclock_init_with_options(&lib, path, &any), FBCLOCK_E_NO_ERROR);
  EXPECT_EQ(fbclock_destroy(&lib), FBCLOCK_E_NO_ERROR);

  close(fd);
  remove(path);
}

TEST(fbclockTest, test_init_ntp_source_reads_the_chrony_record) {
  if (fbclock_is_ptp_host()) {
    GTEST_SKIP() << "needs a host without " << FBCLOCK_PTPPATH;
  }
  char path[] = "/tmp/fbclock_testXXXXXX";
  const int sfd_rw = mkstemp(path);
  ASSERT_GE(sfd_rw, 0);
  ASSERT_EQ(ftruncate(sfd_rw, FBCLOCK_SHMDATA_V2_SIZE), 0);

  // A chrony-mode record: the anchor is the system clock in TAI and the base
  // is CLOCK_REALTIME, so a reading is the system clock in TAI.
  struct timespec ts = {};
  ASSERT_EQ(clock_gettime(CLOCK_REALTIME, &ts), 0);
  const int64_t realtime_ns = ts.tv_sec * 1000000000LL + ts.tv_nsec;
  const uint32_t error_bound_ns = 100000;
  const uint32_t holdover_ns_per_s = 50000; // 50 ppm
  fbclock_clockdata_v2 data = {
      .ingress_time_ns = realtime_ns - UTC_TAI_OFFSET_NS,
      .error_bound_ns = error_bound_ns,
      .holdover_multiplier_ns = (uint32_t)(holdover_ns_per_s * FBCLOCK_POW2_16),
      .utc_offset_pre_s = 37,
      .utc_offset_post_s = 37,
      .clockId = CLOCK_REALTIME,
      .phc_time_ns = realtime_ns - UTC_TAI_OFFSET_NS,
      .sysclock_time_ns = realtime_ns,
      .coef_ppb = 0,
  };
  ASSERT_EQ(fbclock_clockdata_store_data_v2(sfd_rw, &data, nullptr), 0);

  fbclock_lib lib = {};
  const fbclock_options options = {.sources = FBCLOCK_SOURCE_NTP};
  ASSERT_EQ(
      fbclock_init_with_options(&lib, path, &options), FBCLOCK_E_NO_ERROR);

  fbclock_truetime tt = {};
  ASSERT_EQ(clock_gettime(CLOCK_REALTIME, &ts), 0);
  const int64_t before_ns = ts.tv_sec * 1000000000LL + ts.tv_nsec;
  ASSERT_EQ(fbclock_gettime(&lib, &tt), FBCLOCK_E_NO_ERROR);
  ASSERT_EQ(clock_gettime(CLOCK_REALTIME, &ts), 0);
  const int64_t after_ns = ts.tv_sec * 1000000000LL + ts.tv_nsec;

  EXPECT_GE(
      fbclock_truetime_midpoint_ns(&tt),
      (uint64_t)(before_ns - UTC_TAI_OFFSET_NS));
  EXPECT_LE(
      fbclock_truetime_midpoint_ns(&tt),
      (uint64_t)(after_ns - UTC_TAI_OFFSET_NS));
  // Holdover runs from the record's time to the library's clock read, which is
  // no later than after_ns; +1 for the library's rounding.
  const uint64_t max_holdover_ns =
      holdover_ns_per_s * (after_ns - realtime_ns) / 1000000000LL + 1;
  const uint64_t window = tt.latest_ns - tt.earliest_ns;
  EXPECT_GE(window, 2ULL * error_bound_ns);
  EXPECT_LE(window, 2 * (error_bound_ns + max_holdover_ns));

  EXPECT_EQ(fbclock_destroy(&lib), FBCLOCK_E_NO_ERROR);
  close(sfd_rw);
  remove(path);
}

// Whatever the host, it serves only its own source: a caller that accepts just
// the other one gets NOTSUP.
TEST(fbclockTest, test_init_serves_only_the_hosts_source) {
  const uint32_t host =
      fbclock_is_ptp_host() ? FBCLOCK_SOURCE_PTP : FBCLOCK_SOURCE_NTP;
  const uint32_t other =
      host == FBCLOCK_SOURCE_PTP ? FBCLOCK_SOURCE_NTP : FBCLOCK_SOURCE_PTP;
  char path[] = "/tmp/fbclock_testXXXXXX";
  const int fd = mkstemp(path);
  ASSERT_GE(fd, 0);
  ASSERT_EQ(ftruncate(fd, FBCLOCK_SHMDATA_V2_SIZE), 0);

  fbclock_lib lib = {};
  const fbclock_options others = {.sources = other};
  EXPECT_EQ(fbclock_init_with_options(&lib, path, &others), FBCLOCK_E_NOTSUP);
  const fbclock_options mine = {.sources = host};
  ASSERT_EQ(fbclock_init_with_options(&lib, path, &mine), FBCLOCK_E_NO_ERROR);
  EXPECT_EQ(fbclock_destroy(&lib), FBCLOCK_E_NO_ERROR);
  const fbclock_options any = {.sources = FBCLOCK_SOURCE_ANY};
  ASSERT_EQ(fbclock_init_with_options(&lib, path, &any), FBCLOCK_E_NO_ERROR);
  EXPECT_EQ(fbclock_destroy(&lib), FBCLOCK_E_NO_ERROR);

  close(fd);
  remove(path);
}

TEST(fbclockTest, test_window_of_uncertainty) {
  int64_t seconds = 0; // how long ago was the last SYNC
  double error_bound_ns = 172.0;
  double holdover_multiplier_ns = 50.5;

  double wou = fbclock_window_of_uncertainty(
      seconds, error_bound_ns, holdover_multiplier_ns);
  EXPECT_DOUBLE_EQ(wou, 172.0);

  seconds = 10;
  wou = fbclock_window_of_uncertainty(
      seconds, error_bound_ns, holdover_multiplier_ns);

  EXPECT_DOUBLE_EQ(wou, 677.0);
}

TEST(fbclockTest, test_fbclock_calculate_time_v2) {
  int err;
  fbclock_truetime truetime;
  fbclock_clockdata_v2 state = {
      .ingress_time_ns = 1647269091803102957,
      .clockId = CLOCK_MONOTONIC_RAW,
      // phc time is before ingress time, error
      .phc_time_ns = 1647269082943150996,
  };
  double error_bound = 172.0;
  double h_value = 50.5;

  struct timespec tp = {};
  clock_gettime(CLOCK_MONOTONIC_RAW, &tp);

  int64_t sysclock_time_ns = tp.tv_sec * 1000000000 + tp.tv_nsec;
  state.sysclock_time_ns = sysclock_time_ns;

  err = fbclock_calculate_time_v2(
      error_bound,
      h_value,
      &state,
      sysclock_time_ns + 1000, // + 1us
      &truetime,
      FBCLOCK_TAI);
  ASSERT_EQ(err, FBCLOCK_E_PHC_IN_THE_PAST);

  // phc time is after ingress time, all good
  state = {
      .ingress_time_ns = 1647269082943150996,
      .clockId = CLOCK_MONOTONIC_RAW,
      .phc_time_ns = 1647269091803102957,
      .sysclock_time_ns = sysclock_time_ns,
      .coef_ppb = 12,
  };
  err = fbclock_calculate_time_v2(
      error_bound,
      h_value,
      &state,
      sysclock_time_ns + 1000,
      &truetime,
      FBCLOCK_TAI);
  ASSERT_EQ(err, 0);

  EXPECT_EQ(truetime.earliest_ns, 1647269091803103338);
  EXPECT_EQ(truetime.latest_ns, 1647269091803104576);

  // WOU is very big — PHC has been free-running for 6h since last GM sync.
  // We keep the cached daemon snapshot fresh (sysclock advanced together with
  // phc), so we exercise the holdover term, not extrapolation.
  error_bound = 1000.0;
  int64_t sixHoursNS = 6 * 3600 * 1000000000LL;
  state.phc_time_ns += sixHoursNS;
  state.sysclock_time_ns += sixHoursNS;
  err = fbclock_calculate_time_v2(
      error_bound,
      h_value,
      &state,
      state.sysclock_time_ns + 1000,
      &truetime,
      FBCLOCK_TAI);
  ASSERT_EQ(err, 0);
  EXPECT_EQ(truetime.earliest_ns, 1647290691802011710);
  EXPECT_EQ(truetime.latest_ns, 1647290691804196204);
}

TEST(fbclockTest, test_fbclock_calculate_time_v2_stale_data) {
  fbclock_truetime truetime;
  fbclock_clockdata_v2 state = {
      .ingress_time_ns = 1647269082943150996,
      .clockId = CLOCK_MONOTONIC_RAW,
      .phc_time_ns = 1647269091803102957,
      .sysclock_time_ns = 1000000000, // 1s on the sysclock
      .coef_ppb = 0,
  };
  double error_bound = 172.0;
  double h_value = 50.5;

  // sysclock has advanced 0.5s since the cached snapshot — within bound, OK.
  int err = fbclock_calculate_time_v2(
      error_bound, h_value, &state, 1500000000, &truetime, FBCLOCK_TAI);
  EXPECT_EQ(err, FBCLOCK_E_NO_ERROR);

  // sysclock has advanced past the 1s extrapolation cap — refuse.
  err = fbclock_calculate_time_v2(
      error_bound,
      h_value,
      &state,
      1000000000 + FBCLOCK_MAX_EXTRAPOLATION_NS + 1,
      &truetime,
      FBCLOCK_TAI);
  EXPECT_EQ(err, FBCLOCK_E_DATA_STALE);

  // sysclock went backwards (clock-source mismatch or stale snapshot) — refuse.
  err = fbclock_calculate_time_v2(
      error_bound, h_value, &state, 999999999, &truetime, FBCLOCK_TAI);
  EXPECT_EQ(err, FBCLOCK_E_DATA_STALE);
}

TEST(fbclockTest, test_fbclock_calculate_time_v2_holdover_grows_while_stale) {
  // A stalled daemon freezes the anchor. The estimate still extrapolates
  // forward from it; the window has to grow from that same instant.
  fbclock_clockdata_v2 state = {
      .ingress_time_ns = 1647269082943150996,
      .clockId = CLOCK_MONOTONIC_RAW,
      .phc_time_ns = 1647269082944150996, // anchor 1 ms after ingress
      .sysclock_time_ns = 1000000000,
      .coef_ppb = 0,
  };
  double error_bound = 172.0;
  double h_value = 50.5; // ns of window per second of holdover

  fbclock_truetime fresh;
  ASSERT_EQ(
      fbclock_calculate_time_v2(
          error_bound,
          h_value,
          &state,
          state.sysclock_time_ns,
          &fresh,
          FBCLOCK_TAI),
      FBCLOCK_E_NO_ERROR);
  EXPECT_EQ(fbclock_truetime_midpoint_ns(&fresh), (uint64_t)state.phc_time_ns);
  // 1 ms of holdover contributes 0 ns, so the window is error_bound each side.
  EXPECT_EQ(fresh.latest_ns - fresh.earliest_ns, 344ULL);

  fbclock_truetime stale;
  int64_t five_seconds_ns = 5000000000LL;
  ASSERT_EQ(
      fbclock_calculate_time_v2(
          error_bound,
          h_value,
          &state,
          state.sysclock_time_ns + five_seconds_ns,
          &stale,
          FBCLOCK_TAI),
      FBCLOCK_E_NO_ERROR);
  EXPECT_EQ(
      fbclock_truetime_midpoint_ns(&stale),
      (uint64_t)(state.phc_time_ns + five_seconds_ns));
  // 5.001 s of holdover at 50.5 ns/s is 252 ns each side, on top of the 172.
  EXPECT_EQ(stale.latest_ns - stale.earliest_ns, 848ULL);
}

TEST(fbclockTest, test_fbclock_calculate_time_v2_backwards_phc_keeps_a_window) {
  // -1111111111 is what the daemon's calcCoeffPPB returns, with no error, for
  // a 1 s backward CLOCK_REALTIME step against 100 ms of PHC. Dating holdover
  // from the estimate puts it before ingress, and a negative holdover would
  // reach fbclock_window_of_uncertainty's cast to uint64_t.
  fbclock_clockdata_v2 state = {
      .ingress_time_ns = 1647269082943150996,
      .clockId = CLOCK_MONOTONIC_RAW,
      .phc_time_ns = 1647269082944150996, // anchor 1 ms after ingress
      .sysclock_time_ns = 1000000000,
      .coef_ppb = -1111111111,
  };
  double error_bound = 172.0;
  double h_value = 50.5;

  fbclock_truetime truetime;
  ASSERT_EQ(
      fbclock_calculate_time_v2(
          error_bound,
          h_value,
          &state,
          state.sysclock_time_ns + 5000000000LL,
          &truetime,
          FBCLOCK_TAI),
      FBCLOCK_E_NO_ERROR);
  // 5 s * (1 - 1.111111111) walks the estimate 555555555 ns backwards.
  EXPECT_EQ(
      fbclock_truetime_midpoint_ns(&truetime),
      (uint64_t)(state.phc_time_ns - 555555555));
  EXPECT_EQ(truetime.latest_ns - truetime.earliest_ns, 344ULL);
}

TEST(fbclockTest, test_fbclock_calculate_time_past_v2) {
  // The REALTIME anchor section is a normal fbclock_clockdata_v2 whose anchor
  // lives in the standard phc_time_ns / sysclock_time_ns / coef_ppb fields.
  fbclock_clockdata_v2 state = {
      .ingress_time_ns = 1647269082943150996,
      .error_bound_ns = 172,
      .phc_time_ns = 1647269082944150996, // ingress + 1ms
      .sysclock_time_ns = 1647269082944000000,
      .coef_ppb = 12,
  };
  double error_bound = state.error_bound_ns;
  double h_value = 50.5;
  fbclock_truetime truetime;

  // ts 500us in the past of the anchor.
  int64_t past_ts = state.sysclock_time_ns - 500000;
  int err = fbclock_calculate_time_past_v2(
      error_bound, h_value, &state, past_ts, &truetime, FBCLOCK_TAI);
  ASSERT_EQ(err, FBCLOCK_E_NO_ERROR);
  // Expected PHC at past moment: anchor + (-500000) + negligible drift.
  int64_t expected_phc = 1647269082943650996;
  EXPECT_GT(truetime.latest_ns, (uint64_t)expected_phc);
  EXPECT_LT(truetime.earliest_ns, (uint64_t)expected_phc);
  // Window roughly 2*error_bound wide (holdover is tiny near ingress).
  EXPECT_LT(truetime.latest_ns - truetime.earliest_ns, 1000ULL);

  // ts within bound, in the future of the snapshot is also allowed.
  int64_t future_ts = state.sysclock_time_ns + 5000;
  err = fbclock_calculate_time_past_v2(
      error_bound, h_value, &state, future_ts, &truetime, FBCLOCK_TAI);
  EXPECT_EQ(err, FBCLOCK_E_NO_ERROR);

  // ts beyond +cap of the anchor: refuse.
  int64_t way_future =
      state.sysclock_time_ns + FBCLOCK_MAX_EXTRAPOLATION_NS + 1;
  err = fbclock_calculate_time_past_v2(
      error_bound, h_value, &state, way_future, &truetime, FBCLOCK_TAI);
  EXPECT_EQ(err, FBCLOCK_E_DATA_STALE);

  // ts beyond -cap of the anchor: refuse.
  int64_t way_past = state.sysclock_time_ns - FBCLOCK_MAX_EXTRAPOLATION_NS - 1;
  err = fbclock_calculate_time_past_v2(
      error_bound, h_value, &state, way_past, &truetime, FBCLOCK_TAI);
  EXPECT_EQ(err, FBCLOCK_E_DATA_STALE);
}

TEST(fbclockTest, test_fbclock_calculate_time_past_v2_predates_ingress) {
  // ingress is in the future of the anchor's PHC value, so any ts at or before
  // the anchor maps to past_phc < ingress. WOU must collapse to error_bound
  // only (holdover seconds clamped to 0).
  fbclock_clockdata_v2 state = {
      .ingress_time_ns = 2002000000, // > phc anchor
      .error_bound_ns = 172,
      .phc_time_ns = 2001000000,
      .sysclock_time_ns = 1000000,
      .coef_ppb = 0,
  };
  double error_bound = state.error_bound_ns;
  double h_value = 1000000.0; // huge holdover, would dominate if seconds > 0

  fbclock_truetime truetime;
  int err = fbclock_calculate_time_past_v2(
      error_bound,
      h_value,
      &state,
      state.sysclock_time_ns, // diff=0, past_phc = anchor < ingress
      &truetime,
      FBCLOCK_TAI);
  ASSERT_EQ(err, FBCLOCK_E_NO_ERROR);
  // WOU should be just error_bound (no holdover term).
  uint64_t window = truetime.latest_ns - truetime.earliest_ns;
  EXPECT_EQ(window, 2 * (uint64_t)error_bound);
}

TEST(fbclockTest, test_fbclock_apply_smear_after_2017_leap_second) {
  uint64_t offset_pre_ns = 36e9;
  uint64_t offset_post_ns = 37e9;
  uint64_t smear_start_ns = 1483228836e9; // Sun, 01 Jan 2017 00:00:36 TAI
  uint64_t smear_end_ns = 1483293836e9; // Sun, 01 Jan 2017 18:03:56 TAI
  int multiplier = 1;

  uint64_t time =
      1714142307961569530; // Friday, 26 April 2024 14:38:27.961:569:530 TAI
  time = fbclock_apply_smear(
      time,
      offset_pre_ns,
      offset_post_ns,
      smear_start_ns,
      smear_end_ns,
      multiplier);
  // Expect UTC time to be 37 seconds behind TAI
  EXPECT_EQ(
      time,
      1714142270961569530); // Friday, 26 April 2024 14:37:50.961:569:530 UTC

  time = 1714142307961570584; // Friday, 26 April 2024 14:38:27.961:570:584 TAI
  time = fbclock_apply_smear(
      time,
      offset_pre_ns,
      offset_post_ns,
      smear_start_ns,
      smear_end_ns,
      multiplier);
  // Expect UTC time to be 37 seconds behind TAI
  EXPECT_EQ(
      time,
      1714142270961570584); // Friday, 26 April 2024 14:37:50.961:570:584 UTC
}

TEST(fbclockTest, test_fbclock_apply_smear_before_2017_leap_second) {
  uint64_t offset_pre_ns = 36e9;
  uint64_t offset_post_ns = 37e9;
  uint64_t smear_start_ns = 1483228836e9; // Sun, 01 Jan 2017 00:00:36 TAI
  uint64_t smear_end_ns = 1483293836e9; // Sun, 01 Jan 2017 18:03:56 TAI
  int multiplier = 1;

  uint64_t time =
      1443142307961555444; // Friday, 25 Sep 2015 00:51:47.961:555:444 TAI
  time = fbclock_apply_smear(
      time,
      offset_pre_ns,
      offset_post_ns,
      smear_start_ns,
      smear_end_ns,
      multiplier);
  // Expect UTC time to be 36 seconds behind TAI
  EXPECT_EQ(
      time,
      1443142271961555444); // Friday, 25 Sep 2015 00:51:11.961:555:444 UTC

  time = 1443142308666555444; // Friday, 25 Sep 2015 00:51:48.666:555:444 TAI
  time = fbclock_apply_smear(
      time,
      offset_pre_ns,
      offset_post_ns,
      smear_start_ns,
      smear_end_ns,
      multiplier);
  // Expect UTC time to be 36 seconds behind TAI
  EXPECT_EQ(
      time,
      1443142272666555444); // Friday, 25 Sep 2015 00:51:12.666:555:444 UTC
}

TEST(fbclockTest, test_fbclock_apply_smear_during_2017_leap_second_params) {
  uint64_t offset_pre_ns = 36e9;
  uint64_t offset_post_ns = 37e9;
  uint64_t smear_start_ns = 1483228836e9; // Sun, 01 Jan 2017 00:00:36 TAI
  uint64_t smear_end_ns = 1483293836e9; // Sun, 01 Jan 2017 18:03:56 TAI
  int multiplier = 1;

  uint64_t input_times[] = {
      1483228835000000000, // Sun, 01 Jan 2017 00:00:35:000:000:000 TAI
      1483228836000000000, // Sun, 01 Jan 2017 00:00:36:000:000:000 TAI (start)
      1483228836000065000, // Sun, 01 Jan 2017 00:00:36:000:065:000 TAI
      1483228836000130000, // Sun, 01 Jan 2017 00:00:36:000:130:000 TAI
      1483228837000000000, // Sun, 01 Jan 2017 00:00:37:000:000:000 TAI
      1483261335000000000, // Sun, 01 Jan 2017 09:02:15:000:000:000 TAI
      1483261336000000000, // Sun, 01 Jan 2017 09:02:16:000:000:000 TAI
                           // (midpoint)
      1483261337000000000, // Sun, 01 Jan 2017 09:02:17:000:000:000 TAI
      1483261345000000000, // Sun, 01 Jan 2017 09:02:25:000:000:000 TAI
      1483261346000000000, // Sun, 01 Jan 2017 09:02:26:000:000:000 TAI
      1483261347000000000, // Sun, 01 Jan 2017 09:02:27:000:000:000 TAI
      1483293836000000000, // Sun, 01 Jan 2017 18:03:56:000:000:000 TAI (end)
      1483293837000000000, // Sun, 01 Jan 2017 18:03:57:000:000:000 TAI
  };

  uint64_t output_times[] = {
      1483228799000000000, // Sat, 31 Dec 2016 23:59:59:000:000:000 UTC
      1483228800000000000, // Sun, 01 Jan 2017 00:00:00:000:000:000 UTC (start)
      1483228800000064999, // Sun, 01 Jan 2017 00:00:00:000:064:999 UTC
      1483228800000129998, // Sun, 01 Jan 2017 00:00:00:000:129:998 UTC
      1483228800999984616, // Sun, 01 Jan 2017 00:00:00:999:984:616 UTC
      1483261298500015385, // Sun, 01 Jan 2017 09:01:38:500:015:385 UTC
      1483261299500000000, // Sun, 01 Jan 2017 09:01:39:500:000:000 UTC
                           // (midpoint)
      1483261300499984616, // Sun, 01 Jan 2017 09:01:40:499:984:616 UTC
      1483261308499861539, // Sun, 01 Jan 2017 09:01:49:499:861:539 UTC
      1483261309499846154, // Sun, 01 Jan 2017 09:01:49:499:846:154 UTC
      1483261310499830770, // Sun, 01 Jan 2017 09:01:50:499:830:770 UTC
      1483293799000000000, // Sun, 01 Jan 2017 18:03:19:000:000:000 UTC (end)
      1483293800000000000, // Sun, 01 Jan 2017 18:03:20:000:000:000 UTC
  };

  ASSERT_EQ(std::size(input_times), std::size(output_times));
  for (size_t i = 0; i < std::size(input_times); ++i) {
    EXPECT_EQ(
        fbclock_apply_smear(
            input_times[i],
            offset_pre_ns,
            offset_post_ns,
            smear_start_ns,
            smear_end_ns,
            multiplier),
        output_times[i]);
  }
}

TEST(fbclockTest, test_fbclock_apply_smear_during_future_leap_second_negative) {
  uint64_t offset_pre_ns = 37e9;
  uint64_t offset_post_ns = 36e9;
  uint64_t smear_start_ns = 1893456037e9; // Sun, 01 Jan 2030 00:00:37 TAI
  uint64_t smear_end_ns = 1893521037e9; // Sun, 01 Jan 2017 18:03:57 TAI
  int multiplier = -1;

  uint64_t input_times[] = {
      1893456037000000000, // Wed, 01 Jan 2030 00:00:37:000:000:000 TAI (start)
      1893488537000000000, // Wed, 01 Jan 2030 09:02:17:000:000:000 TAI
                           // (midpoint)
      1893521037000000000, // Wed, 01 Jan 2030 18:03:57:000:000:000 TAI (end)

  };

  uint64_t output_times[] = {
      1893456000000000000, // Wed, 01 Jan 2030 00:00:00:000:000:000 UTC (start)
      1893488500500000000, // Wed, 01 Jan 2030 09:01:40.500:000:000 UTC
                           // (midpoint)
      1893521001000000000, // Wed, 01 Jan 2030 18:03:21:000:000:000 UTC (end)
  };

  ASSERT_EQ(std::size(input_times), std::size(output_times));
  for (size_t i = 0; i < std::size(input_times); ++i) {
    EXPECT_EQ(
        fbclock_apply_smear(
            input_times[i],
            offset_pre_ns,
            offset_post_ns,
            smear_start_ns,
            smear_end_ns,
            multiplier),
        output_times[i]);
  }
}

// The windows above are 65000s wide. The daemon publishes a 62500s one
// (fbclock/daemon/daemon.go: leapDurationS), so these pin the smear against
// the width production actually gets.
TEST(fbclockTest, test_fbclock_apply_smear_over_daemon_window) {
  const uint64_t offset_pre_ns = 36e9;
  const uint64_t offset_post_ns = 37e9;
  const uint64_t smear_start_ns = 1483228836e9; // Sun, 01 Jan 2017 00:00:36 TAI
  const uint64_t smear_end_ns = 1483291336e9; // 62500s later
  const auto smear = [&](uint64_t time) {
    return fbclock_apply_smear(
        time,
        offset_pre_ns,
        offset_post_ns,
        smear_start_ns,
        smear_end_ns,
        /*multiplier=*/1);
  };

  // Nothing is smeared yet at the start, and all of it is by the end.
  EXPECT_EQ(smear(smear_start_ns), smear_start_ns - offset_pre_ns);
  EXPECT_EQ(smear(smear_end_ns), smear_end_ns - offset_post_ns);

  // The last nanosecond of the window and the first one after it are one
  // nanosecond of UTC apart: no step where the ramp hands over.
  EXPECT_EQ(smear(smear_end_ns + 1), smear(smear_end_ns) + 1);

  // Half the window in, half the leap second has been smeared.
  const uint64_t midpoint_ns =
      smear_start_ns + (smear_end_ns - smear_start_ns) / 2;
  EXPECT_EQ(
      smear(midpoint_ns),
      midpoint_ns - offset_pre_ns - (offset_post_ns - offset_pre_ns) / 2);
}

TEST(fbclockTest, test_fbclock_apply_smear_over_daemon_window_negative) {
  const uint64_t offset_pre_ns = 37e9;
  const uint64_t offset_post_ns = 36e9;
  const uint64_t smear_start_ns = 1893456037e9; // Wed, 01 Jan 2030 00:00:37 TAI
  const uint64_t smear_end_ns = 1893518537e9; // 62500s later
  const auto smear = [&](uint64_t time) {
    return fbclock_apply_smear(
        time,
        offset_pre_ns,
        offset_post_ns,
        smear_start_ns,
        smear_end_ns,
        /*multiplier=*/-1);
  };

  EXPECT_EQ(smear(smear_start_ns), smear_start_ns - offset_pre_ns);
  EXPECT_EQ(smear(smear_end_ns), smear_end_ns - offset_post_ns);
  EXPECT_EQ(smear(smear_end_ns + 1), smear(smear_end_ns) + 1);

  const uint64_t midpoint_ns =
      smear_start_ns + (smear_end_ns - smear_start_ns) / 2;
  EXPECT_EQ(
      smear(midpoint_ns),
      midpoint_ns - offset_pre_ns + (offset_pre_ns - offset_post_ns) / 2);
}

// fbclock_apply_smear is exported, so pin the short windows no daemon produces.
TEST(fbclockTest, test_fbclock_apply_smear_over_short_windows) {
  const uint64_t offset_pre_ns = 36e9;
  const uint64_t offset_post_ns = 37e9;
  const uint64_t smear_start_ns = 1483228836e9; // Sun, 01 Jan 2017 00:00:36 TAI

  for (const uint64_t window_ns :
       {0ULL, 1ULL, 500000000ULL, 1000000000ULL, 1500000000ULL}) {
    const uint64_t smear_end_ns = smear_start_ns + window_ns;
    const auto smear = [&](uint64_t time) {
      return fbclock_apply_smear(
          time,
          offset_pre_ns,
          offset_post_ns,
          smear_start_ns,
          smear_end_ns,
          /*multiplier=*/1);
    };

    EXPECT_EQ(smear(smear_end_ns), smear_end_ns - offset_post_ns)
        << "window_ns=" << window_ns;
    EXPECT_EQ(smear(smear_end_ns + 1), smear(smear_end_ns) + 1)
        << "window_ns=" << window_ns;
    if (window_ns == 1500000000ULL) {
      const uint64_t midpoint_ns = smear_start_ns + 750000000ULL;
      EXPECT_EQ(smear(midpoint_ns), midpoint_ns - offset_pre_ns - 500000000ULL);
    }
  }
}

TEST(fbclockTest, test_fbclock_truetime_midpoint_ns) {
  // Even window: center is exact.
  fbclock_truetime even = {.earliest_ns = 100, .latest_ns = 200};
  EXPECT_EQ(fbclock_truetime_midpoint_ns(&even), 150ULL);

  // Odd window: integer division truncates toward earliest (100 + 101/2).
  fbclock_truetime odd = {.earliest_ns = 100, .latest_ns = 201};
  EXPECT_EQ(fbclock_truetime_midpoint_ns(&odd), 150ULL);

  // Zero window (earliest == latest): midpoint is that instant.
  fbclock_truetime point = {
      .earliest_ns = 1647269091803102957, .latest_ns = 1647269091803102957};
  EXPECT_EQ(fbclock_truetime_midpoint_ns(&point), 1647269091803102957ULL);

  // Realistic large values, no overflow: 1647269091803102338 + 1238 / 2.
  fbclock_truetime wide = {
      .earliest_ns = 1647269091803102338, .latest_ns = 1647269091803103576};
  EXPECT_EQ(fbclock_truetime_midpoint_ns(&wide), 1647269091803102957ULL);
}

TEST(fbclockTest, test_fbclock_check_max_wou) {
  const uint64_t earliest = 1000;

  // max_wou_ns == FBCLOCK_MAX_WOU_NS_UNSET disables the check: even a 10ms
  // window passes.
  fbclock_truetime tenMs = {
      .earliest_ns = earliest, .latest_ns = earliest + 10000000};
  EXPECT_EQ(
      fbclock_check_max_wou(FBCLOCK_MAX_WOU_NS_UNSET, &tenMs),
      FBCLOCK_E_NO_ERROR);

  // Window narrower than the limit: allowed.
  fbclock_truetime under = {
      .earliest_ns = earliest, .latest_ns = earliest + 999};
  EXPECT_EQ(fbclock_check_max_wou(1000, &under), FBCLOCK_E_NO_ERROR);

  // Window exactly at the limit: allowed (limit is the max acceptable width).
  fbclock_truetime atLimit = {
      .earliest_ns = earliest, .latest_ns = earliest + 1000};
  EXPECT_EQ(fbclock_check_max_wou(1000, &atLimit), FBCLOCK_E_NO_ERROR);

  // One nanosecond over the limit: rejected.
  fbclock_truetime overByOne = {
      .earliest_ns = earliest, .latest_ns = earliest + 1001};
  EXPECT_EQ(fbclock_check_max_wou(1000, &overByOne), FBCLOCK_E_WOU_TOO_BIG);

  // Far over the limit: rejected.
  EXPECT_EQ(fbclock_check_max_wou(1000, &tenMs), FBCLOCK_E_WOU_TOO_BIG);
}

TEST(fbclockTest, test_fbclock_max_wou_on_calculated_window) {
  // Drive the same path the gettime functions use: compute a real window, then
  // apply the caller's max-WOU policy to it. Also confirms the midpoint of a
  // TAI window reconstructs the PHC point estimate it was built around.
  fbclock_clockdata_v2 state = {
      .ingress_time_ns = 1647269082943150996,
      .phc_time_ns = 1647269091803102957,
      .sysclock_time_ns = 1000000000,
  };
  double error_bound = 172.0;
  double h_value = 50.5;

  fbclock_truetime tt;
  int err = fbclock_calculate_time_v2(
      error_bound, h_value, &state, state.sysclock_time_ns, &tt, FBCLOCK_TAI);
  ASSERT_EQ(err, FBCLOCK_E_NO_ERROR);

  // TAI window is symmetric around phc_time_ns, so the midpoint is that
  // estimate.
  EXPECT_EQ(fbclock_truetime_midpoint_ns(&tt), (uint64_t)state.phc_time_ns);

  const uint64_t window = tt.latest_ns - tt.earliest_ns;
  ASSERT_GT(window, 1ULL);

  // Disabled, at-limit, and generous limits pass; below-window rejects.
  EXPECT_EQ(
      fbclock_check_max_wou(FBCLOCK_MAX_WOU_NS_UNSET, &tt), FBCLOCK_E_NO_ERROR);
  EXPECT_EQ(fbclock_check_max_wou(window, &tt), FBCLOCK_E_NO_ERROR);
  EXPECT_EQ(fbclock_check_max_wou(window + 1000, &tt), FBCLOCK_E_NO_ERROR);
  EXPECT_EQ(fbclock_check_max_wou(window - 1, &tt), FBCLOCK_E_WOU_TOO_BIG);
}

TEST(fbclockTest, test_gettime_max_wou_rejects_the_stalled_window) {
  // Callers never see this window raw: fbclock_gettime runs it through
  // max_wou_ns, so widening it during a stall also changes which reads serve.
  char* test_shm = std::tmpnam(nullptr);
  FILE* f_rw = fopen(test_shm, "wb+");
  ASSERT_NE(f_rw, nullptr);
  int sfd_rw = fileno(f_rw);
  ASSERT_EQ(ftruncate(sfd_rw, FBCLOCK_SHMDATA_V2_SIZE), 0);

  FILE* f_ro = fopen(test_shm, "r");
  ASSERT_NE(f_ro, nullptr);
  fbclock_shmdata_v2* shmp = (fbclock_shmdata_v2*)mmap(
      nullptr, FBCLOCK_SHMDATA_V2_SIZE, PROT_READ, MAP_SHARED, fileno(f_ro), 0);
  ASSERT_NE(shmp, MAP_FAILED);

  struct timespec ts = {};
  ASSERT_EQ(clock_gettime(CLOCK_MONOTONIC_RAW, &ts), 0);
  int64_t sysclock_now_ns = ts.tv_sec * 1000000000LL + ts.tv_nsec;

  // The same 172 ns error bound and 50.5 ns/s holdover as
  // test_fbclock_calculate_time_v2_holdover_grows_while_stale.
  fbclock_clockdata_v2 data = {
      .ingress_time_ns = 1647269082943150996,
      .error_bound_ns = 172,
      .holdover_multiplier_ns = (uint32_t)(50.5 * FBCLOCK_POW2_16),
      .clockId = CLOCK_MONOTONIC_RAW,
      .phc_time_ns = 1647269082944150996, // anchor 1 ms after ingress
      .sysclock_time_ns = sysclock_now_ns,
      .coef_ppb = 0,
  };
  ASSERT_EQ(fbclock_clockdata_store_data_v2(sfd_rw, &data, nullptr), 0);

  const uint64_t cap_ns = 500; // between the fresh and the stalled window
  fbclock_lib lib = {};
  lib.shmp_v2 = shmp;
  lib.max_wou_ns = cap_ns;

  // Widths are not pinned here: this read races the library's own clock read.
  // The sibling test pins the exact arithmetic against a synthetic sysclock;
  // what matters here is which side of the cap each read lands on.
  fbclock_truetime fresh;
  ASSERT_EQ(fbclock_gettime(&lib, &fresh), FBCLOCK_E_NO_ERROR);
  EXPECT_LE(fresh.latest_ns - fresh.earliest_ns, cap_ns);

  // The same snapshot, five seconds without a daemon update.
  data.sysclock_time_ns = sysclock_now_ns - 5000000000LL;
  ASSERT_EQ(fbclock_clockdata_store_data_v2(sfd_rw, &data, nullptr), 0);

  fbclock_truetime stalled;
  EXPECT_EQ(fbclock_gettime(&lib, &stalled), FBCLOCK_E_WOU_TOO_BIG);

  // The cap is what rejected it, not the stall itself.
  lib.max_wou_ns = FBCLOCK_MAX_WOU_NS_UNSET;
  ASSERT_EQ(fbclock_gettime(&lib, &stalled), FBCLOCK_E_NO_ERROR);
  EXPECT_GT(stalled.latest_ns - stalled.earliest_ns, cap_ns);

  munmap(shmp, FBCLOCK_SHMDATA_V2_SIZE);
  fclose(f_ro);
  fclose(f_rw);
  remove(test_shm);
}

int main(int argc, char** argv) {
  ::testing::InitGoogleTest(&argc, argv);
  return RUN_ALL_TESTS();
}
