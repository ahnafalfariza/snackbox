package pricing

import (
	"testing"
	"time"

	"github.com/ahnafalfariza/snackbox/internal/pricebook"
	"github.com/ahnafalfariza/snackbox/internal/protocol"
)

var products = []protocol.Product{
	{SKU: "A1", Category: "drinks", Price: 8000},
	{SKU: "B1", Category: "snacks", Price: 10000},
}

func friDrinks() protocol.Promo {
	return protocol.Promo{ID: "promo-881", Match: protocol.Match{Category: []string{"drinks"}},
		Discount: protocol.Discount{Type: "percent", Value: 20},
		Schedule: protocol.Schedule{Days: []string{"FRI"}, Start: "15:00", End: "17:00"}, Priority: 10}
}

func at(tz string, y int, mo time.Month, d, h, m int) time.Time {
	loc, _ := time.LoadLocation(tz)
	return time.Date(y, mo, d, h, m, 0, 0, loc)
}

func price(t *testing.T, pb *protocol.Pricebook, sku string, now time.Time) (int64, string) {
	t.Helper()
	p, promo, _, ok := pricebook.Lookup(pb, sku, now, now)
	if !ok {
		t.Fatalf("no price for %s", sku)
	}
	return p, promo
}

func TestFridayWindowInMachineLocalTime(t *testing.T) {
	compiled := at("Asia/Jakarta", 2026, 11, 4, 9, 0) // Wednesday
	for _, tz := range []string{"Asia/Jakarta", "Asia/Jayapura"} {
		pb, err := Compile(2, "M1", tz, products, []protocol.Promo{friDrinks()}, compiled)
		if err != nil {
			t.Fatal(err)
		}
		cases := []struct {
			when  time.Time
			want  int64
			promo string
		}{
			{at(tz, 2026, 11, 6, 14, 59), 8000, ""},
			{at(tz, 2026, 11, 6, 15, 0), 6400, "promo-881"},
			{at(tz, 2026, 11, 6, 16, 59), 6400, "promo-881"},
			{at(tz, 2026, 11, 6, 17, 0), 8000, ""},
			{at(tz, 2026, 11, 7, 15, 30), 8000, ""},           // Saturday
			{at(tz, 2026, 11, 13, 15, 30), 6400, "promo-881"}, // next Friday, still in horizon
		}
		for _, c := range cases {
			if p, promo := price(t, &pb, "A1", c.when); p != c.want || promo != c.promo {
				t.Errorf("%s %s: got %d %q, want %d %q", tz, c.when, p, promo, c.want, c.promo)
			}
		}
		if p, _ := price(t, &pb, "B1", at(tz, 2026, 11, 6, 15, 30)); p != 10000 {
			t.Errorf("snacks should not be discounted, got %d", p)
		}
	}
}

func TestOvernightAndPriority(t *testing.T) {
	late := protocol.Promo{ID: "late", Match: protocol.Match{Category: []string{"snacks"}},
		Discount: protocol.Discount{Type: "percent", Value: 10}, Schedule: protocol.Schedule{Start: "22:00", End: "02:00"}, Priority: 1}
	vip := protocol.Promo{ID: "vip", Match: protocol.Match{SKU: []string{"B1"}},
		Discount: protocol.Discount{Type: "percent", Value: 5}, Schedule: protocol.Schedule{Start: "23:00", End: "23:30"}, Priority: 9}
	pb, err := Compile(3, "M1", "Asia/Jakarta", products, []protocol.Promo{late, vip}, at("Asia/Jakarta", 2026, 11, 4, 12, 0))
	if err != nil {
		t.Fatal(err)
	}
	check := func(h, m int, want int64, promo string) {
		day := 4
		if h < 12 {
			day = 5
		}
		if p, pr := price(t, &pb, "B1", at("Asia/Jakarta", 2026, 11, day, h, m)); p != want || pr != promo {
			t.Errorf("%02d:%02d got %d %q want %d %q", h, m, p, pr, want, promo)
		}
	}
	check(21, 59, 10000, "")
	check(22, 0, 9000, "late")
	check(23, 10, 9500, "vip") // higher priority wins even though it is a smaller discount
	check(23, 30, 9000, "late")
	check(1, 59, 9000, "late") // after midnight
	check(2, 0, 10000, "")
	// Windows never overlap for one SKU.
	for i, a := range pb.Windows {
		for _, b := range pb.Windows[i+1:] {
			for sku := range a.Prices {
				if _, both := b.Prices[sku]; both && a.From.Before(b.To) && b.From.Before(a.To) {
					t.Fatalf("overlap on %s: %v and %v", sku, a, b)
				}
			}
		}
	}
}

func TestDeviceFallbacks(t *testing.T) {
	compiled := at("Asia/Jakarta", 2026, 11, 6, 9, 0)
	pb, _ := Compile(2, "M1", "Asia/Jakarta", products, []protocol.Promo{friDrinks()}, compiled)
	inWindow := at("Asia/Jakarta", 2026, 11, 6, 15, 30)
	if _, _, src, _ := pricebook.Lookup(&pb, "A1", inWindow, inWindow.Add(-8*24*time.Hour)); src != pricebook.SourceClock {
		t.Errorf("RTC unsynced > 7 days must fall back to base, got %s", src)
	}
	past := compiled.Add(Horizon + time.Hour)
	if p, _, src, _ := pricebook.Lookup(&pb, "A1", past, past); p != 8000 || src != pricebook.SourceHorizon {
		t.Errorf("past horizon must charge base, got %d %s", p, src)
	}
}

func TestValidateRejectsDeepDiscount(t *testing.T) {
	p := friDrinks()
	p.Discount.Value = 80
	if err := Validate(p, products); err == nil {
		t.Fatal("80% off should need an override")
	}
}
