package pinner

import (
	"context"
	"fmt"
	"github.com/application-research/estuary/pinner/types"
	"github.com/stretchr/testify/assert"
	"os"
	"sync"
	"testing"
	"time"
)

var countLock sync.Mutex

func onPinStatusUpdate(cont uint, location string, status types.PinningStatus) error {
	//fmt.Println("onPinStatusUpdate", status, cont)
	return nil
}
func newManager(count *int) *PinManager {

	_ = os.RemoveAll("/tmp/duplicateGuard")
	_ = os.RemoveAll("/tmp/pinQueue")

	return NewPinManager(
		func(ctx context.Context, op *PinningOperation, cb PinProgressCB) error {
			go cb(1)
			countLock.Lock()
			*count += 1
			countLock.Unlock()
			return nil
		}, onPinStatusUpdate, &PinManagerOpts{
			MaxActivePerUser: 30,
			QueueDataDir:     "/tmp/",
		})
}

func newManagerNoDelete(count *int) *PinManager {
	return NewPinManager(
		func(ctx context.Context, op *PinningOperation, cb PinProgressCB) error {
			go cb(1)
			countLock.Lock()
			*count += 1
			countLock.Unlock()
			return nil
		}, onPinStatusUpdate, &PinManagerOpts{
			MaxActivePerUser: 30,
			QueueDataDir:     "/tmp/",
		})
}

func newPinData(name string, userid int, contid int) PinningOperation {
	return PinningOperation{
		Name:   name,
		UserId: uint(userid),
		lk:     sync.Mutex{},
		ContId: uint(contid),
	}
}

var N = 20
var sleeptime time.Duration = 100

func TestSend1Pin1worker(t *testing.T) {
	//run 1 worker

	i := 1
	var count = 0
	mgr := newManager(&count)
	go mgr.Run(1)
	pin := newPinData("name"+fmt.Sprint(i), i, i)
	go mgr.Add(&pin)

	sleepWhileWork(mgr, 0)
	assert.Equal(t, 0, int(mgr.pinQueue.Length()), "first pin doesn't enter queue")
	assert.Equal(t, 1, count, "DoPin called once")
	mgr.closeQueueDataStructures()
}

func TestSend1Pin0workers(t *testing.T) {

	//run 0 workers
	i := 1
	var count = 0
	mgr := newManager(&count)
	go mgr.Run(0)
	pin := newPinData("name"+fmt.Sprint(i), i, i)
	go mgr.Add(&pin)

	sleepWhileWork(mgr, 0)
	assert.Equal(t, 0, mgr.PinQueueSize(), "first pin doesn't enter queue")
	assert.Equal(t, 0, count, "DoPin called once")
	mgr.closeQueueDataStructures()
}

func TestNUniqueNames(t *testing.T) {
	var count = 0
	mgr := newManager(&count)

	go mgr.Run(0)
	for i := 0; i < N; i++ {
		pin := newPinData("name"+fmt.Sprint(i), i, i)
		go mgr.Add(&pin)
	}
	sleepWhileWork(mgr, N-1)
	assert.Equal(t, N-1, mgr.PinQueueSize(), "queue should have N pins in it")
	mgr.closeQueueDataStructures()
}

func TestNUniqueNamesWorker(t *testing.T) {
	var count = 0
	mgr := newManager(&count)
	go mgr.Run(5)
	for i := 0; i < N; i++ {
		pin := newPinData("name"+fmt.Sprint(i), i, i)
		go mgr.Add(&pin)
	}
	sleepWhileWork(mgr, 0)
	assert.Equal(t, 0, mgr.PinQueueSize(), "queue should be empty")
	assert.Equal(t, N, count, "work done should be N")
	mgr.closeQueueDataStructures()
}

func TestNUniqueNamesSameUserWorker(t *testing.T) {

	var count = 0
	mgr := newManager(&count)

	for j := 0; j < N; j++ {

		for i := 0; i < N; i++ {
			pin := newPinData("name"+fmt.Sprint(i), i, i*N+j)
			go mgr.Add(&pin)
		}
	}

	go mgr.Run(1)

	sleepWhileWork(mgr, 0)
	assert.Equal(t, 0, mgr.PinQueueSize(), "queue should be empty")
	assert.LessOrEqual(t, N+1, count, "Should do  at least N work + 1 for the first item")
	assert.Greater(t, N*N+1, count, "Should less than N*N work + 1 for the first item")
	mgr.closeQueueDataStructures()
}

