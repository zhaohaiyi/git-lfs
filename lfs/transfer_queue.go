package lfs

import (
	"sync"

	"github.com/github/git-lfs/api"
	"github.com/github/git-lfs/config"
	"github.com/github/git-lfs/errors"
	"github.com/github/git-lfs/git"
	"github.com/github/git-lfs/progress"
	"github.com/github/git-lfs/transfer"
	"github.com/rubyist/tracerx"
)

const (
	batchSize         = 100
	defaultMaxRetries = 1
)

type Transferable interface {
	Oid() string
	Size() int64
	Name() string
	Path() string
	Object() *api.ObjectResource
	SetObject(*api.ObjectResource)
	// Legacy API check - TODO remove this and only support batch
	LegacyCheck() (*api.ObjectResource, error)
}

// TransferQueue organises the wider process of uploading and downloading,
// including calling the API, passing the actual transfer request to transfer
// adapters, and dealing with progress, errors and retries.
type TransferQueue struct {
	direction         transfer.Direction
	adapter           transfer.TransferAdapter
	adapterInProgress bool
	adapterResultChan chan transfer.TransferResult
	adapterInitMutex  sync.Mutex
	dryRun            bool
	meter             *progress.ProgressMeter
	errors            []error
	transferables     map[string]Transferable
	batcher           *Batcher
	apic              chan Transferable // Channel for processing individual API requests
	retriesc          chan Transferable // Channel for processing retries
	errorc            chan error        // Channel for processing errors
	watchers          []chan string
	trMutex           *sync.Mutex
	errorwait         sync.WaitGroup
	retrywait         sync.WaitGroup
	// wait is used to keep track of pending transfers. It is incremented
	// once per unique OID on Add(), and is decremented when that transfer
	// is marked as completed or failed, but not retried.
	wait          sync.WaitGroup
	oldApiWorkers int // Number of non-batch API workers to spawn (deprecated)
	manifest      *transfer.Manifest
	rmu           sync.Mutex        // rmu guards retryCount
	retryCount    map[string]uint32 // maps OIDs to number of retry attempts
	// maxRetries is the maximum number of retries a single object can
	// attempt to make before it will be dropped.
	maxRetries uint32
}

// newTransferQueue builds a TransferQueue, direction and underlying mechanism determined by adapter
func newTransferQueue(files int, size int64, dryRun bool, dir transfer.Direction) *TransferQueue {
	logPath, _ := config.Config.Os.Get("GIT_LFS_PROGRESS")

	q := &TransferQueue{
		direction:     dir,
		dryRun:        dryRun,
		meter:         progress.NewProgressMeter(files, size, dryRun, logPath),
		apic:          make(chan Transferable, batchSize),
		retriesc:      make(chan Transferable, batchSize),
		errorc:        make(chan error),
		oldApiWorkers: config.Config.ConcurrentTransfers(),
		transferables: make(map[string]Transferable),
		trMutex:       &sync.Mutex{},
		manifest:      transfer.ConfigureManifest(transfer.NewManifest(), config.Config),
		retryCount:    make(map[string]uint32),
		maxRetries:    defaultMaxRetries,
	}

	q.errorwait.Add(1)
	q.retrywait.Add(1)

	q.run()

	return q
}

// Add adds a Transferable to the transfer queue. It only increments the amount
// of waiting the TransferQueue has to do if the Transferable "t" is new.
func (q *TransferQueue) Add(t Transferable) {
	q.trMutex.Lock()
	if _, ok := q.transferables[t.Oid()]; !ok {
		q.wait.Add(1)
		q.transferables[t.Oid()] = t
	}
	q.trMutex.Unlock()

	if q.batcher != nil {
		q.batcher.Add(t)
		return
	}

	q.apic <- t
}

func (q *TransferQueue) useAdapter(name string) {
	q.adapterInitMutex.Lock()
	defer q.adapterInitMutex.Unlock()

	if q.adapter != nil {
		if q.adapter.Name() == name {
			// re-use, this is the normal path
			return
		}
		// If the adapter we're using isn't the same as the one we've been
		// told to use now, must wait for the current one to finish then switch
		// This will probably never happen but is just in case server starts
		// changing adapter support in between batches
		q.finishAdapter()
	}
	q.adapter = q.manifest.NewAdapterOrDefault(name, q.direction)
}

func (q *TransferQueue) finishAdapter() {
	if q.adapterInProgress {
		q.adapter.End()
		q.adapterInProgress = false
		q.adapter = nil
	}
}

func (q *TransferQueue) addToAdapter(t Transferable) {
	tr := transfer.NewTransfer(t.Name(), t.Object(), t.Path())

	if q.dryRun {
		// Don't actually transfer
		res := transfer.TransferResult{tr, nil}
		q.handleTransferResult(res)
		return
	}
	err := q.ensureAdapterBegun()
	if err != nil {
		q.errorc <- err
		q.Skip(t.Size())
		q.wait.Done()
		return
	}
	q.adapter.Add(tr)
}

func (q *TransferQueue) Skip(size int64) {
	q.meter.Skip(size)
}

