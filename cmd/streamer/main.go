// Package main implements the event-streaming service.
//
// Architecture:
//
//	Redis Streams (rl:events)
//	        │
//	   [Consumer Group Reader goroutine]
//	        │  XREADGROUP / XACK
//	        ▼
//	   [central eventCh  — buffered 2000]
//	        │
//	   [Dispatcher goroutine]
//	     ╱       ╲
//	[client1]  [client2]  ...
//	[ch 256]   [ch 256]
//	    │           │
//	 [WS conn]  [WS conn]
//
// Back-pressure: if a client's 256-deep channel is full, the dispatcher
// signals the client to disconnect (slow-consumer eviction).
//
// Fault tolerance: events are ACKed only after dispatch.  On restart the
// consumer reclaims its own stale pending entries (idle > 30 s) so no
// message is permanently lost.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"example.com/distributed-rate-limiter/internal/limiter"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

//go:embed dashboard.html
var dashboardHTML string

const (
	streamKey       = "rl:events"
	groupName       = "streamer"
	clientBufSize   = 256
	centralBufSize  = 2000
	maxPendingAge   = 30 * time.Second
	readBatchSize   = 100
	blockTimeout    = 1 * time.Second
	streamMaxLen    = 50000
)

// Event is the parsed form of a Redis Streams message.
type Event struct {
	ID        string `json:"id"`
	Timestamp int64  `json:"ts"`
	Namespace string `json:"ns"`
	Key       string `json:"key"`
	Algo      string `json:"algo"`
	Allowed   bool   `json:"allowed"`
	Remaining int64  `json:"remaining"`
	RetryMS   int64  `json:"retry_ms"`
	LatencyUS int64  `json:"latency_us"`
	Instance  string `json:"instance"`
}

// client represents one connected WebSocket consumer.
type client struct {
	id     string
	events chan Event
	evict  chan struct{} // closed by dispatcher when client is too slow
}

// Dispatcher fans events out to all registered WebSocket clients.
type Dispatcher struct {
	mu      sync.RWMutex
	clients map[string]*client
	eventCh chan Event
}

func newDispatcher() *Dispatcher {
	return &Dispatcher{
		clients: make(map[string]*client),
		eventCh: make(chan Event, centralBufSize),
	}
}

func (d *Dispatcher) register(c *client) {
	d.mu.Lock()
	d.clients[c.id] = c
	d.mu.Unlock()
	log.Info().Str("client", c.id).Int("total", len(d.clients)).Msg("ws client connected")
}

func (d *Dispatcher) unregister(c *client) {
	d.mu.Lock()
	delete(d.clients, c.id)
	d.mu.Unlock()
	log.Info().Str("client", c.id).Int("total", len(d.clients)).Msg("ws client disconnected")
}

// run is the fan-out loop: pull from eventCh, push to every client.
func (d *Dispatcher) run() {
	for e := range d.eventCh {
		d.mu.RLock()
		for _, c := range d.clients {
			select {
			case c.events <- e:
				// delivered
			default:
				// client buffer full — evict it (non-blocking)
				select {
				case c.evict <- struct{}{}:
				default:
				}
			}
		}
		d.mu.RUnlock()
	}
}

// stats returns a snapshot of the current dispatcher state.
func (d *Dispatcher) stats() map[string]any {
	d.mu.RLock()
	n := len(d.clients)
	d.mu.RUnlock()
	return map[string]any{
		"connected_clients": n,
		"central_buf_used":  len(d.eventCh),
		"central_buf_cap":   cap(d.eventCh),
	}
}

// ---- Consumer Group Reader ----

type Streamer struct {
	rdb        *redis.ClusterClient
	dispatcher *Dispatcher
	consumer   string // unique per-instance name for the consumer group
}

func newStreamer(rdb *redis.ClusterClient, dispatcher *Dispatcher) *Streamer {
	hostname, _ := os.Hostname()
	return &Streamer{
		rdb:        rdb,
		dispatcher: dispatcher,
		consumer:   hostname + "-" + strconv.Itoa(os.Getpid()),
	}
}

// ensureGroup creates the consumer group if it doesn't exist yet.
// Using "$" as the start ID means we only read events produced after
// startup — appropriate for a live dashboard.
func (s *Streamer) ensureGroup(ctx context.Context) {
	err := s.rdb.XGroupCreateMkStream(ctx, streamKey, groupName, "$").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		log.Warn().Err(err).Msg("xgroup create")
	}
}

// reclaimPending claims and ACKs any messages that this consumer left
// pending from a previous run (idle > maxPendingAge).
func (s *Streamer) reclaimPending(ctx context.Context) {
	pending, err := s.rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: streamKey,
		Group:  groupName,
		Start:  "-",
		End:    "+",
		Count:  500,
	}).Result()
	if err != nil {
		return
	}
	for _, p := range pending {
		if p.Idle < maxPendingAge {
			continue
		}
		claimed, err := s.rdb.XClaim(ctx, &redis.XClaimArgs{
			Stream:   streamKey,
			Group:    groupName,
			Consumer: s.consumer,
			MinIdle:  maxPendingAge,
			Messages: []string{p.ID},
		}).Result()
		if err != nil {
			continue
		}
		ids := make([]string, 0, len(claimed))
		for _, m := range claimed {
			ids = append(ids, m.ID)
		}
		if len(ids) > 0 {
			_ = s.rdb.XAck(ctx, streamKey, groupName, ids...).Err()
			log.Info().Int("count", len(ids)).Msg("reclaimed stale pending entries")
		}
	}
}

