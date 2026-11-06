package mail

import (
	"encoding/json"
	"errors"
	"fmt"
)

// legacyMoveGroup moves every id from one source mailbox to one destination
// in a single JXA run: the account, both mailboxes, and every message are
// resolved first, then the confirmed ids move as one whose-clause selector —
// at most maxMoveGroupRefs ids per move call — and the run reports per-ID
// status as one JSON reply. The outcome contract matches nativeMoveGroup: it
// returns the ids the script confirmed and moved, and the ids it could not
// confirm in the source; those were never dispatched. Ids in neither list
// were not safely completed and the error classifies them: a "rejected"
// reply proves Mail refused the grouped move before executing it, so those
// ids may be re-sent per item; an "error" reply proves the run stopped
// before any mutation; any other failure — a lost process, a timeout, an
// unparsable reply — leaves the outcome uncertain and they must never be
// replayed.
func (c *Client) legacyMoveGroup(account, mailbox, destination string, ids []string) (moved, missing []string, err error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	script := legacyMoveGroupScript(account, mailbox, destination, ids)
	output, err := c.runJXAWithTimeout(script, moveGroupTimeout(len(ids)))
	if err != nil {
		return nil, nil, classifyLegacyMoveGroupTransport(err)
	}
	var reply struct {
		Status  string   `json:"status"`
		Moved   []string `json:"moved"`
		Missing []string `json:"missing"`
		Message string   `json:"message"`
	}
	if err := json.Unmarshal([]byte(output), &reply); err != nil {
		return nil, nil, &moveGroupUncertainError{cause: fmt.Errorf("failed to parse bulk move status: %w", err)}
	}
	switch reply.Status {
	case "ok":
		return reply.Moved, reply.Missing, nil
	case "rejected":
		return reply.Moved, reply.Missing, fmt.Errorf("%w: %s", errMoveGroupRejected, reply.Message)
	case "uncertain":
		return reply.Moved, reply.Missing, &moveGroupUncertainError{cause: errors.New(reply.Message)}
	case "error":
		// The script stopped before any mutation was attempted, so the
		// per-item path keeps its own classification and words.
		return nil, nil, bridgeError(reply.Message)
	}
	return nil, nil, &moveGroupUncertainError{cause: fmt.Errorf("unrecognized bulk move status %q", reply.Status)}
}

// moveGroup dispatches one homogeneous move group through the active
// backend: the native automation transaction or the legacy one-script run.
// Both primitives share the outcome contract applyMoveGroup consumes.
func (c *Client) moveGroup(account, mailbox, destination string, ids []string) (moved, missing []string, err error) {
	if c.nativeEnabled() {
		return c.nativeMoveGroup(account, mailbox, destination, ids)
	}
	return c.legacyMoveGroup(account, mailbox, destination, ids)
}

// classifyLegacyMoveGroupTransport maps a transport failure of the group
// script onto the group outcome contract. A lock timeout proves the run
// never began, so it stays raw and replayable, exactly like the native
// transaction's own lock errors; any other failure may have reached the
// move call before the process, bridge, or deadline was lost, so the
// outcome is uncertain and the ids are never replayed.
func classifyLegacyMoveGroupTransport(err error) error {
	var lockErr *AutomationLockTimeoutError
	if errors.As(err, &lockErr) {
		return err
	}
	return &moveGroupUncertainError{cause: err}
}

// legacyMoveGroupScript builds the one JXA run a legacy group performs:
// per-ID resolution with the same helper the per-item move uses, one
// whose-clause move per maxMoveGroupRefs confirmed ids, and a JSON status
// reply. The script catches its own failures so a reply always classifies
// the outcome; only the transport losing the process or the reply is
// uncertain.
func legacyMoveGroupScript(account, mailbox, destination string, ids []string) string {
	encoded, err := json.Marshal(ids)
	if err != nil {
		// ids is a []string; marshaling cannot fail.
		encoded = []byte("[]")
	}
	return fmt.Sprintf(`
const mail = Application('Mail');
const requestedMailbox = '%s';
const requestedTargetMailbox = '%s';
const ids = %s;
%s
%s
function moveGroupRefused(e) {
	const number = e ? e.errorNumber : undefined;
	return number === -1708 || number === -1700 || number === -10000;
}
function idSelector(mbox, ids) {
	if (ids.length === 1) {
		return mbox.messages.whose({id: {_equals: ids[0]}});
	}
	return mbox.messages.whose({_or: ids.map(function (id) { return {id: {_equals: id}}; })});
}
let reply;
try {
	const acc = mail.accounts.byName('%s');
	const sourceMbox = %s;
	if (sourceMbox === null) throw 'Error: Mailbox not found: ' + requestedMailbox;
	const destMbox = %s;
	if (destMbox === null) throw 'Error: Mailbox not found: ' + requestedTargetMailbox;
	const pending = [];
	const missing = [];
	for (const id of ids) {
		if (messageById(sourceMbox, String(id)) === null) {
			missing.push(String(id));
			continue;
		}
		pending.push(String(id));
	}
	const moved = [];
	try {
		for (let start = 0; start < pending.length; start += %d) {
			const end = Math.min(start + %d, pending.length);
			mail.move(idSelector(sourceMbox, pending.slice(start, end)), {to: destMbox});
			for (let i = start; i < end; i++) {
				moved.push(pending[i]);
			}
		}
		reply = {status: 'ok', moved: moved, missing: missing};
	} catch (e) {
		if (moveGroupRefused(e)) {
			// Mail refused the grouped move itself: a parameter error
			// that precedes execution, so nothing unaccounted for was
			// applied.
			reply = {status: 'rejected', moved: moved, missing: missing, message: String(e)};
		} else {
			// A group move that fails mid-execution may have applied a
			// prefix; partial application cannot be ruled out.
			reply = {status: 'uncertain', moved: moved, missing: missing, message: String(e)};
		}
	}
} catch (e) {
	// Resolution failed before any mutation was attempted.
	reply = {status: 'error', message: String(e)};
}
JSON.stringify(reply);
`, escapeJSString(mailbox), escapeJSString(destination), encoded, jxaMailboxLookupHelper(), jxaMessageByIdHelper(), escapeJSString(account), jxaMailboxLookupExpression(mailbox), jxaMailboxLookupExpressionFor(destination, "requestedTargetMailbox"), maxMoveGroupRefs, maxMoveGroupRefs)
}
