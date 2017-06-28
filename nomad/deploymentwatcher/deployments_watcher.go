package deploymentwatcher

import (
	"context"
	"fmt"
	"log"
	"sync"

	"golang.org/x/time/rate"

	"github.com/hashicorp/nomad/nomad/structs"
)

// DeploymentRaftEndpoints exposes the deployment watcher to a set of functions
// to apply data transforms via Raft.
type DeploymentRaftEndpoints interface {
	// UpsertEvals is used to upsert a set of evaluations
	UpsertEvals([]*structs.Evaluation) (uint64, error)

	// UpsertJob is used to upsert a job
	UpsertJob(job *structs.Job) (uint64, error)

	// UpsertDeploymentStatusUpdate is used to upsert a deployment status update
	// and potentially create an evaluation.
	UpsertDeploymentStatusUpdate(u *structs.DeploymentStatusUpdateRequest) (uint64, error)

	// UpsertDeploymentPromotion is used to promote canaries in a deployment
	UpsertDeploymentPromotion(req *structs.ApplyDeploymentPromoteRequest) (uint64, error)

	// UpsertDeploymentAllocHealth is used to set the health of allocations in a
	// deployment
	UpsertDeploymentAllocHealth(req *structs.ApplyDeploymentAllocHealthRequest) (uint64, error)
}

// DeploymentStateWatchers are the set of functions required to watch objects on
// behalf of a deployment
type DeploymentStateWatchers interface {
	// Evaluations returns the set of evaluations for the given job
	Evaluations(args *structs.JobSpecificRequest, reply *structs.JobEvaluationsResponse) error

	// Allocations returns the set of allocations that are part of the
	// deployment.
	Allocations(args *structs.DeploymentSpecificRequest, reply *structs.AllocListResponse) error

	// List is used to list all the deployments in the system
	List(args *structs.DeploymentListRequest, reply *structs.DeploymentListResponse) error

	// GetJobVersions is used to lookup the versions of a job. This is used when
	// rolling back to find the latest stable job
	GetJobVersions(args *structs.JobSpecificRequest, reply *structs.JobVersionsResponse) error

	// GetJob is used to lookup a particular job.
	GetJob(args *structs.JobSpecificRequest, reply *structs.SingleJobResponse) error
}

const (
	// limitStateQueriesPerSecond is the number of state queries allowed per
	// second
	limitStateQueriesPerSecond = 15.0
)

// Watcher is used to watch deployments and their allocations created
// by the scheduler and trigger the scheduler when allocation health
// transistions.
type Watcher struct {
	enabled bool
	logger  *log.Logger

	// queryLimiter is used to limit the rate of blocking queries
	queryLimiter *rate.Limiter

	// raft contains the set of Raft endpoints that can be used by the
	// deployments watcher
	raft DeploymentRaftEndpoints

	// stateWatchers is the set of functions required to watch a deployment for
	// state changes
	stateWatchers DeploymentStateWatchers

	// watchers is the set of active watchers, one per deployment
	watchers map[string]*deploymentWatcher

	// evalBatcher is used to batch the creation of evaluations
	evalBatcher *EvalBatcher

	// ctx and exitFn are used to cancel the watcher
	ctx    context.Context
	exitFn context.CancelFunc

	l sync.RWMutex
}

// NewDeploymentsWatcher returns a deployments watcher that is used to watch
// deployments and trigger the scheduler as needed.
func NewDeploymentsWatcher(logger *log.Logger, w DeploymentStateWatchers, raft DeploymentRaftEndpoints) *Watcher {
	ctx, exitFn := context.WithCancel(context.Background())
	return &Watcher{
		queryLimiter:  rate.NewLimiter(limitStateQueriesPerSecond, 100),
		stateWatchers: w,
		raft:          raft,
		watchers:      make(map[string]*deploymentWatcher, 32),
		evalBatcher:   NewEvalBatcher(raft, ctx),
		logger:        logger,
		ctx:           ctx,
		exitFn:        exitFn,
	}
}

// SetEnabled is used to control if the watcher is enabled. The watcher
// should only be enabled on the active leader.
func (w *Watcher) SetEnabled(enabled bool) {
	w.l.Lock()
	wasEnabled := w.enabled
	w.enabled = enabled
	w.l.Unlock()
	if !enabled {
		w.Flush()
	} else if !wasEnabled {
		// Start the watcher if we are transistioning to an enabled state
		go w.watchDeployments()
	}
}

// Flush is used to clear the state of the watcher
func (w *Watcher) Flush() {
	w.l.Lock()
	defer w.l.Unlock()

	// Stop all the watchers and clear it
	for _, watcher := range w.watchers {
		watcher.StopWatch()
	}

	// Kill everything associated with the watcher
	w.exitFn()

	w.watchers = make(map[string]*deploymentWatcher, 32)
	w.ctx, w.exitFn = context.WithCancel(context.Background())
	w.evalBatcher = NewEvalBatcher(w.raft, w.ctx)
}

// watchDeployments is the long lived go-routine that watches for deployments to
// add and remove watchers on.
func (w *Watcher) watchDeployments() {
	dindex := uint64(0)
	for {
		// Block getting all deployments using the last deployment index.
		resp, err := w.getDeploys(dindex)
		if err != nil {
			if err == context.Canceled {
				return
			}

			w.logger.Printf("[ERR] nomad.deployments_watcher: failed to retrieve deploylements: %v", err)
		}

		// Guard against npe
		if resp == nil {
			continue
		}

		// Ensure we are tracking the things we should and not tracking what we
		// shouldn't be
		for _, d := range resp.Deployments {
			if d.Active() {
				if err := w.add(d); err != nil {
					w.logger.Printf("[ERR] nomad.deployments_watcher: failed to track deployment %q: %v", d.ID, err)
				}
			} else {
				w.remove(d)
			}
		}

		// Update the latest index
		dindex = resp.Index
	}
}

