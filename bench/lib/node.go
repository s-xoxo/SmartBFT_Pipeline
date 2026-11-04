package lib

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hyperledger/SmartBFT/pkg/api"
	"github.com/hyperledger/SmartBFT/pkg/consensus"
	"github.com/hyperledger/SmartBFT/pkg/metrics"
	"github.com/hyperledger/SmartBFT/pkg/metrics/disabled"
	"github.com/hyperledger/SmartBFT/pkg/types"
	"github.com/hyperledger/SmartBFT/smartbftprotos"
	"google.golang.org/protobuf/proto"
)

// Request wire format: client id, sequence and submit time (UnixNano), each 8 bytes big endian,
// followed by zero padding up to RequestSize bytes.
const (
	RequestSize = 64
	reqHeader   = 24
)

// EncodeRequest builds a request payload.
func EncodeRequest(client, seq uint64, submitted time.Time) []byte {
	b := make([]byte, RequestSize)
	binary.BigEndian.PutUint64(b[0:], client)
	binary.BigEndian.PutUint64(b[8:], seq)
	binary.BigEndian.PutUint64(b[16:], uint64(submitted.UnixNano()))
	return b
}

func decodeRequest(b []byte) (client, seq uint64, submitted int64, err error) {
	if len(b) < reqHeader {
		return 0, 0, 0, errors.New("request too short")
	}
	return binary.BigEndian.Uint64(b[0:]), binary.BigEndian.Uint64(b[8:]), int64(binary.BigEndian.Uint64(b[16:])), nil
}

func requestInfo(b []byte) (types.RequestInfo, error) {
	client, seq, _, err := decodeRequest(b)
	if err != nil {
		return types.RequestInfo{}, err
	}
	return types.RequestInfo{ClientID: strconv.FormatUint(client, 10), ID: strconv.FormatUint(seq, 10)}, nil
}

// A batch payload is a sequence of (uint32 length, request bytes).
func encodeBatch(requests [][]byte) []byte {
	size := 0
	for _, r := range requests {
		size += 4 + len(r)
	}
	b := make([]byte, 0, size)
	for _, r := range requests {
		b = binary.BigEndian.AppendUint32(b, uint32(len(r)))
		b = append(b, r...)
	}
	return b
}

func decodeBatch(b []byte) ([][]byte, error) {
	var reqs [][]byte
	for len(b) > 0 {
		if len(b) < 4 {
			return nil, errors.New("truncated batch")
		}
		n := int(binary.BigEndian.Uint32(b))
		b = b[4:]
		if n > len(b) {
			return nil, errors.New("truncated request in batch")
		}
		reqs = append(reqs, b[:n])
		b = b[n:]
	}
	return reqs, nil
}

// quietLogger implements api.Logger. Debug and Info are dropped, except that view starts are
// counted; warnings and errors are counted and the first few are printed.
type quietLogger struct {
	id         uint64
	viewStarts atomic.Uint64 // "Starting view with number X" with X > 0
	warnings   atomic.Uint64
	printed    atomic.Uint64
	mu         sync.Mutex
	first      []string
}

const viewStartPrefix = "Starting view with number"

func (l *quietLogger) Debugf(string, ...any) {}
func (l *quietLogger) Infof(template string, args ...any) {
	if strings.HasPrefix(template, viewStartPrefix) && len(args) > 0 {
		if v, ok := args[0].(uint64); ok && v > 0 {
			l.viewStarts.Add(1)
		}
	}
}
func (l *quietLogger) Warnf(template string, args ...any)  { l.warn("WARN", template, args...) }
func (l *quietLogger) Errorf(template string, args ...any) { l.warn("ERROR", template, args...) }
func (l *quietLogger) Panicf(template string, args ...any) {
	panic(fmt.Sprintf("node %d: "+template, append([]any{l.id}, args...)...))
}
func (l *quietLogger) warn(level, template string, args ...any) {
	l.warnings.Add(1)
	msg := fmt.Sprintf(template, args...)
	l.mu.Lock()
	if len(l.first) < 5 {
		l.first = append(l.first, msg)
	}
	l.mu.Unlock()
	if l.printed.Add(1) <= 10 {
		log.Printf("bench: node %d %s: %s", l.id, level, msg)
	}
}

// memWAL is an api.WriteAheadLog that keeps nothing: the benchmark measures consensus, not disks.
type memWAL struct{ bytes atomic.Uint64 }

