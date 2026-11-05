package lib

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hyperledger/SmartBFT/pkg/types"
	"github.com/hyperledger/SmartBFT/smartbftprotos"
	"google.golang.org/protobuf/proto"
)

// Wire format of the process-separated harness (cmd/node): one TCP connection per direction per
// pair of nodes. Every frame is kind(1) | length(4, big endian) | payload. The first frame of a
// connection is a hello carrying the 8-byte id of the dialing node.
const (
	frameHello    byte = 0
	frameMessage  byte = 1 // smartbftprotos.Message (consensus)
	frameRequest  byte = 2 // a request forwarded by the library (api.Comm.SendTransaction)
	frameSubmit   byte = 3 // a client request to be submitted at the receiver (Consensus.SubmitRequest)
	frameSyncReq  byte = 4 // 8 bytes: the requester's latest sequence
	frameSyncResp byte = 5 // encoded decisions after that sequence (see encodeDecisions)

	maxFrame     = 64 << 20
	tcpLinkQueue = 100_000
)

// Receiver is what the transport delivers into (consensus.Consensus plus a submit entry point).
type Receiver interface {
	Handler
	SubmitRequest(req []byte) error
}

type outFrame struct {
	deadline time.Time
	kind     byte
	msg      *smartbftprotos.Message // marshalled by the link goroutine, off the consensus goroutines
	data     []byte
}

type tcpLink struct {
	t     *Transport
	to    uint64
	addr  string
	ch    chan outFrame
	conn  net.Conn
	w     *bufio.Writer
	up    atomic.Bool
	wasUp bool
}

// Transport is the TCP network of one node: it listens for the peers, keeps one outbound link per
// peer (FIFO, with the one-way delay applied before writing to the socket, reconnecting when the
// peer goes away) and implements api.Comm and Fetcher.
type Transport struct {
	id       uint64
	ids      []uint64 // all nodes, sorted, including self
	delay    time.Duration
	links    map[uint64]*tcpLink
	ledger   *Ledger
	recv     Receiver
	ready    chan struct{} // closed by Start: inbound frames are dispatched only after that
	ln       net.Listener
	wg       sync.WaitGroup
	closing  atomic.Bool
	inbound  sync.Map // peer id -> struct{} once its hello arrived
	inMu     sync.Mutex
	inConns  []net.Conn
	fetchMu  sync.Mutex
	fetchers map[uint64]chan []types.Decision // sync responses by requester round id
	fetchSeq uint64

	Dropped    atomic.Uint64 // frames dropped because the link was down
	Reconnects atomic.Uint64 // successful (re)connections after the first
	Submits    atomic.Uint64 // frameSubmit received
	SyncServed atomic.Uint64 // frameSyncReq answered
	BytesOut   atomic.Uint64
	BytesIn    atomic.Uint64
}

// NewTransport creates the transport of node id. peers maps every other node id to host:port;
// recv and ledger must be set with Start before the first inbound frame is dispatched.
func NewTransport(id uint64, listen string, peers map[uint64]string, oneWay time.Duration) (*Transport, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	t := &Transport{id: id, delay: oneWay, links: map[uint64]*tcpLink{}, ln: ln, ready: make(chan struct{}),
		fetchers: map[uint64]chan []types.Decision{}}
	t.ids = append(t.ids, id)
	for pid, addr := range peers {
		t.ids = append(t.ids, pid)
		t.links[pid] = &tcpLink{t: t, to: pid, addr: addr, ch: make(chan outFrame, tcpLinkQueue)}
	}
	slices.Sort(t.ids)
	t.wg.Add(1)
	go t.accept()
	for _, l := range t.links {
		t.wg.Add(1)
		go l.run()
	}
	return t, nil
}

// Start sets the receiver and lets inbound frames flow; call once the consensus instance exists.
func (t *Transport) Start(recv Receiver, ledger *Ledger) {
	t.recv, t.ledger = recv, ledger
	close(t.ready)
}

