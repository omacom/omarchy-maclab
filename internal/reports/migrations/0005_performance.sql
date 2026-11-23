-- Performance (spec section 22, "Publishing and Counted"): the published
-- values of Performance runs and macOS results, which labd copies here from
-- labd.db each time they change, so that the public pages render without
-- labd.db. Only the fields bench.json carries, and only what is published:
-- a Volunteer's Mac only while its publishing is on, a Release only while it
-- is qualified or has no verdict yet. A deletion in labd.db is carried here.
--
-- A Mac is an opaque key here, never its name, with its kind (lab_owned,
-- counted or volunteer) and its Configuration. created_at is when it was
-- first copied here.
CREATE TABLE perf_macs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  mac_key TEXT NOT NULL CHECK (length(mac_key) = 32 AND mac_key NOT GLOB '*[^0-9a-f]*'),
  kind TEXT NOT NULL CHECK (kind IN ('lab_owned', 'counted', 'volunteer')),
  board TEXT NOT NULL,
  cpu_cores INTEGER NOT NULL CHECK (cpu_cores > 0),
  gpu_cores INTEGER NOT NULL CHECK (gpu_cores > 0),
  created_at INTEGER NOT NULL CHECK (created_at BETWEEN 1000000000000000 AND 99999999999999999)
) STRICT;
CREATE UNIQUE INDEX perf_macs_mac_key ON perf_macs (mac_key);

-- A Release is its kernel version and its verdict state, and nothing else of
-- it: an opaque key and its place among the Releases here, 1 the oldest
-- frozen.
CREATE TABLE perf_releases (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  release_key TEXT NOT NULL CHECK (length(release_key) = 32 AND release_key NOT GLOB '*[^0-9a-f]*'),
  kernel TEXT NOT NULL,
  verdict TEXT NOT NULL CHECK (verdict IN ('qualified', 'no verdict yet')),
  position INTEGER NOT NULL CHECK (position > 0),
  created_at INTEGER NOT NULL CHECK (created_at BETWEEN 1000000000000000 AND 99999999999999999)
) STRICT;
CREATE UNIQUE INDEX perf_releases_release_key ON perf_releases (release_key);

-- One current value per Mac, set, side, Release (Omarchy) or macOS build
-- (macOS) and metric: a value or a fixed reason code, never both. Booted is
-- the versions the Omarchy run booted, by package name. measured_at is when
-- labd took the value.
CREATE TABLE perf_values (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  mac_key TEXT NOT NULL,
  set_name TEXT NOT NULL,
  side TEXT NOT NULL CHECK (side IN ('omarchy', 'macos')),
  release_key TEXT NOT NULL,
  macos_version TEXT NOT NULL,
  macos_build TEXT NOT NULL,
  metric TEXT NOT NULL,
  value REAL,
  spread REAL,
  unit TEXT NOT NULL,
  api TEXT NOT NULL,
  capacity INTEGER,
  reason TEXT,
  booted TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(booted)),
  measured_at INTEGER NOT NULL CHECK (measured_at BETWEEN 1000000000000000 AND 99999999999999999),
  CHECK ((value IS NULL) != (reason IS NULL)),
  CHECK ((side = 'omarchy' AND release_key != '' AND macos_build = '' AND macos_version = '') OR (side = 'macos' AND release_key = '' AND macos_build != ''))
) STRICT;
CREATE UNIQUE INDEX perf_values_one ON perf_values (mac_key, set_name, side, release_key, macos_build, metric);
