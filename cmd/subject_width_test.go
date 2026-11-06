package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xSMW/mail.app-cli/v3/internal/output"
	"github.com/0xSMW/mail.app-cli/v3/pkg/mail"
)

// longSubject is long enough that every built-in budget truncates it.
const longSubject = "Sed ut perspiciatis unde omnis iste natus error sit voluptatem accusantium doloremque laudantium totam rem"

func tableLines(t *testing.T, table string) []string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(table, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want a header and one row, got %d lines:\n%s", len(lines), table)
	}
	return lines
}

func TestRenderMessagesSubjectColumnObeysWidthOverride(t *testing.T) {
	messages := []mail.Message{{
		ID:           "42",
		Subject:      longSubject,
		Sender:       "Work Billing <billing@example.com>",
		Account:      "Work",
		Mailbox:      "INBOX",
		DateReceived: "2026-08-27T00:00:00Z",
		Read:         true,
	}}
	render := func(width int) string {
		var b bytes.Buffer
		renderMessages(messages, true)(&output.Printer{Out: &b, SubjectWidth: width})
		return b.String()
	}

	// The subject stays the last column so a full subject cannot push the
	// location (or anything else) out of view.
	lines := tableLines(t, render(0))
	header, row := lines[0], lines[1]
	if !strings.Contains(header, "LOCATION") || strings.Index(header, "LOCATION") > strings.Index(header, "SUBJECT") {
		t.Fatalf("LOCATION must precede SUBJECT: %q", header)
	}
	if strings.Index(row, "Work/INBOX") > strings.Index(row, "Sed ut") {
		t.Fatalf("location must precede the subject: %q", row)
	}

	// The zero value keeps the built-in 70-rune budget.
	if !strings.Contains(row, "…") || strings.Contains(row, longSubject) {
		t.Fatalf("default subject budget not applied: %q", row)
	}

	// A negative override prints the subject in full.
	lines = tableLines(t, render(-1))
	if row = lines[1]; !strings.Contains(row, longSubject) || strings.Contains(row, "…") {
		t.Fatalf("full subject not printed: %q", row)
	}

	// A positive override caps the subject at that many runes.
	lines = tableLines(t, render(25))
	if row = lines[1]; !strings.Contains(row, "…") || strings.Contains(row, longSubject[:26]) {
		t.Fatalf("--subject-width 25 not applied: %q", row)
	}
}

func TestRenderThreadsSubjectColumnObeysWidthOverride(t *testing.T) {
	threads := []threadSummary{{
		ID:           "42",
		Subject:      longSubject,
		Count:        2,
		LatestDate:   "2026-08-27T00:00:00Z",
		Participants: []string{"billing@example.com"},
	}}
	render := func(width int) string {
		var b bytes.Buffer
		renderThreads(threads)(&output.Printer{Out: &b, SubjectWidth: width})
		return b.String()
	}

	lines := tableLines(t, render(0))
	header, row := lines[0], lines[1]
	if !strings.Contains(header, "PARTICIPANTS") || strings.Index(header, "PARTICIPANTS") > strings.Index(header, "SUBJECT") {
		t.Fatalf("PARTICIPANTS must precede SUBJECT: %q", header)
	}
	if strings.Index(row, "billing@example.com") > strings.Index(row, "Sed ut") {
		t.Fatalf("participants must precede the subject: %q", row)
	}
	if !strings.Contains(row, "…") || strings.Contains(row, longSubject) {
		t.Fatalf("default subject budget not applied: %q", row)
	}

	lines = tableLines(t, render(-1))
	if row = lines[1]; !strings.Contains(row, longSubject) || strings.Contains(row, "…") {
		t.Fatalf("full subject not printed: %q", row)
	}
}

func TestSubjectWidthFlagShapesPlainSearch(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MAIL_APP_CLI_DISABLE_ENVELOPE_INDEX", "1")
	t.Setenv("PATH", bin)
	script := `#!/bin/sh
case "$*" in
  *"const searchTerms"*) printf '%s' '{"messages":[{"id":"42","subject":"` + longSubject + `","sender":"billing@example.com","dateReceived":"2026-08-27T00:00:00Z","dateSent":"2026-08-27T00:00:00Z","read":true,"flagged":false,"messageSize":1,"mailbox":"INBOX","account":"Work"}],"truncated":false,"readFailures":0,"scannedMessages":1,"totalMessages":1}' ;;
  *) printf '%s' '[{"id":"work","name":"Work","enabled":true}]' ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "osascript"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"search", "anything", "--account", "Work", "--mailbox", "INBOX", "--plain", "--no-color"}

	// --subject-width 0 prints the subject in full.
	code, stdout, stderr := run(t, append(args, "--subject-width", "0")...)
	if code != 0 {
		t.Fatalf("exit = %d, stdout = %s, stderr = %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, longSubject) || strings.Contains(stdout, "…") {
		t.Fatalf("--subject-width 0 did not print the full subject: %s", stdout)
	}

	// Without the flag the built-in 70-rune budget applies.
	code, stdout, stderr = run(t, args...)
	if code != 0 {
		t.Fatalf("exit = %d, stdout = %s, stderr = %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "…") || strings.Contains(stdout, longSubject) {
		t.Fatalf("default subject budget missing: %s", stdout)
	}
	var header string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "SUBJECT") {
			header = line
			break
		}
	}
	if !strings.Contains(header, "LOCATION") || strings.Index(header, "LOCATION") > strings.Index(header, "SUBJECT") {
		t.Fatalf("LOCATION must precede SUBJECT: %q", header)
	}

	// A positive value caps the subject at that many runes.
	code, stdout, stderr = run(t, append(args, "--subject-width", "25")...)
	if code != 0 {
		t.Fatalf("exit = %d, stdout = %s, stderr = %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "…") || strings.Contains(stdout, longSubject[:26]) {
		t.Fatalf("--subject-width 25 not applied: %s", stdout)
	}
}

func TestSubjectWidthFlagValidation(t *testing.T) {
	code, _, stderr := run(t, "version", "--subject-width", "-3")
	if code != 1 || !strings.Contains(stderr, "--subject-width must be 0 or greater") {
		t.Fatalf("negative width exit = %d, stderr = %s", code, stderr)
	}
	code, _, stderr = run(t, "version", "--subject-width", "0")
	if code != 0 || stderr != "" {
		t.Fatalf("zero width exit = %d, stderr = %s", code, stderr)
	}
}
