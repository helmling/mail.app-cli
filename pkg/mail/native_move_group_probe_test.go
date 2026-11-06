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
	// Mail applies a group move asynchronously: the Apple Event returns once
	// the move is accepted, and the IMAP copy lands in the destination over
	// the following minutes — observed landing times exceed the 5m --settle
	// ceiling, so no single settle window can observe the arrival, and the
	// schedule only spans the settle, never the budget. Verification
	// therefore runs in repeated short rounds, stopping at the first round
	// that confirms every identity, bounded well inside the ten-minute
	// ceiling --verify-budget enforces. Identity capture runs before
	// anything is mutated and keeps its own budget: it must survive slow
	// per-message metadata reads without shortening the observation window.
	captureOpts := BatchOptions{Action: "move", TargetMailbox: destination, VerifyBudget: 5 * time.Minute}
	if err := captureBatchIdentities(c, captureOpts, items); err != nil {
		t.Fatalf("capture stable identities: %v", err)
	}
	for i := range items {
		if items[i].Status == "failed" {
			t.Fatalf("identity capture failed for item %d of %d: %s", i+1, len(items), items[i].Error)
		}
	}
	// Verification matches on fallback identities. RFC confirmation costs one
	// live per-candidate read inside every snapshot tick, and each read
	// re-opens an automation session and re-resolves the account and both
	// mailboxes — measured at about 2.2s per identity, so a 30-identity tick
	// alone exceeds every budget --verify-budget allows. Fallback matching
	// (sender, subject, date sent, size) is index-only — about 2s for both
	// scopes at 30 identities — and is the same verification semantics the
	// CLI itself uses for any message without an RFC identity.
	for i := range items {
		items[i].Identity.RFCMessageID = ""
	}

	before := BackendStatistics()
	// Calibrate the account-dependent fixed cost of one dispatch: the same
	// account resolve and two hardened mailbox walks nativeMoveGroup's
	// resolve_group phase performs, measured read-only against this live
	// account. The walk scales with the account's mailbox tree, so a flat
	// event bound cannot hold across accounts.
	if err := c.withNativeAutomation(defaultAutomationTimeout, func(n *nativeMail) error {
		acc, err := n.account(account)
		if err != nil {
			return err
		}
		if _, err := n.mailbox(acc, source); err != nil {
			return err
		}
		_, err = n.mailbox(acc, destination)
		return err
	}); err != nil {
		t.Fatalf("resolve calibration: %v", err)
	}
	resolveEvents := BackendStatistics().NativeEvents - before.NativeEvents

	before = BackendStatistics()
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
	// Acceptance bound: one group dispatch is O(1) in group size. The
	// account/mailbox resolve dominates on a large account, so the probe
	// bounds only the dispatch tail beyond the calibrated resolve: one
	// grouped confirm read plus one move per maxMoveGroupRefs chunk — at
	// most 4 events for a bounded 30-id group. The per-item path re-pays
	// the whole resolve for every message; a tail that grows with the
	// group means grouping broke.
	if tail := events - resolveEvents; tail > 4 {
		t.Fatalf("group dispatch sent %d native events beyond the %d-event resolve, want at most 4", tail, resolveEvents)
	}

	for i := range items {
		items[i].Status = "succeeded"
	}
	// Short observation rounds; see the capture comment above for why the
	// window repeats instead of growing past the settle ceiling. Each round
	// samples on a wall-clock-anchored cadence and every tick takes a fresh
	// index-only snapshot, so rounds observe arrivals continuously. Observed
	// 30-message landings complete four to ten minutes after the move event
	// returns, so the loop bounds itself at ten minutes of rounds.
	verifyOpts := BatchOptions{Action: "move", TargetMailbox: destination, Settle: 30 * time.Second, VerifyBudget: 40 * time.Second}
	verifyStarted := time.Now()
	rounds := 0
	unverified := len(items)
	var verified []BatchItem
	for {
		rounds++
		verified = VerifyMutations(c, verifyOpts, items)
		unverified = 0
		for _, item := range verified {
			if item.Observed == nil || !item.Observed.DestinationPresent || item.VerifyError != "" {
				unverified++
			}
		}
		if unverified == 0 || time.Since(verifyStarted) >= 10*time.Minute {
			break
		}
	}
	if unverified != 0 {
		statuses := map[string]int{}
		for _, item := range verified {
			statuses[item.VerifyStatus]++
		}
		t.Fatalf("identity verification confirmed %d of %d moved messages over %d rounds (%s of observation), statuses: %v",
			len(items)-unverified, len(items), rounds, time.Since(verifyStarted).Round(time.Second), statuses)
	}
	t.Logf("probe=move-group count=%d nativeEvents=%d resolveEvents=%d nativeEventMS=%.1f elapsedMS=%.1f verifyRounds=%d verifyMS=%.1f",
		len(ids), events, resolveEvents, stats.NativeEventMS-before.NativeEventMS, float64(elapsed.Microseconds())/1000,
		rounds, float64(time.Since(verifyStarted).Microseconds())/1000)
}
