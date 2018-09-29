// MIT License

// Copyright (c) 2018 Andy Pan

// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package ants

import (
	"sync"
	"sync/atomic"
	"time"
	"strconv"
)

type sig struct{}

type Job struct {
	Tag string
	F   f
}

type Runner interface {
	Work()
}

type f func()

// Pool accept the tasks from client,it limits the total
// of goroutines to a given number by recycling goroutines.
type Pool struct {
	// capacity of the pool.
	capacity int32

	// running is the number of the currently running goroutines.
	running int32

	// expiryDuration set the expired time (second) of every worker.
	expiryDuration time.Duration

	// workers is a slice that store the available workers.
	workers []*Worker

	// release is used to notice the pool to closed itself.
	release chan sig

	// lock for synchronous operation.
	lock sync.Mutex

	once sync.Once

	panicHandlers []PanicRecover

	jobs sync.Map
}

// clear expired workers periodically.
func (p *Pool) periodicallyPurge() {
	heartbeat := time.NewTicker(p.expiryDuration)
	for range heartbeat.C {
		currentTime := time.Now()
		p.lock.Lock()
		idleWorkers := p.workers
		if len(idleWorkers) == 0 && p.RunningJobCount() == 0 && len(p.release) > 0 {
			p.lock.Unlock()
			return
		}
		n := -1
		for i, w := range idleWorkers {
			if currentTime.Sub(w.recycleTime) <= p.expiryDuration {
				break
			}
			n = i
			w.task <- nil
			idleWorkers[i] = nil
		}
		if n > -1 {
			if n >= len(idleWorkers)-1 {
				p.workers = idleWorkers[:0]
			} else {
				p.workers = idleWorkers[n+1:]
			}
		}
		p.lock.Unlock()
	}
}

func initPool() *Pool {
	return &Pool{
		release:       make(chan sig, 1),
		panicHandlers: make([]PanicRecover, 0),
	}
}

// NewPool generates an instance of ants pool. (固定 Size 的 Pool)
func NewPool(size int) (*Pool, error) {
	if size <= 0 {
		return nil, ErrInvalidPoolSize
	}

	p := initPool()
	p.capacity = int32(size)

	return p, nil
}

// NewTimingPool generates an instance of ants pool with a custom timed task. (如果协程一定时间没有任务则杀掉自己)
func NewTimingPool(size, expiry int) (*Pool, error) {
	if size <= 0 {
		return nil, ErrInvalidPoolSize
	}
	if expiry <= 0 {
		return nil, ErrInvalidPoolExpiry
	}

	p := initPool()
	p.capacity = int32(size)
	p.expiryDuration = time.Duration(expiry) * time.Second

	go p.periodicallyPurge()
	return p, nil
}

// -------------------------------------------------------------------------

func (p *Pool) AppendPanicHandler(recover PanicRecover) {
	p.panicHandlers = append(p.panicHandlers, recover)
}

// Submit submits a task to this pool.
func (p *Pool) Submit(task f, tag ...string) error {
	if len(p.release) > 0 {
		return ErrPoolClosed
	}

	jobTag := "Job" + strconv.Itoa(p.RunningJobCount())
	if len(tag) > 0 {
		jobTag = tag[0]
	}

	newJob := &Job{
		Tag: jobTag,
		F:   task,
	}

	p.getWorker().task <- newJob
	p.jobs.Store(jobTag, task)
	return nil
}

func (p *Pool) Runnable(r Runner) error {
	return p.Submit(r.Work)
}

// Running returns the number of the currently running goroutines.
func (p *Pool) RunningJobCount() int {
	return int(atomic.LoadInt32(&p.running))
}

// Free returns the available goroutines to work.
func (p *Pool) FreeJobCount() int {
	return int(atomic.LoadInt32(&p.capacity) - atomic.LoadInt32(&p.running))
}

// Cap returns the capacity of this pool.
func (p *Pool) Cap() int {
	return int(atomic.LoadInt32(&p.capacity))
}

// ReSize changes the capacity of this pool.
func (p *Pool) ReSize(size int) {
	if size == p.Cap() {
		return
	}
	atomic.StoreInt32(&p.capacity, int32(size))
	diff := p.RunningJobCount() - size
	if diff > 0 {
		for i := 0; i < diff; i++ {
			p.getWorker().task <- nil
		}
	}
}

// Release Closes this pool.
func (p *Pool) Release() error {
	p.once.Do(func() {
		p.release <- sig{}
		p.lock.Lock()
		idleWorkers := p.workers
		for i, w := range idleWorkers {
			w.task <- nil
			idleWorkers[i] = nil
		}
		p.workers = nil
		p.lock.Unlock()
	})
	return nil
}

// -------------------------------------------------------------------------

// incRunning increases the number of the currently running goroutines.
func (p *Pool) incRunning() {
	atomic.AddInt32(&p.running, 1)
}

// decRunning decreases the number of the currently running goroutines.
func (p *Pool) decRunning() {
	atomic.AddInt32(&p.running, -1)
}

// getWorker returns a available worker to run the tasks.
func (p *Pool) getWorker() *Worker {
	var w *Worker
	waiting := false

	p.lock.Lock()
	idleWorkers := p.workers
	n := len(idleWorkers) - 1
	if n < 0 {
		waiting = p.RunningJobCount() >= p.Cap()
	} else {
		w = idleWorkers[n]
		idleWorkers[n] = nil
		p.workers = idleWorkers[:n]
	}
	p.lock.Unlock()

	if waiting {
		for {
			p.lock.Lock()
			idleWorkers = p.workers
			l := len(idleWorkers) - 1
			if l < 0 {
				p.lock.Unlock()
				continue
			}
			w = idleWorkers[l]
			idleWorkers[l] = nil
			p.workers = idleWorkers[:l]
			p.lock.Unlock()
			break
		}
	} else if w == nil {
		w = &Worker{
			pool: p,
			task: make(chan *Job, 1),
		}
		w.run()
		p.incRunning()
	}
	return w
}

// putWorker puts a worker back into free pool, recycling the goroutines.
func (p *Pool) putWorker(worker *Worker) {
	worker.recycleTime = time.Now()
	p.DeleteJob(worker.Tag)

	p.lock.Lock()
	p.workers = append(p.workers, worker)
	p.lock.Unlock()
}

func (p *Pool) DeleteJob(tag string) {
	p.jobs.Delete(tag)
}
