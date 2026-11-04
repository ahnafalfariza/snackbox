// Package pricing is the cloud-side compiler: promo rules in, a signed
// per-machine price timeline out. The console preview would call the same
// Compile, so what operators see is what machines charge.
package pricing

import (
	"fmt"
	"sort"
	"time"

	"github.com/ahnafalfariza/snackbox/internal/protocol"
)

const (
	Horizon        = 14 * 24 * time.Hour
	MaxDiscountPct = 50  // above this needs an override (not built here)
	RoundTo        = 100 // IDR rounding step
	MinPricePct    = 50  // signed bounds: [50%, 200%] of base
	MaxPricePct    = 200
)

var dayNames = map[string]time.Weekday{
	"SUN": time.Sunday, "MON": time.Monday, "TUE": time.Tuesday, "WED": time.Wednesday,
	"THU": time.Thursday, "FRI": time.Friday, "SAT": time.Saturday,
}

func parseHM(s string) (h, m int, err error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, 0, fmt.Errorf("bad time %q, want HH:MM", s)
	}
	return t.Hour(), t.Minute(), nil
}

// Validate is the compile-time gate an operator hits before publishing.
func Validate(p protocol.Promo, products []protocol.Product) error {
	if p.ID == "" {
		return fmt.Errorf("promo needs an id")
	}
	if p.Discount.Type != "percent" {
		return fmt.Errorf("%s: only percent discounts are supported", p.ID)
	}
	if p.Discount.Value < 1 || p.Discount.Value > MaxDiscountPct {
		return fmt.Errorf("%s: discount must be 1-%d%% (more needs an override)", p.ID, MaxDiscountPct)
	}
	if p.Stacking != "" && p.Stacking != "exclusive" {
		return fmt.Errorf("%s: only exclusive stacking is supported", p.ID)
	}
	sh, sm, err := parseHM(p.Schedule.Start)
	if err != nil {
		return fmt.Errorf("%s: %w", p.ID, err)
	}
	eh, em, err := parseHM(p.Schedule.End)
	if err != nil {
		return fmt.Errorf("%s: %w", p.ID, err)
	}
	if sh == eh && sm == em {
		return fmt.Errorf("%s: start equals end", p.ID)
	}
	for _, d := range p.Schedule.Days {
		if _, ok := dayNames[d]; !ok {
			return fmt.Errorf("%s: bad day %q", p.ID, d)
		}
	}
	if p.Schedule.TZ != "" {
		if _, err := time.LoadLocation(p.Schedule.TZ); err != nil {
			return fmt.Errorf("%s: bad tz %q", p.ID, p.Schedule.TZ)
		}
	}
	for _, d := range []string{p.Schedule.ValidFrom, p.Schedule.ValidTo} {
		if d != "" {
			if _, err := time.Parse("2006-01-02", d); err != nil {
				return fmt.Errorf("%s: bad date %q", p.ID, d)
			}
		}
	}
	hit := false
	for _, pr := range products {
		if matches(p.Match, pr) {
			hit = true
		}
	}
	if !hit {
		return fmt.Errorf("%s: matches no product", p.ID)
	}
	return nil
}

func matches(m protocol.Match, pr protocol.Product) bool {
	for _, c := range m.Category {
		if c == pr.Category {
			return true
		}
	}
	for _, s := range m.SKU {
		if s == pr.SKU {
			return true
		}
	}
	return false
}

func discounted(base, pct int64) int64 {
	p := base * (100 - pct) / 100
	return p / RoundTo * RoundTo
}

// Bounds are signed into every pricebook and enforced by the agent.
func Bounds(products []protocol.Product) map[string][2]int64 {
	b := make(map[string][2]int64, len(products))
	for _, p := range products {
		b[p.SKU] = [2]int64{p.Price * MinPricePct / 100, p.Price * MaxPricePct / 100}
	}
	return b
}

type occurrence struct {
	from, to time.Time
	promo    protocol.Promo
}

