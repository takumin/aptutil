# Change Log

All notable changes to this project will be documented in this file.

## [Unreleased]
### Added
- Publish prebuilt binaries for linux/amd64 and linux/arm64 on GitHub
  Releases, with SHA256 checksums, cosign signatures and build provenance.

### Changed
- Require Go 1.26 or later, and update dependencies.
- [mirror] `max_conns = 0` now means no limit as documented; negative
  `max_conns` is rejected by both commands instead of panicking.
- [cacher] Error responses no longer include internal error details;
  they are logged instead.
- [cacher] Keep idle upstream connections up to `max_conns`.

### Fixed
- [cacher] A downloaded item that failed checksum validation was treated
  as success and re-downloaded in a tight loop; it is now reported as 502.
- [cacher] Items larger than `cache_capacity` were evicted right after
  being cached and re-downloaded endlessly; they are now served without
  being cached.
- [cacher] Memory grew without bound because file info entries of
  superseded packages were never removed.
- [cacher] Indices retrieved via by-hash paths were neither validated
  nor parsed, so items listed in them were not validated either.
- [cacher] Memory grew without bound because file info entries of items
  not listed in any meta data file were never removed.
- [cacher] Cached items not listed in any meta data file were downloaded
  again after restart.
- [cacher] Calculating checksums of items loaded at startup read whole
  files into memory and blocked other requests.
- [cacher] Panics on requests without a path, on failures to create the
  cache directory, and on failures to cache an item.
- [cacher] Shutdown hung when a Release file remained in `meta_dir` for
  a prefix removed from the mapping.
- [cacher] Handlers kept waiting for downloads after clients disconnected.
- [cacher] A newer download result could be dropped by the invalidation
  timer of an older one.
- [mirror] Release files lacking some checksum fields caused requests
  for invalid by-hash paths.
- [mirror] Failures to flush `info.json` were ignored, which could leave
  a truncated index.
- [mirror] A broken symlink in `dir` stopped removing old mirrors.
- [mirror] Failed responses stayed open across retries, and the retry
  backoff could not be interrupted by cancellation.

### Security
- [cacher] Set `ReadHeaderTimeout` on the HTTP server to limit slow
  clients.

## [1.4.3] - 2026-09-07
### Added
- [mirror] support xz compressed metadata (#59).

### Changed
- [cacher] [mirror] send `Cache-Control` and `User-Agent` headers imitating
  the apt-get command (#67).
- Use GitHub Actions instead of CircleCI.

### Fixed
- [apt] accept Sources entries that have only `Checksums-Sha1` or
  `Checksums-Sha256` without the `Files` field (#68).
- [mirror] clean up temporary directories upon update failure (#61, #63).
- [mirror] initialize `http.Transport` by copying `http.DefaultTransport`
  for forward compatibility (#66).

### Security
- Reject repository metadata paths (Packages `Filename`, Release/Index
  checksums, Sources `Directory`/`Files`) that are absolute or escape the
  mirror directory via `..`, preventing directory traversal.  Base
  directories and file entries of index entries are validated
  separately, so an entry escaping its base via `..` is rejected even if
  it would resolve within the mirror directory, and `.`/`./` are now
  treated as unsafe file paths. (#73)

## [1.4.2] - 2020-12-23
### Changed
- Minor fixes
- Upgrade CI to go1.15

## [1.4.1] - 2018-11-16
### Changed
- Handle renaming of cybozu-go/cmd to [cybozu-go/well][well]
- Introduce support for Go modules

## [1.4.0] - 2018-03-02
### Changed
- No notable changes since RC1.

## [1.4.0rc1] - 2018-01-19
### Changed
- Do not consume too much memory when downloading large files (#14, #28).
- [mirror] could not find Release/InRelease if suites=["/"] (#25, #26).
- [cacher] create cache directories automatically (#29).  
  Contributed by @jacksgt.
- [cacher] prevent panic for URL whose path is a mapping prefix (#30).

## [1.3.2] - 2017-09-01
### Changed
- [mirror] file modes of by-hash indices were erroneously 0600.

## [1.3.1] - 2017-08-21
### Changed
- [mirror] failed to detect by-hash support in some cases (#21).

## [1.3.0] - 2017-08-02
### Added
- [mirror] support by-hash index acquisition (#15, #16).

### Changed
- [cacher] workaround for bad contents in Release (#13, #17).

## [1.2.2] - 2016-08-31
### Changed
- Check errors of wrong log configurations.

## [1.2.1] - 2016-08-24
### Changed
- Fix for the latest cybozu-go/cmd.

## [1.2.0] - 2016-08-21
### Added
- aptuitl now adopts [github.com/cybozu-go/cmd][cmd] framework.  
  As a result, commands implement [the common spec][spec].
- [cacher] added `listen_address` configuration parameter to specify listening address (#9).
- [cacher] added `log` configuration section to specify logging options.
- [mirror] added `log` configuration section to specify logging options.

### Changed
- aptutil now requires Go 1.7 or better.

### Removed
- [cacher] `-s` and `-l` command-line flags are gone.
- [mirror] `-s` command-line flag is gone.

## [1.1.0]
### Changed
- Update docs (kudos to @xipmix).
- [cacher] extend Release file check interval from 15 to 600 seconds (#8).

## [1.0.1]
### Changed
- [mirror] ignore Sources if `include_source` is not specified in mirror.toml.  
  This works as a workaround for some badly configured web servers.


[well]: https://github.com/cybozu-go/well
[cmd]: https://github.com/cybozu-go/cmd
[spec]: https://github.com/cybozu-go/cmd/blob/master/README.md#specifications
[Unreleased]: https://github.com/takumin/aptutil/compare/v1.4.3...HEAD
[1.4.3]: https://github.com/cybozu-go/aptutil/compare/v1.4.2...v1.4.3
[1.4.2]: https://github.com/cybozu-go/aptutil/compare/v1.4.1...v1.4.2
[1.4.1]: https://github.com/cybozu-go/aptutil/compare/v1.4.0...v1.4.1
[1.4.0]: https://github.com/cybozu-go/aptutil/compare/v1.4.0rc1...v1.4.0
[1.4.0rc1]: https://github.com/cybozu-go/aptutil/compare/v1.3.2...v1.4.0rc1
[1.3.2]: https://github.com/cybozu-go/aptutil/compare/v1.3.1...v1.3.2
[1.3.1]: https://github.com/cybozu-go/aptutil/compare/v1.3.0...v1.3.1
[1.3.0]: https://github.com/cybozu-go/aptutil/compare/v1.2.2...v1.3.0
[1.2.2]: https://github.com/cybozu-go/aptutil/compare/v1.2.1...v1.2.2
[1.2.1]: https://github.com/cybozu-go/aptutil/compare/v1.2.0...v1.2.1
[1.2.0]: https://github.com/cybozu-go/aptutil/compare/v1.1.0...v1.2.0
[1.1.0]: https://github.com/cybozu-go/aptutil/compare/v1.0.1...v1.1.0
[1.0.1]: https://github.com/cybozu-go/aptutil/compare/v1.0.0...v1.0.1
