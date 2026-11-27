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

func TestRenderMessagesNoTruncatePrintsOnlyTrailingColumnFull(t *testing.T) {
	senderName := "Alice Bob Charlie Delta Echo Frank Gina"
	messages := []mail.Message{{
		ID:           "42",
		Subject:      longSubject,
		Sender:       senderName + " <billing@example.com>",
		Account:      "Work",
		Mailbox:      "INBOX",
		DateReceived: "2026-08-27T00:00:00Z",
		Read:         true,
	}}
	render := func(noTruncate bool) string {
		var b bytes.Buffer
		renderMessages(messages, true)(&output.Printer{Out: &b, NoTruncate: noTruncate})
		return b.String()
	}

	// The subject stays the last column so --fzf can print it in full
	// without pushing the location (or anything else) out of view.
	lines := tableLines(t, render(false))
	header, row := lines[0], lines[1]
	if !strings.Contains(header, "LOCATION") || strings.Index(header, "LOCATION") > strings.Index(header, "SUBJECT") {
		t.Fatalf("LOCATION must precede SUBJECT: %q", header)
	}
	if strings.Index(row, "Work/INBOX") > strings.Index(row, "Sed ut") {
		t.Fatalf("location must precede the subject: %q", row)
	}

	// The zero value keeps the built-in 70-rune subject budget.
	if !strings.Contains(row, "…") || strings.Contains(row, longSubject) {
		t.Fatalf("default subject budget not applied: %q", row)
	}

	// NoTruncate (--fzf) prints the subject in full...
	lines = tableLines(t, render(true))
	if row = lines[1]; !strings.Contains(row, longSubject) {
		t.Fatalf("full subject not printed: %q", row)
	}
	// ...while the sender column keeps its budget.
	if strings.Contains(row, senderName) {
		t.Fatalf("sender budget must survive NoTruncate: %q", row)
	}
}

func TestRenderThreadsNoTruncatePrintsOnlyTrailingColumnFull(t *testing.T) {
	threadID := strings.Repeat("x", 50)
	threads := []threadSummary{{
		ID:           threadID,
		Subject:      longSubject,
		Count:        2,
		LatestDate:   "2026-08-27T00:00:00Z",
		Participants: []string{"billing@example.com"},
	}}
	render := func(noTruncate bool) string {
		var b bytes.Buffer
		renderThreads(threads)(&output.Printer{Out: &b, NoTruncate: noTruncate})
		return b.String()
	}

	lines := tableLines(t, render(false))
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

	// NoTruncate prints the subject in full but leaves the thread ID at
	// its 40-rune budget.
	lines = tableLines(t, render(true))
	if row = lines[1]; !strings.Contains(row, longSubject) {
		t.Fatalf("full subject not printed: %q", row)
	}
	if strings.Contains(row, threadID) {
		t.Fatalf("thread ID budget must survive NoTruncate: %q", row)
	}
}

func TestFZFFlagShapesPlainSearch(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MAIL_APP_CLI_DISABLE_ENVELOPE_INDEX", "1")
	t.Setenv("NO_COLOR", "")
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
	args := []string{"search", "anything", "--account", "Work", "--mailbox", "INBOX"}

	// --fzf implies the human view, keeps color in the pipe, and prints
	// the subject in full.
	code, stdout, stderr := run(t, append(args, "--fzf")...)
	if code != 0 {
		t.Fatalf("exit = %d, stdout = %s, stderr = %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, longSubject) {
		t.Fatalf("--fzf did not print the full subject: %s", stdout)
	}
	if !strings.Contains(stdout, "\x1b[") {
		t.Fatalf("--fzf did not keep color in a pipe: %s", stdout)
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

	// --fzf overrides NO_COLOR.
	t.Setenv("NO_COLOR", "1")
	code, stdout, stderr = run(t, append(args, "--fzf")...)
	if code != 0 {
		t.Fatalf("exit = %d, stdout = %s, stderr = %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "\x1b[") {
		t.Fatalf("--fzf did not override NO_COLOR: %s", stdout)
	}

	// Without --fzf a pipe gets the uncolored JSON envelope.
	t.Setenv("NO_COLOR", "")
	code, stdout, stderr = run(t, args...)
	if code != 0 {
		t.Fatalf("exit = %d, stdout = %s, stderr = %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "\"schemaVersion\"") || strings.Contains(stdout, "\x1b[") {
		t.Fatalf("piped default = %s, want the JSON envelope without ANSI", stdout)
	}
}
