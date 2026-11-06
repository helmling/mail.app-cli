package mail

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLegacyMoveGroupParsesStatuses(t *testing.T) {
	cases := []struct {
		name     string
		output   string
		moved    []string
		missing  []string
		checkErr func(t *testing.T, err error)
	}{
		{
			name:    "ok classifies moved and missing",
			output:  `{"status":"ok","moved":["42","43"],"missing":["44"]}`,
			moved:   []string{"42", "43"},
			missing: []string{"44"},
		},
		{
			name:    "rejected is replayable and keeps per-ID status",
			output:  `{"status":"rejected","moved":["42"],"missing":["44"],"message":"no grouped moves"}`,
			moved:   []string{"42"},
			missing: []string{"44"},
			checkErr: func(t *testing.T, err error) {
				if !errors.Is(err, errMoveGroupRejected) {
					t.Fatalf("err = %v, want errMoveGroupRejected", err)
				}
			},
		},
		{
			name:   "uncertain is never replayed",
			output: `{"status":"uncertain","moved":["42"],"missing":[],"message":"mid-group failure"}`,
			moved:  []string{"42"},
			checkErr: func(t *testing.T, err error) {
				var uncertain *moveGroupUncertainError
				if !errors.As(err, &uncertain) || errors.Is(err, errMoveGroupRejected) {
					t.Fatalf("err = %v, want uncertain and not rejected", err)
				}
			},
		},
		{
			name:   "error precedes any mutation",
			output: `{"status":"error","message":"account missing"}`,
			checkErr: func(t *testing.T, err error) {
				if err == nil || errors.Is(err, errMoveGroupRejected) {
					t.Fatalf("err = %v, want plain pre-mutation error", err)
				}
				var uncertain *moveGroupUncertainError
				if errors.As(err, &uncertain) {
					t.Fatalf("err = %v, want replayable classification", err)
				}
			},
		},
		{
			name:   "mailbox resolution keeps not-found typing",
			output: `{"status":"error","message":"Error: Mailbox not found: Processed"}`,
			checkErr: func(t *testing.T, err error) {
				if !IsNotFound(err) {
					t.Fatalf("err = %v, want not-found classification", err)
				}
				var notFound *NotFoundError
				if !errors.As(err, &notFound) || notFound.Kind != "mailbox" || notFound.Name != "Processed" {
					t.Fatalf("err = %#v, want mailbox not-found", err)
				}
			},
		},
		{
			name:     "unparsable reply is uncertain",
			output:   `Success`,
			checkErr: wantsUncertain,
		},
		{
			name:     "unrecognized status is uncertain",
			output:   `{"status":"weird"}`,
			checkErr: wantsUncertain,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MAIL_APP_CLI_AUTOMATION_LOCK_PATH", filepath.Join(t.TempDir(), "lock"))
			writeFakeOsaScript(t, "printf '%s' '"+tc.output+"'")
			moved, missing, err := NewClient().legacyMoveGroup("Personal", "INBOX", "All Mail", []string{"42", "43", "44"})
			if tc.checkErr != nil {
				tc.checkErr(t, err)
			} else if err != nil {
				t.Fatalf("legacyMoveGroup error = %v", err)
			}
			if fmt.Sprint(moved) != fmt.Sprint(tc.moved) || fmt.Sprint(missing) != fmt.Sprint(tc.missing) {
				t.Fatalf("moved/missing = %v/%v, want %v/%v", moved, missing, tc.moved, tc.missing)
			}
		})
	}
}

func wantsUncertain(t *testing.T, err error) {
	t.Helper()
	var uncertain *moveGroupUncertainError
	if !errors.As(err, &uncertain) || errors.Is(err, errMoveGroupRejected) {
		t.Fatalf("err = %v, want uncertain and not rejected", err)
	}
}

