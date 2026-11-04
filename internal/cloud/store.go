// Package cloud is the SnackBox cloud side for this slice: promo rules,
// per-machine signed pricebooks behind a desired-state shadow, and an
// idempotent event ingest. State lives in memory behind one mutex; the README
// maps each piece to the production design (Postgres, IoT Core, KMS).
package cloud

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/ahnafalfariza/snackbox/internal/clock"
	"github.com/ahnafalfariza/snackbox/internal/pricebook"
	"github.com/ahnafalfariza/snackbox/internal/pricing"
	"github.com/ahnafalfariza/snackbox/internal/protocol"
)

func Catalog() []protocol.Product {
	return []protocol.Product{
		{SKU: "A1", Name: "Iced tea", Category: "drinks", Price: 8000},
		{SKU: "A2", Name: "Cola", Category: "drinks", Price: 9000},
		{SKU: "A3", Name: "Mineral water", Category: "drinks", Price: 5000},
		{SKU: "A4", Name: "Canned coffee", Category: "drinks", Price: 12000},
		{SKU: "B1", Name: "Potato chips", Category: "snacks", Price: 10000},
		{SKU: "B2", Name: "Chocolate bar", Category: "snacks", Price: 8500},
		{SKU: "B3", Name: "Peanuts", Category: "snacks", Price: 6000},
		{SKU: "B4", Name: "Seaweed snack", Category: "snacks", Price: 12000},
	}
}

// DemoPromo is the scenario's promo, published at boot.
func DemoPromo() protocol.Promo {
	return protocol.Promo{
		ID: "promo-881", Name: "20% off drinks, Fri 15:00-17:00",
		Match:    protocol.Match{Category: []string{"drinks"}},
		Discount: protocol.Discount{Type: "percent", Value: 20},
		Schedule: protocol.Schedule{Days: []string{"FRI"}, Start: "15:00", End: "17:00"},
		Priority: 10, Stacking: "exclusive",
	}
}

// Version is one published pricebook version. Each machine gets its own
// compiled, signed pricebook for it (machines sit in different timezones).
type Version struct {
	Ver        uint64
	Note       string
	Promos     []protocol.Promo
	CompiledAt time.Time
	Bad        bool // demo: a compiler bug that bypassed the compile-time checks
	byMachine  map[string]compiled
	refusedBy  map[string]string // machine -> reason, kept after it moves on
}

type compiled struct {
	blob protocol.Blob
	sha  string
}

type Machine struct {
	ID, TZ, Site  string
	FirstSeen     time.Time
	LastSeen      time.Time
	ReportedVer   uint64
	RejectedVer   uint64
	RejectReason  string
	ActivePromos  []string
	PriceSource   string
	LocalTime     time.Time
	Stock         map[string]int
	StockSeq      uint64
	Backlog       int
	ClockSyncedAt time.Time

	// ingest: every seq <= Watermark is stored; pending = stored seqs above a gap
	Watermark  uint64
	pending    map[uint64]struct{}
	Count      int
	Revenue    int64
	Received   int
	Duplicates int
}

type Store struct {
	mu       sync.Mutex
	clk      clock.Clock
	priv     ed25519.PrivateKey
	pub      ed25519.PublicKey
	products []protocol.Product
	versions []*Version
	machines map[string]*Machine

	count, received, dups int
	revenue               int64
	byPromo               map[string]*Agg
	recent                []Recent
	maxLag                time.Duration
	maxLagMachine         string

	sim     *protocol.SimReport
	simCmds []protocol.SimCommand
}

type Agg struct {
	Count   int   `json:"count"`
	Revenue int64 `json:"revenue"`
}

type Recent struct {
	protocol.Event
	TsReceived time.Time `json:"ts_received"`
	LagMin     float64   `json:"lag_min"`
}

func NewStore(clk clock.Clock, seedDemo bool) (*Store, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	s := &Store{
		clk: clk, priv: priv, pub: priv.Public().(ed25519.PublicKey),
		products: Catalog(), machines: map[string]*Machine{}, byPromo: map[string]*Agg{},
	}
	s.publishLocked(nil, "factory pricebook (installed by technician)", false)
	if seedDemo {
		s.publishLocked([]protocol.Promo{DemoPromo()}, "publish promo-881", false)
	}
	return s, nil
}

func (s *Store) PublicKey() ed25519.PublicKey { return s.pub }

func (s *Store) latest() *Version { return s.versions[len(s.versions)-1] }

