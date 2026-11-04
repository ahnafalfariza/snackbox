// Package agent is the machine-side edge agent: it sells from its local
// pricebook with no connection, keeps every sale in an append-only outbox
// until the cloud acks it, and converges on the cloud's desired state.
//
// Production: Rust on the controller with SQLite WAL, fsync per sale before
// the vend is confirmed. Here the "disk" is memory; the protocol is the same.
package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/ahnafalfariza/snackbox/internal/clock"
	"github.com/ahnafalfariza/snackbox/internal/pricebook"
	"github.com/ahnafalfariza/snackbox/internal/protocol"
)

var (
	ErrOutOfStock  = errors.New("out of stock")
	ErrNoPricebook = errors.New("no pricebook")
)

type Options struct {
	ID, TZ, Site string
	Clock        clock.Clock
	BaseURL      string
	HTTP         *http.Client
	PublicKey    ed25519.PublicKey
	BatchSize    int // events per upload
}

type Agent struct {
	o   Options
	loc *time.Location

	mu            sync.Mutex
	pb            *protocol.Pricebook // last-known-good
	rejectedVer   uint64
	rejectReason  string
	nextSeq       uint64
	outbox        []protocol.Event // unacked, oldest first
	stock         map[string]int
	stockSeq      uint64
	clockSyncedAt time.Time
}

func New(o Options) (*Agent, error) {
	loc, err := time.LoadLocation(o.TZ)
	if err != nil {
		return nil, err
	}
	if o.BatchSize == 0 {
		o.BatchSize = 50
	}
	return &Agent{o: o, loc: loc, nextSeq: 1, stock: map[string]int{}, clockSyncedAt: o.Clock.Now()}, nil
}

// Install applies the factory pricebook a technician loads on site.
func (a *Agent) Install(blob protocol.Blob, capacity int) error {
	pb, err := pricebook.Verify(blob, protocol.PricebookRef{}, a.o.PublicKey, a.o.ID, 0)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pb = pb
	for sku := range pb.Base {
		a.stock[sku] = capacity
	}
	return nil
}

func (a *Agent) now() time.Time { return a.o.Clock.Now().In(a.loc) }

type Quote struct {
	SKU     string
	Price   int64
	Promo   string
	InStock bool
}

func (a *Agent) Quotes() []Quote {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pb == nil {
		return nil
	}
	now := a.now()
	out := make([]Quote, 0, len(a.pb.Base))
	for sku := range a.pb.Base {
		p, promo, _, _ := pricebook.Lookup(a.pb, sku, now, a.clockSyncedAt)
		out = append(out, Quote{SKU: sku, Price: p, Promo: promo, InStock: a.stock[sku] > 0})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SKU < out[j].SKU })
	return out
}

// Sell prices a vend from the local pricebook and appends it to the outbox.
// It never touches the network.
func (a *Agent) Sell(sku string) (protocol.Event, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pb == nil {
		return protocol.Event{}, ErrNoPricebook
	}
	if a.stock[sku] <= 0 {
		return protocol.Event{}, ErrOutOfStock
	}
	now := a.now()
	price, promo, _, ok := pricebook.Lookup(a.pb, sku, now, a.clockSyncedAt)
	if !ok {
		return protocol.Event{}, fmt.Errorf("unknown sku %s", sku)
	}
	ev := protocol.Event{
		MachineID: a.o.ID, Seq: a.nextSeq, TsDevice: now, Type: "sale", SKU: sku,
		PriceMinor: price, Currency: protocol.Currency, PromoID: promo, PricebookVer: a.pb.Ver,
	}
	// Production: this append + seq bump is one fsynced SQLite transaction,
	// committed before the VMC is told to vend.
	a.nextSeq++
	a.outbox = append(a.outbox, ev)
	a.stock[sku]--
	a.stockSeq = ev.Seq
	return ev, nil
}

func (a *Agent) Restock(capacity int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for sku := range a.stock {
		a.stock[sku] = capacity
	}
}

type Status struct {
	PricebookVer uint64
	Backlog      int
	LastSeq      uint64
	ActivePromos []string
}