func TestLegacyMoveGroupTransportFailureIsUncertain(t *testing.T) {
	t.Setenv("MAIL_APP_CLI_AUTOMATION_LOCK_PATH", filepath.Join(t.TempDir(), "lock"))
	writeFakeOsaScript(t, "/bin/sleep 2")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	moved, missing, err := NewClient().WithContext(ctx).legacyMoveGroup("Personal", "INBOX", "All Mail", []string{"42", "43"})
	if len(moved) != 0 || len(missing) != 0 {
		t.Fatalf("moved/missing = %v/%v, want none", moved, missing)
	}
	var uncertain *moveGroupUncertainError
	if !errors.As(err, &uncertain) {
		t.Fatalf("err = %v, want uncertain outcome", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline cause preserved", err)
	}
	if errors.Is(err, errMoveGroupRejected) {
		t.Fatalf("err = %v, a lost process must never be replayable", err)
	}
}

func TestClassifyLegacyMoveGroupTransport(t *testing.T) {
	lockErr := &AutomationLockTimeoutError{Engine: "jxa", Timeout: time.Second}
	if got := classifyLegacyMoveGroupTransport(lockErr); !errors.Is(got, lockErr) {
		t.Fatalf("lock timeout = %v, want raw replayable error", got)
	}
	for name, cause := range map[string]error{
		"canceled":            context.Canceled,
		"session interrupted": fmt.Errorf("%w: %v", errSessionInterrupted, errors.New("eof")),
		"process timeout":     &AutomationTimeoutError{Engine: "jxa", Timeout: time.Second},
	} {
		got := classifyLegacyMoveGroupTransport(cause)
		var uncertain *moveGroupUncertainError
		if !errors.As(got, &uncertain) {
			t.Fatalf("%s: err = %v, want uncertain classification", name, got)
		}
		if !errors.Is(got, cause) {
			t.Fatalf("%s: err = %v, want cause %v preserved", name, got, cause)
		}
	}
}

func TestLegacyMoveGroupScriptShape(t *testing.T) {
	t.Setenv("MAIL_APP_CLI_AUTOMATION_LOCK_PATH", filepath.Join(t.TempDir(), "lock"))
	scriptPath := filepath.Join(t.TempDir(), "group.js")
	t.Setenv("LEGACY_GROUP_SCRIPT_LOG", scriptPath)
	writeFakeOsaScript(t, `for arg in "$@"; do script="$arg"; done
printf '%s' "$script" > "$LEGACY_GROUP_SCRIPT_LOG"
printf '%s' '{"status":"ok","moved":["42","43","44"],"missing":[]}'
`)
	_, _, err := NewClient().legacyMoveGroup("Personal", "INBOX", "All Mail", []string{"42", "43", "44"})
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, want := range []string{
		"const requestedMailbox = 'INBOX';",
		"const requestedTargetMailbox = 'All Mail';",
		"mail.accounts.byName('Personal')",
		`const ids = ["42","43","44"];`,
		"function messageById",
		"function findMailbox",
		"function idSelector",
		"mbox.messages.whose({_or:",
		"mail.move(idSelector(",
		fmt.Sprintf("start += %d", maxMoveGroupRefs),
		"JSON.stringify(reply);",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("group script missing %q", want)
		}
	}
	if strings.Contains(text, ".mailbox = destMbox") {
		t.Errorf("group script must move the group, not set per-item mailboxes")
	}
	if got := strings.Count(text, "mail.move("); got != 1 {
		t.Errorf("mail.move call sites = %d, want the one group dispatch", got)
	}
}

