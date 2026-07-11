# Recovery's self-durable rewrite assumes encode∘decode identity it never verifies

Lands: when a foreign or older-format writer can produce
checksum-valid metas with nonzero padding (post-v1 compatibility, or
any second implementation)

## Finding

**[L] The gated Open's self-durable recovery rewrite re-encodes the
SELECTED meta and pwrites it over its own slot assuming the bytes are
identical to what it read — the tear-safety of that rewrite rests on
`EncodeMeta∘DecodeMeta` being the identity, which fails for a
checksum-valid meta carrying nonzero padding** (`DecodeMeta` ignores
the padding bytes, `EncodeMeta` zeroes them, and the checksum covers
them): the rewrite would then place DIFFERENT valid bytes over the
sole durable carrier of the meta's own assertion — the changed-bytes
hazard the byte-identical design exists to avoid. Not reachable
in-spec today: every writer in this codebase zeroes padding, and
pre-v1 there are no foreign writers.

The live-peer tear-safe anchor gate closed the same latent dependency
with hard enforcement (a read-back memcmp that skips conservatively on
divergence — durability.md §Anchoring, "byte-identity is VERIFIED,
not assumed"). Recovery cannot simply skip: the rewrite IS its gate
for opening. Candidate shapes: memcmp and, on mismatch, fall back to
the recovery-commit path (publish a fresh meta to the OTHER slot —
tear-safe by dual-slot — instead of rewriting the carrier), or a
property test pinning encode∘decode identity over the meta codec.

## Provenance

Tear-safe anchor persist channel change-set review: the pattern was
found while hardening the live-peer gate; the recovery instance is
pre-existing and unreachable in-spec, deferred to the condition above.
