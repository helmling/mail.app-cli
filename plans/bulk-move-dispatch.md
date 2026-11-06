# Bulk move dispatch plan

A bulk move of N messages between the same source and destination currently
costs one Mail transaction per message. Each `nativeMove`
(`pkg/mail/native_actions.go:33`) re-resolves the account, walks the account's
mailbox hierarchy twice through `mailboxRefs`
(`pkg/mail/native_lookup.go:59` — once for the source, once for the
destination), checks message existence with one `getd` (`message`,
`native_lookup.go:261`), and then sends one `core/move`. That is roughly 8-15
serialized Apple Event round trips per message, so a 30-message `batch move`
performs 250-450 round trips. Mail's own bulk move is one internal operation
over a list of messages, which is why the CLI is dramatically slower and why
Mail's activity window shows the CLI's moves arriving as many small, restarting
batches instead of one "Moving 1 of 30".

This plan collapses a homogeneous move group to a handful of events by sending
one `core/move` whose direct object is a single whose-clause selector: every
message of the source mailbox whose ID equals one of the confirmed IDs, joined
by OR. The original design sent a list of message references — the operation
Mail's own interface appears to perform — but live probing proved Mail refuses
a list direct object for `move` (-1700, encoder-independent across native
Apple Events, JXA, and AppleScript), while the whose-clause selector is
accepted. The remaining wall time is Mail's irreducible copy work into the
destination, identical to a native drag move.

## Scope and invariants

- Serialize every Mail transaction: one group dispatch is one automation
  transaction holding both the process gate and the cross-process lock
  (`withNativeAutomation`, `pkg/mail/native_backend.go:57`).
- Bound every dispatch: the group event receives a size-scaled timeout and the
  existing phase-trace machinery.
- Preserve the explicit unknown outcome: a failed or timed-out group move marks
  its items unknown; reconciliation stays read-only and never replays a
  mutation (at-most-once discipline from `plans/native-mail-v3.md`).
- Preserve per-item durable receipts and per-item verification by stable
  identity (`VerifyMutations` already matches identity, not local IDs, which
  Mail regenerates on move).
- No CLI surface changes: `RunBatch`, `Mutator`, and all flags stay compatible.
  `batch move`, the `move` verb, and the TUI move action benefit without new
  flags; `.surface` stays unchanged.
- Archive stays out of scope: Gmail Inbox archive keeps Mail's Archive action
  and its viewer path. Only plain `move` groups.

## Design

### Grouping

A group is a maximal run of items sharing `(Account, SourceMailbox,
TargetMailbox)` within one dispatch chunk. Groups never span chunks, so chunk
semantics (progress lines, journal granularity, legacy session reuse) are
unchanged. With the default chunk (all items) a same-source/same-target
`batch move` is exactly one group. Items skipped by `skipReason` drop out
before groups form.

### Native group dispatch

Per group, inside one `withNativeAutomation` transaction:

1. Resolve account once (`n.account`), source mailbox once, destination
   mailbox once (same alias logic as today, `n.mailbox`). Replaces 2N mailbox
   walks with 2.
2. Build the selector without per-item events: one ID-equality comparison per
   item — `appleevents.Comparison("=   ", appleevents.Property("ID  ",
   appleevents.Examined()), appleevents.Int(id))` — joined with
   `appleevents.Logical("OR  ", ...)` and wrapped in
   `appleevents.ByTest("mssg", ...)` over the source mailbox. Message IDs are
   numeric and validated the same way `n.message` validates them today;
   missing IDs simply never match the selector.
3. One grouped existence `getd` asking for the `ID  ` property of every
   message of the source whose ID equals one of the requested IDs — the
   read-side twin of the move's selector, and the only grouped read shape
   live Mail accepts: a list direct object is refused for reads exactly as
   it is for moves (-1700), which degrades the whole phase to per-item
   reads. IDs present in the reply are confirmed; non-matching IDs simply
   never match the selector and classify missing. If Mail refuses the
   grouped read or answers in an unusable shape, fall back to per-item
   existence checks for this group only, which preserves today's clean
   "message not found" classification; a transport-level failure is
   returned without fallback.
4. One `core/move` with `----` = the whose-clause selector over the confirmed
   IDs and `insh` = the destination, sent through `eventWithTimeout` with the
   group timeout.