// legacyMoveBatchJournal isolates the automation lock to the test and
// returns a durable receipt the batch writes to.
func legacyMoveBatchJournal(t *testing.T) *BatchJournal {
	t.Helper()
	t.Setenv("MAIL_APP_CLI_AUTOMATION_LOCK_PATH", filepath.Join(t.TempDir(), "lock"))
	journal, err := CreateBatchJournal(filepath.Join(t.TempDir(), "receipt.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { journal.Close() })
	return journal
}

func TestRunBatchLegacyMoveGroupRunsOneScript(t *testing.T) {
	journal := legacyMoveBatchJournal(t)
	launchLog := filepath.Join(t.TempDir(), "launches")
	t.Setenv("LEGACY_GROUP_LAUNCH_LOG", launchLog)
	writeFakeOsaScript(t, `case "$*" in
  *"mail.move("*)
    printf 'group\n' >> "$LEGACY_GROUP_LAUNCH_LOG"
    printf '%s' '{"status":"ok","moved":["42","43"],"missing":[]}'
    ;;
  *) printf 'Success' ;;
esac
`)
	result, err := RunBatch(NewClient(), BatchOptions{Action: "move", Receipt: journal}, moveBatchItems(2), MoveMutator(false))
	if err != nil {
		t.Fatalf("RunBatch error = %v", err)
	}
	if result.Succeeded != 2 {
		t.Fatalf("result = %+v", result)
	}
	launches, err := os.ReadFile(launchLog)
	if err != nil || strings.Count(string(launches), "group") != 1 {
		t.Fatalf("group script launches = %q, %v; want exactly one", launches, err)
	}
	events := readJournalEvents(t, journal.Path())
	if countJournalEvents(events, "bulk_move") != 1 || countJournalEvents(events, "mutation_succeeded") != 2 {
		t.Fatalf("events = %+v", events)
	}
}

func TestRunBatchLegacyMoveGroupClassifiesPerIDStatus(t *testing.T) {
	journal := legacyMoveBatchJournal(t)
	exchangeLog := filepath.Join(t.TempDir(), "exchanges")
	t.Setenv("LEGACY_GROUP_EXCHANGE_LOG", exchangeLog)
	writeFakeOsaScript(t, `while IFS= read -r line; do
 printf 'exchange\n' >> "$LEGACY_GROUP_EXCHANGE_LOG"
 printf '{"output":"%s"}\n' '{\"status\":\"ok\",\"moved\":[\"42\"],\"missing\":[\"43\"]}'
done
`)
	result, err := RunBatch(NewClient(), BatchOptions{Action: "move", ReuseBridge: true, Receipt: journal}, moveBatchItems(2), MoveMutator(false))
	var failed *BatchFailedError
	if !errors.As(err, &failed) || failed.Failed != 1 || result.Succeeded != 1 || len(result.Items) != 2 {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if result.Items[1].Status != "failed" || result.Items[1].Error != "message not found: 43" {
		t.Fatalf("missing item = %+v, want per-item not-found words", result.Items[1])
	}
	exchanges, err := os.ReadFile(exchangeLog)
	if err != nil || strings.Count(string(exchanges), "exchange") != 1 {
		t.Fatalf("session exchanges = %q, %v; want the one group script", exchanges, err)
	}
	events := readJournalEvents(t, journal.Path())
	if countJournalEvents(events, "bulk_move") != 1 || countJournalEvents(events, "mutation_succeeded") != 1 || countJournalEvents(events, "mutation_failed") != 1 {
		t.Fatalf("events = %+v", events)
	}
}

func TestRunBatchLegacyMoveGroupUncertainStopsRun(t *testing.T) {
	journal := legacyMoveBatchJournal(t)
	exchangeLog := filepath.Join(t.TempDir(), "exchanges")
	t.Setenv("LEGACY_GROUP_EXCHANGE_LOG", exchangeLog)
	writeFakeOsaScript(t, `while IFS= read -r line; do
 printf 'exchange\n' >> "$LEGACY_GROUP_EXCHANGE_LOG"
 printf '{"output":"%s"}\n' '{\"status\":\"uncertain\",\"moved\":[\"42\"],\"missing\":[],\"message\":\"mid-group failure\"}'
done
`)
	result, err := RunBatch(NewClient(), BatchOptions{Action: "move", ReuseBridge: true, Receipt: journal}, moveBatchItems(2), MoveMutator(false))
	var uncertain *moveGroupUncertainError
	if !errors.As(err, &uncertain) || !strings.Contains(err.Error(), "mid-group failure") {
		t.Fatalf("err = %v, want uncertain outcome with script cause", err)
	}
	if result.Succeeded != 1 || len(result.Items) != 2 || result.Items[1].Status != "unknown" {
		t.Fatalf("result = %+v", result)
	}
	exchanges, err := os.ReadFile(exchangeLog)
	if err != nil || strings.Count(string(exchanges), "exchange") != 1 {
		t.Fatalf("session exchanges = %q, %v; uncertain items must never be replayed", exchanges, err)
	}
	events := readJournalEvents(t, journal.Path())
	if countJournalEvents(events, "bulk_move") != 1 || countJournalEvents(events, "mutation_succeeded") != 1 || countJournalEvents(events, "mutation_unknown") != 1 || events[len(events)-1]["event"] != "interrupted" {
		t.Fatalf("events = %+v", events)
	}
}

func TestRunBatchLegacyMoveGroupRejectionReplaysPerItemAndLatches(t *testing.T) {
	journal := legacyMoveBatchJournal(t)
	launchLog := filepath.Join(t.TempDir(), "launches")
	t.Setenv("LEGACY_GROUP_LAUNCH_LOG", launchLog)
	writeFakeOsaScript(t, `printf 'launch\n' >> "$LEGACY_GROUP_LAUNCH_LOG"
n=0
while IFS= read -r line; do
 n=$((n+1))
 if [ "$n" -eq 1 ]; then
  printf '{"output":"%s"}\n' '{\"status\":\"rejected\",\"moved\":[],\"missing\":[],\"message\":\"no grouped moves\"}'
 else
  printf '{"output":"Success"}\n'
 fi
done
`)
	client := NewClient()
	result, err := RunBatch(client, BatchOptions{Action: "move", ReuseBridge: true, Receipt: journal}, moveBatchItems(2), MoveMutator(false))
	if err != nil || result.Succeeded != 2 {
		t.Fatalf("rejection replay result = %+v, %v", result, err)
	}
	if !client.shared.moveGroupRejected.Load() {
		t.Fatal("rejection did not latch the per-item fallback")
	}
	events := readJournalEvents(t, journal.Path())
	if countJournalEvents(events, "bulk_move") != 1 || countJournalEvents(events, "mutation_succeeded") != 2 || countJournalEvents(events, "mutation_unknown") != 0 {
		t.Fatalf("events = %+v", events)
	}
	launches, err := os.ReadFile(launchLog)
	if err != nil || strings.Count(string(launches), "launch") != 1 {
		t.Fatalf("bridge launches = %q, %v; replay must reuse the session", launches, err)
	}

	// A latched client never offers the group form again.
	writeFakeOsaScript(t, `printf 'launch\n' >> "$LEGACY_GROUP_LAUNCH_LOG"
while IFS= read -r line; do printf '{"output":"Success"}\n'; done
`)
	secondJournal := legacyMoveBatchJournal(t)
	result, err = RunBatch(client, BatchOptions{Action: "move", ReuseBridge: true, Receipt: secondJournal}, moveBatchItems(2), MoveMutator(false))
	if err != nil || result.Succeeded != 2 {
		t.Fatalf("latched rerun result = %+v, %v", result, err)
	}
	events = readJournalEvents(t, secondJournal.Path())
	if countJournalEvents(events, "bulk_move") != 0 || countJournalEvents(events, "mutation_succeeded") != 2 {
		t.Fatalf("latched rerun events = %+v", events)
	}
}