func (w *memWAL) Append(entry []byte, _ bool) error { w.bytes.Add(uint64(len(entry))); return nil }

// maxProvider is a metrics.Provider that records the maximum of one gauge and discards the rest.
type maxProvider struct {
	name string
	max  atomic.Uint64 // float64 bits
	disabled.Provider
}

type maxGauge struct{ p *maxProvider }

func (p *maxProvider) NewGauge(o metrics.GaugeOpts) metrics.Gauge {
	if o.Name == p.name {
		return &maxGauge{p: p}
	}
	return p.Provider.NewGauge(o)
}
func (g *maxGauge) With(...string) metrics.Gauge { return g }
func (g *maxGauge) Add(float64)                  {}
func (g *maxGauge) Set(v float64) {
	for {
		old := g.p.max.Load()
		if v <= math.Float64frombits(old) || g.p.max.CompareAndSwap(old, math.Float64bits(v)) {
			return
		}
	}
}
func (p *maxProvider) Max() float64 { return math.Float64frombits(p.max.Load()) }

// Ledger is the decisions of a run, shared by all nodes, from which a node that fell behind
// catches up in Sync (as a real application would fetch blocks from its peers). Only the most
// recent ledgerKeep decisions are kept; a node further behind than that cannot catch up.
type Ledger struct {
	mu        sync.Mutex
	decisions map[uint64]types.Decision // by sequence
	latest    uint64
}

const ledgerKeep = 4096

func NewLedger() *Ledger { return &Ledger{decisions: map[uint64]types.Decision{}} }

func (l *Ledger) add(seq uint64, d types.Decision) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, dup := l.decisions[seq]; dup {
		return
	}
	l.decisions[seq] = d
	if seq > l.latest {
		l.latest = seq
	}
	delete(l.decisions, seq-ledgerKeep)
}

func (l *Ledger) get(seq uint64) (types.Decision, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	d, ok := l.decisions[seq]
	return d, ok
}

// App implements Application, Assembler, Signer, Verifier, RequestInspector, MembershipNotifier and
// Synchronizer for one node. Deliver only parses the batch and updates counters.
type App struct {
	id     uint64
	ledger *Ledger
	// Decisions and Requests count everything delivered; the m* fields count only while Measuring.
	Decisions, Requests   atomic.Uint64
	Measuring             atomic.Bool
	mDecisions, mRequests atomic.Uint64
	mDeliverNanos         atomic.Int64
	Syncs                 atomic.Uint64
	onDeliver             func(client, seq uint64, latency time.Duration) // leader only, may be nil
	mu                    sync.Mutex
	latencies             []time.Duration
	lastProposal          types.Proposal
	lastSignatures        []types.Signature
	latestSeq             uint64
	metadata              *smartbftprotos.ViewMetadata // the metadata handed to Consensus, never written after Start
}

func (a *App) Deliver(proposal types.Proposal, signatures []types.Signature) types.Reconfig {
	start := time.Now()
	reqs, err := decodeBatch(proposal.Payload)
	if err != nil {
		panic(err)
	}
	measuring := a.Measuring.Load()
	lats := make([]time.Duration, 0, len(reqs))
	for _, r := range reqs {
		client, seq, submitted, err := decodeRequest(r)
		if err != nil {
			panic(err)
		}
		lat := time.Duration(start.UnixNano() - submitted)
		lats = append(lats, lat)
		if a.onDeliver != nil {
			a.onDeliver(client, seq, lat)
		}
	}
	md := &smartbftprotos.ViewMetadata{}
	if err := proto.Unmarshal(proposal.Metadata, md); err != nil {
		panic(err)
	}
	a.mu.Lock()
	if measuring {
		a.latencies = append(a.latencies, lats...)
	}
	a.lastProposal, a.lastSignatures, a.latestSeq = proposal, signatures, md.LatestSequence
	a.mu.Unlock()
	a.ledger.add(md.LatestSequence, types.Decision{Proposal: proposal, Signatures: signatures})
	a.Decisions.Add(1)
	a.Requests.Add(uint64(len(reqs)))
	if measuring {
		a.mDecisions.Add(1)
		a.mRequests.Add(uint64(len(reqs)))
		a.mDeliverNanos.Add(int64(time.Since(start)))
	}
	return types.Reconfig{InLatestDecision: false}
}