The event count is flat in group size: account resolve plus two hardened
mailbox walks (the walks scale with the account's mailbox tree — 27 events
total in the test fixture, 91 on a live 177-mailbox account), then one
grouped existence read plus one move. The per-item path costs roughly 25
events per message on top of the same resolve.

### Group cap and splitting

Cap the selector's ID terms per move event (100 initially). Larger groups
split into consecutive sub-events within the same transaction; each sub-event
is its own bounded dispatch with its own outcome mapping. Even at the cap this
is two orders of magnitude fewer round trips than per-message dispatch.

### Timeout scaling

Group budget = `defaultAutomationTimeout` (30s) plus 2s per message beyond the
first, capped at 5 minutes (the same ceiling `ValidateBatchOptions` enforces
for `--settle`). Apply it both to the `withNativeAutomation` budget and to the
move event's own timeout so a large group is not killed while Mail is still
working through the list.

### Outcome mapping

- Bulk move succeeds → every confirmed item records `succeeded` through the
  existing per-item bookkeeping.
- An item's reference was not confirmed in step 3 → `failed` with "message not
  found"; no mutation was issued for it.
- The move event fails in a way that proves nothing was applied (for example
  Mail rejects the grouped form, or a parameter error precedes execution) → the
  runner falls back to the per-item `MoveMutator` for that group, at most once
  per process; later groups skip the bulk path.
- The move event times out or its reply is lost → every item in the group is
  `unknown` (`mutation_unknown` journal phase, `isUnknownMutationError`,
  `unknown_after_timeout` in `finish()`). No replay. Verification, when
  enabled, classifies each item by stable identity from per-scope snapshots,
  exactly as it does today; partial application of a group lands in machinery
  that already exists.

### Receipts, journal, and progress

- `mutation_started` is journaled per item before the group dispatch, exactly
  as `applyItem` does today.
- Per-item outcome records (`succeeded`, `mutation_failed`, `mutation_unknown`,
  `skipped`) are written after the group result. The journal gains one
  group-level record (`bulk_move` with the group key and item count) so a
  receipt shows the dispatch shape.
- `--progress` prints one line per group before dispatch
  (`move: group 1/2 (30 messages)`) and keeps the existing per-item outcome
  lines after it.
- `RecordRecentEnvelope` (verb journaling) stays per item; it is an index
  write, not automation, and does not lengthen the transaction.
- `MarkReadBefore` stays per item in this plan; batching it is a follow-up.

### Mutator plumbing without breaking the API

`MoveMutator` previously returned a `Mutator` closure. It returns the named
top-level funcs `moveMutatorNoJournal`/`moveMutatorJournal`, recognized by
function-code pointer (`moveMutatorCodes`/`isMoveMutator`): an inlined
closure literal gets a fresh code symbol per call site, so closure
recognition depended on where the caller was compiled. Callers that only
pass it to `RunBatch` are unaffected; `batchRunner.dispatch` recognizes the
built-in mutator when the action is `move`, on either backend, and
consecutive items share the group key. Single items, mixed keys, and the
fallback path use the per-item `Mutator` unchanged.

### Legacy backend

One script per group replaces one process per message: resolve each ID
(`messageById`-style lookup, classify missing ones), move the confirmed IDs to
the destination in the whose form (`mail.move(mbox.messages.whose({_or: [...]},
{to: destMbox})`), and return per-ID status in one run. The `ReuseBridge`
session is reused when active. Semantics mirror the native outcome mapping,
including the unknown classification for a lost process.

## Release 1: native bulk move primitive

- [x] Add `nativeMoveGroup` next to `nativeMove`: shared account/mailbox
      resolution, whose-clause selector building, grouped existence read,
      single `core/move`, alias-aware destination, scaled timeout, phase
      records (`resolve_group`, `confirm_refs`, `bulk_move`).
- [x] Add the group cap with consecutive splitting inside one transaction.
- [x] Add rejection-versus-uncertain error classification.
- [x] Unit tests with the fake descriptor transport
      (`c.shared.nativeSender`, the pattern used throughout
      `pkg/mail/native_*_test.go`): all refs confirmed, one missing,
      grouped form rejected, timeout mid-group, oversized group splitting,
      progress and phase traces.
