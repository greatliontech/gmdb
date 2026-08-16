# Platform Ports (darwin, windows)

- [x] 1. Darwin mmap: widen the pager and lock mmap shims to the unix
      family (`linux || darwin || freebsd`); `GOOS=darwin` and
      `GOOS=freebsd` build + vet green.
- [x] 2. Darwin durability: `F_FULLFSYNC` on darwin behind a new
      option defaulting to full-flush; `durability.md` platform
      durability contract; correct the fdatasync fallback's
      "fsync is strictly stronger" claim.
- [ ] 3. Darwin CI + soak: macOS test job (unit + `-race`, which
      carries the crash harness and cross-process tests; DST stays
      local-only per dst-testing.md); disposition the optional
      niceties (sysctl start time, boot-session UUID, madvise
      hints).
- [ ] 4. Windows flock: move `syscall.Flock` call sites behind a
      per-platform seam; `LockFileEx`/`UnlockFileEx` implementation;
      settle the windows platform rows in `cross-process.md` first.
- [ ] 5. Windows clock + process identity:
      `QueryUnbiasedInterruptTime` monotonic clock,
      `GetProcessTimes` start time, boot id zero (designed
      degradation); `GOOS=windows` build + vet green.
- [ ] 6. Windows mmap: `CreateFileMapping`/`MapViewOfFile` for the
      pager RO and lock RW mappings.
- [ ] 7. Windows filesystem semantics: publish/replace-path
      share-mode audit (`FILE_SHARE_DELETE` discipline); windows
      directory-durability ritual for the dirent guarantees.
- [ ] 8. Windows CI + soak; plan close-out (promote issue rationale
      into kept-current artifacts, delete both issue docs and their
      README rows, retarget surviving `Lands:` references).