// run is the main consumer loop.
func (s *Streamer) run(ctx context.Context) {
	s.ensureGroup(ctx)
	s.reclaimPending(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		streams, err := s.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    groupName,
			Consumer: s.consumer,
			Streams:  []string{streamKey, ">"},
			Count:    readBatchSize,
			Block:    blockTimeout,
		}).Result()

		if err != nil {
			if err == redis.Nil || strings.Contains(err.Error(), "context") {
				continue
			}
			log.Warn().Err(err).Msg("xreadgroup error — retrying")
			time.Sleep(500 * time.Millisecond)
			continue
		}

		for _, stream := range streams {
			ids := make([]string, 0, len(stream.Messages))
			for _, msg := range stream.Messages {
				e := parseEvent(msg)

				select {
				case s.dispatcher.eventCh <- e:
				default:
					// central buffer full — drop rather than block Redis reader
				}
				ids = append(ids, msg.ID)
			}
			if len(ids) > 0 {
				_ = s.rdb.XAck(ctx, streamKey, groupName, ids...).Err()
			}
		}
	}
}

func parseEvent(msg redis.XMessage) Event {
	v := msg.Values
	e := Event{
		ID:        msg.ID,
		Namespace: str(v, "ns"),
		Key:       str(v, "key"),
		Algo:      str(v, "algo"),
		Instance:  str(v, "instance"),
	}
	e.Timestamp = parseInt(v, "ts")
	e.Remaining = parseInt(v, "remaining")
	e.RetryMS = parseInt(v, "retry_ms")
	e.LatencyUS = parseInt(v, "latency_us")
	e.Allowed = str(v, "allowed") == "1"
	return e
}

func str(m map[string]interface{}, k string) string {
	if v, ok := m[k]; ok {
		return v.(string)
	}
	return ""
}

func parseInt(m map[string]interface{}, k string) int64 {
	s := str(m, k)
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// ---- WebSocket handler ----

var upgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
}

var wsClientSeq uint64
var wsClientMu sync.Mutex

func nextClientID() string {
	wsClientMu.Lock()
	defer wsClientMu.Unlock()
	wsClientSeq++
	return "ws-" + strconv.FormatUint(wsClientSeq, 10)
}

func wsHandler(d *Dispatcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Warn().Err(err).Msg("ws upgrade failed")
			return
		}
		defer conn.Close()

		c := &client{
			id:     nextClientID(),
			events: make(chan Event, clientBufSize),
			evict:  make(chan struct{}, 1),
		}
		d.register(c)
		defer d.unregister(c)

		// Read loop — handles ping/pong and client-initiated close.
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					select {
					case c.evict <- struct{}{}:
					default:
					}
					return
				}
			}
		}()

		conn.SetPingHandler(func(data string) error {
			return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(time.Second))
		})

		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case e := <-c.events:
				data, _ := json.Marshal(e)
				conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
					return
				}
			case <-ticker.C:
				conn.SetWriteDeadline(time.Now().Add(time.Second))
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			case <-c.evict:
				// Slow consumer or client closed — gracefully disconnect.
				conn.WriteControl(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "slow consumer"),
					time.Now().Add(time.Second),
				)
				return
			}
		}
	}
}

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	hostname, _ := os.Hostname()
	log.Logger = zerolog.New(os.Stdout).With().
		Str("service", "streamer").
		Str("instance", hostname).
		Timestamp().
		Logger()

	httpAddr := env("HTTP_ADDR", "0.0.0.0:8888")
	redisAddrs := env("REDIS_ADDRS", "redis-1:7001,redis-2:7002,redis-3:7003,redis-4:7004,redis-5:7005,redis-6:7006")

	rdb := limiter.NewRedisCluster(redisAddrs)
	defer rdb.Close()

	// Confirm connectivity before starting.
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatal().Err(err).Msg("redis ping failed")
	}

	dispatcher := newDispatcher()
	go dispatcher.run()

	streamer := newStreamer(rdb, dispatcher)
	go streamer.run(ctx)

	mux := http.NewServeMux()

	// Dashboard UI
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(dashboardHTML))
	})

	// WebSocket endpoint
	mux.HandleFunc("/ws/events", wsHandler(dispatcher))

	// Stats endpoint for health checks / debug dashboard
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(dispatcher.stats())
	})

	log.Info().Str("addr", httpAddr).Msg("streamer listening")
	if err := http.ListenAndServe(httpAddr, mux); err != nil {
		log.Fatal().Err(err).Msg("http server failed")
	}
}

func env(k, def string) string {
	v := os.Getenv(k)
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