func TestNUniqueNamesSameUser(t *testing.T) {
	var count = 0
	mgr := newManager(&count)
	go mgr.Run(0)
	for j := 0; j < N; j++ {

		for i := 0; i < N; i++ {
			pin := newPinData("name"+fmt.Sprint(i), i, i)
			go mgr.Add(&pin)
		}
	}
	sleepWhileWork(mgr, N)
	assert.Equal(t, N, mgr.PinQueueSize(), "queue should have N pins in it")
	assert.Equal(t, count, 0, "no work done")
	mgr.closeQueueDataStructures()
}

func TestNDuplicateNamesWorker(t *testing.T) {
	var count = 0
	mgr := newManager(&count)

	pin := newPinData("name", 0, 0)
	go mgr.Add(&pin)
	time.Sleep(100 * time.Millisecond)

	for i := 0; i < N; i++ {
		pin := newPinData("name", 0, 0)
		go mgr.Add(&pin)
	}

	go mgr.Run(8)

	sleepWhileWork(mgr, 0)
	assert.Equal(t, 0, mgr.PinQueueSize(), "queue should have 0 pins in it")
	assert.Less(t, count, N, "work done should be less than N pins ")
	//with the way the chnnels works it's sometimes finishes the work before it gets added to the queue
	mgr.closeQueueDataStructures()

}
func TestNDuplicateNames(t *testing.T) {
	var count = 0
	mgr := newManager(&count)
	go mgr.Run(0)

	for i := 0; i < N; i++ {
		pin := newPinData("name", 0, 0)
		go mgr.Add(&pin)
	}
	sleepWhileWork(mgr, 1)
	assert.Equal(t, 1, mgr.PinQueueSize(), "queue should have only 1 pin in it")
	assert.Equal(t, 0, count, "no work")
	mgr.closeQueueDataStructures()
}

func TestNDuplicateNamesNDuplicateUsersNTimeWork5Workers(t *testing.T) {
	var count = 0
	mgr := newManager(&count)
	go mgr.Run(5)
	for k := 0; k < N; k++ {
		for j := 0; j < N; j++ {
			for i := 0; i < N; i++ {
				pin := newPinData("name"+fmt.Sprint(i), j, i*N+j)
				go mgr.Add(&pin)
			}
		}
	}
	sleepWhileWork(mgr, 0)
	assert.Equal(t, 0, mgr.PinQueueSize(), "queue should have 0 pins in it")
	assert.Greater(t, count, N*N, "work done should be greater than N*N")
	assert.Less(t, count, N*N*N, "work done should be less than N*N*N")
	mgr.closeQueueDataStructures()
}

func TestNDuplicateNamesNDuplicateUsersNTime(t *testing.T) {
	var count = 0
	mgr := newManager(&count)
	go mgr.Run(0)

	for i := 0; i < N; i++ {
		pin := newPinData("name"+fmt.Sprint(i), i, i)
		go mgr.Add(&pin)
	}

	sleepWhileWork(mgr, N-1)
	assert.Equal(t, N-1, mgr.PinQueueSize(), "queue should have N*N pins in it")
	assert.Equal(t, 0, count, "no work done")
	mgr.closeQueueDataStructures()
}

func TestNDuplicateNamesNDuplicateUsersNTimeWork(t *testing.T) {
	var count = 0
	mgr := newManager(&count)
	go mgr.Run(1)
	for k := 0; k < N; k++ {
		for j := 0; j < N; j++ {
			for i := 0; i < N; i++ {
				pin := newPinData("name"+fmt.Sprint(i), j, i*N+j)
				go mgr.Add(&pin)
			}
		}
	}
	sleepWhileWork(mgr, 0)
	assert.Equal(t, 0, mgr.PinQueueSize(), "queue should have 0 pins in it")
	assert.Greater(t, count, N*N, "work done should be greater than N*N")
	assert.Less(t, count, N*N*N, "work done should be less than N*N*N")
	mgr.closeQueueDataStructures()
}

