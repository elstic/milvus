package cache

import (
	"container/list"
	"fmt"
	"sync"
	"time"

	"go.uber.org/atomic"
	"golang.org/x/sync/singleflight"

	"github.com/milvus-io/milvus/pkg/util/merr"
)

var (
	ErrNoSuchItem     = merr.WrapErrServiceInternal("no such item")
	ErrNotEnoughSpace = merr.WrapErrServiceInternal("not enough space")
	ErrTimeOut        = merr.WrapErrServiceInternal("time out")
)

type cacheItem[K comparable, V any] struct {
	key      K
	value    V
	pinCount atomic.Int32
}

type (
	Loader[K comparable, V any]    func(key K) (V, bool)
	Finalizer[K comparable, V any] func(key K, value V) error
)

// Scavenger records occupation of cache and decide whether to evict if necessary.
//
//	The scavenger makes decision based on keys only, and it is called before value loading,
//	because value loading could be very expensive.
type Scavenger[K comparable] interface {
	// Collect records entry additions, if there is room, return true, or else return false and a collector.
	//	The collector is a function which can be invoked repetedly, each invocation will test if there is enough
	//	room provided that all entries in the collector is evicted. Typically, the collector will get multiple false
	//	before it gets a true.
	Collect(key K) (bool, func(K) bool)
	// Throw records entry removals.
	Throw(key K)
}

type LazyScavenger[K comparable] struct {
	capacity int64
	size     int64
	weight   func(K) int64
}

func NewLazyScavenger[K comparable](weight func(K) int64, capacity int64) *LazyScavenger[K] {
	return &LazyScavenger[K]{
		capacity: capacity,
		weight:   weight,
	}
}

func (s *LazyScavenger[K]) Collect(key K) (bool, func(K) bool) {
	w := s.weight(key)
	if s.size+w > s.capacity {
		needCollect := s.size + w - s.capacity
		return false, func(key K) bool {
			needCollect -= s.weight(key)
			return needCollect <= 0
		}
	}
	s.size += w
	return true, nil
}

func (s *LazyScavenger[K]) Throw(key K) {
	s.size -= s.weight(key)
}

type Cache[K comparable, V any] interface {
	// Do the operation `doer` on the given key `key`. The key is kept in the cache until the operation
	// completes.
	// Throws `ErrNoSuchItem` if the key is not found or not able to be loaded from given loader.
	// Throws `ErrNotEnoughSpace` if there is no room for the operation.
	Do(key K, doer func(V) error) (missing bool, err error)
	// Do the operation `doer` on the given key `key`. The key is kept in the cache until the operation
	// completes. The function waits for `timeout` if there is not enough space for the given key.
	// Throws `ErrNoSuchItem` if the key is not found or not able to be loaded from given loader.
	// Throws `ErrTimeOut` if timed out.
	DoWait(key K, timeout time.Duration, doer func(V) error) (missing bool, err error)
}

type Waiter[K comparable] struct {
	key K
	c   *sync.Cond
}

func newWaiter[K comparable](key K) Waiter[K] {
	return Waiter[K]{
		key: key,
		c:   sync.NewCond(&sync.Mutex{}),
	}
}

// lruCache extends the ccache library to provide pinning and unpinning of items.
type lruCache[K comparable, V any] struct {
	rwlock sync.RWMutex
	// the value is *cacheItem[V]
	items              map[K]*list.Element
	accessList         *list.List
	loaderSingleFlight singleflight.Group

	waitQueue *list.List

	loader    Loader[K, V]
	finalizer Finalizer[K, V]
	scavenger Scavenger[K]
}

type CacheBuilder[K comparable, V any] struct {
	loader    Loader[K, V]
	finalizer Finalizer[K, V]
	scavenger Scavenger[K]
}

func NewCacheBuilder[K comparable, V any]() *CacheBuilder[K, V] {
	return &CacheBuilder[K, V]{
		loader:    nil,
		finalizer: nil,
		scavenger: NewLazyScavenger(
			func(key K) int64 {
				return 1
			},
			64,
		),
	}
}

func (b *CacheBuilder[K, V]) WithLoader(loader Loader[K, V]) *CacheBuilder[K, V] {
	b.loader = loader
	return b
}

func (b *CacheBuilder[K, V]) WithFinalizer(finalizer Finalizer[K, V]) *CacheBuilder[K, V] {
	b.finalizer = finalizer
	return b
}

func (b *CacheBuilder[K, V]) WithLazyScavenger(weight func(K) int64, capacity int64) *CacheBuilder[K, V] {
	b.scavenger = NewLazyScavenger(weight, capacity)
	return b
}

func (b *CacheBuilder[K, V]) WithCapacity(capacity int64) *CacheBuilder[K, V] {
	b.scavenger = NewLazyScavenger(
		func(key K) int64 {
			return 1
		},
		capacity,
	)
	return b
}

func (b *CacheBuilder[K, V]) Build() Cache[K, V] {
	return newLRUCache(b.loader, b.finalizer, b.scavenger)
}

func newLRUCache[K comparable, V any](
	loader Loader[K, V],
	finalizer Finalizer[K, V],
	scavenger Scavenger[K],
) Cache[K, V] {
	return &lruCache[K, V]{
		items:              make(map[K]*list.Element),
		accessList:         list.New(),
		waitQueue:          list.New(),
		loaderSingleFlight: singleflight.Group{},
		loader:             loader,
		finalizer:          finalizer,
		scavenger:          scavenger,
	}
}

