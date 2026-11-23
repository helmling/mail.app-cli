package mail

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/0xSMW/mail.app-cli/v3/internal/appleevents"
)

// moveGroupFixture extends the shared native read fixture with messages whose
// only property is the ID, which is everything the group confirm read and the
// per-item fallback look up.
func moveGroupFixture(t *testing.T, ids ...int64) (*nativeReadFixture, *Client) {
	t.Helper()
	f := moveGroupFixtureData(ids...)
	return f, fixtureClient(t, f)
}

// moveGroupFixtureData builds the group fixture without touching the
// environment, so background goroutines can construct their own. The
// requested ids become the source mailbox's entire message list: the grouped
// existence read iterates that list, so every confirmable message must be a
// member of it.
func moveGroupFixtureData(ids ...int64) *nativeReadFixture {
	f := newNativeReadFixture()
	inbox := make([]appleevents.Value, 0, len(ids))
	for _, id := range ids {
		key := fmt.Sprintf("mssg:%d", id)
		f.properties[key] = map[string]appleevents.Value{"ID  ": appleevents.Int(id)}
		inbox = append(inbox, fixtureRef("mssg", strconv.FormatInt(id, 10)))
	}
	// Group tests move in both directions between the fixture's two
	// mailboxes, and the grouped existence read confirms membership per
	// source mailbox, so members register in both; whose selectors are
	// ID-scoped, so cross-membership can never match a foreign group.
	f.messages["mbxp:inbox"] = inbox
	f.messages["mbxp:archive"] = inbox
	return f
}

func eventsWithID(f *nativeReadFixture, id string) []appleevents.Event {
	var out []appleevents.Event
	for _, event := range f.events {
		if event.ID == id {
			out = append(out, event)
		}
	}
	return out
}

// confirmReads are the grouped existence reads: one getd whose direct object
// is the ID property of a whose-clause message selector over the source.
func confirmReads(f *nativeReadFixture) []appleevents.Event {
	var out []appleevents.Event
	for _, event := range eventsWithID(f, "getd") {
		if isGroupConfirmRead(event) {
			out = append(out, event)
		}
	}
	return out
}

// isGroupConfirmRead reports whether a getd event is the grouped existence
// read: the ID property of a whose-clause message selector. Only that shape
// qualifies — the mailbox walk also reads properties but over plain
// containers, and account resolution reads a test-form selector of accounts.
func isGroupConfirmRead(event appleevents.Event) bool {
	if event.Class != "core" || event.ID != "getd" {
		return false
	}
	direct := event.Params["----"]
	if direct.Kind != "object" || direct.Fields["want"].Code != "prop" || direct.Fields["seld"].Code != "ID  " {
		return false
	}
	container := direct.Fields["from"]
	return container.Kind == "object" && container.Fields["want"].Code == "mssg" && container.Fields["form"].Code == "test"
}

// isGroupMove reports whether a core/move event carries the group dispatch
// form: a whose-clause selector instead of one per-item reference. Only the
// move event itself qualifies — account resolution and searches also send
// test-form selectors, but on reads.
func isGroupMove(event appleevents.Event) bool {
	if event.Class != "core" || event.ID != "move" {
		return false
	}
	direct := event.Params["----"]
	return direct.Kind == "object" && direct.Fields["form"].Code == "test"
}

// assertGroupMoveSelector checks a group move's direct object: one
// whose-clause selector over the source mailbox whose ID-equality terms
// cover exactly the confirmed ids, in dispatch order.
func assertGroupMoveSelector(t *testing.T, f *nativeReadFixture, event appleevents.Event, sourceKey string, wantIDs ...int64) {
	t.Helper()
	direct := event.Params["----"]
	if !isGroupMove(event) || direct.Fields["want"].Code != "mssg" {
		t.Fatalf("group move direct object = %+v, want a whose-clause message selector", direct)
	}
	if got := f.key(direct.Fields["from"]); got != sourceKey {
		t.Fatalf("group move selector is scoped to %s, want %s", got, sourceKey)
	}
	assertIDEqualityTerms(t, "group move", direct.Fields["seld"], wantIDs...)
}