// lastGood is the newest version that is not the injected bad one.
func (s *Store) lastGood() *Version {
	for i := len(s.versions) - 1; i >= 0; i-- {
		if !s.versions[i].Bad {
			return s.versions[i]
		}
	}
	return s.versions[0]
}

func (s *Store) publishLocked(promos []protocol.Promo, note string, bad bool) *Version {
	v := &Version{
		Ver: uint64(len(s.versions) + 1), Note: note,
		Promos: append([]protocol.Promo{}, promos...), CompiledAt: s.clk.Now(),
		Bad: bad, byMachine: map[string]compiled{}, refusedBy: map[string]string{},
	}
	s.versions = append(s.versions, v)
	return v
}

// compileFor builds (and caches) one machine's signed pricebook for v.
func (s *Store) compileFor(v *Version, m *Machine) (compiled, error) {
	if c, ok := v.byMachine[m.ID]; ok {
		return c, nil
	}
	pb, err := pricing.Compile(v.Ver, m.ID, m.TZ, s.products, v.Promos, v.CompiledAt)
	if err != nil {
		return compiled{}, err
	}
	if v.Bad {
		// Simulates a compiler bug: a signed pricebook selling A1 for 0 IDR.
		pb.Base["A1"] = 0
	}
	payload, err := json.Marshal(pb)
	if err != nil {
		return compiled{}, err
	}
	c := compiled{blob: protocol.Blob{Payload: payload, Sig: ed25519.Sign(s.priv, payload)}, sha: pricebook.SHA256(payload)}
	v.byMachine[m.ID] = c
	return c, nil
}

func (s *Store) AddPromo(p protocol.Promo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Stacking == "" {
		p.Stacking = "exclusive"
	}
	if err := pricing.Validate(p, s.products); err != nil {
		return err
	}
	cur := s.lastGood().Promos
	if len(cur) >= 10 {
		return fmt.Errorf("demo limit: 10 promos")
	}
	for _, q := range cur {
		if q.ID == p.ID {
			return fmt.Errorf("promo %q already exists", p.ID)
		}
	}
	s.publishLocked(append(append([]protocol.Promo{}, cur...), p), "publish "+p.ID, false)
	return nil
}

func (s *Store) DeletePromo(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.lastGood().Promos
	var out []protocol.Promo
	for _, q := range cur {
		if q.ID != id {
			out = append(out, q)
		}
	}
	if len(out) == len(cur) {
		return fmt.Errorf("promo %q not found", id)
	}
	s.publishLocked(out, "end "+id, false)
	return nil
}

// PublishBad publishes a validly signed pricebook with a 0 IDR price, as if
// the compiler had a bug. Machines must refuse it and keep selling on their
// last-known-good pricebook.
func (s *Store) PublishBad() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishLocked(s.lastGood().Promos, "BAD: compiler bug, A1 = 0 IDR", true)
}

// RollForward republishes the last good rules as a new, higher version.
// Machines never accept a lower version, so recovery is always forward.
func (s *Store) RollForward() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishLocked(s.lastGood().Promos, "roll forward to last good rules", false)
}

// RefreshHorizon recompiles once a (sim) day so every machine always holds
// close to 14 days of timeline.
func (s *Store) RefreshHorizon() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clk.Now().Sub(s.latest().CompiledAt) >= 24*time.Hour {
		s.publishLocked(s.lastGood().Promos, "daily horizon refresh", false)
	}
}

// Register is factory provisioning (faked: real machines get an X.509 cert).
func (s *Store) Register(id, tz, site string) error {
	if _, err := time.LoadLocation(tz); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.machines[id] == nil {
		now := s.clk.Now()
		s.machines[id] = &Machine{ID: id, TZ: tz, Site: site, pending: map[uint64]struct{}{}, FirstSeen: now, ReportedVer: 1} // factory pricebook
	}
	return nil
}

func (s *Store) Shadow(id string) (protocol.Shadow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sh protocol.Shadow
	m := s.machines[id]
	if m == nil {
		return sh, fmt.Errorf("unknown machine %s", id)
	}
	v := s.latest()
	c, err := s.compileFor(v, m)
	if err != nil {
		return sh, err
	}
	sh.Desired.Pricebook = protocol.PricebookRef{Ver: v.Ver, SHA256: c.sha}
	sh.Reported.Pricebook = protocol.PricebookRef{Ver: m.ReportedVer}
	return sh, nil
}

func (s *Store) PricebookBlob(id string, ver uint64) (protocol.Blob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.machines[id]
	if m == nil || ver == 0 || ver > uint64(len(s.versions)) {
		return protocol.Blob{}, fmt.Errorf("not found")
	}
	c, err := s.compileFor(s.versions[ver-1], m)
	return c.blob, err
}

