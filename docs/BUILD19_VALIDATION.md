# kownload build 19 validation

2026-10-26 · macOS · Apple Silicon

## Identity

- Application version: 1.9.3 (build 19).
- Core version: `1.9.3-kownload.8`.
- Core SHA-256: `1ae47c7609f078183f6235589513077a1092946b9ead1d5f74810fed6db483f9`.
- Flutter AOT SHA-256: `2714618142776d8f9f27828c7819342618f9287dff20875eea54862c666d666f`.
- Candidate and installed bundle: all 113 regular-file hashes match; strict deep ad-hoc signature verification passed. The app is not notarized.

## Fixes and checks

HTTP scheduling now rechecks an extended source cooldown and observes later queue arrivals after non-Range resume. Escaped JSON Cookie values are fully redacted. Exhausted rate-limit retries and an excessive Retry-After end in an error rather than being deferred again.

Eight previous reproductions, the new scheduling/redaction regressions and terminal-error checks passed. Final HTTP package race tests, other affected core packages, focused Downloader race tests and vet passed. Flutter: 151 tests passed. Whole-project Flutter analysis reports 0 errors, 67 warnings and 88 infos, mostly in existing generated bindings; this is not a clean analysis result.

The installed app completed 4 KiB and 16 MiB HTTP downloads, including pause/resume, with correct SHA-256. These tasks were submitted through the application's local API. The panel, settings and tutorial were inspected in the installed app; complete GUI submission/navigation, native directory selection, live browser/cloud/media flows and a fresh 60-minute stability run were not completed in this build's acceptance.

## Performance

Alternating independent build 18 / build 19 processes used memory storage and temporary output directories.

| Scenario | build 18 median | build 19 median | Change |
| --- | ---: | ---: | ---: |
| 100 same-origin TLS files, 3 pairs | 0.7878 s | 0.7889 s | +0.14% |
| 1,000 same-origin TLS files, 3 pairs | 3.0281 s | 3.0289 s | +0.03% |
| 32 MiB hidden Range with per-connection cap, 3 pairs | 4.1998 s | 4.1836 s | -0.39% |
| Token-bucket limited source, 5 pairs | 6.6333 s | 7.4091 s | +11.70% |

All 28 runs completed; all 6,706 file hashes were correct. The token-bucket scenario had 43.49% / 40.84% spread (range divided by median), so stable non-regression within 5% is not established. The earlier fixed-rejection-source timing deviation remains open; respecting Retry-After is retained.

## Existing-data preservation

Independent API snapshots before/after installation and after restart, plus direct cold-backup/database and Hive comparisons, matched: 38 tasks, 37 restore records, all configuration fields, 13 extensions, extension/protocol state, 176 extension-file hashes, three plist files and all existing Hive keys. Both existing task files retained their full hashes and metadata. Creation history and window state were unchanged. Only the two explicitly identified acceptance tasks were removed after the smoke test.

Raw local acceptance logs and user data backups are intentionally not included in the public repository.
