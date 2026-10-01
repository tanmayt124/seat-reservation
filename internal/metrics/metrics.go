// Package metrics defines the Prometheus metrics. Counters are incremented
// by the HTTP layer as outcomes happen; seat gauges are read from the
// database at scrape time, so they can never drift from the real state.
package metrics

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Decline reasons as metric label values. The brief names seat-taken,
// per-user-limit and idempotent-replay; the rest cover every other way a
// reservation request can end without a new booking.
const (
	DeclineSeatTaken        = "seat_taken"
	DeclinePerUserLimit     = "per_user_limit"
	DeclineIdempotentReplay = "idempotent_replay"
	DeclineKeyReused        = "idempotency_key_reused"
	DeclineSeatContended    = "seat_contended"
	DeclineOverloaded       = "overloaded"
	DeclineUnknownSeats     = "unknown_seats"
	DeclineShowNotFound     = "show_not_found"
	DeclineValidation       = "validation"
	DeclineOther            = "other"
)

type Metrics struct {
	reg *prometheus.Registry

	ReservationsConfirmed prometheus.Counter
	SeatsConfirmed        prometheus.Counter
	ReservationsDeclined  *prometheus.CounterVec
	ReservationsCancelled prometheus.Counter
	SeatsReleased         prometheus.Counter
	HTTPRequests          *prometheus.CounterVec
	HTTPDuration          *prometheus.HistogramVec
}

func New(pool *pgxpool.Pool, log *slog.Logger) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,
		ReservationsConfirmed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "reservations_confirmed_total",
			Help: "Reservations confirmed (one per successful booking, replays excluded).",
		}),
		SeatsConfirmed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "seats_confirmed_total",
			Help: "Seats confirmed across all reservations.",
		}),
		ReservationsDeclined: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reservations_declined_total",
			Help: "Reservation requests that did not create a booking, by reason.",
		}, []string{"reason"}),
		ReservationsCancelled: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "reservations_cancelled_total",
			Help: "Reservations cancelled by their owner (repeat cancels excluded).",
		}),
		SeatsReleased: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "seats_released_total",
			Help: "Seats returned to available by cancellations.",
		}),
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "HTTP requests by route pattern, method and status code.",
		}, []string{"route", "method", "code"}),
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency by route pattern and method.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"route", "method"}),
	}

	// Pre-create every decline label so each series exists from the first
	// scrape (a counter that appears mid-burst breaks rate()).
	for _, r := range []string{DeclineSeatTaken, DeclinePerUserLimit, DeclineIdempotentReplay, DeclineKeyReused,
		DeclineSeatContended, DeclineOverloaded, DeclineUnknownSeats, DeclineShowNotFound, DeclineValidation, DeclineOther} {
		m.ReservationsDeclined.WithLabelValues(r)
	}

	reg.MustRegister(
		m.ReservationsConfirmed, m.SeatsConfirmed, m.ReservationsDeclined,
		m.ReservationsCancelled, m.SeatsReleased, m.HTTPRequests, m.HTTPDuration,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	if pool != nil {
		reg.MustRegister(newSeatCollector(pool, log), newPoolCollector(pool))
	}
	return m
}

// Handler serves /metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Registry is exposed for tests.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// seatCollector reads seat counts per show and status from the database on
// every scrape. One GROUP BY, bounded by a short timeout; on failure it
// reports scrape_error=1 and no seat series rather than stale numbers.
type seatCollector struct {
	pool      *pgxpool.Pool
	log       *slog.Logger
	seats     *prometheus.Desc
	total     *prometheus.Desc
	invariant *prometheus.Desc
	scrapeErr *prometheus.Desc
}

func newSeatCollector(pool *pgxpool.Pool, log *slog.Logger) *seatCollector {
	return &seatCollector{
		pool: pool,
		log:  log,
		seats: prometheus.NewDesc("seats",
			"Seats per show by status (available, held, confirmed), read from the database at scrape time.",
			[]string{"show_id", "status"}, nil),
		total: prometheus.NewDesc("seats_total",
			"Total seats per show.", []string{"show_id"}, nil),
		invariant: prometheus.NewDesc("seats_invariant_ok",
			"1 if available + held + confirmed == total_seats for the show, else 0.", []string{"show_id"}, nil),
		scrapeErr: prometheus.NewDesc("seats_scrape_error",
			"1 if the seat counts could not be read on this scrape.", nil, nil),
	}
}

func (c *seatCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.seats
	ch <- c.total
	ch <- c.invariant
	ch <- c.scrapeErr
}