func (s *Store) Heartbeat(id string, hb protocol.Heartbeat) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.machines[id]
	if m == nil {
		return fmt.Errorf("unknown machine %s", id)
	}
	m.LastSeen = s.clk.Now()
	if hb.Pricebook.Ver > m.ReportedVer { // a reordered heartbeat never moves it back
		m.ReportedVer = hb.Pricebook.Ver
	}
	m.RejectedVer, m.RejectReason = hb.RejectedVer, hb.RejectReason
	if v := hb.RejectedVer; v > 0 && v <= uint64(len(s.versions)) {
		s.versions[v-1].refusedBy[id] = hb.RejectReason
	}
	m.ActivePromos, m.PriceSource, m.LocalTime = hb.ActivePromos, hb.PriceSource, hb.LocalTime
	m.Backlog, m.ClockSyncedAt = hb.Backlog, hb.ClockSyncedAt
	if hb.StockSeq >= m.StockSeq {
		m.Stock, m.StockSeq = hb.Stock, hb.StockSeq
	}
	return nil
}

// IngestEvents is INSERT ... ON CONFLICT (machine_id, seq) DO NOTHING plus
// a contiguous watermark. Retrying a batch whose ack was lost stores
// nothing new and returns the same upto_seq.
func (s *Store) IngestEvents(id string, b protocol.EventBatch) (protocol.Ack, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.machines[id]
	if m == nil {
		return protocol.Ack{}, fmt.Errorf("unknown machine %s", id)
	}
	now := s.clk.Now()
	for _, ev := range b.Events {
		m.Received++
		s.received++
		if ev.Seq == 0 || ev.MachineID != id {
			continue
		}
		if _, dup := m.pending[ev.Seq]; dup || ev.Seq <= m.Watermark {
			m.Duplicates++
			s.dups++
			continue
		}
		m.pending[ev.Seq] = struct{}{}
		for {
			if _, ok := m.pending[m.Watermark+1]; !ok {
				break
			}
			delete(m.pending, m.Watermark+1)
			m.Watermark++
		}
		m.Count++
		m.Revenue += ev.PriceMinor
		s.count++
		s.revenue += ev.PriceMinor
		key := ev.PromoID
		if key == "" {
			key = "base price"
		}
		if s.byPromo[key] == nil {
			s.byPromo[key] = &Agg{}
		}
		s.byPromo[key].Count++
		s.byPromo[key].Revenue += ev.PriceMinor
		lag := now.Sub(ev.TsDevice)
		if lag > s.maxLag {
			s.maxLag, s.maxLagMachine = lag, id
		}
		s.recent = append(s.recent, Recent{Event: ev, TsReceived: now, LagMin: lag.Minutes()})
		if len(s.recent) > 12 {
			s.recent = s.recent[len(s.recent)-12:]
		}
	}
	return protocol.Ack{UptoSeq: m.Watermark}, nil
}

func (s *Store) SalesSummary() map[string]protocol.MachineSales {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]protocol.MachineSales, len(s.machines))
	for id, m := range s.machines {
		out[id] = protocol.MachineSales{Watermark: m.Watermark, Pending: len(m.pending), Count: m.Count, Revenue: m.Revenue}
	}
	return out
}

func (s *Store) SetSimReport(r protocol.SimReport) []protocol.SimCommand {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sim = &r
	cmds := s.simCmds
	s.simCmds = nil
	return cmds
}

func (s *Store) EnqueueSimCommand(c protocol.SimCommand) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.simCmds) < 100 {
		s.simCmds = append(s.simCmds, c)
	}
}

// ---------------- dashboard view ----------------

type VersionView struct {
	Ver        uint64    `json:"ver"`
	Note       string    `json:"note"`
	Bad        bool      `json:"bad"`
	CompiledAt time.Time `json:"compiled_at"`
	Holding    int       `json:"holding"`  // machines whose reported ver == this
	Rejected   int       `json:"rejected"` // machines that refused it
}

type PromoView struct {
	protocol.Promo
	Coverage  int `json:"coverage"` // machines holding a pricebook that contains it
	FiringNow int `json:"firing_now"`
}

