package lib

import (
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// Client is a closed-loop load generator: K workers each keep exactly one request outstanding at
// the leader and submit the next one as soon as the leader delivers the previous one.
type Client struct {
	node   *Node
	done   []chan struct{} // one per worker, capacity 1
	stop   chan struct{}
	wg     sync.WaitGroup
	Failed atomic.Uint64 // SubmitRequest errors (pool full / timeout)
}

// NewClient attaches K workers to the leader node. It must be called before the node starts.
func NewClient(leader *Node, k int) *Client {
	c := &Client{node: leader, done: make([]chan struct{}, k), stop: make(chan struct{})}
	for i := range c.done {
		c.done[i] = make(chan struct{}, 1)
	}
	leader.App.onDeliver = func(client, _ uint64, _ time.Duration) {
		if client == 0 || client > uint64(k) {
			return
		}
		select {
		case c.done[client-1] <- struct{}{}:
		default:
		}
	}
	return c
}

// Start starts the workers.
func (c *Client) Start() {
	for i := range c.done {
		c.wg.Add(1)
		go c.worker(uint64(i + 1))
	}
}

// Stop stops the workers and waits for them.
func (c *Client) Stop() {
	close(c.stop)
	c.wg.Wait()
}

func (c *Client) worker(client uint64) {
	defer c.wg.Done()
	done := c.done[client-1]
	for seq := uint64(1); ; seq++ {
		for {
			err := c.node.Consensus.SubmitRequest(EncodeRequest(client, seq, time.Now()))
			if err == nil {
				break
			}
			if n := c.Failed.Add(1); n <= 3 {
				log.Printf("bench: client %d submit failed: %v", client, err)
			}
			select {
			case <-c.stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
		select {
		case <-done:
		case <-c.stop:
			return
		}
	}
}
