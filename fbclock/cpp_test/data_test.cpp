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

#include <errno.h>
#include <gtest/gtest.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>
#include <chrono>
#include <filesystem>
#include <fstream>
#include <iterator>
#include <new>
#include <string>

#include "../fbclock.h"

namespace {

using SteadyClock = std::chrono::steady_clock;

// A PTP host's sources, the same host once it also offers NTP, and a host
// without PTP.
constexpr uint32_t kPtpHost = FBCLOCK_SOURCE_PTP | FBCLOCK_SOURCE_PRTC;
constexpr uint32_t kPtpAndNtpHost = kPtpHost | FBCLOCK_SOURCE_NTP;
constexpr uint32_t kNtpHost = FBCLOCK_SOURCE_NTP;

// The layout assumes a 64-byte cache line; this fails on a host whose L1 data
// cache line is another size.
TEST(FBClockDataLayoutTest, SectionIsOneCacheLine) {
  const auto line = sysconf(_SC_LEVEL1_DCACHE_LINESIZE);
  if (line <= 0) {
    GTEST_SKIP() << "this host doesn't report its cache line size";
  }
  EXPECT_EQ(sizeof(fbclock_data_section), static_cast<size_t>(line));
}

// Bit k is sections[k], line k + 1, at offset 64 * (k + 1); anything but one
// bit has no section.
TEST(FBClockDataLayoutTest, EachSourceBitIsItsSection) {
  EXPECT_EQ(fbclock_data_section_index(FBCLOCK_SOURCE_PTP), 0);
  EXPECT_EQ(fbclock_data_section_index(FBCLOCK_SOURCE_PRTC), 1);
  EXPECT_EQ(fbclock_data_section_index(FBCLOCK_SOURCE_NTP), 2);
  EXPECT_EQ(fbclock_data_section_index(1u << 31), 31);
  for (uint32_t source :
       {0u, FBCLOCK_SOURCE_PTP | FBCLOCK_SOURCE_NTP, FBCLOCK_SOURCE_ANY}) {
    EXPECT_EQ(fbclock_data_section_index(source), -1) << source;
  }
  for (uint32_t k = 0; k < 32; k++) {
    const auto offset =
        offsetof(fbclock_data, sections) + k * sizeof(fbclock_data_section);
    EXPECT_EQ(offset, 64 * (k + 1)) << k;
  }
}

// Waits give up after this, so a broken writer fails a test instead of hanging.
SteadyClock::time_point deadline() {
  return SteadyClock::now() + std::chrono::seconds(10);
}

bool waitForFlag(
    const atomic_uint64* flag,
    uint64_t value,
    SteadyClock::time_point until) {
  while (atomic_load(flag) != value) {
    if (SteadyClock::now() >= until) {
      return false;
    }
  }
  return true;
}

bool waitForMagic(
    const fbclock_data_header* header,
    fbclock_data_header* copy,
    SteadyClock::time_point until) {
  fbclock_data_header_load(header, copy);
  while (memcmp(copy->magic, FBCLOCK_DATA_MAGIC, sizeof(copy->magic))) {
    if (SteadyClock::now() >= until) {
      return false;
    }
    fbclock_data_header_load(header, copy);
  }
  return true;
}

fbclock_data_section* sectionOf(fbclock_data* data, uint32_t source) {
  return &data->sections[fbclock_data_section_index(source)];
}

fbclock_data_clock clockFor(int64_t generation) {
  return {
      .ref_time_ns = generation * 2,
      .sysclock_time_ns = generation * 3,
      .coef_ppb = -generation * 4,
      .clock_id = CLOCK_MONOTONIC_RAW,
      .error_bound_ns = static_cast<uint32_t>(generation * 5),
      .ingress_time_ns = generation,
      .holdover_multiplier_ns = static_cast<uint32_t>(generation * 6),
  };
}

class FBClockDataTest : public testing::Test {
 protected:
  void SetUp() override {
    file_ = tmpfile();
    ASSERT_NE(file_, nullptr);
    ASSERT_EQ(ftruncate(fileno(file_), FBCLOCK_DATA_SIZE), 0);
    writer_ = static_cast<fbclock_data*>(mmap(
        nullptr,
        FBCLOCK_DATA_SIZE,
        PROT_READ | PROT_WRITE,
        MAP_SHARED,
        fileno(file_),
        0));
    ASSERT_NE(writer_, MAP_FAILED);
    reader_ = static_cast<fbclock_data*>(mmap(
        nullptr, FBCLOCK_DATA_SIZE, PROT_READ, MAP_SHARED, fileno(file_), 0));
    ASSERT_NE(reader_, MAP_FAILED);
    // Shared with a forked writer, outside the file under test.
    flags_ = static_cast<atomic_uint64*>(mmap(
        nullptr,
        kFlagsSize,
        PROT_READ | PROT_WRITE,
        MAP_SHARED | MAP_ANONYMOUS,
        -1,
        0));
    ASSERT_NE(flags_, MAP_FAILED);
    for (size_t i = 0; i < kFlags; i++) {
      new (&flags_[i]) atomic_uint64(0);
    }
  }