func sleepWhileWork(mgr *PinManager, SIZE int) {
	for i := 0; i < N; i++ {
		time.Sleep(sleeptime * time.Millisecond)
		if mgr.PinQueueSize() == SIZE {
			time.Sleep(sleeptime * time.Millisecond)
			return
		}
	}
	fmt.Println("Waking up before finding correct result")
}

func TestNDuplicateNamesNDuplicateUsersNTimes(t *testing.T) {

	var count = 0
	mgr := newManager(&count)
	go mgr.Run(0)

	for k := 0; k < N; k++ {
		for j := 0; j < N; j++ {
			for i := 0; i < N; i++ {
				pin := newPinData("name"+fmt.Sprint(i), j, i)
				go mgr.Add(&pin)
			}
		}
	}

	sleepWhileWork(mgr, N)
	assert.Equal(t, N, mgr.PinQueueSize(), "queue should have N pins in it")
	assert.Equal(t, 0, count, "no work")
	mgr.closeQueueDataStructures()
}
func TestResumeQueue(t *testing.T) {

	var count = 0
	mgr := newManager(&count)
	go mgr.Run(0)
	for k := 0; k < N; k++ {
		for j := 0; j < N; j++ {
			for i := 0; i < N; i++ {
				pin := newPinData("name"+fmt.Sprint(i), j, j*N+i)
				go mgr.Add(&pin)
			}
		}
	}
	sleepWhileWork(mgr, N*N)
	assert.Equal(t, N*N, mgr.PinQueueSize(), "queue should have N pins in it")
	assert.Equal(t, 0, count, "no work")
	mgr.closeQueueDataStructures()
	time.Sleep(time.Second)

	mgr2 := newManagerNoDelete(&count)
	assert.Equal(t, N*N, mgr2.PinQueueSize(), "queue should have N pins in it")
	assert.Equal(t, 0, count, "no work")
	mgr2.closeQueueDataStructures()

	workermgr := newManagerNoDelete(&count)
	assert.Equal(t, N*N, workermgr.PinQueueSize(), "queue should have N pins in it")
	assert.Equal(t, 0, count, "no work")

	go workermgr.Run(N)
	sleepWhileWork(workermgr, 0)
	assert.Equal(t, 0, workermgr.PinQueueSize(), "queue should have no pins in it")
	assert.Equal(t, N*N, count, "all work should be done")

}

/*

test run that iterates above and makes the above code redundant.

func test_run(worker_count int, repeat_count int, user_id_count int, name_count int, t *testing.T) {

	var count int = 0
	var work_completed_count int = 0
	var queue_end_count int = repeat_count*user_id_count*name_count - 1 // total work done minus 1 because first entry is stored as "next" and doesnot enter queue
	if worker_count > 0 {
		work_completed_count = repeat_count * user_id_count * name_count
		queue_end_count = 0 // queue shoud be empty at end
		mgr := newManager(&count)
		go mgr.Run(worker_count)

		for k := 0; k < repeat_count; k++ {
			for j := 0; j < user_id_count; j++ {
				for i := 0; i < name_count; i++ {
					pin := newPinData("name"+fmt.Sprint(i), j)
					go mgr.Add(&pin)
				}
			}
		}
		time.Sleep(sleeptime * time.Millisecond)
		assert.Equal(t, queue_end_count, mgr.PinQueueSize(), "queue has wrong number of pins in it")
		assert.Equal(t, work_completed_count, count, "incorrect amount of work done")
	}

}

func TestPinMgr(t *testing.T) {
	t.Parallel() // marks TLog as capable of running in parallel with other tests

	for worker_count := 0; worker_count < N; worker_count++ {

		for repeat_count := 0; repeat_count < N; repeat_count++ {
			for user_id_count := 0; user_id_count < N; user_id_count++ {
				t.Run(fmt.Sprint("Test_%i_%i_%i_%i", worker_count, repeat_count, user_id_count, user_id_count), func(t *testing.T) {
					t.Parallel() // marks each test case as capable of running in parallel with each other
					test_run(worker_count, repeat_count, user_id_count, user_id_count, t)
				})
			}
		}
	}
}
*/
