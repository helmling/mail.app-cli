//go:build darwin && cgo

package mail

// Temporary read-only diagnostic: times identity snapshots on the live
// Envelope Index at increasing identity counts, mirroring the two-scope
// (source + destination) shape verifyMutationRelocations builds. Nothing is
// mutated; the ids must be real messages of MAIL_AE_PROBE_MAILBOX.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestIdentityScaleProbe(t *testing.T) {
	if os.Getenv("MAIL_AE_IDENTITY_SCALE_PROBE") != "1" {
		t.Skip("identity scale probe disabled")
	}
	account := os.Getenv("MAIL_AE_PROBE_ACCOUNT")
	source := os.Getenv("MAIL_AE_PROBE_MAILBOX")
	destination := os.Getenv("MAIL_AE_IDENTITY_SCALE_PROBE_DEST")
	idsArg := os.Getenv("MAIL_AE_IDENTITY_SCALE_PROBE_IDS")
	if account == "" || source == "" || destination == "" || idsArg == "" {
		t.Fatal("identity scale probe requires MAIL_AE_PROBE_ACCOUNT, MAIL_AE_PROBE_MAILBOX, MAIL_AE_IDENTITY_SCALE_PROBE_DEST, MAIL_AE_IDENTITY_SCALE_PROBE_IDS")
	}
	var ids []string
	for _, field := range strings.Split(idsArg, ",") {
		id := strings.TrimSpace(field)
		if id == "" {
			continue
		}
		if _, err := strconv.ParseInt(id, 10, 64); err != nil {
			t.Fatalf("probe id %q is not numeric", id)
		}
		ids = append(ids, id)
	}
	if len(ids) < 3 {
		t.Fatalf("need at least 3 ids, got %d", len(ids))
	}

	c := NewClient()
	c.shared.backend = BackendNative

	identities := make([]StableIdentity, 0, len(ids))
	for _, id := range ids {
		message, err := c.GetMessageDetailsForVerificationJSON(account, source, id)
		if err != nil {
			t.Fatalf("read identity for %s: %v", id, err)
		}
		identity, err := completeStableIdentity(StableIdentityFromMessage(*message), message, nil)
		if err != nil {
			t.Fatalf("identity for %s: %v", id, err)
		}
		identities = append(identities, identity)
	}

	// RFC identities force one live per-candidate enrichment read inside the
	// snapshot's match phase; fallback identities match on indexed metadata
	// alone. Timing both variants separates SQL cost from enrichment cost.
	variants := []struct {
		name      string
		stripRFC  bool
		identities []StableIdentity
	}{
		{name: "rfc"},
		{name: "fallback", stripRFC: true},
	}
	for _, count := range []int{3, 10, len(identities)} {
		if count > len(identities) {
			continue
		}
		for i := range variants {
			variant := &variants[i]
			variant.identities = append([]StableIdentity(nil), identities[:count]...)
			if variant.stripRFC {
				for j := range variant.identities {
					variant.identities[j].RFCMessageID = ""
				}
			}
			scopes := []identityVerificationScope{
				{key: verificationMailboxKey{account, source}, identities: variant.identities},
				{key: verificationMailboxKey{account, destination}, identities: variant.identities},
			}
			started := time.Now()
			snapshot := c.getIdentitySnapshotForVerification(scopes)
			elapsed := time.Since(started)
			matchedSource, matchedDest, errs := 0, 0, 0
			for key, result := range snapshot {
				if result.err != nil {
					errs++
					t.Logf("scope %s error: %v", key.mailbox, result.err)
					continue
				}
				for _, identity := range variant.identities {
					if len(result.matches[identity.verificationKey()]) > 0 {
						if key.mailbox == source {
							matchedSource++
						} else {
							matchedDest++
						}
					}
				}
			}
			fmt.Printf("identityScale variant=%s count=%d elapsedMS=%.1f matchedSource=%d matchedDest=%d scopeErrors=%d\n",
				variant.name, count, float64(elapsed.Microseconds())/1000, matchedSource, matchedDest, errs)
			t.Logf("identityScale variant=%s count=%d elapsedMS=%.1f matchedSource=%d matchedDest=%d scopeErrors=%d",
				variant.name, count, float64(elapsed.Microseconds())/1000, matchedSource, matchedDest, errs)
		}
	}
}
