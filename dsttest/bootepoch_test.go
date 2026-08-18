//go:build dst

package dsttest

import (
	"context"
	"os"
	"runtime"
	"testing"
	"testing/simulation"
	"time"

	"github.com/greatliontech/gmdb"
)

// Boot-epoch suite (cross-process.md §BootID; docs/specs/dst-testing.md
// §Suites): cross-boot invalidation walked end-to-end over a real power
// cycle. The construction makes the adoption reset the ONLY possible
// recovery: the wedged reader slot belongs to a CROSS-NAMESPACE process
// of the dead boot, so the pid leg is meaningless (namespace inodes
// differ — probes route to the heartbeat), and the heartbeat leg reads
// the pre-crash stamp as the NEW boot's FUTURE (uptime clocks reset at
// reboot; future stamps are fresh forever, by design — the underflow
// guard). Only the boot-id comparison knows the boot that wrote the
// slot is gone. A durable stale slot is realistic: the kernel's
// writeback flushes dirty lock-file pages within seconds on a live
// machine, modeled here by an explicit sync before the power cut.
func TestSimulationBootEpochResetUnwedgesReaders(t *testing.T) {
	opts := smallOpts
	opts.MaxReaders = 1
	for _, seed := range []uint64{7, 8} { // anchor seeds (spec §Seed policy)
		simulation.Test(t, seed, func(t *testing.T) {
			simulation.Host("m", simulation.HostConfig{}, func() {
				simulation.Process("setup", func() {
					ctx := context.Background()
					db, err := gmdb.Open(ctx, "/db", opts)
					if err != nil {
						t.Fatalf("seed %d: Open: %v", seed, err)
					}
					defer db.Close()
					if err := db.Update(ctx, func(tx *gmdb.Tx) error {
						ks, e := tx.CreateKeyspace("k")
						if e != nil {
							return e
						}
						return ks.Put([]byte("a"), []byte("v"))
					}); err != nil {
						t.Fatalf("seed %d: Update: %v", seed, err)
					}
				})
				go simulation.ProcessWith("pinner", simulation.ProcessConfig{PIDNamespace: "container-a"}, func() {
					ctx := context.Background()
					db, err := gmdb.Open(ctx, "/db", opts)
					if err != nil {
						t.Errorf("seed %d: pinner Open: %v", seed, err)
						return
					}
					rtx, err := db.BeginRead(ctx)
					if err != nil {
						t.Errorf("seed %d: pinner BeginRead: %v", seed, err)
						return
					}
					if err := os.WriteFile("/pinned", nil, 0o600); err != nil {
						t.Errorf("seed %d: signal: %v", seed, err)
						return
					}
					time.Sleep(time.Hour) // slot held until the power cut
					// Held, not leaked: an unreferenced ReadTx's leak-detection
					// cleanup would release the slot under the deterministic GC.
					runtime.KeepAlive(rtx)
					t.Errorf("seed %d: pinner survived the host crash", seed)
				})
				simulation.Process("writeback", func() {
					mustAwait(t, "/pinned")
					// The kernel's periodic writeback, modeled: the dirty
					// lock-file pages (the pinned slot's TxnID residue, the
					// boot-1 epoch stamp) reach the platter before power is
					// lost, so the stale state survives into boot 2.
					f, err := os.OpenFile("/db.lock", os.O_RDWR, 0)
					if err != nil {
						t.Fatalf("seed %d: open lock file: %v", seed, err)
					}
					defer f.Close()
					if err := f.Sync(); err != nil {
						t.Fatalf("seed %d: writeback sync: %v", seed, err)
					}
				})
			})
			simulation.CrashHost("m")
			simulation.Host("m", simulation.HostConfig{}, func() {
				simulation.Process("adopter", func() {
					ctx := context.Background()
					db, err := gmdb.Open(ctx, "/db", opts)
					if err != nil {
						t.Fatalf("seed %d: post-reboot Open: %v", seed, err)
					}
					defer db.Close()
					// The single reader slot was wedged by the dead boot's
					// cross-namespace pinner. BeginRead succeeds only if the
					// adoption's boot-epoch reset cleared it — no other
					// classifier can: the pid leg is cross-namespace and the
					// heartbeat stamp reads as this boot's future.
					rtx, err := db.BeginRead(ctx)
					if err != nil {
						t.Fatalf("seed %d: post-reboot BeginRead (needs the epoch reset): %v", seed, err)
					}
					// The durable data survived the same power cut intact.
					ks, err := rtx.OpenKeyspaceReadOnly("k")
					if err != nil {
						t.Fatalf("seed %d: OpenKeyspaceReadOnly: %v", seed, err)
					}
					if v, err := ks.Get([]byte("a")); err != nil || string(v) != "v" {
						t.Fatalf("seed %d: Get = %q, %v; want v", seed, v, err)
					}
					// Release the single slot before Check — it needs its own
					// read snapshot.
					if err := rtx.Rollback(); err != nil {
						t.Fatalf("seed %d: Rollback: %v", seed, err)
					}
					for issue := range db.Check() {
						t.Errorf("seed %d: Check: %+v", seed, issue)
					}
				})
			})
		})
	}
}