// occurrences expands a promo schedule into concrete [from, to) intervals
// inside [start, end), in the promo's timezone (or the machine's).
func occurrences(p protocol.Promo, machineLoc *time.Location, start, end time.Time) []occurrence {
	loc := machineLoc
	if p.Schedule.TZ != "" {
		if l, err := time.LoadLocation(p.Schedule.TZ); err == nil {
			loc = l
		}
	}
	sh, sm, _ := parseHM(p.Schedule.Start)
	eh, em, _ := parseHM(p.Schedule.End)
	var out []occurrence
	first := start.In(loc).AddDate(0, 0, -1)
	for d := 0; ; d++ {
		day := first.AddDate(0, 0, d)
		y, mo, dd := day.Date()
		from := time.Date(y, mo, dd, sh, sm, 0, 0, loc)
		if !from.Before(end) {
			break
		}
		to := time.Date(y, mo, dd, eh, em, 0, 0, loc)
		if !to.After(from) {
			to = to.AddDate(0, 0, 1)
		}
		if len(p.Schedule.Days) > 0 {
			ok := false
			for _, n := range p.Schedule.Days {
				if dayNames[n] == from.Weekday() {
					ok = true
				}
			}
			if !ok {
				continue
			}
		}
		date := from.Format("2006-01-02")
		if (p.Schedule.ValidFrom != "" && date < p.Schedule.ValidFrom) || (p.Schedule.ValidTo != "" && date > p.Schedule.ValidTo) {
			continue
		}
		if !to.After(start) {
			continue
		}
		if from.Before(start) {
			from = start
		}
		if to.After(end) {
			to = end
		}
		out = append(out, occurrence{from, to, p})
	}
	return out
}

// Compile resolves base price, schedule, priority and rounding into a
// timeline of windows that never overlap for any one SKU.
func Compile(ver uint64, machine, tz string, products []protocol.Product, promos []protocol.Promo, compiledAt time.Time) (protocol.Pricebook, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return protocol.Pricebook{}, err
	}
	start := compiledAt.In(loc)
	end := start.Add(Horizon)
	pb := protocol.Pricebook{
		Ver: ver, Machine: machine, TZ: tz, Currency: protocol.Currency,
		CompiledAt: start, HorizonEnd: end,
		Base: map[string]int64{}, Bounds: Bounds(products), Windows: []protocol.Window{},
	}
	for _, p := range products {
		pb.Base[p.SKU] = p.Price
	}

	var occ []occurrence
	cuts := map[int64]time.Time{}
	for _, p := range promos {
		for _, o := range occurrences(p, loc, start, end) {
			occ = append(occ, o)
			cuts[o.from.UnixNano()] = o.from
			cuts[o.to.UnixNano()] = o.to
		}
	}
	bounds := make([]time.Time, 0, len(cuts))
	for _, t := range cuts {
		bounds = append(bounds, t.In(loc))
	}
	sort.Slice(bounds, func(i, j int) bool { return bounds[i].Before(bounds[j]) })

	last := map[string]int{} // promo id -> index of its latest window, for merging
	for i := 0; i+1 < len(bounds); i++ {
		segFrom, segTo := bounds[i], bounds[i+1]
		byPromo := map[string]map[string]int64{}
		for _, pr := range products {
			var win *occurrence
			var winPrice int64
			for k := range occ {
				o := &occ[k]
				if o.from.After(segFrom) || o.to.Before(segTo) || !matches(o.promo.Match, pr) {
					continue
				}
				price := discounted(pr.Price, o.promo.Discount.Value)
				// exclusive stacking: highest priority wins, then the lower price
				if win == nil || o.promo.Priority > win.promo.Priority ||
					(o.promo.Priority == win.promo.Priority && price < winPrice) {
					win, winPrice = o, price
				}
			}
			if win != nil {
				if byPromo[win.promo.ID] == nil {
					byPromo[win.promo.ID] = map[string]int64{}
				}
				byPromo[win.promo.ID][pr.SKU] = winPrice
			}
		}
		ids := make([]string, 0, len(byPromo))
		for id := range byPromo {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if j, ok := last[id]; ok && pb.Windows[j].To.Equal(segFrom) && samePrices(pb.Windows[j].Prices, byPromo[id]) {
				pb.Windows[j].To = segTo
				continue
			}
			pb.Windows = append(pb.Windows, protocol.Window{From: segFrom, To: segTo, Prices: byPromo[id], Promo: id})
			last[id] = len(pb.Windows) - 1
		}
	}
	sort.SliceStable(pb.Windows, func(i, j int) bool { return pb.Windows[i].From.Before(pb.Windows[j].From) })
	return pb, nil
}

func samePrices(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
