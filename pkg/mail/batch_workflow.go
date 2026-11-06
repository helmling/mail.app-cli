package mail

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

type batchRunner struct {
	client              *Client
	opts                BatchOptions
	result              *BatchResult
	items               []BatchItem
	chunkSize           int
	autoReceipt         bool
	archiveDestinations map[string]string
	nativeArchiveReady  bool
}

func (r *batchRunner) finish(retErr *error) {
	client, opts, result := r.client, r.opts, r.result

	if (opts.Verify || opts.MarkReadOnSkip) && *retErr != nil {
		for i := range result.Items {
			item := &result.Items[i]
			if (item.Status != "succeeded" && !(item.MarkReadOnSkip && item.MarkedRead)) || item.VerifyStatus != "" {
				continue
			}
			item.VerifyStatus = "applied_destination_unverified"
			if isUnknownMutationError(client, *retErr) {
				item.VerifyStatus = "unknown_after_timeout"
			}
			item.VerifyError = "verification not completed: " + (*retErr).Error()
			if err := r.recordVerification(*item); err != nil {
				result.JournalError = err.Error()
				*retErr = errors.Join(*retErr, err)
			}
		}
	}
	result.EndedAt = time.Now().Format(time.RFC3339)
	terminal := "completed"
	if client.Done() != nil || isUnknownMutationError(client, *retErr) || hasUnknownItem(result.Items) {
		terminal = "interrupted"
	}
	if err := opts.Receipt.Record(terminal, map[string]any{"result": result}); err != nil {
		result.JournalError = err.Error()
		*retErr = errors.Join(*retErr, err)
	}
	if r.autoReceipt {
		if err := opts.Receipt.Close(); err != nil {
			result.JournalError = err.Error()
			*retErr = errors.Join(*retErr, err)
		}
	}
}

func (r *batchRunner) prepare() error {
	client, opts, result, items := r.client, r.opts, r.result, r.items
	for i := range items {
		if opts.TargetMailbox != "" {
			items[i].TargetMailbox = opts.TargetMailbox
		}
	}
	r.chunkSize = normalizedChunkSize(len(items), opts.ChunkSize)
	if opts.ReuseBridge && opts.Action != "archive" && !client.nativeEnabled() {
		// The cap bounds one bridge process's lifetime. Native chunks never
		// open a bridge session, and one chunk lets a homogeneous move
		// collapse into a single group dispatch.
		r.chunkSize = min(r.chunkSize, 25)
	}
	result.Chunks = (len(items) + r.chunkSize - 1) / r.chunkSize
	if opts.Action == "archive" && !opts.TrustSource {
		items = client.archiveSources(items, opts.ExplicitSource)
	}
	if opts.Verify && !opts.DryRun {
		opts.Settle = relocationSettle(opts, items)
		if err := ValidateBatchOptions(opts); err != nil {
			return err
		}
	}
	r.opts, r.items = opts, items
	return nil
}

func (r *batchRunner) preview() error {
	client, opts, result, items := r.client, r.opts, r.result, r.items

	// A preview shows the same sources and skips the real run would use.
	for _, item := range items {
		if item.Status == "failed" {
			result.Failed++
			result.Items = append(result.Items, item)
			continue
		}
		item.Status = "dry-run"
		if skip := skipReason(client, opts, item); skip != "" {
			item.Status = "skipped"
			item.Error = skip
			item.MarkReadOnSkip = opts.MarkReadOnSkip && opts.MarkReadBefore && opts.Action == "archive"
		} else if opts.Action == "archive" && item.TargetMailbox == "" {
			if err := r.resolveArchiveDestination(client, &item); err != nil {
				item.Status, item.Error = "failed", "resolve archive destination: "+err.Error()
				result.Failed++
				result.Items = append(result.Items, item)
				if isVerificationTimeout(err) {
					return err
				}
				continue
			}
		}
		result.Skipped++
		result.Items = append(result.Items, item)
	}
	if result.Failed > 0 {
		return &BatchFailedError{Action: opts.Action, Failed: result.Failed, Attempted: len(items)}
	}
	return nil
}