func (c *seatCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	rows, err := c.pool.Query(ctx, `
		SELECT s.id::text, s.total_seats,
		       count(st.label) FILTER (WHERE st.status = 'available'),
		       count(st.label) FILTER (WHERE st.status = 'held'),
		       count(st.label) FILTER (WHERE st.status = 'confirmed')
		FROM shows s
		LEFT JOIN seats st ON st.show_id = s.id
		GROUP BY s.id, s.total_seats`)
	if err != nil {
		c.fail(ch, err)
		return
	}
	defer rows.Close()

	type row struct {
		id                         string
		total, avail, held, booked int
	}
	var out []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.total, &r.avail, &r.held, &r.booked); err != nil {
			c.fail(ch, err)
			return
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		c.fail(ch, err)
		return
	}

	for _, r := range out {
		ch <- prometheus.MustNewConstMetric(c.seats, prometheus.GaugeValue, float64(r.avail), r.id, "available")
		ch <- prometheus.MustNewConstMetric(c.seats, prometheus.GaugeValue, float64(r.held), r.id, "held")
		ch <- prometheus.MustNewConstMetric(c.seats, prometheus.GaugeValue, float64(r.booked), r.id, "confirmed")
		ch <- prometheus.MustNewConstMetric(c.total, prometheus.GaugeValue, float64(r.total), r.id)
		ok := 0.0
		if r.avail+r.held+r.booked == r.total {
			ok = 1
		}
		ch <- prometheus.MustNewConstMetric(c.invariant, prometheus.GaugeValue, ok, r.id)
	}
	ch <- prometheus.MustNewConstMetric(c.scrapeErr, prometheus.GaugeValue, 0)
}

func (c *seatCollector) fail(ch chan<- prometheus.Metric, err error) {
	c.log.Warn("metrics_seat_scrape_failed", "err", err)
	ch <- prometheus.MustNewConstMetric(c.scrapeErr, prometheus.GaugeValue, 1)
}

// poolCollector exposes pgxpool statistics, the first place to look when
// latency climbs under a burst.
type poolCollector struct {
	pool                                                *pgxpool.Pool
	acquired, idle, total, max, acquires, emptyAcquires *prometheus.Desc
	acquireSeconds                                      *prometheus.Desc
}

func newPoolCollector(pool *pgxpool.Pool) *poolCollector {
	d := func(name, help string) *prometheus.Desc { return prometheus.NewDesc(name, help, nil, nil) }
	return &poolCollector{
		pool:           pool,
		acquired:       d("db_pool_acquired_conns", "Connections currently checked out."),
		idle:           d("db_pool_idle_conns", "Idle connections in the pool."),
		total:          d("db_pool_total_conns", "Open connections."),
		max:            d("db_pool_max_conns", "Configured maximum connections."),
		acquires:       d("db_pool_acquires_total", "Successful connection acquires."),
		emptyAcquires:  d("db_pool_empty_acquires_total", "Acquires that had to wait because the pool was empty."),
		acquireSeconds: d("db_pool_acquire_seconds_total", "Total time spent waiting to acquire connections."),
	}
}

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.acquired, c.idle, c.total, c.max, c.acquires, c.emptyAcquires, c.acquireSeconds} {
		ch <- d
	}
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()
	ch <- prometheus.MustNewConstMetric(c.acquired, prometheus.GaugeValue, float64(s.AcquiredConns()))
	ch <- prometheus.MustNewConstMetric(c.idle, prometheus.GaugeValue, float64(s.IdleConns()))
	ch <- prometheus.MustNewConstMetric(c.total, prometheus.GaugeValue, float64(s.TotalConns()))
	ch <- prometheus.MustNewConstMetric(c.max, prometheus.GaugeValue, float64(s.MaxConns()))
	ch <- prometheus.MustNewConstMetric(c.acquires, prometheus.CounterValue, float64(s.AcquireCount()))
	ch <- prometheus.MustNewConstMetric(c.emptyAcquires, prometheus.CounterValue, float64(s.EmptyAcquireCount()))
	ch <- prometheus.MustNewConstMetric(c.acquireSeconds, prometheus.CounterValue, s.AcquireDuration().Seconds())
}

// DeclineReason maps a reservation outcome code to its metric label.
func DeclineReason(code string) string {
	switch code {
	case "seat_taken", "check_violation":
		return DeclineSeatTaken
	case "per_user_limit_exceeded":
		return DeclinePerUserLimit
	case "idempotent_replay":
		return DeclineIdempotentReplay
	case "idempotency_key_reused":
		return DeclineKeyReused
	case "seat_contended":
		return DeclineSeatContended
	case "overloaded", "busy_try_again":
		return DeclineOverloaded
	case "unknown_seats":
		return DeclineUnknownSeats
	case "show_not_found":
		return DeclineShowNotFound
	case "validation_failed", "invalid_idempotency_key", "invalid_json", "invalid_show_id":
		return DeclineValidation
	}
	return DeclineOther
}

// The methods below are nil-safe so handlers and tests can run without
// metrics wired in.

// Confirmed records one new booking of n seats.
func (m *Metrics) Confirmed(seats int) {
	if m == nil {
		return
	}
	m.ReservationsConfirmed.Inc()
	m.SeatsConfirmed.Add(float64(seats))
}

// Declined records a reservation request that did not create a booking.
func (m *Metrics) Declined(reason string) {
	if m == nil {
		return
	}
	m.ReservationsDeclined.WithLabelValues(reason).Inc()
}

// Cancelled records a cancellation that released n seats.
func (m *Metrics) Cancelled(seats int) {
	if m == nil {
		return
	}
	m.ReservationsCancelled.Inc()
	m.SeatsReleased.Add(float64(seats))
}

// ObserveHTTP records one finished HTTP request.
func (m *Metrics) ObserveHTTP(route, method string, code int, d time.Duration) {
	if m == nil {
		return
	}
	m.HTTPRequests.WithLabelValues(route, method, strconv.Itoa(code)).Inc()
	m.HTTPDuration.WithLabelValues(route, method).Observe(d.Seconds())
}
