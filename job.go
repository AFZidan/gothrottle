// FILENAME: job.go
package gothrottle

import (
	"container/heap"
)

// Job represents a function to be executed by the Limiter.
type Job struct {
	Task     func() (interface{}, error)
	Priority int
	Weight   int

	// Internal fields for returning results
	resultChan chan interface{}
	errorChan  chan error
	index      int

	// seq orders jobs of equal priority. It is assigned when the job is
	// enqueued so equal-priority work runs first-in-first-out; container/heap
	// is not stable, so without it equal priorities run in arbitrary order.
	seq uint64

	// lease is the capacity reservation this job holds, when the datastore
	// tracks individual leases. Releasing by lease token means a late release
	// from an expired job cannot disturb a newer job's reservation.
	lease *Lease
}

// PriorityQueue implements heap.Interface and holds Jobs.
type PriorityQueue []*Job

func (pq PriorityQueue) Len() int { return len(pq) }

func (pq PriorityQueue) Less(i, j int) bool {
	return jobBefore(pq[i], pq[j])
}

func (pq PriorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

func (pq *PriorityQueue) Push(x interface{}) {
	n := len(*pq)
	item := x.(*Job)
	item.index = n
	*pq = append(*pq, item)
}

func (pq *PriorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	item := old[n-1]
	old[n-1] = nil  // avoid memory leak
	item.index = -1 // for safety
	*pq = old[0 : n-1]
	return item
}

// NewPriorityQueue creates a new priority queue.
func NewPriorityQueue() *PriorityQueue {
	pq := &PriorityQueue{}
	heap.Init(pq)
	return pq
}

// PushJob adds a job to the priority queue.
func (pq *PriorityQueue) PushJob(job *Job) {
	heap.Push(pq, job)
}

// PopJob removes and returns the highest priority job.
func (pq *PriorityQueue) PopJob() *Job {
	if pq.Len() == 0 {
		return nil
	}
	return heap.Pop(pq).(*Job)
}

// Peek returns the highest priority job without removing it, or nil when the
// queue is empty.
func (pq *PriorityQueue) Peek() *Job {
	if pq.Len() == 0 {
		return nil
	}
	return (*pq)[0]
}

// peekFit returns the highest priority job whose weight does not exceed
// maxWeight, or nil when no queued job is light enough. It lets the scheduler
// look past a heavy head job that cannot fit in the remaining capacity.
func (pq *PriorityQueue) peekFit(maxWeight int) *Job {
	var best *Job
	for _, job := range *pq {
		if job.Weight > maxWeight {
			continue
		}
		if best == nil || jobBefore(job, best) {
			best = job
		}
	}
	return best
}

// jobBefore reports whether a would be dispatched before b: higher priority
// values first (max heap), then FIFO within a priority.
func jobBefore(a, b *Job) bool {
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	return a.seq < b.seq
}

// Remove takes a specific job out of the queue. It reports whether the job was
// still queued, which lets a caller distinguish "cancelled before it ran" from
// "already dispatched".
func (pq *PriorityQueue) Remove(job *Job) bool {
	if job == nil || job.index < 0 || job.index >= pq.Len() {
		return false
	}
	if (*pq)[job.index] != job {
		return false
	}
	heap.Remove(pq, job.index)
	return true
}

// IsEmpty returns true if the queue is empty.
func (pq *PriorityQueue) IsEmpty() bool {
	return pq.Len() == 0
}
