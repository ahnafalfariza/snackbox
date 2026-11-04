// Package protocol holds the wire types between the cloud and the machine
// agent. Shapes follow the design doc: a promo as authored, a per-machine
// compiled pricebook, a desired/reported shadow, and seq-numbered events.
//
// In production the device channel is MQTT over mTLS (sb/{id}/events,
// sb/{id}/heartbeat, sb/{id}/ack, device shadow). This slice carries the same
// messages over plain HTTP so it runs on any free host.
package protocol

import "time"

const Currency = "IDR"

// Product is one SKU in the catalog. Price is in minor units (IDR has none,
// so it is whole rupiah).
type Product struct {
	SKU      string `json:"sku"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Price    int64  `json:"price"`
}

// Promo is a rule as an operator authors it in the console.
type Promo struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Match    Match    `json:"match"`
	Discount Discount `json:"discount"`
	Schedule Schedule `json:"schedule"`
	Priority int      `json:"priority"`
	Stacking string   `json:"stacking"` // only "exclusive" in this slice
}

type Match struct {
	Category []string `json:"category,omitempty"`
	SKU      []string `json:"sku,omitempty"`
}

type Discount struct {
	Type  string `json:"type"` // "percent"
	Value int64  `json:"value"`
}

type Schedule struct {
	TZ        string   `json:"tz,omitempty"`   // empty = each machine's own timezone
	Days      []string `json:"days,omitempty"` // MON..SUN, empty = every day
	Start     string   `json:"start"`          // HH:MM
	End       string   `json:"end"`            // HH:MM, End <= Start wraps past midnight
	ValidFrom string   `json:"valid_from,omitempty"`
	ValidTo   string   `json:"valid_to,omitempty"`
}

// Pricebook is the promo compiled for one machine: base prices plus a
// timeline of non-overlapping price windows up to HorizonEnd. The machine
// runs no rule engine; it looks up "which window contains now".
type Pricebook struct {
	Ver        uint64              `json:"ver"`
	Machine    string              `json:"machine"`
	TZ         string              `json:"tz"`
	Currency   string              `json:"currency"`
	CompiledAt time.Time           `json:"compiled_at"`
	HorizonEnd time.Time           `json:"horizon_end"`
	Base       map[string]int64    `json:"base"`
	Bounds     map[string][2]int64 `json:"bounds"` // signed per-SKU [min, max]
	Windows    []Window            `json:"windows"`
}

type Window struct {
	From   time.Time        `json:"from"`
	To     time.Time        `json:"to"`
	Prices map[string]int64 `json:"prices"`
	Promo  string           `json:"promo"`
}

// Blob is a signed pricebook as fetched over HTTPS. Payload is the exact
// bytes that were signed; the agent verifies before it parses.
type Blob struct {
	Payload []byte `json:"payload"`
	Sig     []byte `json:"sig"` // ed25519 over Payload (KMS in production)
}

type PricebookRef struct {
	Ver    uint64 `json:"ver"`
	SHA256 string `json:"sha256,omitempty"`
}

// Shadow is desired vs reported state. The cloud never sends commands.
type Shadow struct {
	Desired struct {
		Pricebook PricebookRef `json:"pricebook"`
	} `json:"desired"`
	Reported struct {
		Pricebook PricebookRef `json:"pricebook"`
	} `json:"reported"`
}

// Event is one append-only log entry. (machine_id, seq) is the idempotency
// key; seq is allocated and fsynced before the vend is confirmed.
type Event struct {
	MachineID    string    `json:"machine_id"`
	Seq          uint64    `json:"seq"`
	TsDevice     time.Time `json:"ts_device"`
	Type         string    `json:"type"` // "sale"
	SKU          string    `json:"sku"`
	PriceMinor   int64     `json:"price_minor"`
	Currency     string    `json:"currency"`
	PromoID      string    `json:"promo_id,omitempty"`
	PricebookVer uint64    `json:"pricebook_ver"`
}

type EventBatch struct {
	SentAt time.Time `json:"sent_at"`
	Events []Event   `json:"events"`
}

// Ack lets the device delete every event with seq <= UptoSeq.
type Ack struct {
	UptoSeq uint64 `json:"upto_seq"`
}

type Heartbeat struct {
	TZ            string         `json:"tz"`
	Site          string         `json:"site"`
	LocalTime     time.Time      `json:"local_time"`
	Pricebook     PricebookRef   `json:"pricebook"` // reported.pricebook
	RejectedVer   uint64         `json:"rejected_ver,omitempty"`
	RejectReason  string         `json:"reject_reason,omitempty"`
	ActivePromos  []string       `json:"active_promos"`
	PriceSource   string         `json:"price_source"` // timeline | base:horizon | base:clock
	Stock         map[string]int `json:"stock"`
	StockSeq      uint64         `json:"stock_seq"` // last event seq this snapshot reflects
	Backlog       int            `json:"backlog"`
	LastSeq       uint64         `json:"last_seq"`
	ClockSyncedAt time.Time      `json:"clock_synced_at"`
}

// MachineSales is the cloud's ingest state per machine; the simulator uses
// it to check exactly-once against its ground truth.
type MachineSales struct {
	Watermark uint64 `json:"watermark"`
	Pending   int    `json:"pending"`
	Count     int    `json:"count"`
	Revenue   int64  `json:"revenue"`
}

// ---- Simulator <-> cloud debug channel (not part of the device protocol) ----

type SimMachine struct {
	ID           string `json:"id"`
	Online       bool   `json:"online"`
	OfflineUntil string `json:"offline_until,omitempty"`
	Forced       bool   `json:"forced,omitempty"`
	TruthCount   int    `json:"truth_count"`
	TruthRevenue int64  `json:"truth_revenue"`
	Backlog      int    `json:"backlog"`
	ReqDropped   int    `json:"req_dropped"`
	RespDropped  int    `json:"resp_dropped"`
	CheckOK      bool   `json:"check_ok"`
	CheckMsg     string `json:"check_msg,omitempty"`
}

type SimReport struct {
	SimTime      time.Time    `json:"sim_time"`
	Speed        float64      `json:"speed"`
	SalesPaused  bool         `json:"sales_paused"`
	Machines     []SimMachine `json:"machines"`
	TruthCount   int          `json:"truth_count"`
	TruthRevenue int64        `json:"truth_revenue"`
	CloudCount   int          `json:"cloud_count"`
	CloudRevenue int64        `json:"cloud_revenue"`
	InFlight     int          `json:"in_flight"`
	ReqDropped   int          `json:"req_dropped"`
	RespDropped  int          `json:"resp_dropped"`
	CheckOK      bool         `json:"check_ok"`
	Violations   []string     `json:"violations"`
}

type SimCommand struct {
	Action  string `json:"action"` // toggle | drain | resume
	Machine string `json:"machine,omitempty"`
}
