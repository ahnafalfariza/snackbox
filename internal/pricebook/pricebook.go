// Package pricebook is the device-side half of pricing: verify a signed
// pricebook, and look up a price. No rules are evaluated here.
package pricebook

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ahnafalfariza/snackbox/internal/protocol"
)

// MaxClockAge: an RTC not synced for longer than this is not trusted for
// promo windows; the machine charges base price.
const MaxClockAge = 7 * 24 * time.Hour

func SHA256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Verify checks hash, signature and content of a blob before it may replace
// the machine's last-known-good pricebook. Any failure rejects the whole
// pricebook.
func Verify(blob protocol.Blob, want protocol.PricebookRef, pub ed25519.PublicKey, machine string, current uint64) (*protocol.Pricebook, error) {
	if want.SHA256 != "" && SHA256(blob.Payload) != want.SHA256 {
		return nil, errors.New("sha256 mismatch")
	}
	if !ed25519.Verify(pub, blob.Payload, blob.Sig) {
		return nil, errors.New("bad signature")
	}
	var pb protocol.Pricebook
	if err := json.Unmarshal(blob.Payload, &pb); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if pb.Machine != machine {
		return nil, fmt.Errorf("pricebook is for %s", pb.Machine)
	}
	if pb.Ver <= current {
		return nil, fmt.Errorf("ver %d is not newer than %d", pb.Ver, current)
	}
	if err := CheckBounds(&pb); err != nil {
		return nil, err
	}
	return &pb, nil
}

// CheckBounds rejects any price outside the signed per-SKU min/max, even in
// a validly signed pricebook (a compiler bug must not sell drinks for 0).
func CheckBounds(pb *protocol.Pricebook) error {
	check := func(sku string, p int64) error {
		b, ok := pb.Bounds[sku]
		if !ok {
			return fmt.Errorf("%s has no bounds", sku)
		}
		if p < b[0] || p > b[1] {
			return fmt.Errorf("%s price %d outside bounds [%d, %d]", sku, p, b[0], b[1])
		}
		return nil
	}
	for sku, p := range pb.Base {
		if err := check(sku, p); err != nil {
			return err
		}
	}
	for _, w := range pb.Windows {
		for sku, p := range w.Prices {
			if err := check(sku, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// Source says where a price came from; it is reported in heartbeats.
const (
	SourceTimeline = "timeline"
	SourceHorizon  = "base:horizon" // offline past horizon_end
	SourceClock    = "base:clock"   // RTC not synced recently
)

// Lookup returns the price for sku at now. Windows never overlap for one SKU
// (the compiler guarantees it), so the first match is the answer.
func Lookup(pb *protocol.Pricebook, sku string, now, clockSyncedAt time.Time) (price int64, promo, source string, ok bool) {
	base, ok := pb.Base[sku]
	if !ok {
		return 0, "", "", false
	}
	if now.Sub(clockSyncedAt) > MaxClockAge {
		return base, "", SourceClock, true
	}
	if !now.Before(pb.HorizonEnd) {
		return base, "", SourceHorizon, true
	}
	for _, w := range pb.Windows {
		if now.Before(w.From) || !now.Before(w.To) {
			continue
		}
		if p, hit := w.Prices[sku]; hit {
			return p, w.Promo, SourceTimeline, true
		}
	}
	return base, "", SourceTimeline, true
}

// Active lists promos whose window contains now.
func Active(pb *protocol.Pricebook, now, clockSyncedAt time.Time) []string {
	out := []string{}
	if now.Sub(clockSyncedAt) > MaxClockAge || !now.Before(pb.HorizonEnd) {
		return out
	}
	seen := map[string]bool{}
	for _, w := range pb.Windows {
		if !now.Before(w.From) && now.Before(w.To) && !seen[w.Promo] {
			seen[w.Promo] = true
			out = append(out, w.Promo)
		}
	}
	return out
}
