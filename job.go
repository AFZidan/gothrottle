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
}

// PriorityQueue implements heap.Interface and holds Jobs.
type PriorityQueue []*Job

func (pq PriorityQueue) Len() int { return len(pq) }

func (pq PriorityQueue) Less(i, j int) bool {
	// Higher priority values have higher priority (max heap)
	return pq[i].Priority > pq[j].Priority
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

// IsEmpty returns true if the queue is empty.
func (pq *PriorityQueue) IsEmpty() bool {
	return pq.Len() == 0
}