type MachineView struct {
	ID           string    `json:"id"`
	TZ           string    `json:"tz"`
	Site         string    `json:"site"`
	LocalTime    time.Time `json:"local_time"`
	LastSeenMin  float64   `json:"last_seen_min"` // sim minutes, -1 = never
	ReportedVer  uint64    `json:"reported_ver"`
	RejectedVer  uint64    `json:"rejected_ver,omitempty"`
	RejectReason string    `json:"reject_reason,omitempty"`
	ActivePromos []string  `json:"active_promos"`
	PriceSource  string    `json:"price_source"`
	StockUnits   int       `json:"stock_units"`
	StockEst     bool      `json:"stock_est"` // estimated: machine silent > 20 sim min
	LowSlots     []string  `json:"low_slots"`
	Backlog      int       `json:"backlog"`
	Count        int       `json:"count"`
	Revenue      int64     `json:"revenue"`
	Duplicates   int       `json:"duplicates"`
}

type StateView struct {
	SimTime    time.Time           `json:"sim_time"`
	LatestVer  uint64              `json:"latest_ver"`
	Versions   []VersionView       `json:"versions"`
	Promos     []PromoView         `json:"promos"`
	Products   []protocol.Product  `json:"products"`
	Machines   []MachineView       `json:"machines"`
	Count      int                 `json:"count"`
	Revenue    int64               `json:"revenue"`
	Received   int                 `json:"received"`
	Duplicates int                 `json:"duplicates"`
	ByPromo    map[string]Agg      `json:"by_promo"`
	Recent     []Recent            `json:"recent"`
	MaxLagMin  float64             `json:"max_lag_min"`
	MaxLagFrom string              `json:"max_lag_from"`
	Sim        *protocol.SimReport `json:"sim"`
}

func (s *Store) State() StateView {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clk.Now()
	v := StateView{SimTime: now, LatestVer: s.latest().Ver, Products: s.products,
		Count: s.count, Revenue: s.revenue, Received: s.received, Duplicates: s.dups,
		ByPromo: map[string]Agg{}, Recent: append([]Recent(nil), s.recent...),
		MaxLagMin: s.maxLag.Minutes(), MaxLagFrom: s.maxLagMachine, Sim: s.sim}
	for k, a := range s.byPromo {
		v.ByPromo[k] = *a
	}
	ids := make([]string, 0, len(s.machines))
	for id := range s.machines {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for i := len(s.versions) - 1; i >= 0 && len(v.Versions) < 6; i-- {
		ver := s.versions[i]
		vv := VersionView{Ver: ver.Ver, Note: ver.Note, Bad: ver.Bad, CompiledAt: ver.CompiledAt, Rejected: len(ver.refusedBy)}
		for _, id := range ids {
			if s.machines[id].ReportedVer == ver.Ver {
				vv.Holding++
			}
		}
		v.Versions = append(v.Versions, vv)
	}
	for _, p := range s.lastGood().Promos {
		pv := PromoView{Promo: p}
		for _, id := range ids {
			m := s.machines[id]
			if m.ReportedVer > 0 && hasPromo(s.versions[m.ReportedVer-1].Promos, p.ID) {
				pv.Coverage++
			}
			for _, a := range m.ActivePromos {
				if a == p.ID {
					pv.FiringNow++
				}
			}
		}
		v.Promos = append(v.Promos, pv)
	}
	for _, id := range ids {
		m := s.machines[id]
		mv := MachineView{ID: id, TZ: m.TZ, Site: m.Site, LocalTime: m.LocalTime, LastSeenMin: -1,
			ReportedVer: m.ReportedVer, RejectedVer: m.RejectedVer, RejectReason: m.RejectReason,
			ActivePromos: m.ActivePromos, PriceSource: m.PriceSource, Backlog: m.Backlog,
			Count: m.Count, Revenue: m.Revenue, Duplicates: m.Duplicates, LowSlots: []string{}}
		if !m.LastSeen.IsZero() {
			mv.LastSeenMin = now.Sub(m.LastSeen).Minutes()
		}
		for sku, n := range m.Stock {
			mv.StockUnits += n
			if n <= 2 {
				mv.LowSlots = append(mv.LowSlots, sku)
			}
		}
		sort.Strings(mv.LowSlots)
		// Offline: estimated stock = last snapshot minus forecast sales at this
		// machine's observed velocity. Labelled as an estimate in the UI.
		if mv.LastSeenMin > 20 {
			hours := now.Sub(m.FirstSeen).Hours()
			if hours > 0 {
				est := mv.StockUnits - int(float64(m.Count)/hours*now.Sub(m.LastSeen).Hours())
				if est < 0 {
					est = 0
				}
				mv.StockUnits, mv.StockEst = est, true
			}
		}
		v.Machines = append(v.Machines, mv)
	}
	return v
}

func hasPromo(ps []protocol.Promo, id string) bool {
	for _, p := range ps {
		if p.ID == id {
			return true
		}
	}
	return false
}