func (r *batchRunner) dispatch(mutate Mutator) error {
	client, opts, result, items, chunkSize := r.client, r.opts, r.result, r.items, r.chunkSize
	for start := 0; start < len(items); start += chunkSize {
		end := min(start+chunkSize, len(items))
		chunkClient := client
		var session *jxaSession
		if !client.nativeEnabled() && opts.ReuseBridge && end-start > 1 && (opts.Action == "mark" || opts.Action == "flag" || opts.Action == "delete" || opts.Action == "move") {
			var err error
			session, err = newJXASession(client.Context())
			if err != nil {
				return err
			}
			defer session.Close()
			copy := *client
			copy.session = session
			chunkClient = &copy
		}
		if opts.Progress != nil {
			fmt.Fprintf(opts.Progress, "%s: chunk %d/%d (%d messages)\n", opts.Action, (start/chunkSize)+1, result.Chunks, end-start)
		}
		if r.moveGroupsEligible(mutate) {
			if err := r.dispatchMoveChunk(chunkClient, items[start:end], mutate); err != nil {
				return err
			}
		} else {
			for _, item := range items[start:end] {
				if err := r.applyItem(chunkClient, item, mutate); err != nil {
					return err
				}
			}
		}
		if session != nil {
			session.Close()
		}
	}
	return nil
}

func (r *batchRunner) applyItem(chunkClient *Client, item BatchItem, mutate Mutator) error {
	client, opts, result := r.client, r.opts, r.result

	if item.Status == "failed" {
		// Source resolution already refused this one.
		result.Attempted++
		return r.recordFailure(item, "mutation_failed", nil)
	}
	if err := client.Done(); err != nil {
		return err
	}
	if skip := skipReason(client, opts, item); skip != "" {
		item.Status = "skipped"
		item.Error = skip
		result.Skipped++
		if opts.Action == "archive" && opts.MarkReadOnSkip {
			item.MarkReadOnSkip = true
			if ready, err := r.markRead(chunkClient, &item, true); !ready {
				return err
			}
		}
		result.Items = append(result.Items, item)
		if err := opts.Receipt.Record("skipped", journalItem(item)); err != nil {
			return err
		}
		return nil
	}
	result.Attempted++
	if opts.Action == "archive" && item.TargetMailbox == "" {
		if err := r.resolveArchiveDestination(chunkClient, &item); err != nil {
			item.Status, item.Error = "failed", "resolve archive destination: "+err.Error()
			// No mutation was issued; a failed read is never an unknown archive.
			var stopErr error
			if isVerificationTimeout(err) {
				stopErr = err
			}
			return r.recordFailure(item, "preflight_failed", stopErr)
		}
	}
	useNativeArchive := false
	if opts.Action == "archive" {
		var err error
		useNativeArchive, err = chunkClient.archiveUsesNative(item.Account, item.SourceMailbox, item.TargetMailbox)
		if err != nil {
			item.Status, item.Error = "failed", "archive account preflight: "+err.Error()
			return r.recordFailure(item, "preflight_failed", err)
		}
	}
	if useNativeArchive && !r.nativeArchiveReady {
		if err := chunkClient.preflightNativeArchive(); err != nil {
			item.Status, item.Error = "failed", "native archive preflight: "+err.Error()
			return r.recordFailure(item, "preflight_failed", err)
		}
		r.nativeArchiveReady = true
	}
	if opts.MarkReadBefore && opts.Action != "mark" {
		if ready, err := r.markRead(chunkClient, &item, false); !ready {
			return err
		}
	}
	if err := opts.Receipt.Record("mutation_started", journalItem(item)); err != nil {
		item.Status = "failed"
		item.Error = "receipt durability before mutation: " + err.Error()
		return r.recordFailure(item, "", err)
	}
	return r.applyPendingMove(chunkClient, item, mutate)
}

// applyPendingMove mutates one item whose mutation_started record is already
// durable and maps the outcome onto the receipt. It is the tail of the
// per-item path and the group path's fallback for items the bulk move did
// not account for. The caller owns the attempt count.
func (r *batchRunner) applyPendingMove(chunkClient *Client, item BatchItem, mutate Mutator) error {
	mutateErr := mutate(chunkClient, &item)
	if journalErr := r.finishMutation(&item, mutateErr); journalErr != nil {
		return journalErr
	}
	if item.Status == "unknown" {
		return mutateErr
	}
	return nil
}

