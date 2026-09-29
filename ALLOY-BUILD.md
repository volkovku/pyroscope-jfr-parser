# Alloy with Java thread metadata

This branch preserves the parser thread-info changes from
[grafana/pyroscope-jfr-parser#116](https://github.com/grafana/pyroscope-jfr-parser/pull/116)
at `b18082aa10d3e8812bdfdd886eb515fba4b4e35c`, plus the module adapter and build
recipe used to combine them with
[grafana/alloy#6725](https://github.com/grafana/alloy/pull/6725)
at `100cfccbc03519f6e29b58ca6bb54b4c63e108ef`.

The Alloy revision expects a separate `github.com/grafana/jfr-parser/pprof`
module. The added `pprof/go.mod` supplies that adapter. Both Alloy modules
(`go.mod` and `collector/go.mod`) must replace the root parser module and the
pprof module together. The script resolves the parser checkout at runtime;
there are no machine-specific source paths in this repository.

## Build on Linux amd64

Requirements: Git, Go with automatic toolchain downloads enabled, Make, network
access, and sufficient disk space for Alloy's dependency and build caches.
The script selects Go 1.26.5 and retains the pinned Alloy source revision.

```sh
git clone --branch alloy-thread-info-pr116 https://github.com/micr0farad/pyroscope-jfr-parser.git
cd pyroscope-jfr-parser
bash build-alloy.sh ../alloy-thread-info
```

The destination must not exist. The script tests the parser and Alloy Java
component, then builds `../alloy-thread-info/build/alloy-linux-amd64` with
`CGO_ENABLED=0`. This avoids a glibc dependency in the Alloy executable; its
embedded async-profiler library still has its own platform requirements.
UI asset rebuilding and collector code generation are skipped, matching the
earlier build. This is a profiling-agent build, not a verified rebuilt web UI.

To only prepare the patched Alloy checkout:

```sh
bash build-alloy.sh --prepare-only ../alloy-thread-info
```

To test the two parser modules independently:

```sh
go test -mod=readonly ./parser/...
(cd pprof && go test -mod=readonly ./...)
```

To keep thread names in sample labels without inserting synthetic stack frames,
use this block inside the existing `profiling_config` of `pyroscope.java`:

```alloy
thread {
  frame = false
  label_name = "thread_name"
}
```

The pinned thread-info patch handles CPU execution samples. Do not assume the
same thread metadata is present in allocation or lock profiles.

The recipe was recovered with AI assistance from the earlier local build.
It does not include private deployment configuration or publish a new binary.