// Do picks up an item from cache and executes doer. The entry of interest is garented in the cache when doer is executing.
func (c *lruCache[K, V]) Do(key K, doer func(V) error) (bool, error) {
	item, missing, err := c.getAndPin(key)
	if err != nil {
		return missing, err
	}
	defer c.Unpin(key)
	return missing, doer(item.value)
}

func (c *lruCache[K, V]) DoWait(key K, timeout time.Duration, doer func(V) error) (bool, error) {
	timedWait := func(cond *sync.Cond, timeout time.Duration) bool {
		c := make(chan struct{})
		go func() {
			cond.L.Lock()
			defer cond.L.Unlock()
			defer close(c)
			cond.Wait()
		}()
		select {
		case <-c:
			return false // completed normally
		case <-time.After(timeout):
			return true // timed out
		}
	}

	var ele *list.Element
	start := time.Now()
	for {
		item, missing, err := c.getAndPin(key)
		if err == nil {
			if ele != nil {
				c.rwlock.Lock()
				c.waitQueue.Remove(ele)
				c.rwlock.Unlock()
			}
			defer c.Unpin(key)
			return missing, doer(item.value)
		} else if err != ErrNotEnoughSpace {
			return true, err
		}
		if ele == nil {
			// If no enough space, enqueue the key
			c.rwlock.Lock()
			waiter := newWaiter(key)
			ele = c.waitQueue.PushBack(&waiter)
			c.rwlock.Unlock()
		}
		// Wait for the key to be available
		timeLeft := time.Until(start.Add(timeout))
		if timeLeft <= 0 || timedWait(ele.Value.(*Waiter[K]).c, timeLeft) {
			return true, ErrTimeOut
		}
	}
}

func (c *lruCache[K, V]) Unpin(key K) {
	c.rwlock.Lock()
	defer c.rwlock.Unlock()
	e, ok := c.items[key]
	if !ok {
		return
	}
	item := e.Value.(*cacheItem[K, V])
	item.pinCount.Dec()
	c.notifyWaiters()
}

func (c *lruCache[K, V]) notifyWaiters() {
	if c.waitQueue.Len() > 0 {
		for e := c.waitQueue.Front(); e != nil; e = e.Next() {
			w := e.Value.(*Waiter[K])
			w.c.Broadcast()
		}
	}
}

func (c *lruCache[K, V]) peekAndPin(key K) *cacheItem[K, V] {
	c.rwlock.Lock()
	defer c.rwlock.Unlock()
	e, ok := c.items[key]
	if ok {
		item := e.Value.(*cacheItem[K, V])
		c.accessList.MoveToFront(e)
		item.pinCount.Inc()
		return item
	}
	return nil
}

// GetAndPin gets and pins the given key if it exists
func (c *lruCache[K, V]) getAndPin(key K) (*cacheItem[K, V], bool, error) {
	if item := c.peekAndPin(key); item != nil {
		return item, false, nil
	}

	if c.loader != nil {
		// Try scavenge if there is room. If not, fail fast.
		//	Note that the test is not accurate since we are not locking `loader` here.
		if _, ok := c.tryScavenge(key); !ok {
			return nil, true, ErrNotEnoughSpace
		}

		strKey := fmt.Sprint(key)
		item, err, _ := c.loaderSingleFlight.Do(strKey, func() (interface{}, error) {
			if item := c.peekAndPin(key); item != nil {
				return item, nil
			}

			value, ok := c.loader(key)
			if !ok {
				return nil, ErrNoSuchItem
			}

			item, err := c.setAndPin(key, value)
			if err != nil {
				return nil, err
			}
			return item, nil
		})

		if err == nil {
			return item.(*cacheItem[K, V]), true, nil
		}
		return nil, true, err
	}

	return nil, true, ErrNoSuchItem
}

func (c *lruCache[K, V]) tryScavenge(key K) ([]K, bool) {
	c.rwlock.Lock()
	defer c.rwlock.Unlock()
	return c.lockfreeTryScavenge(key)
}

func (c *lruCache[K, V]) lockfreeTryScavenge(key K) ([]K, bool) {
	ok, collector := c.scavenger.Collect(key)
	toEvict := make([]K, 0)
	if !ok {
		done := false
		for p := c.accessList.Back(); p != nil && !done; p = p.Prev() {
			evictItem := p.Value.(*cacheItem[K, V])
			if evictItem.pinCount.Load() > 0 {
				continue
			}
			toEvict = append(toEvict, evictItem.key)
			done = collector(evictItem.key)
		}
		if !done {
			return nil, false
		}
	} else {
		// If no collection needed, give back the space.
		c.scavenger.Throw(key)
	}
	return toEvict, true
}

// for cache miss
func (c *lruCache[K, V]) setAndPin(key K, value V) (*cacheItem[K, V], error) {
	c.rwlock.Lock()
	defer c.rwlock.Unlock()

	item := &cacheItem[K, V]{key: key, value: value}
	item.pinCount.Inc()

	// tryScavenge is done again since the load call is lock free.
	toEvict, ok := c.lockfreeTryScavenge(key)

	if !ok {
		if c.finalizer != nil {
			c.finalizer(key, value)
		}
		return nil, ErrNotEnoughSpace
	}

	for _, ek := range toEvict {
		e := c.items[ek]
		delete(c.items, ek)
		c.accessList.Remove(e)
		c.scavenger.Throw(ek)

		if c.finalizer != nil {
			item := e.Value.(*cacheItem[K, V])
			c.finalizer(ek, item.value)
		}
	}

	c.scavenger.Collect(key)
	e := c.accessList.PushFront(item)
	c.items[item.key] = e

	return item, nil
}
