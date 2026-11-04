// Package sim runs a fleet of machine agents against the cloud over real
// HTTP, on an accelerated clock, with machines that go offline for minutes
// to days and a network that loses requests and responses.
//
// It also holds the ground truth (every vend that really happened) and
// checks the cloud's counts against it: the exactly-once check.
package sim

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/ahnafalfariza/snackbox/internal/agent"
	"github.com/ahnafalfariza/snackbox/internal/clock"
	"github.com/ahnafalfariza/snackbox/internal/protocol"
)

type Options struct {
	CloudURL     string
	Machines     int
	Clock        *clock.Sim
	Seed         int64
	Tick         time.Duration // real time between sale/network steps
	SyncEvery    time.Duration // real time between connected sessions (mean)
	ReportEvery  time.Duration
	DropRequest  float64 // request lost before reaching the cloud
	DropResponse float64 // cloud processed it, response lost (causes retries)
	OfflinePerHr float64 // chance per sim hour an online machine drops off
	SalesPerHr   float64 // per machine, at base price
	PromoUplift  float64
	Capacity     int
	ForceDemo    bool // pin a few machines to long outages for the demo story
}

func DefaultOptions() Options {
	return Options{
		Machines: 30, Seed: 1, Tick: 250 * time.Millisecond, SyncEvery: 3 * time.Second,
		ReportEvery: 2 * time.Second, DropRequest: 0.05, DropResponse: 0.08,
		OfflinePerHr: 0.045, SalesPerHr: 4, PromoUplift: 2, Capacity: 8, ForceDemo: true,
	}
}

var sites = []struct{ city, tz string }{
	{"Jakarta", "Asia/Jakarta"}, {"Bandung", "Asia/Jakarta"}, {"Surabaya", "Asia/Jakarta"},
	{"Denpasar", "Asia/Makassar"}, {"Makassar", "Asia/Makassar"}, {"Jayapura", "Asia/Jayapura"},
}
var places = []string{"Sudirman office", "Campus hall", "Gambir station", "Mall lobby", "Hospital", "Airport gate"}

type Fleet struct {
	o        Options
	machines []*machine
	ctl      *http.Client

	mu     sync.Mutex
	paused bool
}

type machine struct {
	id, tz, site string
	agent        *agent.Agent

	mu           sync.Mutex
	rng          *rand.Rand
	online       bool
	offlineUntil time.Time
	forced       bool // held online by the drain test, or offline by a click
	lastStep     time.Time
	restockAt    time.Time
	truthCount   int
	prefixRev    []int64 // prefixRev[seq] = revenue of seqs 1..seq (ground truth)
	reqDropped   int
	respDropped  int
}

func New(o Options) *Fleet {
	f := &Fleet{o: o, ctl: &http.Client{Timeout: 5 * time.Second}}
	seeds := rand.New(rand.NewSource(o.Seed)) // independent stream per machine
	for i := 0; i < o.Machines; i++ {
		s := sites[i%len(sites)]
		f.machines = append(f.machines, &machine{
			id:     fmt.Sprintf("SB-ID-%04d", i+1),
			tz:     s.tz,
			site:   s.city + " · " + places[(i/len(sites))%len(places)],
			rng:    rand.New(rand.NewSource(seeds.Int63())),
			online: true, prefixRev: []int64{0},
		})
	}
	return f
}

// offlineFor draws an outage length: mostly minutes, sometimes hours,
// occasionally the 1-3 days the brief calls out.
func offlineFor(r *rand.Rand) time.Duration {
	x := r.Float64()
	switch {
	case x < 0.65:
		return time.Duration(5+r.Intn(55)) * time.Minute
	case x < 0.93:
		return time.Duration(60+r.Intn(420)) * time.Minute
	default:
		return time.Duration(24*60+r.Intn(48*60)) * time.Minute
	}
}