// finishMutation records item's mutation outcome through the standard receipt
// phases, progress line, and result counters. It writes the classification
// back through item and returns only a receipt-durability error, which stops
// the run immediately; item.Status == "unknown" afterwards means the caller
// must stop the run once its siblings are recorded.
func (r *batchRunner) finishMutation(item *BatchItem, err error) error {
	client, opts, result := r.client, r.opts, r.result
	if err != nil {
		phase := "mutation_failed"
		item.Status = "failed"
		if isUnknownMutationError(client, err) {
			item.Status = "unknown"
			phase = "mutation_unknown"
		}
		item.Error = err.Error()
		result.Failed++
		if journalErr := opts.Receipt.Record(phase, journalItem(*item)); journalErr != nil {
			result.Items = append(result.Items, *item)
			return journalErr
		}
		if item.Status == "unknown" {
			result.Items = append(result.Items, *item)
			return nil
		}
	} else {
		item.Status = "succeeded"
		if journalErr := opts.Receipt.Record("mutation_succeeded", journalItem(*item)); journalErr != nil {
			item.Status = "unknown"
			item.Error = "receipt durability after mutation: " + journalErr.Error()
			return r.recordFailure(*item, "", journalErr)
		}
		result.Succeeded++
	}
	if opts.Progress != nil {
		fmt.Fprintf(opts.Progress, "%s: %d/%d %s %s\n", opts.Action, result.Attempted, result.Matched, item.ID, item.Status)
	}
	result.Items = append(result.Items, *item)
	return nil
}

// moveGroupsEligible reports whether this run's chunks may collapse their
// moves into group dispatch: the built-in move mutator, before Mail has
// refused the list form. Both backends dispatch groups — the native
// automation transaction and the legacy one-script run — so only a custom
// mutator or another action keeps the per-item path.
func (r *batchRunner) moveGroupsEligible(mutate Mutator) bool {
	return r.opts.Action == "move" && isMoveMutator(mutate) &&
		!r.client.shared.moveGroupRejected.Load()
}

// dispatchMoveChunk applies the move action to one chunk through group
// dispatch: consecutive pending items sharing account, source, and target
// collapse into one bulk move; everything else keeps the per-item mutator.
// Every per-item record — skips, mark-read, the durable mutation_started —
// is written before any group transaction, so the receipt keeps its
// per-item shape.
func (r *batchRunner) dispatchMoveChunk(chunkClient *Client, chunk []BatchItem, mutate Mutator) error {
	pending, err := r.prepareMoveItems(chunkClient, chunk)
	if err != nil {
		return err
	}
	groups := groupMoveItems(pending)
	bulk := 0
	for _, group := range groups {
		if len(group.items) > 1 {
			bulk++
		}
	}
	index := 0
	for _, group := range groups {
		if len(group.items) == 1 || r.client.shared.moveGroupRejected.Load() {
			for _, item := range group.items {
				if err := r.applyGroupItem(chunkClient, item, mutate); err != nil {
					return err
				}
			}
			continue
		}
		index++
		if r.opts.Progress != nil {
			fmt.Fprintf(r.opts.Progress, "%s: group %d/%d (%d messages)\n", r.opts.Action, index, bulk, len(group.items))
		}
		if err := r.applyMoveGroup(chunkClient, group, mutate); err != nil {
			return err
		}
	}
	return nil
}

// prepareMoveItems runs the per-item bookkeeping that precedes any move
// mutation — failed passthrough, cancellation, skip classification,
// mark-read, and the durable mutation_started record — and returns the items
// still pending mutation, in chunk order. Items that drop out here are fully
// recorded; a non-nil error stops the run exactly as the per-item path
// would. Pending items are not counted as attempted yet: their count is
// deferred to the outcome so per-item progress keeps its ordinal.
func (r *batchRunner) prepareMoveItems(chunkClient *Client, chunk []BatchItem) ([]BatchItem, error) {
	client, opts := r.client, r.opts
	pending := make([]BatchItem, 0, len(chunk))
	for _, item := range chunk {
		if item.Status == "failed" {
			// Source resolution already refused this one.
			r.result.Attempted++
			if err := r.recordFailure(item, "mutation_failed", nil); err != nil {
				return nil, err
			}
			continue
		}
		if err := client.Done(); err != nil {
			return nil, err
		}
		if skip := skipReason(client, opts, item); skip != "" {
			item.Status = "skipped"
			item.Error = skip
			r.result.Skipped++
			r.result.Items = append(r.result.Items, item)
			if err := opts.Receipt.Record("skipped", journalItem(item)); err != nil {
				return nil, err
			}
			continue
		}
		if opts.MarkReadBefore && opts.Action != "mark" {
			if ready, err := r.markRead(chunkClient, &item, false); !ready {
				r.result.Attempted++
				if err != nil {
					return nil, err
				}
				continue
			}
		}
		if err := opts.Receipt.Record("mutation_started", journalItem(item)); err != nil {
			item.Status = "failed"
			item.Error = "receipt durability before mutation: " + err.Error()
			r.result.Attempted++
			return nil, r.recordFailure(item, "", err)
		}
		if opts.Journal {
			// The same pre-move envelope record moveItem writes, kept
			// per item: it is an index write, not automation.
			_ = chunkClient.RecordRecentEnvelope(item.Account, item.SourceMailbox, item.ID, "move")
		}
		if number, err := strconv.ParseInt(item.ID, 10, 64); err != nil || number <= 0 {
			// Neither group primitive can confirm a non-numeric ID, so it
			// drops out here with clean words instead of a confusing
			// not-found classification inside the group.
			r.result.Attempted++
			if journalErr := r.finishMutation(&item, fmt.Errorf("invalid message ID: %q", item.ID)); journalErr != nil {
				return nil, journalErr
			}
			continue
		}
		pending = append(pending, item)
	}
	return pending, nil
}

