package sim

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ahnafalfariza/snackbox/internal/clock"
	"github.com/ahnafalfariza/snackbox/internal/cloud"
	"github.com/ahnafalfariza/snackbox/internal/protocol"
)

// TestFleetExactlyOnce runs 20 machines against a real HTTP cloud with a
// hostile network (10% requests lost, 20% responses lost after the cloud
// acted) and frequent outages, then drains and checks the cloud counted
// every sale exactly once and every machine converged on the promo.
func TestFleetExactlyOnce(t *testing.T) {
	jkt, _ := time.LoadLocation("Asia/Jakarta")
	clk := clock.NewSim(time.Date(2026, 11, 6, 14, 30, 0, 0, jkt), 600)
	st, err := cloud.NewStore(clk, true)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(cloud.NewServer(st).Handler())
	defer ts.Close()

	o := DefaultOptions()
	o.CloudURL, o.Clock, o.Machines = ts.URL, clk, 20
	o.Tick, o.SyncEvery, o.ReportEvery = 20*time.Millisecond, 100*time.Millisecond, time.Hour
	o.DropRequest, o.DropResponse, o.OfflinePerHr, o.SalesPerHr = 0.10, 0.20, 0.5, 30
	f := New(o)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.Run(ctx)

	time.Sleep(4 * time.Second) // ~40 sim minutes of selling through outages
	f.Apply(protocol.SimCommand{Action: "drain"})

	var rep protocol.SimReport
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		rep, err = f.Check(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !rep.CheckOK {
			t.Fatalf("exactly-once violated: %v", rep.Violations)
		}
		if rep.InFlight == 0 && rep.TruthCount > 0 && converged(st) {
			break
		}
	}
	if rep.InFlight != 0 || rep.CloudRevenue != rep.TruthRevenue {
		t.Fatalf("after drain: truth %d (Rp %d), cloud %d (Rp %d)", rep.TruthCount, rep.TruthRevenue, rep.CloudCount, rep.CloudRevenue)
	}
	state := st.State()
	if state.Duplicates == 0 || rep.RespDropped == 0 {
		t.Fatalf("test did not exercise retries: dups=%d respDropped=%d", state.Duplicates, rep.RespDropped)
	}
	for _, m := range state.Machines {
		if m.ReportedVer != state.LatestVer {
			t.Errorf("%s holds v%d, want v%d", m.ID, m.ReportedVer, state.LatestVer)
		}
	}
	t.Logf("%d sales, Rp %d, %d duplicate uploads dropped, %d responses lost", rep.TruthCount, rep.TruthRevenue, state.Duplicates, rep.RespDropped)
}

func converged(st *cloud.Store) bool {
	s := st.State()
	for _, m := range s.Machines {
		if m.ReportedVer != s.LatestVer {
			return false
		}
	}
	return true
}
