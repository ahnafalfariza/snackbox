// Command snackbox runs the cloud service and the fleet simulator in one
// process. The simulator still talks to the cloud over real HTTP.
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
	_ "time/tzdata" // IANA zones inside a distroless image

	"github.com/ahnafalfariza/snackbox/internal/clock"
	"github.com/ahnafalfariza/snackbox/internal/cloud"
	"github.com/ahnafalfariza/snackbox/internal/sim"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return def
}

func main() {
	port := env("PORT", "8080")
	speed := envFloat("SPEED", 60)
	machines := int(envFloat("MACHINES", 30))

	// Sim starts Friday 6 Nov 2026, 13:40 in Jakarta: Jayapura (UTC+9) is
	// already inside the 15:00-17:00 promo window, Makassar (UTC+8) enters in
	// 20 sim minutes, Jakarta (UTC+7) in 80.
	jkt, _ := time.LoadLocation("Asia/Jakarta")
	start := time.Date(2026, 11, 6, 13, 40, 0, 0, jkt)
	clk := clock.NewSim(start, speed)

	st, err := cloud.NewStore(clk, true)
	if err != nil {
		log.Fatal(err)
	}
	srv := cloud.NewServer(st)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatal(err)
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	go srv.Run(ctx)
	log.Printf("snackbox: dashboard on http://localhost:%s  (%d machines, %gx time)", port, machines, speed)

	o := sim.DefaultOptions()
	o.CloudURL = "http://127.0.0.1:" + port
	o.Machines = machines
	o.Clock = clk
	go func() {
		if err := sim.New(o).Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("simulator stopped: %v", err)
		}
	}()

	<-ctx.Done()
	sc, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	hs.Shutdown(sc)
}