func (f *Fleet) Run(ctx context.Context) error {
	pub, err := f.waitPubKey(ctx)
	if err != nil {
		return err
	}
	now := f.o.Clock.Now()
	for i, m := range f.machines {
		if err := f.ctlJSON(ctx, "POST", "/v1/machines", map[string]string{"ID": m.id, "TZ": m.tz, "Site": m.site}, nil); err != nil {
			return err
		}
		var factory protocol.Blob
		if err := f.ctlJSON(ctx, "GET", "/v1/machines/"+m.id+"/pricebooks/1", nil, &factory); err != nil {
			return err
		}
		skew := time.Duration(m.rng.Intn(120)-60) * time.Second // RTC drift
		a, err := agent.New(agent.Options{
			ID: m.id, TZ: m.tz, Site: m.site, Clock: clock.Skewed{Base: f.o.Clock, Skew: skew},
			BaseURL: f.o.CloudURL, PublicKey: pub,
			HTTP: &http.Client{Timeout: 5 * time.Second, Transport: &flaky{m: m, f: f, base: http.DefaultTransport}},
		})
		if err != nil {
			return err
		}
		if err := a.Install(factory, f.o.Capacity); err != nil {
			return fmt.Errorf("%s install: %w", m.id, err)
		}
		m.agent = a
		m.lastStep = now
		// About 15% start offline, as in the brief.
		if m.rng.Float64() < 0.15 {
			m.online, m.offlineUntil = false, now.Add(offlineFor(m.rng))
		}
		if f.o.ForceDemo {
			switch i {
			case 3: // misses the promo publish, back mid-window, jumps to latest
				m.online, m.offlineUntil = false, now.Add(100*time.Minute)
			case 8: // the 3-day case: sells for days from its factory pricebook
				m.online, m.offlineUntil = false, now.Add(50*time.Hour)
			}
		}
	}
	var wg sync.WaitGroup
	for _, m := range f.machines {
		wg.Add(2)
		go func(m *machine) { defer wg.Done(); f.stepLoop(ctx, m) }(m)
		go func(m *machine) { defer wg.Done(); f.syncLoop(ctx, m) }(m)
	}
	f.reportLoop(ctx)
	wg.Wait()
	return nil
}

func (f *Fleet) isPaused() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused
}

func (f *Fleet) stepLoop(ctx context.Context, m *machine) {
	t := time.NewTicker(f.o.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.step(m)
		}
	}
}

func (f *Fleet) step(m *machine) {
	now := f.o.Clock.Now()
	paused := f.isPaused()
	m.mu.Lock()
	defer m.mu.Unlock()
	hours := now.Sub(m.lastStep).Hours()
	m.lastStep = now
	if hours <= 0 {
		return
	}
	if !m.forced {
		if m.online && m.rng.Float64() < 1-math.Exp(-f.o.OfflinePerHr*hours) {
			m.online, m.offlineUntil = false, now.Add(offlineFor(m.rng))
		} else if !m.online && !now.Before(m.offlineUntil) {
			m.online = true
		}
	}
	if !m.restockAt.IsZero() && !now.Before(m.restockAt) {
		m.agent.Restock(f.o.Capacity) // the restock truck arrives
		m.restockAt = time.Time{}
	}
	if paused {
		return
	}
	quotes := m.agent.Quotes()
	perSKU := f.o.SalesPerHr / float64(len(quotes)) * hours
	for _, q := range quotes {
		if !q.InStock {
			if m.restockAt.IsZero() {
				m.restockAt = now.Add(time.Duration(2+m.rng.Intn(5)) * time.Hour)
			}
			continue
		}
		p := perSKU
		if q.Promo != "" {
			p *= f.o.PromoUplift
		}
		if m.rng.Float64() >= p {
			continue
		}
		ev, err := m.agent.Sell(q.SKU) // works the same online or offline
		if err != nil {
			continue
		}
		m.truthCount++
		m.prefixRev = append(m.prefixRev, m.prefixRev[len(m.prefixRev)-1]+ev.PriceMinor)
	}
}

func (f *Fleet) syncLoop(ctx context.Context, m *machine) {
	m.mu.Lock()
	r := rand.New(rand.NewSource(m.rng.Int63()))
	m.mu.Unlock()
	for {
		wait := time.Duration(float64(f.o.SyncEvery) * (0.5 + r.Float64()))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if !m.isOnline() {
			continue
		}
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = m.agent.SyncOnce(c) // failures are normal; the next session retries
		cancel()
	}
}

func (m *machine) isOnline() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.online
}

var errNoCarrier = errors.New("modem: no carrier")
var errTimeout = errors.New("network: timeout")

// flaky models the cellular link: offline means nothing gets through; online
// still loses some requests, and some responses after the cloud acted on them.
type flaky struct {
	m    *machine
	f    *Fleet
	base http.RoundTripper
}

func (t *flaky) roll() float64 {
	t.m.mu.Lock()
	defer t.m.mu.Unlock()
	return t.m.rng.Float64()
}

func (t *flaky) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.m.isOnline() {
		return nil, errNoCarrier
	}
	if t.roll() < t.f.o.DropRequest {
		t.m.mu.Lock()
		t.m.reqDropped++
		t.m.mu.Unlock()
		return nil, errTimeout
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if t.roll() < t.f.o.DropResponse {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		t.m.mu.Lock()
		t.m.respDropped++
		t.m.mu.Unlock()
		return nil, errTimeout
	}
	return resp, nil
}

// ---- reporting + exactly-once check ----