- [x] Live probe test, env-gated like the existing probes
      (`internal/appleevents/permission_timing_probe_test.go` style): moves a
      bounded set of user-authorized disposable messages supplied through
      explicit fixture paths, asserts the group's `BackendStatistics()`
      native event count stays under a small bound, verifies stable identities
      land in the destination, and records wall-time medians. Fail-closed
      without the environment fixtures; no private account data in output.

## Release 2: batch runner integration

- [x] Convert `MoveMutator` to the named type with `moveGroup`; keep the
      per-item `Mutator` implementation as the fallback.
- [x] Group consecutive same-key items per chunk in `dispatch()`
      (`pkg/mail/batch_workflow.go:118`), running the existing per-item skip,
      journal, and `MarkReadBefore` bookkeeping before the group transaction.
- [x] Wire per-item outcome records, the `bulk_move` journal record, group
      progress lines, and the unknown classification through `finish()`.
- [x] Fallback triggers: singleton groups, mixed keys, Mail rejecting the
      grouped form (once per process), and any probe-discovered incompatibility.
- [x] Tests: receipts and journal contents for a group run, unknown items
      after a simulated timeout, verification classifying a partially applied
      group by identity, `--progress` output, chunk-boundary grouping, and the
      non-cgo compatibility suite.
- [x] Race tests over concurrent batch runners; `.surface` snapshot verified
      unchanged (regenerate only if a flag ever appears).

## Release 3: legacy group script

- [x] One JXA script per group with per-ID resolution, whose-clause move, and
      per-ID status, reusing the existing bridge session when `ReuseBridge` is
      set.
- [x] Unit tests using the fake `osascript` fixtures (the
      `pkg/mail/health_test.go` pattern), covering missing IDs, process
      timeout (unknown, no replay), and status parsing.

## Release 4: follow-ups (optional, separate changes)

- [ ] Group dispatch for `delete` (`core/delo` on a whose-clause selector)
      and `mark`/`flag` (`core/setd` on a whose-clause selector with one
      value), following the same grouping and outcome rules.
- [ ] Batch `MarkReadBefore` with its group.
- [ ] Revisit Gmail Inbox archive bulk only after the Archive-action semantics
      are understood for multi-message selections.

## Measurement and acceptance

Follow the mutation-latency protocol from
`plans/native-mail-v3-validation.md`: paired observations against identical
Mail state, disposable user-authorized messages, medians reported without
message content.

- A 30-message same-source/same-target `batch move` is O(1) in group size
  (receipt-observable via `BackendStatistics`): one grouped confirm read and
  one move beyond the account/mailbox resolve — a dispatch tail of at most 4
  events, probe-calibrated because the resolve scales with the account's
  mailbox tree — versus the per-item path's full resolve per message.
- Wall time moves into the same ballpark as Mail's own drag move for the same
  messages; the copy into the destination remains the floor and is not
  expected to improve.
- Mail's activity window shows a single "Moving 1 of 30" progress during the
  batch instead of restarting counters. This is the user-visible acceptance
  signal and is recorded manually in the validation notes; it cannot be
  asserted automatically.
- The full Go suite, vet, race tests, non-cgo compatibility suite, and surface
  snapshot pass; existing mutation tests keep their semantics.

## Risks

- Mail could reject the grouped form of `core/move`. This happened: a list
  direct object is refused with -1700 on every transport tried (native Apple
  Events, JXA, AppleScript), encoder-independent. The redesign ships the
  whose-clause selector, which live probing proved Mail accepts and which
  moved real messages both directions; if any Mail build refuses that too,
  the per-item fallback preserves today's behavior with no user-visible
  regression beyond the status quo.
- A bulk existence read may fail wholesale when one reference is missing. The
  design already falls back to per-item confirmation for that group, so
  not-found classification is preserved.
- Partial application before a failure or timeout is coarser than per-item
  dispatch. It is absorbed by the unknown classification plus identity-based
  verification, which is the existing reconciliation path for uncertain
  mutations.
- Very large groups increase descriptor size and Mail's evaluation cost. The
  100-reference cap bounds this; splitting keeps events bounded.
- Expectation setting: IMAP copy time for large messages or slow servers
  dominates after the fix, exactly as it does for Mail's own UI.