func (q *TransferQueue) transferKind() string {
	if q.direction == transfer.Download {
		return "download"
	} else {
		return "upload"
	}
}

func (q *TransferQueue) ensureAdapterBegun() error {
	q.adapterInitMutex.Lock()
	defer q.adapterInitMutex.Unlock()

	if q.adapterInProgress {
		return nil
	}

	adapterResultChan := make(chan transfer.TransferResult, 20)

	// Progress callback - receives byte updates
	cb := func(name string, total, read int64, current int) error {
		q.meter.TransferBytes(q.transferKind(), name, read, total, current)
		return nil
	}

	tracerx.Printf("tq: starting transfer adapter %q", q.adapter.Name())
	err := q.adapter.Begin(config.Config.ConcurrentTransfers(), cb, adapterResultChan)
	if err != nil {
		return err
	}
	q.adapterInProgress = true

	// Collector for completed transfers
	// q.wait.Done() in handleTransferResult is enough to know when this is complete for all transfers
	go func() {
		for res := range adapterResultChan {
			q.handleTransferResult(res)
		}
	}()

	return nil
}

// handleTransferResult is responsible for dealing with the result of a
// successful or failed transfer.
//
// If there was an error assosicated with the given transfer, "res.Error", and
// it is retriable (see: `q.canRetryObject`), it will be placed in the next
// batch and be retried. If that error is not retriable for any reason, the
// transfer will be marked as having failed, and the error will be reported.
//
// If the transfer was successful, the watchers of this transfer queue will be
// notified, and the transfer will be marked as having been completed.
func (q *TransferQueue) handleTransferResult(res transfer.TransferResult) {
	oid := res.Transfer.Object.Oid

	if res.Error != nil {
		if q.canRetryObject(oid, res.Error) {
			tracerx.Printf("tq: retrying object %s", oid)
			q.trMutex.Lock()
			t, ok := q.transferables[oid]
			q.trMutex.Unlock()
			if ok {
				q.retry(t)
			} else {
				q.errorc <- res.Error
			}
		} else {
			q.errorc <- res.Error
			q.wait.Done()
		}
	} else {
		for _, c := range q.watchers {
			c <- oid
		}

		q.meter.FinishTransfer(res.Transfer.Name)
		q.wait.Done()
	}
}

// Wait waits for the queue to finish processing all transfers. Once Wait is
// called, Add will no longer add transferables to the queue. Any failed
// transfers will be automatically retried once.
func (q *TransferQueue) Wait() {
	if q.batcher != nil {
		q.batcher.Exit()
	}

	q.wait.Wait()

	// Handle any retries
	close(q.retriesc)
	q.retrywait.Wait()

	close(q.apic)
	q.finishAdapter()
	close(q.errorc)

	for _, watcher := range q.watchers {
		close(watcher)
	}

	q.meter.Finish()
	q.errorwait.Wait()
}

// Watch returns a channel where the queue will write the OID of each transfer
// as it completes. The channel will be closed when the queue finishes processing.
func (q *TransferQueue) Watch() chan string {
	c := make(chan string, batchSize)
	q.watchers = append(q.watchers, c)
	return c
}

// individualApiRoutine processes the queue of transfers one at a time by making
// a POST call for each object, feeding the results to the transfer workers.
// If configured, the object transfers can still happen concurrently, the
// sequential nature here is only for the meta POST calls.
// TODO LEGACY API: remove when legacy API removed
func (q *TransferQueue) individualApiRoutine(apiWaiter chan interface{}) {
	for t := range q.apic {
		obj, err := t.LegacyCheck()
		if err != nil {
			if q.canRetryObject(obj.Oid, err) {
				q.retry(t)
			} else {
				q.errorc <- err
				q.wait.Done()
			}
			continue
		}

		if apiWaiter != nil { // Signal to launch more individual api workers
			q.meter.Start()
			select {
			case apiWaiter <- 1:
			default:
			}
		}

		// Legacy API has no support for anything but basic transfer adapter
		q.useAdapter(transfer.BasicAdapterName)
		if obj != nil {
			t.SetObject(obj)
			q.meter.Add(t.Name())
			q.addToAdapter(t)
		} else {
			q.Skip(t.Size())
			q.wait.Done()
		}
	}
}

// legacyFallback is used when a batch request is made to a server that does
// not support the batch endpoint. When this happens, the Transferables are
// fed from the batcher into apic to be processed individually.
// TODO LEGACY API: remove when legacy API removed
func (q *TransferQueue) legacyFallback(failedBatch []interface{}) {
	tracerx.Printf("tq: batch api not implemented, falling back to individual")

	q.launchIndividualApiRoutines()

	for _, t := range failedBatch {
		q.apic <- t.(Transferable)
	}

	for {
		batch := q.batcher.Next()
		if batch == nil {
			break
		}

		for _, t := range batch {
			q.apic <- t.(Transferable)
		}
	}
}