  void TearDown() override {
    if (flags_ != nullptr && flags_ != MAP_FAILED) {
      munmap(flags_, kFlagsSize);
    }
    if (reader_ != nullptr && reader_ != MAP_FAILED) {
      munmap(reader_, FBCLOCK_DATA_SIZE);
    }
    if (writer_ != nullptr && writer_ != MAP_FAILED) {
      munmap(writer_, FBCLOCK_DATA_SIZE);
    }
    if (file_ != nullptr) {
      fclose(file_);
    }
  }

  static constexpr size_t kFlags = 2;
  static constexpr size_t kFlagsSize = kFlags * sizeof(atomic_uint64);
  FILE* file_ = nullptr;
  fbclock_data* writer_ = nullptr;
  fbclock_data* reader_ = nullptr;
  atomic_uint64* flags_ = nullptr;
};

TEST_F(FBClockDataTest, StoreOnlyChangesItsSection) {
  auto* writer = sectionOf(writer_, FBCLOCK_SOURCE_NTP);
  auto* reader = sectionOf(reader_, FBCLOCK_SOURCE_NTP);
  // Reserved bytes are copied as given, so a field a later minor adds survives.
  auto value = clockFor(7);
  memset(value.reserved, 0xcd, sizeof(value.reserved));
  memset(writer->clock.reserved, 0xff, sizeof(writer->clock.reserved));
  fbclock_data_section_store(writer, &value);
  fbclock_data_clock copy = {};
  ASSERT_EQ(fbclock_data_section_load(reader, &copy), 0);
  EXPECT_EQ(atomic_load(&reader->seq), 2);
  EXPECT_EQ(memcmp(&copy, &value, sizeof(copy)), 0);
  const auto* bytes = reinterpret_cast<const uint8_t*>(reader_);
  const size_t ntpBegin = reinterpret_cast<const uint8_t*>(reader) - bytes;
  const size_t ntpEnd = ntpBegin + sizeof(*reader);
  for (size_t i = 0; i < FBCLOCK_DATA_SIZE; i++) {
    if (i < ntpBegin || i >= ntpEnd) {
      EXPECT_EQ(bytes[i], 0) << "byte " << i;
    }
  }
  fbclock_data_section_store(writer, &value);
  ASSERT_EQ(fbclock_data_section_load(reader, &copy), 0);
  EXPECT_EQ(atomic_load(&reader->seq), 4);
}

TEST_F(FBClockDataTest, UninitializedAndBusySectionsAreRefused) {
  auto* writer = sectionOf(writer_, FBCLOCK_SOURCE_PTP);
  auto* reader = sectionOf(reader_, FBCLOCK_SOURCE_PTP);
  writer->clock = clockFor(1);
  // A failed load leaves the caller's record as it was.
  fbclock_data_clock copy;
  memset(&copy, 0xab, sizeof(copy));
  const auto untouched = copy;
  EXPECT_EQ(fbclock_data_section_load(reader, &copy), FBCLOCK_E_NO_DATA);
  EXPECT_EQ(memcmp(&copy, &untouched, sizeof(copy)), 0);
  atomic_store(&writer->seq, 3);
  EXPECT_EQ(fbclock_data_section_load(reader, &copy), FBCLOCK_E_CRC_MISMATCH);
  EXPECT_EQ(memcmp(&copy, &untouched, sizeof(copy)), 0);
}

TEST_F(FBClockDataTest, SequenceWrapNeverPublishesZero) {
  auto* writer = sectionOf(writer_, FBCLOCK_SOURCE_PTP);
  auto* reader = sectionOf(reader_, FBCLOCK_SOURCE_PTP);
  atomic_store(&writer->seq, UINT64_MAX - 1);
  auto value = clockFor(1);
  fbclock_data_section_store(writer, &value);
  fbclock_data_clock copy = {};
  ASSERT_EQ(fbclock_data_section_load(reader, &copy), 0);
  EXPECT_EQ(atomic_load(&reader->seq), 2);
  EXPECT_EQ(memcmp(&copy, &value, sizeof(copy)), 0);
}

TEST_F(FBClockDataTest, HeaderReplacesOldVersionAndReservedBytes) {
  memset(&writer_->header, 0xff, sizeof(writer_->header));
  fbclock_data_header_store(&writer_->header, kPtpHost);
  fbclock_data_header expected = {
      .magic = {'F', 'B', 'C', 'L', 'O', 'C', 'K', '\0'},
      .major = FBCLOCK_DATA_MAJOR,
      .minor = FBCLOCK_DATA_MINOR,
      .sources = kPtpHost,
  };
  fbclock_data_header copy;
  memset(&copy, 0xab, sizeof(copy));
  fbclock_data_header_load(&reader_->header, &copy);
  EXPECT_EQ(memcmp(&copy, &expected, sizeof(expected)), 0);
  EXPECT_EQ(atomic_load(&sectionOf(reader_, FBCLOCK_SOURCE_PTP)->seq), 0);
}

TEST_F(FBClockDataTest, ConcurrentWriterNeverProducesATornSnapshot) {
  auto* writer = sectionOf(writer_, FBCLOCK_SOURCE_PTP);
  auto* reader = sectionOf(reader_, FBCLOCK_SOURCE_PTP);
  auto value = clockFor(1);
  fbclock_data_section_store(writer, &value);
  auto* done = &flags_[0];
  auto* seen = &flags_[1];
  pid_t child = fork();
  if (child == -1) {
    ADD_FAILURE() << "fork failed";
    return;
  }
  if (child == 0) {
    // Long enough that a missing retry lets a torn copy through on every run.
    for (int64_t i = 2; i <= 2000000 || atomic_load(seen) == 0; i++) {
      auto next = clockFor(i);
      fbclock_data_section_store(writer, &next);
    }
    atomic_store(done, 1);
    _exit(0);
  }
  int successful = 0;
  int concurrent = 0;
  bool consistent = true;
  const auto until = deadline();
  while ((atomic_load(done) == 0 || successful < 1000) &&
         SteadyClock::now() < until) {
    fbclock_data_clock copy = {};
    int result = fbclock_data_section_load(reader, &copy);
    if (result != 0) {
      consistent &= result == FBCLOCK_E_CRC_MISMATCH;
      continue;
    }
    auto expected = clockFor(copy.ingress_time_ns);
    consistent &= memcmp(&copy, &expected, sizeof(copy)) == 0;
    successful++;
    if (copy.ingress_time_ns > 1 && atomic_load(done) == 0) {
      concurrent++;
      atomic_store(seen, 1);
    }
  }
  const bool finished = atomic_load(done) != 0 && successful >= 1000;
  if (!finished) {
    kill(child, SIGKILL);
  }
  int status = 0;
  ASSERT_EQ(waitpid(child, &status, 0), child);
  EXPECT_TRUE(finished);
  EXPECT_EQ(status, 0);
  EXPECT_TRUE(consistent);
  EXPECT_GT(concurrent, 0);
}

TEST_F(FBClockDataTest, MagicPublishesTheInitializedHeader) {
  auto* phase = &flags_[0];
  constexpr uint64_t iterations = 1000;
  pid_t child = fork();
  if (child == -1) {
    ADD_FAILURE() << "fork failed";
    return;
  }
  if (child == 0) {
    for (uint64_t i = 0; i < iterations; i++) {
      memset(writer_->header.magic, 0, sizeof(writer_->header.magic));
      writer_->header.major = 0xff;
      writer_->header.minor = 0xff;
      memset(
          writer_->header.reserved0, 0xff, sizeof(writer_->header.reserved0));
      writer_->header.sources = UINT32_MAX;
      memset(writer_->header.reserved, 0xff, sizeof(writer_->header.reserved));
      atomic_store(phase, 3 * i + 1);
      while (atomic_load(phase) != 3 * i + 2) {
      }
      fbclock_data_header_store(&writer_->header, kPtpHost);
      while (atomic_load(phase) != 3 * i + 3) {
      }
    }
    _exit(0);
  }
  bool initialized = true;
  bool published = true;
  const auto until = deadline();
  for (uint64_t i = 0; i < iterations; i++) {
    if (!waitForFlag(phase, 3 * i + 1, until)) {
      published = false;
      break;
    }
    atomic_store(phase, 3 * i + 2);
    fbclock_data_header copy;
    if (!waitForMagic(&reader_->header, &copy, until)) {
      published = false;
      break;
    }
    initialized &= copy.major == FBCLOCK_DATA_MAJOR;
    initialized &= copy.minor == FBCLOCK_DATA_MINOR;
    initialized &= copy.sources == kPtpHost;
    for (auto byte : copy.reserved0) {
      initialized &= byte == 0;
    }
    for (auto byte : copy.reserved) {
      initialized &= byte == 0;
    }
    atomic_store(phase, 3 * i + 3);
  }
  if (!published) {
    kill(child, SIGKILL);
  }
  int status = 0;
  ASSERT_EQ(waitpid(child, &status, 0), child);
  EXPECT_TRUE(published);
  EXPECT_EQ(status, 0);
  EXPECT_TRUE(initialized);
}

// A restart rewrites a valid header (new minor, NTP toggled); readers opening
// meanwhile see the magic, the major, and whole minor and sources values.
TEST_F(FBClockDataTest, RewritingAValidHeaderStaysValid) {
  fbclock_data_header_store(&writer_->header, kPtpHost);
  auto* stop = &flags_[0];
  auto* stores = &flags_[1];
  pid_t child = fork();
  if (child == -1) {
    ADD_FAILURE() << "fork failed";
    return;
  }
  if (child == 0) {
    for (uint64_t i = 1; atomic_load(stop) == 0; i++) {
      writer_->header.minor = FBCLOCK_DATA_MINOR + 1;
      fbclock_data_header_store(
          &writer_->header, i % 2 ? kPtpAndNtpHost : kPtpHost);
      atomic_store(stores, i);
    }
    _exit(0);
  }
  bool valid = true;
  const auto until = deadline();
  while (atomic_load(stores) < 100000 && SteadyClock::now() < until) {
    fbclock_data_header copy;
    fbclock_data_header_load(&reader_->header, &copy);
    valid &= memcmp(copy.magic, FBCLOCK_DATA_MAGIC, 8) == 0;
    valid &= copy.major == FBCLOCK_DATA_MAJOR;
    valid &= copy.minor == FBCLOCK_DATA_MINOR ||
        copy.minor == FBCLOCK_DATA_MINOR + 1;
    valid &= copy.sources == kPtpHost || copy.sources == kPtpAndNtpHost;
    for (auto byte : copy.reserved) {
      valid &= byte == 0;
    }
  }
  const bool finished = atomic_load(stores) >= 100000;
  atomic_store(stop, 1);
  int status = 0;
  ASSERT_EQ(waitpid(child, &status, 0), child);
  EXPECT_TRUE(finished);
  EXPECT_EQ(status, 0);
  EXPECT_TRUE(valid);
}

// The daemon's leapsectz.UTCOffset: its NTP anchor is CLOCK_REALTIME plus this.
constexpr int64_t kDaemonTaiOffsetNs = 37000000000;

int64_t nowNs(clockid_t clock_id) {
  struct timespec ts = {};
  clock_gettime(clock_id, &ts);
  return ts.tv_sec * 1000000000LL + ts.tv_nsec;
}

int64_t realtimeNs() {
  return nowNs(CLOCK_REALTIME);
}

long openFdCount() {
  return std::distance(
      std::filesystem::directory_iterator("/proc/self/fd"),
      std::filesystem::directory_iterator{});
}

int mappingsOf(const char* path) {
  std::ifstream maps("/proc/self/maps");
  int count = 0;
  for (std::string line; std::getline(maps, line);) {
    count += line.ends_with(path);
  }
  return count;
}

// The NTP section as the daemon writes it at realtime_ns, from a chronyd poll
// since_ingress_ns earlier.
fbclock_data_clock ntpClock(
    int64_t realtime_ns,
    uint32_t error_bound_ns,
    uint32_t holdover_ns_per_s,
    int64_t since_ingress_ns = 0) {
  return {
      .ref_time_ns = realtime_ns + kDaemonTaiOffsetNs,
      .sysclock_time_ns = realtime_ns,
      .coef_ppb = 0,
      .clock_id = CLOCK_REALTIME,
      .error_bound_ns = error_bound_ns,
      .ingress_time_ns = realtime_ns + kDaemonTaiOffsetNs - since_ingress_ns,
      .holdover_multiplier_ns =
          static_cast<uint32_t>(holdover_ns_per_s * FBCLOCK_POW2_16),
  };
}

// A PTP section anchored now on CLOCK_MONOTONIC_RAW, with no holdover.
fbclock_data_clock ptpClock(uint32_t error_bound_ns) {
  const int64_t tai_ns = realtimeNs() + kDaemonTaiOffsetNs;
  return {
      .ref_time_ns = tai_ns,
      .sysclock_time_ns = nowNs(CLOCK_MONOTONIC_RAW),
      .coef_ppb = 0,
      .clock_id = CLOCK_MONOTONIC_RAW,
      .error_bound_ns = error_bound_ns,
      .ingress_time_ns = tai_ns,
  };
}

// The v3 reader on a file the test writes as the daemon would.
class FBClockDataReaderTest : public testing::Test {
 protected:
  void SetUp() override {
    fd_ = mkstemp(path_);
    ASSERT_GE(fd_, 0);
    ASSERT_EQ(ftruncate(fd_, FBCLOCK_DATA_SIZE), 0);
    writer_ = static_cast<fbclock_data*>(mmap(
        nullptr,
        FBCLOCK_DATA_SIZE,
        PROT_READ | PROT_WRITE,
        MAP_SHARED,
        fd_,
        0));
    ASSERT_NE(writer_, MAP_FAILED);
  }

