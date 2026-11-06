package mail

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/0xSMW/mail.app-cli/v3/internal/appleevents"
)

// moveBatchItems builds n move items from INBOX to All Mail on the fixture's
// Personal account, with consecutive IDs starting at 42.
func moveBatchItems(n int) []BatchItem {
	items := make([]BatchItem, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, BatchItem{
			ID:            fmt.Sprintf("%d", 42+i),
			Account:       "Personal",
			SourceMailbox: "INBOX",
			TargetMailbox: "All Mail",
		})
	}
	return items
}

// runFixtureMoveBatch runs one fixture-backed move batch with a durable
// receipt and the automation lock isolated to the test, and returns the
// receipt's decoded events alongside the result.
func runFixtureMoveBatch(t *testing.T, c *Client, opts BatchOptions, items []BatchItem, mutate Mutator) (BatchResult, []map[string]any, error) {
	t.Helper()
	t.Setenv("MAIL_APP_CLI_AUTOMATION_LOCK_PATH", filepath.Join(t.TempDir(), "lock"))
	journal, err := CreateBatchJournal(filepath.Join(t.TempDir(), "receipt.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	opts.Action = "move"
	opts.Receipt = journal
	result, err := RunBatch(c, opts, items, mutate)
	return result, readJournalEvents(t, journal.Path()), err
}

// groupMoveEvents are core/move events carrying the group dispatch form: a
// whose-clause selector over the source mailbox. Per-item moves carry a
// single reference.
func groupMoveEvents(f *nativeReadFixture) []appleevents.Event {
	var out []appleevents.Event
	for _, event := range eventsWithID(f, "move") {
		if isGroupMove(event) {
			out = append(out, event)
		}
	}
	return out
}

func itemMoveEvents(f *nativeReadFixture) []appleevents.Event {
	var out []appleevents.Event
	for _, event := range eventsWithID(f, "move") {
		if !isGroupMove(event) {
			out = append(out, event)
		}
	}
	return out
}

func TestRunBatchMoveGroupCollapsesHomogeneousChunk(t *testing.T) {
	f, c := moveGroupFixture(t, 42, 43, 44)
	result, events, err := runFixtureMoveBatch(t, c, BatchOptions{}, moveBatchItems(3), MoveMutator(false))
	if err != nil {
		t.Fatal(err)
	}
	if result.Attempted != 3 || result.Succeeded != 3 || result.Failed != 0 || result.Chunks != 1 {
		t.Fatalf("result = %+v", result)
	}
	moves := groupMoveEvents(f)
	if len(moves) != 1 {
		t.Fatalf("group moves = %d, want one over 3 references", len(moves))
	}
	assertGroupMoveSelector(t, f, moves[0], "mbxp:inbox", 42, 43, 44)
	reads := confirmReads(f)
	if len(reads) != 1 || len(reads[0].Params["----"].Items) != 3 {
		t.Fatalf("confirm reads = %d, want one over 3 references", len(reads))
	}
	// Account resolve plus two mailbox lookups, one bulk existence read, one
	// move: the same twenty-seven events the primitive dispatches, and a
	// fraction of the per-item path's twenty-seven per message.
	if got := len(f.events); got != 27 {
		t.Fatalf("group dispatch sent %d events, want 27", got)
	}
	// The receipt keeps its per-item shape around the one group transaction.
	sequence := []string{
		"started",
		"mutation_started", "mutation_started", "mutation_started",
		"bulk_move",
		"mutation_succeeded", "mutation_succeeded", "mutation_succeeded",
		"completed",
	}
	if len(events) != len(sequence) {
		t.Fatalf("journal events = %+v", events)
	}
	for i, want := range sequence {
		if got := events[i]["event"]; got != want {
			t.Fatalf("journal event %d = %v, want %s", i, got, want)
		}
	}
	bulk := events[4]
	if bulk["action"] != "move" || bulk["account"] != "Personal" ||
		bulk["sourceMailbox"] != "INBOX" || bulk["targetMailbox"] != "All Mail" ||
		bulk["count"] != float64(3) {
		t.Fatalf("bulk_move record = %+v", bulk)
	}
}

func TestRunBatchMoveThirtyMessagesShareOneTransaction(t *testing.T) {
	ids := make([]int64, 30)
	for i := range ids {
		ids[i] = 42 + int64(i)
	}
	f, c := moveGroupFixture(t, ids...)
	// ReuseBridge mirrors the CLI batch flags. The native backend must not
	// inherit the bridge session's 25-item chunk cap, or this batch would
	// split into two groups.
	result, events, err := runFixtureMoveBatch(t, c, BatchOptions{ReuseBridge: true}, moveBatchItems(30), MoveMutator(false))
	if err != nil {
		t.Fatal(err)
	}
	if result.Chunks != 1 || result.Succeeded != 30 || result.Attempted != 30 {
		t.Fatalf("result = %+v", result)
	}
	moves := groupMoveEvents(f)
	if len(moves) != 1 {
		t.Fatalf("group moves = %d, want one over 30 references", len(moves))
	}
	assertGroupMoveSelector(t, f, moves[0], "mbxp:inbox", ids...)
	if reads := confirmReads(f); len(reads) != 1 || len(reads[0].Params["----"].Items) != 30 {
		t.Fatalf("confirm reads = %d, want one over 30 references", len(reads))
	}
	// Twenty-seven events for thirty messages: the same cost as one
	// per-item move. The per-item path would send roughly eight hundred.
	if got := len(f.events); got != 27 {
		t.Fatalf("30-message group sent %d events, want 27", got)
	}
	if got := countJournalEvents(events, "bulk_move"); got != 1 {
		t.Fatalf("bulk_move records = %d, want 1", got)
	}
}

func TestRunBatchMoveGroupRejectionReplaysPerItemAndLatches(t *testing.T) {
	f, c := moveGroupFixture(t, 42, 43, 44, 45)
	f.fail = func(event appleevents.Event) error {
		if isGroupMove(event) {
			return &appleevents.Error{Code: -1708, Message: "no handler for group move", Attempted: true}
		}
		return nil
	}
	// Two chunks: the second must not offer the group form again after the
	// first group's refusal latched.
	result, events, err := runFixtureMoveBatch(t, c, BatchOptions{ChunkSize: 2}, moveBatchItems(4), MoveMutator(false))
	if err != nil {
		t.Fatal(err)
	}
	if result.Succeeded != 4 || result.Failed != 0 || result.Attempted != 4 {
		t.Fatalf("result = %+v", result)
	}
	if got := len(groupMoveEvents(f)); got != 1 {
		t.Fatalf("group move attempts = %d, want 1", got)
	}
	if got := len(itemMoveEvents(f)); got != 4 {
		t.Fatalf("per-item moves = %d, want 4", got)
	}
	if got := countJournalEvents(events, "bulk_move"); got != 1 {
		t.Fatalf("bulk_move records = %d, want 1", got)
	}
	if got := countJournalEvents(events, "mutation_succeeded"); got != 4 {
		t.Fatalf("mutation_succeeded records = %d, want 4", got)
	}
}

func TestRunBatchMoveGroupUncertainOutcomeRecordsUnknownsAndStops(t *testing.T) {
	f, c := moveGroupFixture(t, 42, 43, 44)
	f.fail = func(event appleevents.Event) error {
		if isGroupMove(event) {
			return fmt.Errorf("automation bridge vanished mid-event")
		}
		return nil
	}
	result, events, err := runFixtureMoveBatch(t, c, BatchOptions{}, moveBatchItems(3), MoveMutator(false))
	var uncertain *moveGroupUncertainError
	if !errors.As(err, &uncertain) {
		t.Fatalf("error %v is not an uncertain group outcome", err)
	}
	if result.Attempted != 3 || result.Failed != 3 || result.Succeeded != 0 {
		t.Fatalf("result = %+v", result)
	}
	for _, item := range result.Items {
		if item.Status != "unknown" {
			t.Fatalf("item %s status = %s, want unknown", item.ID, item.Status)
		}
	}
	// Uncertain messages are never replayed: the failed group move is the
	// only move event the run ever sent.
	if got := len(itemMoveEvents(f)); got != 0 {
		t.Fatalf("per-item moves after uncertain outcome = %d, want 0", got)
	}
	if got := countJournalEvents(events, "mutation_unknown"); got != 3 {
		t.Fatalf("mutation_unknown records = %d, want 3", got)
	}
	if got := countJournalEvents(events, "mutation_succeeded"); got != 0 {
		t.Fatalf("mutation_succeeded records = %d, want 0", got)
	}
	if got := events[len(events)-1]["event"]; got != "interrupted" {
		t.Fatalf("terminal event = %v, want interrupted", got)
	}
}

func TestRunBatchMoveGroupKeepsNotFoundClassification(t *testing.T) {
	f, c := moveGroupFixture(t, 42)
	items := moveBatchItems(2)
	items[1].ID = "999"
	result, events, err := runFixtureMoveBatch(t, c, BatchOptions{}, items, MoveMutator(false))
	var failed *BatchFailedError
	if !errors.As(err, &failed) || failed.Failed != 1 {
		t.Fatalf("error = %v", err)
	}
	if result.Items[0].ID != "42" || result.Items[0].Status != "succeeded" {
		t.Fatalf("confirmed item = %+v", result.Items[0])
	}
	if result.Items[1].ID != "999" || result.Items[1].Status != "failed" ||
		result.Items[1].Error != "message not found: 999" {
		t.Fatalf("missing item = %+v", result.Items[1])
	}
	// Only the confirmed reference was dispatched.
	moves := groupMoveEvents(f)
	if len(moves) != 1 {
		t.Fatalf("group moves = %d, want one over 1 reference", len(moves))
	}
	assertGroupMoveSelector(t, f, moves[0], "mbxp:inbox", 42)
	if got := countJournalEvents(events, "mutation_failed"); got != 1 {
		t.Fatalf("mutation_failed records = %d, want 1", got)
	}
	if got := countJournalEvents(events, "mutation_succeeded"); got != 1 {
		t.Fatalf("mutation_succeeded records = %d, want 1", got)
	}
}

func TestRunBatchMoveGroupInvalidIDFailsWithPerItemWords(t *testing.T) {
	// The middle ID is invalid; the valid neighbors 42 and 44 form the group.
	f, c := moveGroupFixture(t, 42, 44)
	items := moveBatchItems(3)
	items[1].ID = "not-a-number"
	result, events, err := runFixtureMoveBatch(t, c, BatchOptions{}, items, MoveMutator(false))
	var failed *BatchFailedError
	if !errors.As(err, &failed) || failed.Failed != 1 {
		t.Fatalf("error = %v", err)
	}
	// The invalid item dropped out before the group formed, with the same
	// words the per-item mutator fails it with.
	if result.Items[0].ID != "not-a-number" || result.Items[0].Status != "failed" ||
		result.Items[0].Error != `invalid message ID: "not-a-number"` {
		t.Fatalf("invalid item = %+v", result.Items[0])
	}
	moves := groupMoveEvents(f)
	if len(moves) != 1 {
		t.Fatalf("group moves = %d, want one over the 2 valid references", len(moves))
	}
	assertGroupMoveSelector(t, f, moves[0], "mbxp:inbox", 42, 44)
	if result.Succeeded != 2 {
		t.Fatalf("result = %+v", result)
	}
	if got := countJournalEvents(events, "mutation_failed"); got != 1 {
		t.Fatalf("mutation_failed records = %d, want 1", got)
	}
}

func TestRunBatchMoveGroupSingletonStaysPerItem(t *testing.T) {
	f, c := moveGroupFixture(t, 42)
	result, events, err := runFixtureMoveBatch(t, c, BatchOptions{}, moveBatchItems(1), MoveMutator(false))
	if err != nil {
		t.Fatal(err)
	}
	if result.Succeeded != 1 {
		t.Fatalf("result = %+v", result)
	}
	if got := len(groupMoveEvents(f)); got != 0 {
		t.Fatalf("group moves = %d, want 0", got)
	}
	if got := len(itemMoveEvents(f)); got != 1 {
		t.Fatalf("per-item moves = %d, want 1", got)
	}
	if got := countJournalEvents(events, "bulk_move"); got != 0 {
		t.Fatalf("bulk_move records = %d, want 0", got)
	}
}

func TestRunBatchMoveCustomMutatorKeepsPerItemPath(t *testing.T) {
	f, c := moveGroupFixture(t, 42, 43, 44)
	builtin := MoveMutator(false)
	custom := func(client *Client, item *BatchItem) error { return builtin(client, item) }
	result, _, err := runFixtureMoveBatch(t, c, BatchOptions{}, moveBatchItems(3), custom)
	if err != nil {
		t.Fatal(err)
	}
	if result.Succeeded != 3 {
		t.Fatalf("result = %+v", result)
	}
	// A mutator the runner cannot recognize is never bypassed, even for a
	// homogeneous native chunk.
	if got := len(groupMoveEvents(f)); got != 0 {
		t.Fatalf("group moves = %d, want 0", got)
	}
	if got := len(itemMoveEvents(f)); got != 3 {
		t.Fatalf("per-item moves = %d, want 3", got)
	}
}

func TestRunBatchMoveGroupProgressLines(t *testing.T) {
	f, c := moveGroupFixture(t, 42, 43, 44, 45)
	progress := &bytes.Buffer{}
	items := []BatchItem{
		{ID: "42", Account: "Personal", SourceMailbox: "INBOX", TargetMailbox: "All Mail"},
		{ID: "43", Account: "Personal", SourceMailbox: "INBOX", TargetMailbox: "All Mail"},
		{ID: "44", Account: "Personal", SourceMailbox: "All Mail", TargetMailbox: "INBOX"},
		{ID: "45", Account: "Personal", SourceMailbox: "All Mail", TargetMailbox: "INBOX"},
	}
	if _, _, err := runFixtureMoveBatch(t, c, BatchOptions{Progress: progress}, items, MoveMutator(false)); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"move: chunk 1/1 (4 messages)",
		"move: group 1/2 (2 messages)",
		"move: 1/4 42 succeeded",
		"move: 2/4 43 succeeded",
		"move: group 2/2 (2 messages)",
		"move: 3/4 44 succeeded",
		"move: 4/4 45 succeeded",
	}
	got := strings.Split(strings.TrimSpace(progress.String()), "\n")
	if len(got) != len(want) {
		t.Fatalf("progress = %q", progress.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("progress line %d = %q, want %q", i, got[i], want[i])
		}
	}
	if got := len(groupMoveEvents(f)); got != 2 {
		t.Fatalf("group moves = %d, want one per group", got)
	}
}

func TestRunBatchMoveGroupJournalOptionSucceeds(t *testing.T) {
	_, c := moveGroupFixture(t, 42, 43)
	result, events, err := runFixtureMoveBatch(t, c, BatchOptions{Journal: true}, moveBatchItems(2), MoveMutator(false))
	if err != nil {
		t.Fatal(err)
	}
	if result.Succeeded != 2 {
		t.Fatalf("result = %+v", result)
	}
	// The per-item envelope and location bookkeeping is best-effort; with
	// the envelope index disabled it fails silently and must not disturb
	// the group receipt shape.
	if got := countJournalEvents(events, "bulk_move"); got != 1 {
		t.Fatalf("bulk_move records = %d, want 1", got)
	}
	if got := countJournalEvents(events, "mutation_succeeded"); got != 2 {
		t.Fatalf("mutation_succeeded records = %d, want 2", got)
	}
}

func TestRunBatchMoveGroupConcurrentRunners(t *testing.T) {
	t.Setenv("MAIL_APP_CLI_DISABLE_ENVELOPE_INDEX", "1")
	t.Setenv("MAIL_APP_CLI_AUTOMATION_LOCK_PATH", filepath.Join(t.TempDir(), "lock"))
	const runners = 4
	receipts := make([]string, runners)
	for i := range receipts {
		receipts[i] = filepath.Join(t.TempDir(), "receipt.jsonl")
	}
	results := make([]BatchResult, runners)
	errs := make([]error, runners)
	var wg sync.WaitGroup
	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each runner gets its own fixture and client: the rejection
			// latch is per client state, never a process-wide shortcut.
			f := moveGroupFixtureData(42, 43)
			c := NewClient()
			c.shared.backend = BackendNative
			c.shared.nativeSender = f.sender
			journal, err := CreateBatchJournal(receipts[i])
			if err != nil {
				errs[i] = err
				return
			}
			defer journal.Close()
			results[i], errs[i] = RunBatch(c, BatchOptions{Action: "move", Receipt: journal}, moveBatchItems(2), MoveMutator(false))
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("runner %d: %v", i, errs[i])
		}
		if results[i].Succeeded != 2 {
			t.Fatalf("runner %d result = %+v", i, results[i])
		}
		events := readJournalEvents(t, receipts[i])
		if got := countJournalEvents(events, "bulk_move"); got != 1 {
			t.Fatalf("runner %d bulk_move records = %d, want 1", i, got)
		}
	}
}