// assertGroupConfirmSelector checks a grouped existence read: the ID property
// of one whose-clause selector over the source whose ID-equality terms cover
// exactly the requested ids, in request order.
func assertGroupConfirmSelector(t *testing.T, f *nativeReadFixture, event appleevents.Event, sourceKey string, wantIDs ...int64) {
	t.Helper()
	direct := event.Params["----"]
	if !isGroupConfirmRead(event) {
		t.Fatalf("confirm read direct object = %+v, want the ID property of a whose-clause selector", direct)
	}
	if got := f.key(direct.Fields["from"].Fields["from"]); got != sourceKey {
		t.Fatalf("confirm read selector is scoped to %s, want %s", got, sourceKey)
	}
	assertIDEqualityTerms(t, "confirm read", direct.Fields["from"].Fields["seld"], wantIDs...)
}

// assertIDEqualityTerms checks a whose-clause test descriptor: an OR whose
// terms compare the examined message's ID against exactly the wanted ids, in
// order.
func assertIDEqualityTerms(t *testing.T, label string, test appleevents.Value, wantIDs ...int64) {
	t.Helper()
	if test.Kind != "logical" || test.Fields["logc"].Code != "OR  " {
		t.Fatalf("%s test = %+v, want an OR over ID comparisons", label, test)
	}
	terms := test.Fields["term"].Items
	if len(terms) != len(wantIDs) {
		t.Fatalf("%s selector carries %d ID terms, want %d", label, len(terms), len(wantIDs))
	}
	for i, want := range wantIDs {
		term := terms[i]
		if term.Kind != "comparison" || term.Fields["relo"].Code != "=   " {
			t.Fatalf("%s term %d = %+v, want an ID equality", label, i, term)
		}
		if prop := term.Fields["obj1"]; prop.Kind != "object" || prop.Fields["seld"].Code != "ID  " || prop.Fields["from"].Kind != "examined" {
			t.Fatalf("%s term %d tests %v, want the ID of the examined message", label, i, prop.Fields["seld"])
		}
		if operand := term.Fields["obj2"]; operand.Kind != "int" || operand.Number != want {
			t.Fatalf("%s term %d compares ID against %d, want %d", label, i, operand.Number, want)
		}
	}
}