// Latencies returns the samples recorded while Measuring and clears them.
func (a *App) Latencies() []time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	l := a.latencies
	a.latencies = nil
	return l
}

func (a *App) AssembleProposal(metadata []byte, requests [][]byte) types.Proposal {
	return types.Proposal{Payload: encodeBatch(requests), Metadata: metadata}
}

func (a *App) idBytes() []byte    { return binary.BigEndian.AppendUint64(nil, a.id) }
func (a *App) Sign([]byte) []byte { return a.idBytes() }
func (a *App) SignProposal(_ types.Proposal, aux []byte) *types.Signature {
	return &types.Signature{ID: a.id, Value: a.idBytes(), Msg: aux}
}

func (a *App) VerifyProposal(p types.Proposal) ([]types.RequestInfo, error) {
	reqs, err := decodeBatch(p.Payload)
	if err != nil {
		return nil, err
	}
	infos := make([]types.RequestInfo, 0, len(reqs))
	for _, r := range reqs {
		info, err := requestInfo(r)
		if err != nil {
			return nil, err
		}
		infos = append(infos, info)
	}
	return infos, nil
}
func (a *App) RequestsFromProposal(p types.Proposal) []types.RequestInfo {
	infos, _ := a.VerifyProposal(p)
	return infos
}
func (a *App) VerifyRequest(r []byte) (types.RequestInfo, error) { return requestInfo(r) }
func (a *App) VerifyConsenterSig(s types.Signature, _ types.Proposal) ([]byte, error) {
	return s.Msg, nil
}
func (a *App) VerifySignature(types.Signature) error { return nil }
func (a *App) VerificationSequence() uint64          { return 0 }
func (a *App) AuxiliaryData(msg []byte) []byte       { return msg }
func (a *App) RequestID(r []byte) types.RequestInfo  { info, _ := requestInfo(r); return info }
func (a *App) MembershipChange() bool                { return false }

// Sync is called when a node falls behind: it delivers the decisions it missed from the shared
// ledger, in order, and hands back the last one.
func (a *App) Sync() types.SyncResponse {
	a.Syncs.Add(1)
	for {
		a.mu.Lock()
		next := a.latestSeq + 1
		a.mu.Unlock()
		d, ok := a.ledger.get(next)
		if !ok {
			break
		}
		a.Deliver(d.Proposal, d.Signatures)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return types.SyncResponse{Latest: types.Decision{Proposal: a.lastProposal, Signatures: a.lastSignatures}}
}

// Node is one consensus instance with its dependencies.
type Node struct {
	ID        uint64
	App       *App
	Consensus *consensus.Consensus
	Log       *quietLogger
	InFlight  *maxProvider
	clock     *time.Ticker
	vcClock   *time.Ticker
}

// NewNode wires a node; cfg.SelfID is set to id. Comm is taken from the network, which must not
// have been started yet; the ledger is shared by all nodes of the run.
func NewNode(id uint64, cfg types.Configuration, net *Network, ledger *Ledger) *Node {
	cfg.SelfID = id
	app := &App{id: id, ledger: ledger, metadata: &smartbftprotos.ViewMetadata{}}
	logger := &quietLogger{id: id}
	prov := &maxProvider{name: "view_in_flight_proposals"}
	n := &Node{ID: id, App: app, Log: logger, InFlight: prov,
		clock: time.NewTicker(time.Second), vcClock: time.NewTicker(time.Second)}
	n.Consensus = &consensus.Consensus{
		Config:             cfg,
		Application:        app,
		Assembler:          app,
		WAL:                &memWAL{},
		Comm:               net.NewComm(id),
		Signer:             app,
		Verifier:           app,
		MembershipNotifier: app,
		RequestInspector:   app,
		Synchronizer:       app,
		Logger:             logger,
		Metrics:            api.NewMetrics(prov),
		Metadata:           app.metadata,
		Scheduler:          n.clock.C,
		ViewChangerTicker:  n.vcClock.C,
	}
	net.SetHandler(id, n.Consensus)
	return n
}

// Start starts consensus.
func (n *Node) Start() error { return n.Consensus.Start() }

// Stop stops consensus and the tickers.
func (n *Node) Stop() {
	n.Consensus.Stop()
	n.clock.Stop()
	n.vcClock.Stop()
}