// getDeploys retrieves all deployments blocking at the given index.
func (w *Watcher) getDeploys(index uint64) (*structs.DeploymentListResponse, error) {
	// Build the request
	args := &structs.DeploymentListRequest{
		QueryOptions: structs.QueryOptions{
			MinQueryIndex: index,
		},
	}
	var resp structs.DeploymentListResponse

	for resp.Index <= index {
		if err := w.queryLimiter.Wait(w.ctx); err != nil {
			return nil, err
		}

		if err := w.stateWatchers.List(args, &resp); err != nil {
			return nil, err
		}
	}

	return &resp, nil
}

// add adds a deployment to the watch list
func (w *Watcher) add(d *structs.Deployment) error {
	w.l.Lock()
	defer w.l.Unlock()

	// Not enabled so no-op
	if !w.enabled {
		return nil
	}

	// Already watched so no-op
	if _, ok := w.watchers[d.ID]; ok {
		return nil
	}

	// Get the job the deployment is referencing
	args := &structs.JobSpecificRequest{
		JobID: d.JobID,
	}
	var resp structs.SingleJobResponse
	if err := w.stateWatchers.GetJob(args, &resp); err != nil {
		return err
	}
	if resp.Job == nil {
		return fmt.Errorf("deployment %q references unknown job %q", d.ID, d.JobID)
	}

	w.watchers[d.ID] = newDeploymentWatcher(w.ctx, w.queryLimiter, w.logger, w.stateWatchers, d, resp.Job, w)
	w.logger.Printf("[TRACE] nomad.deployments_watcher: tracking deployment %q", d.ID)
	return nil
}

// remove stops watching a deployment. This can be because the deployment is
// complete or being deleted.
func (w *Watcher) remove(d *structs.Deployment) {
	w.l.Lock()
	defer w.l.Unlock()

	// Not enabled so no-op
	if !w.enabled {
		return
	}

	if watcher, ok := w.watchers[d.ID]; ok {
		watcher.StopWatch()
		delete(w.watchers, d.ID)
		w.logger.Printf("[TRACE] nomad.deployments_watcher: untracking deployment %q", d.ID)
	}
}

// SetAllocHealth is used to set the health of allocations for a deployment. If
// there are any unhealthy allocations, the deployment is updated to be failed.
// Otherwise the allocations are updated and an evaluation is created.
func (w *Watcher) SetAllocHealth(req *structs.DeploymentAllocHealthRequest, resp *structs.DeploymentUpdateResponse) error {
	w.l.Lock()
	defer w.l.Unlock()

	// Not enabled so no-op
	if !w.enabled {
		return nil
	}

	watcher, ok := w.watchers[req.DeploymentID]
	if !ok {
		return fmt.Errorf("deployment %q not being watched for updates", req.DeploymentID)
	}

	return watcher.SetAllocHealth(req, resp)
}

// PromoteDeployment is used to promote a deployment. If promote is false,
// deployment is marked as failed. Otherwise the deployment is updated and an
// evaluation is created.
func (w *Watcher) PromoteDeployment(req *structs.DeploymentPromoteRequest, resp *structs.DeploymentUpdateResponse) error {
	w.l.Lock()
	defer w.l.Unlock()

	// Not enabled so no-op
	if !w.enabled {
		return nil
	}

	watcher, ok := w.watchers[req.DeploymentID]
	if !ok {
		return fmt.Errorf("deployment %q not being watched for updates", req.DeploymentID)
	}

	return watcher.PromoteDeployment(req, resp)
}

// PauseDeployment is used to toggle the pause state on a deployment. If the
// deployment is being unpaused, an evaluation is created.
func (w *Watcher) PauseDeployment(req *structs.DeploymentPauseRequest, resp *structs.DeploymentUpdateResponse) error {
	w.l.Lock()
	defer w.l.Unlock()

	// Not enabled so no-op
	if !w.enabled {
		return nil
	}

	watcher, ok := w.watchers[req.DeploymentID]
	if !ok {
		return fmt.Errorf("deployment %q not being watched for updates", req.DeploymentID)
	}

	return watcher.PauseDeployment(req, resp)
}

// createEvaluation commits the given evaluation to Raft but batches the commit
// with other calls.
func (w *Watcher) createEvaluation(eval *structs.Evaluation) (uint64, error) {
	w.l.Lock()
	f := w.evalBatcher.CreateEval(eval)
	w.l.Unlock()

	return f.Results()
}

// upsertJob commits the given job to Raft
func (w *Watcher) upsertJob(job *structs.Job) (uint64, error) {
	return w.raft.UpsertJob(job)
}

// upsertDeploymentStatusUpdate commits the given deployment update and optional
// evaluation to Raft
func (w *Watcher) upsertDeploymentStatusUpdate(
	u *structs.DeploymentStatusUpdate,
	e *structs.Evaluation,
	j *structs.Job) (uint64, error) {
	return w.raft.UpsertDeploymentStatusUpdate(&structs.DeploymentStatusUpdateRequest{
		DeploymentUpdate: u,
		Eval:             e,
		Job:              j,
	})
}

// upsertDeploymentPromotion commits the given deployment promotion to Raft
func (w *Watcher) upsertDeploymentPromotion(req *structs.ApplyDeploymentPromoteRequest) (uint64, error) {
	return w.raft.UpsertDeploymentPromotion(req)
}

// upsertDeploymentAllocHealth commits the given allocation health changes to
// Raft
func (w *Watcher) upsertDeploymentAllocHealth(req *structs.ApplyDeploymentAllocHealthRequest) (uint64, error) {
	return w.raft.UpsertDeploymentAllocHealth(req)
}
