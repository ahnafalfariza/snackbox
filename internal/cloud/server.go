package cloud

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/ahnafalfariza/snackbox/internal/protocol"
)

//go:embed dashboard.html
var dashboardHTML []byte

type Server struct {
	st  *Store
	mux *http.ServeMux
}

func NewServer(st *Store) *Server {
	s := &Server{st: st, mux: http.NewServeMux()}
	m := s.mux

	// Device API. Production: MQTT topics + device shadow + CloudFront.
	m.HandleFunc("GET /v1/pubkey", s.pubkey)
	m.HandleFunc("POST /v1/machines", s.register)
	m.HandleFunc("GET /v1/machines/{id}/shadow", s.shadow)
	m.HandleFunc("GET /v1/machines/{id}/pricebooks/{ver}", s.blob)
	m.HandleFunc("POST /v1/machines/{id}/events", s.events)       // sb/{id}/events -> sb/{id}/ack
	m.HandleFunc("POST /v1/machines/{id}/heartbeat", s.heartbeat) // sb/{id}/heartbeat

	// Operator API (no auth in the demo; production uses OIDC + roles + audit log).
	m.HandleFunc("POST /v1/promotions", s.addPromo)
	m.HandleFunc("DELETE /v1/promotions/{id}", s.delPromo)
	m.HandleFunc("POST /v1/pricebooks:publish-bad", func(w http.ResponseWriter, r *http.Request) { st.PublishBad(); ok(w) })
	m.HandleFunc("POST /v1/pricebooks:roll-forward", func(w http.ResponseWriter, r *http.Request) { st.RollForward(); ok(w) })

	// Dashboard + simulator channel.
	m.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, st.State()) })
	m.HandleFunc("GET /api/sales-summary", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, st.SalesSummary()) })
	m.HandleFunc("POST /api/sim/command", s.simCommand)
	m.HandleFunc("POST /internal/sim-report", s.simReport)
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(dashboardHTML)
	})
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

// Run does background work: the daily horizon refresh.
func (s *Server) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.st.RefreshHorizon()
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func ok(w http.ResponseWriter) { writeJSON(w, 200, map[string]string{"status": "ok"}) }

func fail(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
}

func (s *Server) pubkey(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"ed25519": base64.StdEncoding.EncodeToString(s.st.PublicKey())})
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req struct{ ID, TZ, Site string }
	if err := decode(r, &req); err != nil {
		fail(w, 400, err)
		return
	}
	if err := s.st.Register(req.ID, req.TZ, req.Site); err != nil {
		fail(w, 400, err)
		return
	}
	ok(w)
}

func (s *Server) shadow(w http.ResponseWriter, r *http.Request) {
	sh, err := s.st.Shadow(r.PathValue("id"))
	if err != nil {
		fail(w, 404, err)
		return
	}
	writeJSON(w, 200, sh)
}

func (s *Server) blob(w http.ResponseWriter, r *http.Request) {
	ver, _ := strconv.ParseUint(r.PathValue("ver"), 10, 64)
	b, err := s.st.PricebookBlob(r.PathValue("id"), ver)
	if err != nil {
		fail(w, 404, err)
		return
	}
	writeJSON(w, 200, b)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	var b protocol.EventBatch
	if err := decode(r, &b); err != nil {
		fail(w, 400, err)
		return
	}
	ack, err := s.st.IngestEvents(r.PathValue("id"), b)
	if err != nil {
		fail(w, 404, err)
		return
	}
	writeJSON(w, 200, ack)
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	var hb protocol.Heartbeat
	if err := decode(r, &hb); err != nil {
		fail(w, 400, err)
		return
	}
	if err := s.st.Heartbeat(r.PathValue("id"), hb); err != nil {
		fail(w, 404, err)
		return
	}
	ok(w)
}

func (s *Server) addPromo(w http.ResponseWriter, r *http.Request) {
	var p protocol.Promo
	if err := decode(r, &p); err != nil {
		fail(w, 400, err)
		return
	}
	if len(p.Name) > 80 {
		p.Name = p.Name[:80]
	}
	if err := s.st.AddPromo(p); err != nil {
		fail(w, 400, err)
		return
	}
	ok(w)
}

func (s *Server) delPromo(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeletePromo(r.PathValue("id")); err != nil {
		fail(w, 404, err)
		return
	}
	ok(w)
}

func (s *Server) simCommand(w http.ResponseWriter, r *http.Request) {
	var c protocol.SimCommand
	if err := decode(r, &c); err != nil {
		fail(w, 400, err)
		return
	}
	s.st.EnqueueSimCommand(c)
	ok(w)
}

func (s *Server) simReport(w http.ResponseWriter, r *http.Request) {
	var rep protocol.SimReport
	if err := decode(r, &rep); err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, 200, s.st.SetSimReport(rep))
}