func TestNativeMoveGroupSingleDispatch(t *testing.T) {
	f, c := moveGroupFixture(t, 42, 43, 44)
	moved, missing, err := c.nativeMoveGroup("Personal", "INBOX", "All Mail", []string{"42", "43", "44"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(moved, []string{"42", "43", "44"}) {
		t.Fatalf("moved = %v", moved)
	}
	if len(missing) != 0 {
		t.Fatalf("missing = %v", missing)
	}
	moves := eventsWithID(f, "move")
	if len(moves) != 1 {
		t.Fatalf("expected one core/move for the group, got %d", len(moves))
	}
	assertGroupMoveSelector(t, f, moves[0], "mbxp:inbox", 42, 43, 44)
	if key := f.key(moves[0].Params["insh"]); key != "mbxp:archive" {
		t.Fatalf("move destination = %s, want mbxp:archive", key)
	}
	reads := confirmReads(f)
	if len(reads) != 1 {
		t.Fatalf("expected one grouped existence read, got %d", len(reads))
	}
	assertGroupConfirmSelector(t, f, reads[0], "mbxp:inbox", 42, 43, 44)
	// Account resolve (1) + source mailboxExact (12: walk 2, account ID 1,
	// two canonicalMailbox passes over the candidate and the resolved ref,
	// 8) + destination mailboxExact (12) + grouped confirm read (1) + move
	// (1): twenty-seven events for three messages, and the same count for
	// thirty. The per-item path costs 27 per message.
	if got := len(f.events); got != 27 {
		t.Fatalf("group dispatch sent %d events, want 27", got)
	}
}

func TestNativeMoveGroupMissingIDClassified(t *testing.T) {
	f, c := moveGroupFixture(t, 42, 43)
	moved, missing, err := c.nativeMoveGroup("Personal", "INBOX", "All Mail", []string{"42", "99", "43"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(moved, []string{"42", "43"}) {
		t.Fatalf("moved = %v", moved)
	}
	if !reflect.DeepEqual(missing, []string{"99"}) {
		t.Fatalf("missing = %v", missing)
	}
	moves := eventsWithID(f, "move")
	if len(moves) != 1 {
		t.Fatalf("expected one move over the 2 confirmed refs, got %d moves", len(moves))
	}
	assertGroupMoveSelector(t, f, moves[0], "mbxp:inbox", 42, 43)
	// The grouped read returns the IDs that exist; 99 simply never matches,
	// so it classifies missing without any per-item fallback.
	reads := confirmReads(f)
	if len(reads) != 1 {
		t.Fatalf("expected one grouped existence read, got %d", len(reads))
	}
	assertGroupConfirmSelector(t, f, reads[0], "mbxp:inbox", 42, 99, 43)
	// Flat group cost (25: account resolve, two mailboxExact lookups) plus
	// the grouped confirm read (1) and the move (1): the same twenty-seven
	// events whether every requested message exists or not.
	if got := len(f.events); got != 27 {
		t.Fatalf("group dispatch sent %d events, want 27", got)
	}
}

func TestNativeMoveGroupInvalidIDsClassifiedMissing(t *testing.T) {
	_, c := moveGroupFixture(t, 42)
	moved, missing, err := c.nativeMoveGroup("Personal", "INBOX", "All Mail", []string{"42", "abc", "0"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(moved, []string{"42"}) {
		t.Fatalf("moved = %v", moved)
	}
	if !reflect.DeepEqual(missing, []string{"abc", "0"}) {
		t.Fatalf("missing = %v", missing)
	}
}

func TestNativeMoveGroupEmpty(t *testing.T) {
	f, c := moveGroupFixture(t)
	moved, missing, err := c.nativeMoveGroup("Personal", "INBOX", "All Mail", nil)
	if err != nil {
		t.Fatal(err)
	}
	if moved != nil || missing != nil {
		t.Fatalf("moved=%v missing=%v for an empty group", moved, missing)
	}
	if len(f.events) != 0 {
		t.Fatalf("empty group sent %d events", len(f.events))
	}
}

func TestNativeMoveGroupAllMissingSkipsDispatch(t *testing.T) {
	f, c := moveGroupFixture(t)
	moved, missing, err := c.nativeMoveGroup("Personal", "INBOX", "All Mail", []string{"nope"})
	if err != nil {
		t.Fatal(err)
	}
	if moved != nil {
		t.Fatalf("moved = %v", moved)
	}
	if !reflect.DeepEqual(missing, []string{"nope"}) {
		t.Fatalf("missing = %v", missing)
	}
	if len(eventsWithID(f, "move")) != 0 {
		t.Fatal("dispatched a move although no reference was confirmed")
	}
	if len(confirmReads(f)) != 0 {
		t.Fatal("issued an existence read although no reference was parseable")
	}
	// Account resolve plus the two mailboxExact lookups; no existence read,
	// no move.
	if got := len(f.events); got != 25 {
		t.Fatalf("group dispatch sent %d events, want 25", got)
	}
}

func TestNativeMoveGroupRejectionIsReplayable(t *testing.T) {
	f, c := moveGroupFixture(t, 42, 43)
	f.fail = func(event appleevents.Event) error {
		if event.ID == "move" {
			return &appleevents.Error{Code: -1708, Message: "no handler for group move", Attempted: true}
		}
		return nil
	}
	moved, missing, err := c.nativeMoveGroup("Personal", "INBOX", "All Mail", []string{"42", "43"})
	if err == nil {
		t.Fatal("expected a rejection error")
	}
	if !errors.Is(err, errMoveGroupRejected) {
		t.Fatalf("error %v is not a rejection", err)
	}
	var uncertain *moveGroupUncertainError
	if errors.As(err, &uncertain) {
		t.Fatal("a rejected group move must not be classified uncertain")
	}
	if len(moved) != 0 || len(missing) != 0 {
		t.Fatalf("moved=%v missing=%v after rejection", moved, missing)
	}
	if len(eventsWithID(f, "move")) != 1 {
		t.Fatal("expected exactly one rejected move attempt")
	}
}

func TestNativeMoveGroupMidGroupFailureIsUncertain(t *testing.T) {
	f, c := moveGroupFixture(t, 42, 43)
	f.fail = func(event appleevents.Event) error {
		if event.ID == "move" {
			return &appleevents.Error{Code: -1728, Message: "element missing mid-group", Attempted: true}
		}
		return nil
	}
	moved, _, err := c.nativeMoveGroup("Personal", "INBOX", "All Mail", []string{"42", "43"})
	if err == nil {
		t.Fatal("expected an uncertain error")
	}
	var uncertain *moveGroupUncertainError
	if !errors.As(err, &uncertain) {
		t.Fatalf("error %v is not uncertain", err)
	}
	if errors.Is(err, errMoveGroupRejected) {
		t.Fatal("a mid-group failure must not be classified as rejection")
	}
	if len(moved) != 0 {
		t.Fatalf("moved = %v after an uncertain failure", moved)
	}
	if !isUnknownMutationError(c, err) {
		t.Fatal("uncertain group failures must classify as unknown")
	}
}

func TestNativeMoveGroupTransportFailureIsUncertain(t *testing.T) {
	errBridgeLost := errors.New("bridge connection lost")
	f, c := moveGroupFixture(t, 42, 43)
	f.fail = func(event appleevents.Event) error {
		if event.ID == "move" {
			return errBridgeLost
		}
		return nil
	}
	moved, _, err := c.nativeMoveGroup("Personal", "INBOX", "All Mail", []string{"42", "43"})
	if err == nil {
		t.Fatal("expected an uncertain error")
	}
	var uncertain *moveGroupUncertainError
	if !errors.As(err, &uncertain) {
		t.Fatalf("error %v is not uncertain", err)
	}
	if !errors.Is(err, errBridgeLost) {
		t.Fatal("the transport cause must survive the uncertain wrapper")
	}
	if len(moved) != 0 {
		t.Fatalf("moved = %v after a lost reply", moved)
	}
	if !isUnknownMutationError(c, err) {
		t.Fatal("lost replies must classify as unknown")
	}
}

func TestNativeMoveGroupConfirmTransportFailureDoesNotFallback(t *testing.T) {
	errBridgeLost := errors.New("bridge connection lost")
	f, c := moveGroupFixture(t, 42, 43)
	f.fail = func(event appleevents.Event) error {
		if isGroupConfirmRead(event) {
			return errBridgeLost
		}
		return nil
	}
	moved, _, err := c.nativeMoveGroup("Personal", "INBOX", "All Mail", []string{"42", "43"})
	if !errors.Is(err, errBridgeLost) {
		t.Fatalf("expected the transport failure, got %v", err)
	}
	var uncertain *moveGroupUncertainError
	if errors.As(err, &uncertain) {
		t.Fatal("a read failure before dispatch is not an uncertain mutation")
	}
	if errors.Is(err, errMoveGroupRejected) {
		t.Fatal("a transport failure is not a rejection")
	}
	if moved != nil {
		t.Fatalf("moved = %v without dispatching", moved)
	}
	if len(eventsWithID(f, "move")) != 0 {
		t.Fatal("dispatched a move after a failed confirm read")
	}
	if len(confirmReads(f)) != 1 {
		t.Fatal("expected exactly the one failed grouped read, with no per-item fallback")
	}
	// Account resolve (1) + two mailboxExact lookups (24) + the failed
	// grouped read (1); no per-item confirms, no move.
	if got := len(f.events); got != 26 {
		t.Fatalf("group dispatch sent %d events, want 26", got)
	}
}

func TestNativeMoveGroupTimeoutKeepsPhaseTrace(t *testing.T) {
	f, c := moveGroupFixture(t, 42, 43)
	f.fail = func(event appleevents.Event) error {
		if event.ID == "move" {
			return context.DeadlineExceeded
		}
		return nil
	}
	moved, _, err := c.nativeMoveGroup("Personal", "INBOX", "All Mail", []string{"42", "43"})
	if err == nil {
		t.Fatal("expected a timeout")
	}
	var timeoutErr *AutomationTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("expected an automation timeout, got %T", err)
	}
	if timeoutErr.Trace == nil || timeoutErr.Trace.Pending != "bulk_move" {
		t.Fatalf("phase trace = %+v", timeoutErr.Trace)
	}
	if timeoutErr.Trace.LastCompleted != "confirm_refs" {
		t.Fatalf("last completed phase = %s", timeoutErr.Trace.LastCompleted)
	}
	wantPhases := []string{"resolve_group", "confirm_refs"}
	gotPhases := make([]string, 0, len(timeoutErr.Trace.Phases))
	for _, phase := range timeoutErr.Trace.Phases {
		gotPhases = append(gotPhases, phase.Name)
	}
	if !reflect.DeepEqual(gotPhases, wantPhases) {
		t.Fatalf("completed phases = %v, want %v", gotPhases, wantPhases)
	}
	var uncertain *moveGroupUncertainError
	if !errors.As(err, &uncertain) {
		t.Fatal("a timeout mid-group must remain discoverable as uncertain")
	}
	if len(moved) != 0 {
		t.Fatalf("moved = %v after a timeout", moved)
	}
	if !isUnknownMutationError(c, err) {
		t.Fatal("timeout group failures must classify as unknown")
	}
}

func TestNativeMoveGroupSplitsOversizedGroups(t *testing.T) {
	const count = maxMoveGroupRefs + 1
	fixtureIDs := make([]int64, count)
	ids := make([]string, count)
	for i := range ids {
		fixtureIDs[i] = int64(i + 1)
		ids[i] = strconv.FormatInt(fixtureIDs[i], 10)
	}
	f, c := moveGroupFixture(t, fixtureIDs...)
	moved, missing, err := c.nativeMoveGroup("Personal", "INBOX", "All Mail", ids)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(moved, ids) {
		t.Fatalf("moved %d of %d ids", len(moved), len(ids))
	}
	if len(missing) != 0 {
		t.Fatalf("missing = %v", missing)
	}
	moves := eventsWithID(f, "move")
	if len(moves) != 2 {
		t.Fatalf("expected the group to split into 2 moves, got %d", len(moves))
	}
	assertGroupMoveSelector(t, f, moves[0], "mbxp:inbox", fixtureIDs[:maxMoveGroupRefs]...)
	assertGroupMoveSelector(t, f, moves[1], "mbxp:inbox", fixtureIDs[maxMoveGroupRefs:]...)
	// The existence read chunks with the moves: one whose-clause read per
	// maxMoveGroupRefs slice of the requested ids.
	reads := confirmReads(f)
	if len(reads) != 2 {
		t.Fatalf("expected the existence read to split into 2 chunks, got %d", len(reads))
	}
	assertGroupConfirmSelector(t, f, reads[0], "mbxp:inbox", fixtureIDs[:maxMoveGroupRefs]...)
	assertGroupConfirmSelector(t, f, reads[1], "mbxp:inbox", fixtureIDs[maxMoveGroupRefs:]...)
}

func TestMoveGroupTimeoutScaling(t *testing.T) {
	base := defaultAutomationTimeout
	cases := []struct {
		count int
		want  time.Duration
	}{
		{0, base},
		{1, base},
		{2, base + 2*time.Second},
		{30, base + 58*time.Second},
		{135, base + 268*time.Second},
		{136, maxMoveGroupTimeout},
		{10000, maxMoveGroupTimeout},
	}
	for _, tc := range cases {
		if got := moveGroupTimeout(tc.count); got != tc.want {
			t.Fatalf("moveGroupTimeout(%d) = %s, want %s", tc.count, got, tc.want)
		}
	}
}