// WaitConnected blocks until every outbound link is up and every peer has said hello on an inbound
// connection, or the timeout passes.
func (t *Transport) WaitConnected(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ok := true
		for pid, l := range t.links {
			if _, in := t.inbound.Load(pid); !in || !l.up.Load() {
				ok = false
				break
			}
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			var missing []string
			for pid, l := range t.links {
				_, in := t.inbound.Load(pid)
				missing = append(missing, fmt.Sprintf("%d(out=%v,in=%v)", pid, l.up.Load(), in))
			}
			return fmt.Errorf("peers not connected after %s: %v", timeout, missing)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Close stops the listener and the links.
func (t *Transport) Close() {
	t.closing.Store(true)
	_ = t.ln.Close()
	for _, l := range t.links {
		close(l.ch)
	}
	t.inMu.Lock()
	for _, c := range t.inConns {
		_ = c.Close()
	}
	t.inMu.Unlock()
	t.wg.Wait()
}

// --- api.Comm ---

// SendConsensus implements api.Comm.
func (t *Transport) SendConsensus(targetID uint64, m *smartbftprotos.Message) {
	t.enqueue(targetID, outFrame{kind: frameMessage, msg: m})
}

// SendTransaction implements api.Comm.
func (t *Transport) SendTransaction(targetID uint64, request []byte) {
	t.enqueue(targetID, outFrame{kind: frameRequest, data: request})
}

// Nodes implements api.Comm; the slice is shared, callers must not modify it.
func (t *Transport) Nodes() []uint64 { return t.ids }

// Submit sends a client request to be submitted at node targetID (self is submitted directly).
func (t *Transport) Submit(targetID uint64, request []byte) error {
	if targetID == t.id {
		return t.recv.SubmitRequest(request)
	}
	t.enqueue(targetID, outFrame{kind: frameSubmit, data: request})
	return nil
}

// Connected reports whether the outbound link to the peer is currently up.
func (t *Transport) Connected(targetID uint64) bool {
	l, ok := t.links[targetID]
	return ok && l.up.Load()
}

func (t *Transport) enqueue(to uint64, f outFrame) {
	l, ok := t.links[to]
	if !ok {
		return
	}
	f.deadline = time.Now().Add(t.delay)
	select {
	case l.ch <- f:
	default:
		t.Dropped.Add(1)
	}
}

// --- Fetcher (state transfer for Sync) ---

// FetchTimeout bounds one Fetch round on top of one RTT (peers answer after one RTT).
var FetchTimeout = time.Second

// Fetch implements Fetcher: it asks every connected peer for the decisions after the given sequence
// and returns the first non-empty answer, or nil once every connected peer answered "nothing newer"
// or FetchTimeout + RTT passed. A dead peer (link down) is not waited for, so a Sync during a view
// change after a leader crash does not stall.
func (t *Transport) Fetch(after uint64) []types.Decision {
	ch := make(chan []types.Decision, len(t.links))
	t.fetchMu.Lock()
	t.fetchSeq++
	round := t.fetchSeq
	t.fetchers[round] = ch
	t.fetchMu.Unlock()
	defer func() {
		t.fetchMu.Lock()
		delete(t.fetchers, round)
		t.fetchMu.Unlock()
	}()
	req := make([]byte, 16)
	binary.BigEndian.PutUint64(req, round)
	binary.BigEndian.PutUint64(req[8:], after)
	expected := 0
	for pid, l := range t.links {
		if l.up.Load() {
			expected++
			t.enqueue(pid, outFrame{kind: frameSyncReq, data: req})
		}
	}
	timeout := time.After(FetchTimeout + 2*t.delay)
	for answers := 0; answers < expected; answers++ {
		select {
		case d := <-ch:
			if len(d) > 0 {
				return d
			}
		case <-timeout:
			return nil
		}
	}
	return nil
}

// syncBatch bounds the decisions per sync response.
const syncBatch = 256

// --- outbound link ---

func (l *tcpLink) run() {
	defer l.t.wg.Done()
	for {
		if l.conn == nil && !l.dial() {
			// Down: retry every 100 ms and drop whatever is queued meanwhile (consensus tolerates
			// loss; the library resends), so the queue never backs up behind a dead peer.
			select {
			case f, ok := <-l.ch:
				if !ok {
					return
				}
				_ = f
				l.t.Dropped.Add(1)
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		f, ok := <-l.ch
		if !ok {
			break
		}
		if d := time.Until(f.deadline); d > 0 {
			time.Sleep(d)
		}
		if err := l.write(f); err != nil {
			l.drop(err)
			l.t.Dropped.Add(1)
			continue
		}
		if len(l.ch) == 0 {
			if err := l.w.Flush(); err != nil {
				l.drop(err)
			}
		}
	}
	if l.conn != nil {
		_ = l.w.Flush()
		_ = l.conn.Close()
	}
}

func (l *tcpLink) dial() bool {
	conn, err := net.DialTimeout("tcp", l.addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	l.conn, l.w = conn, bufio.NewWriterSize(conn, 256<<10)
	hello := binary.BigEndian.AppendUint64(nil, l.t.id)
	if err := l.write(outFrame{kind: frameHello, data: hello}); err != nil {
		l.drop(err)
		return false
	}
	if err := l.w.Flush(); err != nil {
		l.drop(err)
		return false
	}
	if l.wasUp {
		l.t.Reconnects.Add(1)
	}
	l.wasUp = true
	l.up.Store(true)
	return true
}

func (l *tcpLink) drop(err error) {
	if l.conn != nil {
		_ = l.conn.Close()
	}
	l.conn, l.w = nil, nil
	if l.up.Swap(false) && !l.t.closing.Load() {
		log.Printf("bench: node %d link to %d down: %v", l.t.id, l.to, err)
	}
}

func (l *tcpLink) write(f outFrame) error {
	data := f.data
	if f.msg != nil {
		var err error
		if data, err = proto.Marshal(f.msg); err != nil {
			return err
		}
	}
	var hdr [5]byte
	hdr[0] = f.kind
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(data)))
	if _, err := l.w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := l.w.Write(data)
	l.t.BytesOut.Add(uint64(5 + len(data)))
	return err
}

// --- inbound ---

func (t *Transport) accept() {
	defer t.wg.Done()
	for {
		conn, err := t.ln.Accept()
		if err != nil {
			if t.closing.Load() {
				return
			}
			log.Printf("bench: node %d accept: %v", t.id, err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}
		t.inMu.Lock()
		t.inConns = append(t.inConns, conn)
		t.inMu.Unlock()
		t.wg.Add(1)
		go t.serve(conn)
	}
}

func readFrame(r *bufio.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("frame of %d bytes", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return hdr[0], buf, nil
}

func (t *Transport) serve(conn net.Conn) {
	defer t.wg.Done()
	defer conn.Close()
	r := bufio.NewReaderSize(conn, 256<<10)
	kind, hello, err := readFrame(r)
	if err != nil || kind != frameHello || len(hello) != 8 {
		return
	}
	sender := binary.BigEndian.Uint64(hello)
	if _, ok := t.links[sender]; !ok {
		log.Printf("bench: node %d: hello from unknown node %d", t.id, sender)
		return
	}
	t.inbound.Store(sender, struct{}{})
	<-t.ready
	for {
		kind, data, err := readFrame(r)
		if err != nil {
			if !t.closing.Load() && !errors.Is(err, io.EOF) {
				log.Printf("bench: node %d: connection from %d: %v", t.id, sender, err)
			}
			return
		}
		t.BytesIn.Add(uint64(5 + len(data)))
		switch kind {
		case frameMessage:
			m := &smartbftprotos.Message{}
			if err := proto.Unmarshal(data, m); err != nil {
				log.Printf("bench: node %d: bad message from %d: %v", t.id, sender, err)
				continue
			}
			t.recv.HandleMessage(sender, m)
		case frameRequest:
			t.recv.HandleRequest(sender, data)
		case frameSubmit:
			t.Submits.Add(1)
			_ = t.recv.SubmitRequest(data) // duplicates and full pools are the client's problem
		case frameSyncReq:
			if len(data) != 16 {
				continue
			}
			t.SyncServed.Add(1)
			after := binary.BigEndian.Uint64(data[8:])
			resp := encodeDecisions(data[:8], t.ledger.After(after, syncBatch))
			t.enqueue(sender, outFrame{kind: frameSyncResp, data: resp})
		case frameSyncResp:
			round, decisions, err := decodeDecisions(data)
			if err != nil {
				log.Printf("bench: node %d: bad sync response from %d: %v", t.id, sender, err)
				continue
			}
			t.fetchMu.Lock()
			ch := t.fetchers[round]
			t.fetchMu.Unlock()
			if ch != nil {
				select {
				case ch <- decisions:
				default:
				}
			}
		}
	}
}

// --- decision encoding (sync responses) ---

func putBytes(b, v []byte) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(v)))
	return append(b, v...)
}

func encodeDecisions(round []byte, ds []types.Decision) []byte {
	b := append([]byte{}, round...)
	b = binary.BigEndian.AppendUint32(b, uint32(len(ds)))
	for _, d := range ds {
		b = putBytes(b, d.Proposal.Header)
		b = putBytes(b, d.Proposal.Payload)
		b = putBytes(b, d.Proposal.Metadata)
		b = binary.BigEndian.AppendUint64(b, uint64(d.Proposal.VerificationSequence))
		b = binary.BigEndian.AppendUint32(b, uint32(len(d.Signatures)))
		for _, s := range d.Signatures {
			b = binary.BigEndian.AppendUint64(b, s.ID)
			b = putBytes(b, s.Value)
			b = putBytes(b, s.Msg)
		}
	}
	return b
}

type reader struct {
	b   []byte
	err error
}

func (r *reader) u32() uint32 {
	if r.err != nil || len(r.b) < 4 {
		r.err = errors.New("truncated")
		return 0
	}
	v := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

func (r *reader) u64() uint64 {
	if r.err != nil || len(r.b) < 8 {
		r.err = errors.New("truncated")
		return 0
	}
	v := binary.BigEndian.Uint64(r.b)
	r.b = r.b[8:]
	return v
}

func (r *reader) bytes() []byte {
	n := int(r.u32())
	if r.err != nil || n > len(r.b) {
		r.err = errors.New("truncated")
		return nil
	}
	v := r.b[:n:n]
	r.b = r.b[n:]
	if n == 0 {
		return nil
	}
	return v
}

func decodeDecisions(b []byte) (round uint64, ds []types.Decision, err error) {
	r := &reader{b: b}
	round = r.u64()
	n := int(r.u32())
	for i := 0; i < n && r.err == nil; i++ {
		var d types.Decision
		d.Proposal.Header = r.bytes()
		d.Proposal.Payload = r.bytes()
		d.Proposal.Metadata = r.bytes()
		d.Proposal.VerificationSequence = int64(r.u64())
		ns := int(r.u32())
		for j := 0; j < ns && r.err == nil; j++ {
			var s types.Signature
			s.ID = r.u64()
			s.Value = r.bytes()
			s.Msg = r.bytes()
			d.Signatures = append(d.Signatures, s)
		}
		ds = append(ds, d)
	}
	return round, ds, r.err
}
