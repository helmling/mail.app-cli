package mail

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/0xSMW/mail.app-cli/v3/internal/appleevents"
)

// errMoveGroupRejected reports Mail refusing the grouped form of move before
// executing it. Nothing was applied, so the un-dispatched messages may be
// re-sent individually.
var errMoveGroupRejected = errors.New("Mail refused the grouped form of move")

// moveGroupUncertainError marks a failed group move whose partial application
// cannot be ruled out. Its messages are unknown and must never be replayed.
type moveGroupUncertainError struct{ cause error }

func (e *moveGroupUncertainError) Error() string {
	return "bulk move outcome uncertain: " + e.cause.Error()
}

func (e *moveGroupUncertainError) Unwrap() error { return e.cause }

// maxMoveGroupRefs bounds one move event's whose-clause selector. Larger
// groups split into consecutive sub-events inside the same transaction, each
// with its own outcome.
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

// groupMessageRef pairs a batch item's message ID with the numeric form the
// whose-clause selectors compare.
type groupMessageRef struct {
	id  string
	num int64
}

// nativeMoveGroup moves every id from one source mailbox to one destination
// inside a single automation transaction: the account, both mailboxes, and
// message existence are resolved once, then one core/move carries a
// whose-clause selector covering the confirmed ids — every message of the
// source whose ID equals one of them. It returns the ids Mail confirmed and
// moved, and the ids it could not confirm in the source; those were never
// dispatched. Ids in neither list were not safely completed and the error
// classifies them: an error wrapping errMoveGroupRejected proves Mail
// refused the grouped move before executing it, so those ids may be re-sent
// per item; any other error leaves the outcome uncertain and they must never
// be replayed.
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
				params := map[string]appleevents.Value{"----": moveGroupSelector(source, batch), "insh": dest}
				if _, err := n.eventWithTimeout("core", "move", params, timeout); err != nil {
					if nativeUnsupportedRead(err) {
						// Mail rejected the grouped move itself: a parameter
						// error that precedes execution, so nothing was
						// applied and per-item dispatch remains safe.
						return fmt.Errorf("%w: %v", errMoveGroupRejected, err)
					}
					// A group move that fails mid-execution may have applied
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

// moveGroupSelector builds the whose-clause selector covering the given
// references: every message of the source mailbox whose ID equals one of
// them, joined by OR over at most maxMoveGroupRefs terms. Mail refuses a
// list direct object for both reads and moves — a coercion error — so the
// grouped existence read and the group move both dispatch this selector.
func moveGroupSelector(source appleevents.Value, refs []groupMessageRef) appleevents.Value {
	tests := make([]appleevents.Value, len(refs))
	for i, ref := range refs {
		tests[i] = appleevents.Comparison("=   ", appleevents.Property("ID  ", appleevents.Examined()), appleevents.Int(ref.num))
	}
	return appleevents.ByTest("mssg", appleevents.Logical("OR  ", tests...), source)
}

// errGroupReadShape reports Mail answering the grouped existence read with a
// value whose shape cannot be classified; the per-item fallback re-asks one
// reference at a time.
var errGroupReadShape = errors.New("Mail answered the grouped existence read in an unusable shape")

// confirmGroupRefs proves each message exists in the source mailbox before
// any mutation is issued. One grouped read asks for the ID property of every
// message of the source whose ID equals one of the requested ids — the
// read-side twin of the group move's selector, and the only grouped read
// shape live Mail accepts: a list direct object is refused for reads exactly
// as it is for moves, which degrades the whole phase to per-item reads. Mail
// refusing the grouped read, or answering in an unusable shape, falls back
// to per-item existence checks so the clean not-found classification
// survives. A transport-level failure is returned without fallback:
// hammering a broken bridge with per-item reads proves nothing.
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
			refs = append(refs, groupMessageRef{id: id, num: number})
		}
		if len(refs) == 0 {
			return nil
		}
		present, gerr := n.readGroupIDs(source, refs)
		if gerr == nil {
			confirmed := refs[:0]
			for _, ref := range refs {
				if present[ref.num] {
					confirmed = append(confirmed, ref)
					continue
				}
				missing = append(missing, ref.id)
			}
			refs = confirmed
			return nil
		}
		var reply *appleevents.Error
		if !errors.As(gerr, &reply) && !errors.Is(gerr, errGroupReadShape) {
			// The bridge failed before Mail answered; no fallback.
			return gerr
		}
		// Mail refused the grouped read or answered in an unusable shape:
		// confirm each reference individually, preserving not-found
		// classification.
		confirmed := make([]groupMessageRef, 0, len(refs))
		for _, ref := range refs {
			if _, merr := n.message(source, ref.id); merr != nil {
				if errors.Is(merr, ErrNotFound) {
					missing = append(missing, ref.id)
					continue
				}
				return merr
			}
			confirmed = append(confirmed, ref)
		}
		refs = confirmed
		return nil
	})
	return refs, missing, err
}

// readGroupIDs asks for the ID property of every message of the source whose
// ID equals one of the requested numbers: one whose-clause read per
// maxMoveGroupRefs chunk, dispatching the same selector the group move later
// sends. The reply carries the IDs of the messages that exist, in Mail's own
// order.
func (n *nativeMail) readGroupIDs(source appleevents.Value, refs []groupMessageRef) (map[int64]bool, error) {
	present := make(map[int64]bool, len(refs))
	for start := 0; start < len(refs); start += maxMoveGroupRefs {
		batch := refs[start:min(start+maxMoveGroupRefs, len(refs))]
		values, err := n.get(appleevents.Property("ID  ", moveGroupSelector(source, batch)))
		if err != nil {
			return nil, err
		}
		switch values.Kind {
		case "int":
			// A single match may come back uncoerced from its list.
			present[values.Number] = true
		case "list":
			for _, item := range values.Items {
				if item.Kind != "int" {
					return nil, errGroupReadShape
				}
				present[item.Number] = true
			}
		default:
			return nil, errGroupReadShape
		}
	}
	return present, nil
}
