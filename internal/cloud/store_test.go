package cloud

import (
	"testing"
	"time"

	"github.com/ahnafalfariza/snackbox/internal/clock"
	"github.com/ahnafalfariza/snackbox/internal/pricebook"
	"github.com/ahnafalfariza/snackbox/internal/protocol"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(clock.Real{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Register("M1", "Asia/Jakarta", "test"); err != nil {
		t.Fatal(err)
	}
	return s
}

func ev(seq uint64, price int64) protocol.Event {
	return protocol.Event{MachineID: "M1", Seq: seq, PriceMinor: price, TsDevice: time.Now()}
}

func TestIngestIsIdempotent(t *testing.T) {
	s := newStore(t)
	batch := protocol.EventBatch{Events: []protocol.Event{ev(1, 100), ev(2, 200), ev(3, 300)}}

	ack1, _ := s.IngestEvents("M1", batch)
	ack2, _ := s.IngestEvents("M1", batch) // ack was lost; machine resends
	if ack1.UptoSeq != 3 || ack2.UptoSeq != 3 {
		t.Fatalf("acks %d %d, want 3 3", ack1.UptoSeq, ack2.UptoSeq)
	}
	sum := s.SalesSummary()["M1"]
	if sum.Count != 3 || sum.Revenue != 600 {
		t.Fatalf("got %+v, want 3 sales / 600", sum)
	}
	if st := s.State(); st.Duplicates != 3 {
		t.Fatalf("want 3 duplicates, got %d", st.Duplicates)
	}
}

func TestIngestGapHoldsWatermark(t *testing.T) {
	s := newStore(t)
	ack, _ := s.IngestEvents("M1", protocol.EventBatch{Events: []protocol.Event{ev(1, 1), ev(3, 3)}})
	if ack.UptoSeq != 1 {
		t.Fatalf("gap at 2: ack must stop at 1, got %d", ack.UptoSeq)
	}
	ack, _ = s.IngestEvents("M1", protocol.EventBatch{Events: []protocol.Event{ev(2, 2), ev(3, 3)}})
	if ack.UptoSeq != 3 {
		t.Fatalf("gap filled: want 3, got %d", ack.UptoSeq)
	}
	if sum := s.SalesSummary()["M1"]; sum.Count != 3 || sum.Revenue != 6 || sum.Pending != 0 {
		t.Fatalf("got %+v", sum)
	}
}

func TestBadPricebookIsSignedButOutOfBounds(t *testing.T) {
	s := newStore(t)
	s.PublishBad()
	sh, _ := s.Shadow("M1")
	blob, _ := s.PricebookBlob("M1", sh.Desired.Pricebook.Ver)
	_, err := pricebook.Verify(blob, sh.Desired.Pricebook, s.PublicKey(), "M1", 2)
	if err == nil {
		t.Fatal("agent must refuse a 0 IDR price even when the signature is valid")
	}
	s.RollForward()
	sh, _ = s.Shadow("M1")
	blob, _ = s.PricebookBlob("M1", sh.Desired.Pricebook.Ver)
	pb, err := pricebook.Verify(blob, sh.Desired.Pricebook, s.PublicKey(), "M1", 2)
	if err != nil || pb.Ver != 4 {
		t.Fatalf("roll forward should give a good v4, got %v %v", pb, err)
	}
}

func TestTamperedPricebookRejected(t *testing.T) {
	s := newStore(t)
	sh, _ := s.Shadow("M1")
	blob, _ := s.PricebookBlob("M1", sh.Desired.Pricebook.Ver)
	blob.Payload[len(blob.Payload)-2] ^= 1
	if _, err := pricebook.Verify(blob, protocol.PricebookRef{}, s.PublicKey(), "M1", 1); err == nil {
		t.Fatal("tampered payload must fail signature check")
	}
	if _, err := pricebook.Verify(blob, sh.Desired.Pricebook, s.PublicKey(), "M1", 1); err == nil {
		t.Fatal("tampered payload must fail sha256 check")
	}
}