  void TearDown() override {
    closeLib();
    if (writer_ != nullptr && writer_ != MAP_FAILED) {
      munmap(writer_, FBCLOCK_DATA_SIZE);
    }
    if (fd_ >= 0) {
      close(fd_);
      unlink(path_);
    }
  }

  int openLib(
      uint32_t sources,
      uint64_t max_wou_ns = FBCLOCK_MAX_WOU_NS_UNSET) {
    closeLib();
    const fbclock_options options = {
        .max_wou_ns = max_wou_ns, .sources = sources, .version = FBCLOCK_V3};
    return fbclock_init_with_options(&lib_, path_, &options);
  }

  void closeLib() {
    fbclock_destroy(&lib_);
    lib_ = fbclock_lib{.shm_fd = -1};
  }

  void store(uint32_t source, const fbclock_data_clock& clock) {
    fbclock_data_section_store(sectionOf(writer_, source), &clock);
  }

  char path_[32] = "/tmp/fbclock_dataXXXXXX";
  int fd_ = -1;
  fbclock_data* writer_ = nullptr;
  fbclock_lib lib_ = {.shm_fd = -1};
};

// PTP, then NTP, if both the caller and the header have it; PRTC never. The
// bounds tell which section a read came from.
TEST_F(FBClockDataReaderTest, InitPicksPtpThenNtp) {
  constexpr uint32_t kPtpBoundNs = 1000;
  constexpr uint32_t kNtpBoundNs = 1000000;
  const struct {
    uint32_t offered;
    uint32_t sources;
    int rcode;
    uint32_t bound_ns;
  } cases[] = {
      {kPtpAndNtpHost, FBCLOCK_SOURCE_PTP, FBCLOCK_E_NO_ERROR, kPtpBoundNs},
      {kPtpAndNtpHost, 0, FBCLOCK_E_NO_ERROR, kPtpBoundNs},
      {kPtpAndNtpHost, FBCLOCK_SOURCE_ANY, FBCLOCK_E_NO_ERROR, kPtpBoundNs},
      {kPtpAndNtpHost, FBCLOCK_SOURCE_NTP, FBCLOCK_E_NO_ERROR, kNtpBoundNs},
      {kPtpAndNtpHost, FBCLOCK_SOURCE_PRTC, FBCLOCK_E_NOTSUP, 0},
      {kNtpHost, FBCLOCK_SOURCE_PTP, FBCLOCK_E_NOTSUP, 0},
      {kNtpHost, 0, FBCLOCK_E_NOTSUP, 0},
      {kNtpHost, FBCLOCK_SOURCE_NTP, FBCLOCK_E_NO_ERROR, kNtpBoundNs},
      {kNtpHost, FBCLOCK_SOURCE_ANY, FBCLOCK_E_NO_ERROR, kNtpBoundNs},
  };
  for (const auto& c : cases) {
    fbclock_data_header_store(&writer_->header, c.offered);
    store(FBCLOCK_SOURCE_PTP, ptpClock(kPtpBoundNs));
    store(FBCLOCK_SOURCE_NTP, ntpClock(realtimeNs(), kNtpBoundNs, 0));
    ASSERT_EQ(openLib(c.sources), c.rcode) << c.offered << " " << c.sources;
    if (c.rcode != FBCLOCK_E_NO_ERROR) {
      continue;
    }
    fbclock_truetime tt = {};
    ASSERT_EQ(fbclock_gettime(&lib_, &tt), FBCLOCK_E_NO_ERROR);
    EXPECT_EQ(tt.latest_ns - tt.earliest_ns, 2ULL * c.bound_ns)
        << c.offered << " " << c.sources;
  }
}

// Only FBCLOCK_V3 reads v3, and init clears the v3 mapping of a lib reused
// after destroy. v2 accepts the file for ANY, with or without a PTP device.
TEST_F(FBClockDataReaderTest, OtherVersionsReadV2) {
  fbclock_data_header_store(&writer_->header, kPtpAndNtpHost);
  for (const uint32_t version : {FBCLOCK_VERSION_DEFAULT, FBCLOCK_V2, 4}) {
    ASSERT_EQ(openLib(FBCLOCK_SOURCE_ANY), FBCLOCK_E_NO_ERROR);
    fbclock_destroy(&lib_);
    const fbclock_options options = {
        .sources = FBCLOCK_SOURCE_ANY, .version = version};
    EXPECT_EQ(
        fbclock_init_with_options(&lib_, path_, &options), FBCLOCK_E_NO_ERROR)
        << version;
    EXPECT_NE(lib_.shmp_v2, nullptr) << version;
    EXPECT_EQ(lib_.shmp_v3, nullptr) << version;
    closeLib();
  }
}

// The daemon sets the magic last, so until then it's still starting. Callers
// retry, so each failed init must give back its mapping.
TEST_F(FBClockDataReaderTest, InitWaitsForTheMagic) {
  const long fds = openFdCount();
  const int mappings = mappingsOf(path_);
  EXPECT_EQ(openLib(FBCLOCK_SOURCE_NTP), FBCLOCK_E_NO_DATA);
  fbclock_data_header_store(&writer_->header, kNtpHost);
  memset(writer_->header.magic, 0, sizeof(writer_->header.magic));
  EXPECT_EQ(openLib(FBCLOCK_SOURCE_NTP), FBCLOCK_E_NO_DATA);
  EXPECT_EQ(lib_.shmp_v3, nullptr);
  EXPECT_EQ(openFdCount(), fds);
  EXPECT_EQ(mappingsOf(path_), mappings);
}

TEST_F(FBClockDataReaderTest, InitRefusesAnotherMajor) {
  fbclock_data_header_store(&writer_->header, kNtpHost);
  for (const int major : {FBCLOCK_DATA_MAJOR - 1, FBCLOCK_DATA_MAJOR + 1}) {
    writer_->header.major = static_cast<uint8_t>(major);
    EXPECT_EQ(openLib(FBCLOCK_SOURCE_NTP), FBCLOCK_E_NOTSUP) << major;
  }
  EXPECT_EQ(lib_.shmp_v3, nullptr);
}

// Empty, as the daemon creates it, or short by a byte.
TEST_F(FBClockDataReaderTest, InitRejectsAShortFile) {
  const long fds = openFdCount();
  for (const off_t size : {(off_t)0, (off_t)FBCLOCK_DATA_SIZE - 1}) {
    ASSERT_EQ(ftruncate(fd_, size), 0);
    testing::internal::CaptureStderr();
    EXPECT_EQ(openLib(FBCLOCK_SOURCE_NTP), FBCLOCK_E_SHMEM_OPEN) << size;
    EXPECT_NE(
        testing::internal::GetCapturedStderr().find(path_), std::string::npos);
    EXPECT_EQ(lib_.shmp_v3, nullptr);
    EXPECT_EQ(openFdCount(), fds);
  }
}

// A zeroed lib's shm_fd is 0, which destroy would close as a v2 fd, and a lib
// reused after a v2 destroy still points at the old mapping.
TEST_F(FBClockDataReaderTest, FailedInitLeavesNoV2State) {
  fbclock_shmdata_v2 stale = {};
  fbclock_lib lib = {.shmp_v2 = &stale};
  const fbclock_options options = {.version = FBCLOCK_V3};
  EXPECT_EQ(
      fbclock_init_with_options(&lib, path_, &options), FBCLOCK_E_NO_DATA);
  EXPECT_EQ(lib.shm_fd, -1);
  EXPECT_EQ(lib.shmp_v2, nullptr);
}

// The fd is closed once the file is mapped, and destroy unmaps it.
TEST_F(FBClockDataReaderTest, LibHoldsOnlyTheMapping) {
  fbclock_data_header_store(&writer_->header, kNtpHost);
  const long fds = openFdCount();
  const int mappings = mappingsOf(path_);
  ASSERT_EQ(openLib(FBCLOCK_SOURCE_NTP), FBCLOCK_E_NO_ERROR);
  EXPECT_EQ(openFdCount(), fds);
  EXPECT_EQ(mappingsOf(path_), mappings + 1);
  EXPECT_EQ(fbclock_destroy(&lib_), FBCLOCK_E_NO_ERROR);
  EXPECT_EQ(mappingsOf(path_), mappings);
  lib_ = fbclock_lib{.shm_fd = -1};
}

TEST_F(FBClockDataReaderTest, GettimeBeforeTheFirstWriteIsNoData) {
  fbclock_data_header_store(&writer_->header, kNtpHost);
  ASSERT_EQ(openLib(FBCLOCK_SOURCE_NTP), FBCLOCK_E_NO_ERROR);
  fbclock_truetime tt = {};
  EXPECT_EQ(fbclock_gettime(&lib_, &tt), FBCLOCK_E_NO_DATA);
  EXPECT_EQ(fbclock_gettime_utc(&lib_, &tt), FBCLOCK_E_NO_DATA);
}

// A write that never ends, as in UninitializedAndBusySectionsAreRefused.
TEST_F(FBClockDataReaderTest, GettimeRefusesATornSection) {
  fbclock_data_header_store(&writer_->header, kNtpHost);
  ASSERT_EQ(openLib(FBCLOCK_SOURCE_NTP), FBCLOCK_E_NO_ERROR);
  store(FBCLOCK_SOURCE_NTP, ntpClock(realtimeNs(), 1000000, 0));
  auto* writer = sectionOf(writer_, FBCLOCK_SOURCE_NTP);
  atomic_store(&writer->seq, 3);
  fbclock_truetime tt = {};
  EXPECT_EQ(fbclock_gettime(&lib_, &tt), FBCLOCK_E_CRC_MISMATCH);
}

// An anchor older than FBCLOCK_MAX_EXTRAPOLATION_NS, or ahead of the clock.
TEST_F(FBClockDataReaderTest, GettimeRefusesAStaleAnchor) {
  fbclock_data_header_store(&writer_->header, kNtpHost);
  ASSERT_EQ(openLib(FBCLOCK_SOURCE_NTP), FBCLOCK_E_NO_ERROR);
  const int64_t now_ns = realtimeNs();
  for (const int64_t anchor_ns :
       {now_ns - FBCLOCK_MAX_EXTRAPOLATION_NS - 1, now_ns + 1000000000}) {
    store(FBCLOCK_SOURCE_NTP, ntpClock(anchor_ns, 1000000, 0));
    fbclock_truetime tt = {};
    EXPECT_EQ(fbclock_gettime(&lib_, &tt), FBCLOCK_E_DATA_STALE)
        << anchor_ns - now_ns;
  }
}

// CLOCK_REALTIME stepped back past the poll, so ingress is after the anchor.
TEST_F(FBClockDataReaderTest, GettimeRefusesIngressAfterTheAnchor) {
  fbclock_data_header_store(&writer_->header, kNtpHost);
  ASSERT_EQ(openLib(FBCLOCK_SOURCE_NTP), FBCLOCK_E_NO_ERROR);
  store(FBCLOCK_SOURCE_NTP, ntpClock(realtimeNs(), 1000000, 0, -1));
  fbclock_truetime tt = {};
  EXPECT_EQ(fbclock_gettime(&lib_, &tt), FBCLOCK_E_PHC_IN_THE_PAST);
  EXPECT_EQ(fbclock_gettime_utc(&lib_, &tt), FBCLOCK_E_PHC_IN_THE_PAST);
}

// UINT32_MAX in either field is the daemon's "too big to serve".
TEST_F(FBClockDataReaderTest, GettimeRefusesASaturatedBound) {
  fbclock_data_header_store(&writer_->header, kNtpHost);
  ASSERT_EQ(openLib(FBCLOCK_SOURCE_NTP), FBCLOCK_E_NO_ERROR);
  auto bound = ntpClock(realtimeNs(), UINT32_MAX, 0);
  auto holdover = ntpClock(realtimeNs(), 1000000, 0);
  holdover.holdover_multiplier_ns = UINT32_MAX;
  for (const auto& clock : {bound, holdover}) {
    store(FBCLOCK_SOURCE_NTP, clock);
    fbclock_truetime tt = {};
    EXPECT_EQ(fbclock_gettime(&lib_, &tt), FBCLOCK_E_WOU_TOO_BIG);
  }
}

TEST_F(FBClockDataReaderTest, GettimeAppliesTheCallersCap) {
  fbclock_data_header_store(&writer_->header, kNtpHost);
  // No holdover rate, so the window is exactly twice the bound.
  const uint32_t bound_ns = 1000000;
  store(FBCLOCK_SOURCE_NTP, ntpClock(realtimeNs(), bound_ns, 0));
  fbclock_truetime tt = {};
  ASSERT_EQ(openLib(FBCLOCK_SOURCE_NTP, 2 * bound_ns), FBCLOCK_E_NO_ERROR);
  ASSERT_EQ(fbclock_gettime(&lib_, &tt), FBCLOCK_E_NO_ERROR);
  EXPECT_EQ(tt.latest_ns - tt.earliest_ns, 2ULL * bound_ns);
  ASSERT_EQ(openLib(FBCLOCK_SOURCE_NTP, 2 * bound_ns - 1), FBCLOCK_E_NO_ERROR);
  EXPECT_EQ(fbclock_gettime(&lib_, &tt), FBCLOCK_E_WOU_TOO_BIG);
  EXPECT_EQ(fbclock_gettime_utc(&lib_, &tt), FBCLOCK_E_WOU_TOO_BIG);
}

// The extrapolation from an anchor 1 s old scales by coef_ppb; a negative one
// needs the signed divisor.
TEST_F(FBClockDataReaderTest, GettimeAppliesTheRateCoefficient) {
  fbclock_data_header_store(&writer_->header, kNtpHost);
  ASSERT_EQ(openLib(FBCLOCK_SOURCE_NTP), FBCLOCK_E_NO_ERROR);
  for (const int64_t coef_ppb : {100000LL, -100000LL}) {
    const int64_t anchor_ns = realtimeNs() - 1000000000;
    auto clock = ntpClock(anchor_ns, 1000, 0);
    clock.coef_ppb = coef_ppb;
    store(FBCLOCK_SOURCE_NTP, clock);
    const auto at = [&](int64_t now_ns) {
      const int64_t diff_ns = now_ns - anchor_ns;
      return static_cast<uint64_t>(
          anchor_ns + kDaemonTaiOffsetNs + diff_ns +
          diff_ns * coef_ppb / 1000000000);
    };
    const int64_t before_ns = realtimeNs();
    fbclock_truetime tt = {};
    ASSERT_EQ(fbclock_gettime(&lib_, &tt), FBCLOCK_E_NO_ERROR);
    const int64_t after_ns = realtimeNs();
    EXPECT_GE(fbclock_truetime_midpoint_ns(&tt), at(before_ns)) << coef_ppb;
    EXPECT_LE(fbclock_truetime_midpoint_ns(&tt), at(after_ns)) << coef_ppb;
  }
}

// Holdover runs from the poll's ingress, not from the anchor the daemon
// rewrites every 10 ms.
TEST_F(FBClockDataReaderTest, GettimeWidensWithHoldover) {
  fbclock_data_header_store(&writer_->header, kNtpHost);
  ASSERT_EQ(openLib(FBCLOCK_SOURCE_NTP), FBCLOCK_E_NO_ERROR);
  const uint32_t bound_ns = 1000000;
  const uint32_t holdover_ns_per_s = 50000; // 50 ppm
  for (const int64_t since_ingress_ns : {0LL, 5000000000LL}) {
    const int64_t written_ns = realtimeNs();
    store(
        FBCLOCK_SOURCE_NTP,
        ntpClock(written_ns, bound_ns, holdover_ns_per_s, since_ingress_ns));
    fbclock_truetime tai = {};
    ASSERT_EQ(fbclock_gettime(&lib_, &tai), FBCLOCK_E_NO_ERROR);
    // UTC's holdover is TAI's: it's measured before the offset comes off.
    fbclock_truetime utc = {};
    ASSERT_EQ(fbclock_gettime_utc(&lib_, &utc), FBCLOCK_E_NO_ERROR);
    const int64_t after_ns = realtimeNs();
    // The library reads the clock before after_ns; +1 for its rounding.
    const uint64_t min_holdover_ns =
        holdover_ns_per_s * since_ingress_ns / 1000000000LL;
    const uint64_t max_holdover_ns = holdover_ns_per_s *
            (since_ingress_ns + after_ns - written_ns) / 1000000000LL +
        1;
    for (const auto& tt : {tai, utc}) {
      const uint64_t window = tt.latest_ns - tt.earliest_ns;
      EXPECT_GE(window, 2 * (bound_ns + min_holdover_ns)) << since_ingress_ns;
      EXPECT_LE(window, 2 * (bound_ns + max_holdover_ns)) << since_ingress_ns;
    }
  }
}

// The client's UTC takes off the daemon's 37 s, so UTC on NTP is
// CLOCK_REALTIME, with no smear.
TEST_F(FBClockDataReaderTest, GettimeUtcIsTaiMinus37Seconds) {
  fbclock_data_header_store(&writer_->header, kNtpHost);
  ASSERT_EQ(openLib(FBCLOCK_SOURCE_NTP), FBCLOCK_E_NO_ERROR);
  store(FBCLOCK_SOURCE_NTP, ntpClock(realtimeNs(), 1000000, 0));

  fbclock_truetime tai = {};
  int64_t before_ns = realtimeNs();
  ASSERT_EQ(fbclock_gettime(&lib_, &tai), FBCLOCK_E_NO_ERROR);
  int64_t after_ns = realtimeNs();
  EXPECT_GE(
      fbclock_truetime_midpoint_ns(&tai),
      static_cast<uint64_t>(before_ns + kDaemonTaiOffsetNs));
  EXPECT_LE(
      fbclock_truetime_midpoint_ns(&tai),
      static_cast<uint64_t>(after_ns + kDaemonTaiOffsetNs));

  fbclock_truetime utc = {};
  before_ns = realtimeNs();
  ASSERT_EQ(fbclock_gettime_utc(&lib_, &utc), FBCLOCK_E_NO_ERROR);
  after_ns = realtimeNs();
  EXPECT_GE(
      fbclock_truetime_midpoint_ns(&utc), static_cast<uint64_t>(before_ns));
  EXPECT_LE(
      fbclock_truetime_midpoint_ns(&utc), static_cast<uint64_t>(after_ns));
  EXPECT_EQ(utc.latest_ns - utc.earliest_ns, tai.latest_ns - tai.earliest_ns);
}

// gettime_past reads v2, which a v3 lib doesn't map.
TEST_F(FBClockDataReaderTest, GettimePastIsNoData) {
  fbclock_data_header_store(&writer_->header, kPtpAndNtpHost);
  ASSERT_EQ(openLib(FBCLOCK_SOURCE_PTP), FBCLOCK_E_NO_ERROR);
  store(FBCLOCK_SOURCE_PTP, ptpClock(1000));
  store(FBCLOCK_SOURCE_PRTC, ntpClock(realtimeNs(), 1000, 0));
  fbclock_truetime tt = {};
  EXPECT_EQ(fbclock_gettime_past(&lib_, realtimeNs(), &tt), FBCLOCK_E_NO_DATA);
  EXPECT_EQ(
      fbclock_gettime_past_utc(&lib_, realtimeNs(), &tt), FBCLOCK_E_NO_DATA);
}

// A NULL path is FBCLOCK_DATA_PATH: where the daemon hasn't written it, init
// fails for the reason opening that path does, not open(NULL)'s EFAULT.
TEST(FBClockDataNullPathTest, NullPathWithoutTheDaemonsFile) {
  if (access(FBCLOCK_DATA_PATH, F_OK) == 0) {
    GTEST_SKIP() << FBCLOCK_DATA_PATH << " exists on this host";
  }
  const std::string reason = strerror(errno);
  fbclock_lib lib = {};
  const fbclock_options options = {.version = FBCLOCK_V3};
  testing::internal::CaptureStderr();
  EXPECT_EQ(
      fbclock_init_with_options(&lib, nullptr, &options), FBCLOCK_E_SHMEM_OPEN);
  EXPECT_NE(
      testing::internal::GetCapturedStderr().find(reason), std::string::npos)
      << reason;
  EXPECT_EQ(fbclock_destroy(&lib), FBCLOCK_E_NO_ERROR);
}

} // namespace