// moveGroupKey identifies the dispatch group of a move item: messages that
// share account, source, and destination are moved by one list event.
type moveGroupKey struct {
	account string
	source  string
	target  string
}

type moveGroup struct {
	key   moveGroupKey
	items []BatchItem
}

// groupMoveItems splits pending items into maximal runs sharing one move
// group key, in chunk order. Runs never span chunks, and items that dropped
// out during preparation no longer break a run.
func groupMoveItems(pending []BatchItem) []moveGroup {
	var groups []moveGroup
	for _, item := range pending {
		key := moveGroupKey{account: item.Account, source: item.SourceMailbox, target: item.TargetMailbox}
		if n := len(groups); n > 0 && groups[n-1].key == key {
			groups[n-1].items = append(groups[n-1].items, item)
			continue
		}
		groups = append(groups, moveGroup{key: key, items: []BatchItem{item}})
	}
	return groups
}

// applyMoveGroup sends one homogeneous move group through the backend's
// bulk primitive and maps its outcome onto the per-item receipt. Confirmed and
// moved items succeed; unconfirmed items failed as not found without ever
// being dispatched; Mail refusing the list form latches the refusal for the
// process and replays the rest per item; any other failure leaves the
// dispatched items unknown and stops the run.
func (r *batchRunner) applyMoveGroup(chunkClient *Client, group moveGroup, mutate Mutator) error {
	client, opts := r.client, r.opts
	if err := opts.Receipt.Record("bulk_move", map[string]any{
		"action":        opts.Action,
		"account":       group.key.account,
		"sourceMailbox": group.key.source,
		"targetMailbox": group.key.target,
		"count":         len(group.items),
	}); err != nil {
		// Durability first: a receipt that cannot show the dispatch shape
		// does not mutate.
		return err
	}
	ids := make([]string, 0, len(group.items))
	for _, item := range group.items {
		ids = append(ids, item.ID)
	}
	moved, missing, err := chunkClient.moveGroup(group.key.account, group.key.source, group.key.target, ids)
	movedSet := make(map[string]bool, len(moved))
	for _, id := range moved {
		movedSet[id] = true
	}
	missingSet := make(map[string]bool, len(missing))
	for _, id := range missing {
		missingSet[id] = true
	}

	if err != nil && errors.Is(err, errMoveGroupRejected) {
		// Nothing unaccounted for was applied, so per-item replay is safe —
		// and it keeps today's classification. Stop offering the list form
		// for the rest of the process.
		client.shared.moveGroupRejected.Store(true)
		for _, item := range group.items {
			if movedSet[item.ID] {
				// A sub-event completed before the refusal; its messages
				// provably moved.
				if ferr := r.finishMovedItem(&item); ferr != nil {
					return ferr
				}
				continue
			}
			if err := r.applyGroupItem(chunkClient, item, mutate); err != nil {
				return err
			}
		}
		return nil
	}

	var runErr error
	for _, item := range group.items {
		switch {
		case movedSet[item.ID]:
			if ferr := r.finishMovedItem(&item); ferr != nil {
				return ferr
			}
		case missingSet[item.ID]:
			// Never dispatched: the clean not-found classification survives.
			if ferr := r.recordGroupOutcome(&item, notFound("message", item.ID)); ferr != nil {
				return ferr
			}
		default:
			itemErr := err
			if itemErr == nil {
				itemErr = &moveGroupUncertainError{cause: fmt.Errorf("bulk move result omitted message %s", item.ID)}
			}
			// An uncertain or raw failure stops the run only after the
			// whole group's outcome is durable.
			if ferr := r.recordGroupOutcome(&item, itemErr); ferr != nil {
				return ferr
			}
			if item.Status == "unknown" {
				runErr = itemErr
			}
		}
	}
	return runErr
}

// applyGroupItem mutates one group item outside the bulk event — a singleton
// group or the per-item replay after Mail refused the list form — counting
// it as attempted only now, when its outcome is about to be recorded.
func (r *batchRunner) applyGroupItem(chunkClient *Client, item BatchItem, mutate Mutator) error {
	r.result.Attempted++
	return r.applyPendingMove(chunkClient, item, mutate)
}

