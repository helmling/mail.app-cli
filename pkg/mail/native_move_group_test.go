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
// environment, so background goroutines can construct their own.
func moveGroupFixtureData(ids ...int64) *nativeReadFixture {
	f := newNativeReadFixture()
	for _, id := range ids {
		key := fmt.Sprintf("mssg:%d", id)
		f.properties[key] = map[string]appleevents.Value{"ID  ": appleevents.Int(id)}
	}
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

// confirmReads are the bulk existence reads: getd events whose direct object
// is a list of property specifiers.
func confirmReads(f *nativeReadFixture) []appleevents.Event {
	var out []appleevents.Event
	for _, event := range eventsWithID(f, "getd") {
		if event.Params["----"].Kind == "list" {
			out = append(out, event)
		}
	}
	return out
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
	direct := moves[0].Params["----"]
	if direct.Kind != "list" || len(direct.Items) != 3 {
		t.Fatalf("move direct object kind=%s items=%d", direct.Kind, len(direct.Items))
	}
	for i, want := range []int64{42, 43, 44} {
		ref := direct.Items[i]
		if ref.Kind != "object" || ref.Fields["want"].Code != "mssg" {
			t.Fatalf("move item %d kind=%s", i, ref.Kind)
		}
		if ref.Fields["seld"].Kind != "int" || ref.Fields["seld"].Number != want {
			t.Fatalf("move item %d references message %v, want %d", i, ref.Fields["seld"].Number, want)
		}
		if key := f.key(ref.Fields["from"]); key != "mbxp:inbox" {
			t.Fatalf("move item %d is scoped to %s, want mbxp:inbox", i, key)
		}
	}
	if key := f.key(moves[0].Params["insh"]); key != "mbxp:archive" {
		t.Fatalf("move destination = %s, want mbxp:archive", key)
	}
	reads := confirmReads(f)
	if len(reads) != 1 || len(reads[0].Params["----"].Items) != 3 {
		t.Fatalf("expected one bulk existence read over 3 refs, got %d reads", len(reads))
	}
	// Account resolve (1) + source mailboxExact (12: walk 2, account ID 1,
	// two canonicalMailbox passes over the candidate and the resolved ref,
	// 8) + destination mailboxExact (12) + bulk confirm (1) + move (1):
	// twenty-seven events for three messages, and the same count for thirty.
	// The per-item path costs 27 per message.
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
	if len(moves) != 1 || len(moves[0].Params["----"].Items) != 2 {
		t.Fatalf("expected one move over the 2 confirmed refs, got %d moves", len(moves))
	}
	// The wholesale bulk read fails on the missing member; the per-item
	// fallback then re-confirms each ref individually.
	if len(confirmReads(f)) != 1 {
		t.Fatalf("expected the failed bulk read to be attempted once")
	}
	// Flat group cost (25: account resolve, two mailboxExact lookups) plus
	// the failed bulk read (1), the per-item confirms (3), and the move (1).
	if got := len(f.events); got != 30 {
		t.Fatalf("group dispatch sent %d events, want 30", got)
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
			return &appleevents.Error{Code: -1708, Message: "no handler for list move", Attempted: true}
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
		t.Fatal("a rejected list move must not be classified uncertain")
	}
	if len(moved) != 0 || len(missing) != 0 {
		t.Fatalf("moved=%v missing=%v after rejection", moved, missing)
	}
	if len(eventsWithID(f, "move")) != 1 {
		t.Fatal("expected exactly one rejected move attempt")
	}
}

func TestNativeMoveGroupMidListFailureIsUncertain(t *testing.T) {
	f, c := moveGroupFixture(t, 42, 43)
	f.fail = func(event appleevents.Event) error {
		if event.ID == "move" {
			return &appleevents.Error{Code: -1728, Message: "element missing mid-list", Attempted: true}
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
		t.Fatal("a mid-list failure must not be classified as rejection")
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
		if event.ID == "getd" && event.Params["----"].Kind == "list" {
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
		t.Fatal("expected exactly the one failed bulk read, with no per-item fallback")
	}
	// Account resolve (1) + two mailboxExact lookups (24) + the failed bulk
	// read (1); no per-item confirms, no move.
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
		t.Fatal("a timeout mid-list must remain discoverable as uncertain")
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
	if got := len(moves[0].Params["----"].Items); got != maxMoveGroupRefs {
		t.Fatalf("first move carried %d refs, want %d", got, maxMoveGroupRefs)
	}
	if got := len(moves[1].Params["----"].Items); got != 1 {
		t.Fatalf("second move carried %d refs, want 1", got)
	}
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
