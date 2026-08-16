# Platform Ports (darwin, windows)

- [x] 1. Darwin mmap: widen the pager and lock mmap shims to the unix
      family (`linux || darwin || freebsd`); `GOOS=darwin` and
      `GOOS=freebsd` build + vet green.
- [x] 2. Darwin durability: `F_FULLFSYNC` on darwin behind a new
      option defaulting to full-flush; `durability.md` platform
      durability contract; correct the fdatasync fallback's
      "fsync is strictly stronger" claim.
- [x] 3. Darwin CI + soak: macOS test job (unit + `-race`, which
      carries the crash harness and cross-process tests; DST stays
      local-only per dst-testing.md); disposition the optional
      niceties (sysctl start time, boot-session UUID, madvise
      hints).
- [x] 4. Windows flock: move `syscall.Flock` call sites behind a
      per-platform seam; `LockFileEx`/`UnlockFileEx` implementation;
      settle the windows platform rows in `cross-process.md` first.
- [x] 5. Windows clock + process identity:
      `QueryUnbiasedInterruptTime` monotonic clock; process liveness
      via `OpenProcess` for the IsAlive seam (classification stays
      heartbeat-only per cross-process.md — `GetProcessTimes`
      remains a PORT DESIGN row, matching the darwin sysctl
      disposition); boot id zero (designed degradation);
      `GOOS=windows` build + vet green.
- [x] 6. Windows mmap: placeholder-reservation model
      (`VirtualAlloc2`/`MapViewOfFile3`) for the pager RO mapping;
      fixed-size `CreateFileMapping` section for the lock RW mapping.
- [x] 7. Windows filesystem semantics: publish/replace-path
      share-mode audit (`FILE_SHARE_DELETE` discipline); windows
      directory-durability ritual for the dirent guarantees.
- [ ] 8. Windows CI + soak; plan close-out (promote issue rationale
      into kept-current artifacts, delete both issue docs and their
      README rows, retarget surviving `Lands:` references).