func (f *Fleet) reportLoop(ctx context.Context) {
	t := time.NewTicker(f.o.ReportEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := f.reportOnce(ctx); err != nil && ctx.Err() == nil {
				log.Printf("sim report: %v", err)
			}
		}
	}
}

// Check compares cloud ingest state with ground truth. The cloud summary is
// read first and the truth second: truth only grows, so a correct cloud can
// never be ahead of it.
func (f *Fleet) Check(ctx context.Context) (protocol.SimReport, error) {
	var sum map[string]protocol.MachineSales
	if err := f.ctlJSON(ctx, "GET", "/api/sales-summary", nil, &sum); err != nil {
		return protocol.SimReport{}, err
	}
	now := f.o.Clock.Now()
	rep := protocol.SimReport{SimTime: now, Speed: f.o.Clock.Speed, SalesPaused: f.isPaused(), CheckOK: true, Violations: []string{}}
	for _, m := range f.machines {
		st := m.agent.Status()
		m.mu.Lock()
		cs := sum[m.id]
		sm := protocol.SimMachine{
			ID: m.id, Online: m.online, Forced: m.forced, TruthCount: m.truthCount,
			TruthRevenue: m.prefixRev[len(m.prefixRev)-1], Backlog: st.Backlog,
			ReqDropped: m.reqDropped, RespDropped: m.respDropped, CheckOK: true,
		}
		if !m.online && !m.forced {
			sm.OfflineUntil = m.offlineUntil.Format(time.RFC3339)
		}
		switch {
		case cs.Count > m.truthCount:
			sm.CheckOK, sm.CheckMsg = false, fmt.Sprintf("cloud has %d sales, machine made %d", cs.Count, m.truthCount)
		case cs.Pending == 0 && cs.Revenue != m.prefixRev[cs.Watermark]:
			sm.CheckOK, sm.CheckMsg = false, fmt.Sprintf("revenue %d != truth %d for seq 1..%d", cs.Revenue, m.prefixRev[cs.Watermark], cs.Watermark)
		}
		m.mu.Unlock()
		if !sm.CheckOK {
			rep.CheckOK = false
			rep.Violations = append(rep.Violations, m.id+": "+sm.CheckMsg)
		}
		rep.TruthCount += sm.TruthCount
		rep.TruthRevenue += sm.TruthRevenue
		rep.CloudCount += cs.Count
		rep.CloudRevenue += cs.Revenue
		rep.ReqDropped += sm.ReqDropped
		rep.RespDropped += sm.RespDropped
		rep.Machines = append(rep.Machines, sm)
	}
	rep.InFlight = rep.TruthCount - rep.CloudCount
	sort.Slice(rep.Machines, func(i, j int) bool { return rep.Machines[i].ID < rep.Machines[j].ID })
	return rep, nil
}

func (f *Fleet) reportOnce(ctx context.Context) error {
	rep, err := f.Check(ctx)
	if err != nil {
		return err
	}
	var cmds []protocol.SimCommand
	if err := f.ctlJSON(ctx, "POST", "/internal/sim-report", rep, &cmds); err != nil {
		return err
	}
	for _, c := range cmds {
		f.Apply(c)
	}
	return nil
}

// Apply handles dashboard controls.
func (f *Fleet) Apply(c protocol.SimCommand) {
	switch c.Action {
	case "toggle":
		for _, m := range f.machines {
			if m.id == c.Machine {
				m.mu.Lock()
				if m.online {
					m.online, m.forced = false, true
				} else {
					m.online, m.forced, m.offlineUntil = true, false, time.Time{}
				}
				m.mu.Unlock()
			}
		}
	case "drain": // stop selling, bring every machine online, let backlogs empty
		f.mu.Lock()
		f.paused = true
		f.mu.Unlock()
		for _, m := range f.machines {
			m.mu.Lock()
			m.online, m.forced, m.offlineUntil = true, true, time.Time{}
			m.mu.Unlock()
		}
	case "resume":
		f.mu.Lock()
		f.paused = false
		f.mu.Unlock()
		for _, m := range f.machines {
			m.mu.Lock()
			m.forced = false
			m.mu.Unlock()
		}
	}
}

func (f *Fleet) waitPubKey(ctx context.Context) (ed25519.PublicKey, error) {
	for {
		var r struct{ Ed25519 string }
		err := f.ctlJSON(ctx, "GET", "/v1/pubkey", nil, &r)
		if err == nil {
			// Production: the key is baked into the agent image at the factory.
			return base64.StdEncoding.DecodeString(r.Ed25519)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// ctlJSON is the simulator's own, reliable control channel to the cloud
// (provisioning, reports). Machine traffic goes through the flaky link.
func (f *Fleet) ctlJSON(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.o.CloudURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.ctl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %s %s", method, path, resp.Status, b)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
