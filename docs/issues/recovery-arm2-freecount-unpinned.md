# Recovery-commit arm's derived free-page count is unpinned

Lands: when a test constructs the RecoverToDurable recovery-commit
arm (non-self-durable or divergent-carrier selected meta) with a
bitmap whose free count differs from the adopted sub-record's.

Mutation `rm.NumFreePages = p.bitmap.NumFree()` → not-derived
SURVIVES the suite: the DST fsyncgate anchor exercises only the
self-durable byte-identical arm. The behavior (durability.md
§Anchoring: "the recovery-commit arm additionally derives its
published free-page count from the bitmap it attached") needs a
pin — natural home: a pager unit test driving RecoverToDurable
with a SyncLazy-shaped meta pair and a divergent bitmap, or a DST
anchor whose pre-crash history ends in a SyncLazy commit.
