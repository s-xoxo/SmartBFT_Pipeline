// Package lib is a small in-process benchmark harness for the SmartBFT fork: N consensus
// nodes wired through a delayed in-memory network, a closed-loop client and a runner.
package lib

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hyperledger/SmartBFT/smartbftprotos"
)

// linkBuffer bounds the number of messages a single (from,to) link may hold before dropping.
const linkBuffer = 100_000

// Handler is the part of consensus.Consensus the network delivers into.
type Handler interface {
	HandleMessage(sender uint64, m *smartbftprotos.Message)
	HandleRequest(sender uint64, req []byte)
}

type envelope struct {
	deadline time.Time
	msg      *smartbftprotos.Message // consensus message, or
	req      []byte                  // forwarded client request
}

// Network connects the nodes. Every directed link (from,to) is a FIFO queue drained by its own
// goroutine, which delivers each message once its one-way delay has elapsed; delay 0 keeps the
// FIFO order without sleeping.
type Network struct {
	delay    time.Duration
	ids      []uint64
	handlers map[uint64]Handler
	links    map[[2]uint64]chan envelope
	wg       sync.WaitGroup
	dropped  atomic.Uint64
	dropOnce sync.Once
}

// NewNetwork creates the links between the given node ids with the given one-way delay.
func NewNetwork(ids []uint64, oneWay time.Duration) *Network {
	n := &Network{delay: oneWay, ids: ids, handlers: map[uint64]Handler{}, links: map[[2]uint64]chan envelope{}}
	for _, from := range ids {
		for _, to := range ids {
			if from != to {
				n.links[[2]uint64{from, to}] = make(chan envelope, linkBuffer)
			}
		}
	}
	return n
}

// SetHandler registers the consensus instance of a node; it must be called before Start.
func (n *Network) SetHandler(id uint64, h Handler) { n.handlers[id] = h }

// Start spawns the link goroutines.
func (n *Network) Start() {
	for key, ch := range n.links {
		n.wg.Add(1)
		go n.serve(key[0], n.handlers[key[1]], ch)
	}
}

// Stop closes all links and waits for the link goroutines; the handlers must already be stopped
// so that pending deliveries return promptly.
func (n *Network) Stop() {
	for _, ch := range n.links {
		close(ch)
	}
	n.wg.Wait()
}

// Dropped reports how many messages were dropped because a link buffer was full.
func (n *Network) Dropped() uint64 { return n.dropped.Load() }

func (n *Network) serve(from uint64, h Handler, ch chan envelope) {
	defer n.wg.Done()
	for e := range ch {
		if d := time.Until(e.deadline); d > 0 {
			time.Sleep(d)
		}
		if e.msg != nil {
			h.HandleMessage(from, e.msg)
		} else {
			h.HandleRequest(from, e.req)
		}
	}
}

func (n *Network) send(from, to uint64, e envelope) {
	ch, ok := n.links[[2]uint64{from, to}]
	if !ok {
		return
	}
	e.deadline = time.Now().Add(n.delay)
	select {
	case ch <- e:
	default:
		n.dropped.Add(1)
		n.dropOnce.Do(func() { log.Printf("bench: link %d->%d full (%d msgs), dropping", from, to, linkBuffer) })
	}
}

// Comm is one node's api.Comm view of the Network.
type Comm struct {
	net *Network
	id  uint64
}

// NewComm returns the Comm of node id.
func (n *Network) NewComm(id uint64) *Comm { return &Comm{net: n, id: id} }

// SendConsensus implements api.Comm.
func (c *Comm) SendConsensus(targetID uint64, m *smartbftprotos.Message) {
	c.net.send(c.id, targetID, envelope{msg: m})
}

// SendTransaction implements api.Comm.
func (c *Comm) SendTransaction(targetID uint64, request []byte) {
	c.net.send(c.id, targetID, envelope{req: request})
}

// Nodes implements api.Comm; the slice is shared, callers must not modify it.
func (c *Comm) Nodes() []uint64 { return c.net.ids }
