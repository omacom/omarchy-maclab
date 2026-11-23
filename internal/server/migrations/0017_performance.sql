-- Performance (spec section 22, "Performance run", "Kept"). A row of
-- perf_runs is one attempt at measuring one Benchmark set on one Mac: on
-- Omarchy (side omarchy) of a Release, or in macOS (side macos) at a macOS
-- build. Every attempt is kept, numbered from 1 per Mac, set, side and
-- Release or build. Booted is the versions the attempt booted, by exact
-- package name; conditions is bench-conditions.json; both JSON objects.
CREATE TABLE perf_runs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  mac TEXT NOT NULL,
  enrollment TEXT NOT NULL,
  set_name TEXT NOT NULL,
  side TEXT NOT NULL CHECK (side IN ('omarchy', 'macos')),
  release_version TEXT NOT NULL,
  macos_version TEXT NOT NULL,
  macos_build TEXT NOT NULL,
  list TEXT NOT NULL CHECK (list IN ('quick', 'full')),
  attempt INTEGER NOT NULL,
  state TEXT NOT NULL,
  booted TEXT NOT NULL DEFAULT '{}',
  boot_seconds REAL,
  conditions TEXT NOT NULL DEFAULT '{}',
  started INTEGER NOT NULL,
  ended INTEGER,
  UNIQUE (mac, set_name, side, release_version, macos_build, attempt)
) STRICT;

-- One result of an attempt per metric: an upload is taken once per attempt
-- and metric. A value or a fixed reason code, never both.
CREATE TABLE perf_results (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  run INTEGER NOT NULL REFERENCES perf_runs(id),
  metric TEXT NOT NULL,
  value REAL,
  spread REAL,
  unit TEXT NOT NULL,
  api TEXT NOT NULL,
  capacity INTEGER,
  fill REAL,
  reason TEXT,
  seconds REAL NOT NULL,
  cool_down REAL NOT NULL,
  taken INTEGER NOT NULL,
  UNIQUE (run, metric)
) STRICT;

-- The one current result per Mac, enrollment, set, side, Release or macOS
-- build (target) and metric: a Mac enrolled under a name used before starts
-- with none. An attempt that succeeds replaces it; one that fails only fills
-- a metric that has no result, or puts a value where there is none.
CREATE TABLE perf_current (
  mac TEXT NOT NULL,
  enrollment TEXT NOT NULL,
  set_name TEXT NOT NULL,
  side TEXT NOT NULL,
  target TEXT NOT NULL,
  metric TEXT NOT NULL,
  result INTEGER NOT NULL REFERENCES perf_results(id),
  PRIMARY KEY (mac, enrollment, set_name, side, target, metric)
) STRICT;
