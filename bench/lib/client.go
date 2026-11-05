package lib

import (
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// SubmitFunc submits a request on behalf of worker (1-based). attempt is 0 for the first submission
// of a request and grows by one for every resubmission after ResubmitAfter, so that a caller can
// fail over to another node. An error means the request was rejected locally and is retried soon.
type SubmitFunc func(worker uint64, attempt int, req []byte) error

// Client is a closed-loop load generator: K workers each keep exactly one request outstanding and
// submit the next one as soon as the node whose App they watch delivers the previous one.
type Client struct {
	submit SubmitFunc
	done   []chan struct{} // one per worker, capacity 1
	stop   chan struct{}
	wg     sync.WaitGroup
	// ResubmitAfter, when > 0, resubmits a request that has not been delivered within that time
	// (e.g. because the node it was sent to died). 0 waits forever, as the in-process harness does.
	ResubmitAfter time.Duration
	Failed        atomic.Uint64 // SubmitRequest errors (pool full / timeout)
	Resubmits     atomic.Uint64 // requests resubmitted after ResubmitAfter
}

// NewClient attaches K workers to the leader node, submitting locally. It must be called before the
// node starts.
func NewClient(leader *Node, k int) *Client {
	return NewClientFunc(leader.App, k, func(_ uint64, _ int, req []byte) error {
		return leader.Consensus.SubmitRequest(req)
	})
}

// NewClientFunc attaches K workers that submit through submit and learn about deliveries from
// app (which must deliver every decision, as every node does). Call before the node starts.
func NewClientFunc(app *App, k int, submit SubmitFunc) *Client {
	c := &Client{submit: submit, done: make([]chan struct{}, k), stop: make(chan struct{})}
	for i := range c.done {
		c.done[i] = make(chan struct{}, 1)
	}
	app.SetOnDeliver(func(client, _ uint64, _ time.Duration) {
		if client == 0 || client > uint64(k) {
			return
		}
		select {
		case c.done[client-1] <- struct{}{}:
		default:
		}
	})
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
	var timer *time.Timer
	if c.ResubmitAfter > 0 {
		timer = time.NewTimer(c.ResubmitAfter)
		defer timer.Stop()
	}
	for seq := uint64(1); ; seq++ {
		select { // a late duplicate delivery of the previous request must not release this one
		case <-done:
		default:
		}
		req := EncodeRequest(client, seq, time.Now())
		for attempt := 0; ; attempt++ {
			for {
				err := c.submit(client, attempt, req)
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
			if timer == nil {
				select {
				case <-done:
				case <-c.stop:
					return
				}
				break
			}
			timer.Reset(c.ResubmitAfter)
			select {
			case <-done:
				if !timer.Stop() {
					<-timer.C
				}
			case <-c.stop:
				return
			case <-timer.C:
				c.Resubmits.Add(1)
				continue
			}
			break
		}
	}
}