func (a *Agent) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := Status{Backlog: len(a.outbox), LastSeq: a.nextSeq - 1, ActivePromos: []string{}}
	if a.pb != nil {
		st.PricebookVer = a.pb.Ver
		st.ActivePromos = pricebook.Active(a.pb, a.now(), a.clockSyncedAt)
	}
	return st
}

// SyncOnce runs one connected session: clock sync, converge on desired
// pricebook, drain the outbox, heartbeat. Each step tolerates failure; the
// next session simply retries.
func (a *Agent) SyncOnce(ctx context.Context) error {
	a.mu.Lock()
	a.clockSyncedAt = a.o.Clock.Now() // stands in for NTP on connect
	a.mu.Unlock()

	errPB := a.syncPricebook(ctx)
	errEv := a.uploadEvents(ctx)
	errHB := a.heartbeat(ctx)
	return errors.Join(errPB, errEv, errHB)
}

func (a *Agent) syncPricebook(ctx context.Context) error {
	var sh protocol.Shadow
	if err := a.call(ctx, "GET", "/shadow", nil, &sh); err != nil {
		return err
	}
	want := sh.Desired.Pricebook
	a.mu.Lock()
	cur, rejected := a.pb.Ver, a.rejectedVer
	a.mu.Unlock()
	// A machine back after 3 days jumps straight to the latest version.
	if want.Ver <= cur || want.Ver == rejected {
		return nil
	}
	var blob protocol.Blob
	if err := a.call(ctx, "GET", fmt.Sprintf("/pricebooks/%d", want.Ver), nil, &blob); err != nil {
		return err
	}
	pb, err := pricebook.Verify(blob, want, a.o.PublicKey, a.o.ID, cur)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		// Keep selling on last-known-good; report the refusal.
		a.rejectedVer, a.rejectReason = want.Ver, err.Error()
		return nil
	}
	a.pb, a.rejectedVer, a.rejectReason = pb, 0, ""
	return nil
}

func (a *Agent) uploadEvents(ctx context.Context) error {
	for i := 0; i < 10; i++ {
		a.mu.Lock()
		n := min(len(a.outbox), a.o.BatchSize)
		batch := append([]protocol.Event(nil), a.outbox[:n]...)
		a.mu.Unlock()
		if n == 0 {
			return nil
		}
		var ack protocol.Ack
		if err := a.call(ctx, "POST", "/events", protocol.EventBatch{SentAt: a.now(), Events: batch}, &ack); err != nil {
			return err // lost request or lost ack: the same events go again next time
		}
		a.mu.Lock()
		k := 0
		for k < len(a.outbox) && a.outbox[k].Seq <= ack.UptoSeq {
			k++
		}
		a.outbox = a.outbox[k:]
		a.mu.Unlock()
		if ack.UptoSeq < batch[n-1].Seq {
			return nil
		}
	}
	return nil
}

func (a *Agent) heartbeat(ctx context.Context) error {
	a.mu.Lock()
	now := a.now()
	price := pricebook.SourceTimeline
	if _, _, src, ok := pricebook.Lookup(a.pb, "A1", now, a.clockSyncedAt); ok {
		price = src
	}
	stock := make(map[string]int, len(a.stock))
	for k, v := range a.stock {
		stock[k] = v
	}
	hb := protocol.Heartbeat{
		TZ: a.o.TZ, Site: a.o.Site, LocalTime: now,
		Pricebook:   protocol.PricebookRef{Ver: a.pb.Ver},
		RejectedVer: a.rejectedVer, RejectReason: a.rejectReason,
		ActivePromos: pricebook.Active(a.pb, now, a.clockSyncedAt), PriceSource: price,
		Stock: stock, StockSeq: a.stockSeq, Backlog: len(a.outbox), LastSeq: a.nextSeq - 1,
		ClockSyncedAt: a.clockSyncedAt,
	}
	a.mu.Unlock()
	return a.call(ctx, "POST", "/heartbeat", hb, nil)
}

func (a *Agent) call(ctx context.Context, method, path string, body, out any) error {
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.o.BaseURL+"/v1/machines/"+a.o.ID+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
