package mail

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/0xSMW/mail.app-cli/v3/internal/appleevents"
)

// errMoveGroupRejected reports Mail refusing the list form of move before
// executing it. Nothing was applied, so the un-dispatched messages may be
// re-sent individually.
var errMoveGroupRejected = errors.New("Mail refused the list form of move")

// moveGroupUncertainError marks a failed group move whose partial application
// cannot be ruled out. Its messages are unknown and must never be replayed.
type moveGroupUncertainError struct{ cause error }

func (e *moveGroupUncertainError) Error() string {
	return "bulk move outcome uncertain: " + e.cause.Error()
}

func (e *moveGroupUncertainError) Unwrap() error { return e.cause }

// maxMoveGroupRefs bounds one move event's direct object. Larger groups split
// into consecutive sub-events inside the same transaction, each with its own
// outcome.
const maxMoveGroupRefs = 100

// maxMoveGroupTimeout matches the ceiling ValidateBatchOptions enforces for
// --settle.
const maxMoveGroupTimeout = 5 * time.Minute

// moveGroupTimeout scales a group's dispatch budget with its size: the
// per-message automation floor plus a per-message allowance for Mail's copy
// work.
func moveGroupTimeout(count int) time.Duration {
	if count < 1 {
		count = 1
	}
	timeout := defaultAutomationTimeout + time.Duration(2*(count-1))*time.Second
	if timeout > maxMoveGroupTimeout {
		return maxMoveGroupTimeout
	}
	return timeout
}

// groupMessageRef pairs a batch item's message ID with the reference Mail
// confirmed inside the source mailbox.
type groupMessageRef struct {
	id  string
	num int64
	ref appleevents.Value
}

// nativeMoveGroup moves every id from one source mailbox to one destination
// inside a single automation transaction: the account, both mailboxes, and
// message existence are resolved once, then one core/move carries the
// confirmed references as a list. It returns the ids Mail confirmed and
// moved, and the ids it could not confirm in the source; those were never
// dispatched. Ids in neither list were not safely completed and the error
// classifies them: an error wrapping errMoveGroupRejected proves Mail
// refused the list form before executing it, so those ids may be re-sent per
// item; any other error leaves the outcome uncertain and they must never be
// replayed.
func (c *Client) nativeMoveGroup(account, mailbox, destination string, ids []string) (moved, missing []string, err error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	timeout := moveGroupTimeout(len(ids))
	err = c.withNativeAutomation(timeout, func(n *nativeMail) error {
		var acc, source, dest appleevents.Value
		if err := n.phase("resolve_group", func() error {
			var err error
			acc, err = n.account(account)
			if err != nil {
				return err
			}
			source, err = n.mailbox(acc, mailbox)
			if err != nil {
				return err
			}
			dest, err = n.mailbox(acc, destination)
			return err
		}); err != nil {
			return err
		}
		refs, unconfirmed, err := n.confirmGroupRefs(source, ids)
		if err != nil {
			return err
		}
		missing = unconfirmed
		if len(refs) == 0 {
			return nil
		}
		return n.phase("bulk_move", func() error {
			for start := 0; start < len(refs); start += maxMoveGroupRefs {
				end := min(start+maxMoveGroupRefs, len(refs))
				batch := refs[start:end]
				list := make([]appleevents.Value, len(batch))
				for i, ref := range batch {
					list[i] = ref.ref
				}
				params := map[string]appleevents.Value{"----": appleevents.List(list...), "insh": dest}
				if _, err := n.eventWithTimeout("core", "move", params, timeout); err != nil {
					if nativeUnsupportedRead(err) {
						// Mail rejected the list form itself: a parameter
						// error that precedes execution, so nothing was
						// applied and per-item dispatch remains safe.
						return fmt.Errorf("%w: %v", errMoveGroupRejected, err)
					}
					// A list move that fails mid-execution may have applied
					// a prefix; partial application cannot be ruled out.
					return &moveGroupUncertainError{cause: err}
				}
				for _, ref := range batch {
					moved = append(moved, ref.id)
				}
			}
			return nil
		})
	})
	return moved, missing, err
}

// confirmGroupRefs proves each message exists in the source mailbox before
// any mutation is issued. One bulk read asks for every reference's ID
// property, the same list-of-property-specifiers shape getFields uses. Mail
// refusing that read, or one reference missing from it, falls back to
// per-item existence checks so the clean not-found classification survives.
// A transport-level failure is returned without fallback: hammering a broken
// bridge with per-item reads proves nothing.
func (n *nativeMail) confirmGroupRefs(source appleevents.Value, ids []string) (refs []groupMessageRef, missing []string, err error) {
	err = n.phase("confirm_refs", func() error {
		refs = make([]groupMessageRef, 0, len(ids))
		for _, id := range ids {
			// Invalid IDs are validated exactly as n.message validates them
			// today; they can never be confirmed, so they classify as
			// missing instead of failing the whole group.
			number, perr := strconv.ParseInt(id, 10, 64)
			if perr != nil || number <= 0 {
				missing = append(missing, id)
				continue
			}
			refs = append(refs, groupMessageRef{id: id, num: number, ref: appleevents.ByID("mssg", appleevents.Int(number), source)})
		}
		if len(refs) == 0 {
			return nil
		}
		specifiers := make([]appleevents.Value, len(refs))
		for i, ref := range refs {
			specifiers[i] = appleevents.Property("ID  ", ref.ref)
		}
		values, gerr := n.get(appleevents.List(specifiers...))
		if gerr == nil && values.Kind == "list" && len(values.Items) == len(refs) {
			confirmed := refs[:0]
			for i, value := range values.Items {
				if value.Kind == "int" && value.Number == refs[i].num {
					confirmed = append(confirmed, refs[i])
					continue
				}
				missing = append(missing, refs[i].id)
			}
			refs = confirmed
			return nil
		}
		var reply *appleevents.Error
		if gerr != nil && !errors.As(gerr, &reply) {
			// The bridge failed before Mail answered; no fallback.
			return gerr
		}
		// Mail refused the list form or returned an unusable shape: confirm
		// each reference individually, preserving not-found classification.
		confirmed := make([]groupMessageRef, 0, len(refs))
		for _, ref := range refs {
			confirmedRef, merr := n.message(source, ref.id)
			if merr != nil {
				if errors.Is(merr, ErrNotFound) {
					missing = append(missing, ref.id)
					continue
				}
				return merr
			}
			confirmed = append(confirmed, groupMessageRef{id: ref.id, num: ref.num, ref: confirmedRef})
		}
		refs = confirmed
		return nil
	})
	return refs, missing, err
}