// batchApiRoutine processes the queue of transfers using the batch endpoint,
// making only one POST call for all objects. The results are then handed
// off to the transfer workers.
func (q *TransferQueue) batchApiRoutine() {
	var startProgress sync.Once

	transferAdapterNames := q.manifest.GetAdapterNames(q.direction)

	for {
		batch := q.batcher.Next()
		if batch == nil {
			break
		}

		tracerx.Printf("tq: sending batch of size %d", len(batch))

		transfers := make([]*api.ObjectResource, 0, len(batch))
		for _, i := range batch {
			t := i.(Transferable)
			transfers = append(transfers, &api.ObjectResource{Oid: t.Oid(), Size: t.Size()})
		}

		if len(transfers) == 0 {
			continue
		}

		objs, adapterName, err := api.Batch(config.Config, transfers, q.transferKind(), transferAdapterNames)
		if err != nil {
			if errors.IsNotImplementedError(err) {
				git.Config.SetLocal("", "lfs.batch", "false")
				go q.legacyFallback(batch)
				return
			}

			var errOnce sync.Once
			for _, o := range batch {
				t := o.(Transferable)

				if q.canRetryObject(t.Oid(), err) {
					q.retry(t)
				} else {
					q.wait.Done()
					errOnce.Do(func() { q.errorc <- err })
				}
			}

			continue
		}

		q.useAdapter(adapterName)
		startProgress.Do(q.meter.Start)

		for _, o := range objs {
			if o.Error != nil {
				q.errorc <- errors.Wrapf(o.Error, "[%v] %v", o.Oid, o.Error.Message)
				q.Skip(o.Size)
				q.wait.Done()
				continue
			}

			if _, ok := o.Rel(q.transferKind()); ok {
				// This object needs to be transferred
				q.trMutex.Lock()
				transfer, ok := q.transferables[o.Oid]
				q.trMutex.Unlock()

				if ok {
					transfer.SetObject(o)
					q.meter.Add(transfer.Name())
					q.addToAdapter(transfer)
				} else {
					q.Skip(transfer.Size())
					q.wait.Done()
				}
			} else {
				q.Skip(o.Size)
				q.wait.Done()
			}
		}
	}
}

// This goroutine collects errors returned from transfers
func (q *TransferQueue) errorCollector() {
	for err := range q.errorc {
		q.errors = append(q.errors, err)
	}
	q.errorwait.Done()
}

// retryCollector collects objects to retry, increments the number of times that
// they have been retried, and then enqueues them in the next batch, or legacy
// API channel. If the transfer queue is using a batcher, the batch will be
// flushed immediately.
//
// retryCollector runs in its own goroutine.
func (q *TransferQueue) retryCollector() {
	for t := range q.retriesc {
		q.rmu.Lock()
		q.retryCount[t.Oid()]++
		count := q.retryCount[t.Oid()]
		q.rmu.Unlock()

		tracerx.Printf("tq: enqueue retry #%d for %q (size: %d)", count, t.Oid(), t.Size())

		q.Add(t)
		if q.batcher != nil {
			tracerx.Printf("tq: flushing batch in response to retry #%d for %q", count, t.Oid(), t.Size())
			q.batcher.Flush()
		}
	}
	q.retrywait.Done()
}

// launchIndividualApiRoutines first launches a single api worker. When it
// receives the first successful api request it launches workers - 1 more
// workers. This prevents being prompted for credentials multiple times at once
// when they're needed.
func (q *TransferQueue) launchIndividualApiRoutines() {
	go func() {
		apiWaiter := make(chan interface{})
		go q.individualApiRoutine(apiWaiter)

		<-apiWaiter

		for i := 0; i < q.oldApiWorkers-1; i++ {
			go q.individualApiRoutine(nil)
		}
	}()
}

// run starts the transfer queue, doing individual or batch transfers depending
// on the Config.BatchTransfer() value. run will transfer files sequentially or
// concurrently depending on the Config.ConcurrentTransfers() value.
func (q *TransferQueue) run() {
	go q.errorCollector()
	go q.retryCollector()

	if config.Config.BatchTransfer() {
		tracerx.Printf("tq: running as batched queue, batch size of %d", batchSize)
		q.batcher = NewBatcher(batchSize)
		go q.batchApiRoutine()
	} else {
		tracerx.Printf("tq: running as individual queue")
		q.launchIndividualApiRoutines()
	}
}

func (q *TransferQueue) retry(t Transferable) {
	q.retriesc <- t
}

// canRetry returns whether or not the given error "err" is retriable.
func (q *TransferQueue) canRetry(err error) bool {
	return errors.IsRetriableError(err)
}

// canRetryObject returns whether the given error is retriable for the object
// given by "oid". If the an OID has met its retry limit, then it will not be
// able to be retried again. If so, canRetryObject returns whether or not that
// given error "err" is retriable.
func (q *TransferQueue) canRetryObject(oid string, err error) bool {
	q.rmu.Lock()
	count := q.retryCount[oid]
	q.rmu.Unlock()

	if count > q.maxRetries {
		tracerx.Printf("tq: refusing to retry %q, too many retries (%d)", oid, count)
		return false
	}

	return q.canRetry(err)
}

// Errors returns any errors encountered during transfer.
func (q *TransferQueue) Errors() []error {
	return q.errors
}
