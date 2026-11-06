//go:build darwin && cgo

package mail

import (
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestNativeMoveGroupProbe moves a bounded set of user-authorized disposable
// messages through one nativeMoveGroup transaction against live Mail.app. It
// fails closed: without MAIL_AE_MOVE_GROUP_PROBE=1 and every explicit fixture
// (account, source, destination, message IDs) it never mutates anything. The
// messages must be disposable; the probe moves them for real. Output carries
// counts and durations only, never message content or account data.
func TestNativeMoveGroupProbe(t *testing.T) {
	if os.Getenv("MAIL_AE_MOVE_GROUP_PROBE") != "1" {
		t.Skip("native move group probe disabled")
	}
	account := os.Getenv("MAIL_AE_PROBE_ACCOUNT")
	if account == "" {
		t.Fatal("move group probe requires explicit MAIL_AE_PROBE_ACCOUNT")
	}
	source := os.Getenv("MAIL_AE_PROBE_MAILBOX")
	if source == "" {
		source = "INBOX"
	}
	destination := os.Getenv("MAIL_AE_MOVE_GROUP_PROBE_DEST")
	if destination == "" {
		t.Fatal("move group probe requires explicit MAIL_AE_MOVE_GROUP_PROBE_DEST")
	}
	if strings.EqualFold(destination, source) {
		t.Fatal("probe destination must differ from the source mailbox")
	}
	idsArg := os.Getenv("MAIL_AE_MOVE_GROUP_PROBE_IDS")
	if idsArg == "" {
		t.Fatal("move group probe requires explicit MAIL_AE_MOVE_GROUP_PROBE_IDS")
	}
	var ids []string
	for _, field := range strings.Split(idsArg, ",") {
		id := strings.TrimSpace(field)
		if id == "" {
			continue
		}
		if _, err := strconv.ParseInt(id, 10, 64); err != nil {
			t.Fatalf("probe id %q is not a numeric Mail message ID", id)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 || len(ids) > 30 {
		t.Fatalf("probe moves a bounded set: got %d ids, want 1..30", len(ids))
	}

	c := NewClient()
	c.shared.backend = BackendNative

	items := make([]BatchItem, len(ids))
	for i, id := range ids {
		items[i] = BatchItem{Account: account, SourceMailbox: source, ID: id, TargetMailbox: destination}
	}
	opts := BatchOptions{Action: "move", TargetMailbox: destination, VerifyBudget: 2 * time.Minute}
	if err := captureBatchIdentities(c, opts, items); err != nil {
		t.Fatalf("capture stable identities: %v", err)
	}
	for i := range items {
		if items[i].Status == "failed" {
			t.Fatalf("identity capture failed for item %d of %d: %s", i+1, len(items), items[i].Error)
		}
	}

	before := BackendStatistics()
	started := time.Now()
	moved, missing, err := c.nativeMoveGroup(account, source, destination, ids)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("nativeMoveGroup: %v", err)
	}
	stats := BackendStatistics()
	events := stats.NativeEvents - before.NativeEvents
	if !reflect.DeepEqual(moved, ids) {
		t.Fatalf("probe moved %d of %d ids, %d classified missing before dispatch", len(moved), len(ids), len(missing))
	}
	if len(missing) != 0 {
		t.Fatalf("probe classified %d ids as missing before dispatch", len(missing))
	}
	// Acceptance bound: one group dispatch is O(1) in group size — account
	// resolve, two hardened mailbox lookups (~12 events each on a flat
	// account, more with nested mailboxes), the bulk confirm, and the
	// move(s). The per-item path costs ~27 events per message, so 30
	// messages would be ~810; anything near that means grouping broke.
	if events > 45 {
		t.Fatalf("group dispatch sent %d native events, want at most 45", events)
	}

	for i := range items {
		items[i].Status = "succeeded"
	}
	verified := VerifyMutations(c, opts, items)
	unverified := 0
	for _, item := range verified {
		if item.Observed == nil || !item.Observed.DestinationPresent || item.VerifyError != "" {
			unverified++
		}
	}
	if unverified != 0 {
		t.Fatalf("identity verification failed for %d of %d moved messages", unverified, len(ids))
	}
	t.Logf("probe=move-group count=%d nativeEvents=%d nativeEventMS=%.1f elapsedMS=%.1f",
		len(ids), events, stats.NativeEventMS-before.NativeEventMS, float64(elapsed.Microseconds())/1000)
}