// recordGroupOutcome counts one dispatched group item as attempted and
// records its outcome. It returns only a receipt-durability error, which
// stops the run immediately; an unknown classification is visible through
// item.Status afterwards, so the caller can record the group's remaining
// outcomes before stopping.
func (r *batchRunner) recordGroupOutcome(item *BatchItem, err error) error {
	r.result.Attempted++
	return r.finishMutation(item, err)
}

// finishMovedItem records a message the group transaction proved moved,
// including its recent-message location update when verb journaling is on.
func (r *batchRunner) finishMovedItem(item *BatchItem) error {
	r.result.Attempted++
	if r.opts.Journal {
		_ = UpdateRecentMessageLocation(item.Account, item.ID, item.TargetMailbox, "move")
	}
	return r.finishMutation(item, nil)
}

func (r *batchRunner) verify() error {
	client, opts, result := r.client, r.opts, r.result
	verifyCtx, verifyCancel := context.WithTimeout(client.Context(), effectiveVerifyBudget(opts))
	defer verifyCancel()
	verificationClient := client.WithContext(verifyCtx)

	if opts.Verify {
		if opts.Progress != nil {
			fmt.Fprintf(opts.Progress, "%s: verifying %d applied message(s)\n", opts.Action, result.Succeeded)
		}
		verified := VerifyMutations(verificationClient, opts, result.Items)
		for i := range verified {
			if verified[i].Status != "succeeded" {
				continue
			}
			result.Items[i] = verified[i]
			if err := r.recordVerification(verified[i]); err != nil {
				return err
			}
			if opts.Progress != nil {
				fmt.Fprintf(opts.Progress, "%s: verified %d/%d %s %s\n", opts.Action, i+1, len(verified), verified[i].ID, verified[i].VerifyStatus)
			}
		}
	}
	// Verify opted-in skipped marks together, sharing the overall verification
	// deadline and indexed snapshots. No archive is replayed for these items.
	return verifySkippedMarks(verificationClient, opts, result.Items, r.recordVerification)
}

// recordFailure retains the item before a supplemental journal failure can stop
// the run. An empty phase is used when the phase's durable write already failed.
func (r *batchRunner) recordFailure(item BatchItem, phase string, stopErr error) error {
	r.result.Failed++
	r.result.Items = append(r.result.Items, item)
	if phase != "" {
		if err := r.opts.Receipt.Record(phase, journalItem(item)); err != nil {
			return err
		}
	}
	return stopErr
}

// markRead shares the durable mark phase for skipped archives and pre-mutation
// marks. Its boolean distinguishes a recorded failure from permission to proceed.
func (r *batchRunner) markRead(client *Client, item *BatchItem, skippedArchive bool) (bool, error) {
	if err := r.opts.Receipt.Record("mark_read_started", journalItem(*item)); err != nil {
		return false, err
	}
	if err := client.MarkMessageAsRead(item.Account, item.SourceMailbox, item.ID, true); err != nil {
		item.Status = "failed"
		if skippedArchive {
			item.Error = "mark-read on skipped archive failed: " + err.Error()
		} else {
			item.Error = fmt.Sprintf("mark-read before %s failed: %v", r.opts.Action, err)
		}
		var stopErr error
		if isUnknownMutationError(r.client, err) {
			item.Status, stopErr = "unknown", err
		}
		return false, r.recordFailure(*item, "mark_read_failed", stopErr)
	}
	item.MarkedRead = true
	if err := r.opts.Receipt.Record("mark_read_succeeded", journalItem(*item)); err != nil {
		prefix := "receipt durability after mark-read: "
		if skippedArchive {
			prefix = "receipt durability after mark-read on skipped archive: "
		}
		item.Status, item.Error = "unknown", prefix+err.Error()
		return false, r.recordFailure(*item, "", err)
	}
	return true, nil
}

func (r *batchRunner) recordVerification(item BatchItem) error {
	if item.VerifyError != "" {
		r.result.Unverified++
	}
	return r.opts.Receipt.Record("verification_result", journalItem(item))
}

// Pin one successfully resolved destination per account for preview and dispatch.
func (r *batchRunner) resolveArchiveDestination(client *Client, item *BatchItem) error {
	destination, cached := r.archiveDestinations[item.Account]
	if !cached {
		var err error
		destination, err = client.ResolveArchiveMailbox(item.Account)
		if err != nil {
			return err
		}
		r.archiveDestinations[item.Account] = destination
	}
	item.TargetMailbox = destination
	return nil
}
