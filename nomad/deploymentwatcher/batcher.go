package deploymentwatcher

import (
	"context"
	"sync"
	"time"

	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// evalBatchDuration is the duration in which evaluations are batched before
	// commiting to Raft.
	evalBatchDuration = 200 * time.Millisecond
)

// EvalBatcher is used to batch the creation of evaluations
type EvalBatcher struct {
	// raft is used to actually commit the evaluations
	raft DeploymentRaftEndpoints

	// future to be returned to callers
	f *EvalFuture

	// inCh is used to pass evaluations to the daemon process
	inCh chan *structs.Evaluation

	// ctx is used to exit the daemon batcher
	ctx context.Context

	l sync.Mutex
}

// NewEvalBatcher returns an EvalBatcher that uses the passed raft endpoints to
// create the evaluations and exits the batcher when the passed exit channel is
// closed.
func NewEvalBatcher(raft DeploymentRaftEndpoints, ctx context.Context) *EvalBatcher {
	b := &EvalBatcher{
		raft: raft,
		ctx:  ctx,
		inCh: make(chan *structs.Evaluation, 10),
	}

	go b.batcher()
	return b
}

// CreateEval batches the creation of the evaluation and returns a future that
// tracks the evaluations creation.
func (b *EvalBatcher) CreateEval(e *structs.Evaluation) *EvalFuture {
	b.l.Lock()
	defer b.l.Unlock()

	if b.f == nil {
		b.f = NewEvalFuture()
	}

	b.inCh <- e
	return b.f
}

// batcher is the long lived batcher goroutine
func (b *EvalBatcher) batcher() {
	ticker := time.NewTicker(evalBatchDuration)
	evals := make(map[string]*structs.Evaluation)
	for {
		select {
		case <-b.ctx.Done():
			ticker.Stop()
			return
		case e := <-b.inCh:
			evals[e.DeploymentID] = e
		case <-ticker.C:
			if len(evals) == 0 {
				continue
			}

			// Capture the future
			b.l.Lock()
			f := b.f
			b.f = nil
			b.l.Unlock()

			// Shouldn't be possible but protect ourselves
			if f == nil {
				continue
			}

			// Capture the evals
			all := make([]*structs.Evaluation, 0, len(evals))
			for _, e := range evals {
				all = append(all, e)
			}

			// Upsert the evals
			f.Set(b.raft.UpsertEvals(all))

			// Reset the evals list
			evals = make(map[string]*structs.Evaluation)
		}
	}
}

// EvalFuture is a future that can be used to retrieve the index the eval was
// created at or any error in the creation process
type EvalFuture struct {
	index  uint64
	err    error
	waitCh chan struct{}
}

// NewEvalFuture returns a new EvalFuture
func NewEvalFuture() *EvalFuture {
	return &EvalFuture{
		waitCh: make(chan struct{}),
	}
}

// Set sets the results of the future, unblocking any client.
func (f *EvalFuture) Set(index uint64, err error) {
	f.index = index
	f.err = err
	close(f.waitCh)
}

// Results returns the creation index and any error.
func (f *EvalFuture) Results() (uint64, error) {
	<-f.waitCh
	return f.index, f.err
}
