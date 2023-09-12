package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"

	"github.com/jetcache-go/encoding"
	"github.com/jetcache-go/logger"
	"github.com/jetcache-go/stats"
	"github.com/jetcache-go/util"
)

const (
	TypeLocal  = "local"
	TypeRemote = "remote"
	TypeBoth   = "both"

	lockKeySuffix = "_#RL#"
)

var (
	_ Cache = (*jetCache)(nil)

	NotFoundPlaceholder   = []byte("*")
	ErrCacheMiss          = errors.New("cache: key is missing")
	ErrRemoteLocalBothNil = errors.New("cache: both remote and local are nil")
)

type (
	// Cache interface is used to define the cache implementation.
	Cache interface {
		// Set sets the cache with Item
		Set(item *Item) error
		// Once gets the item.Value for the given item.Key from the cache or
		// executes, caches, and returns the results of the given item.Func,
		// making sure that only one execution is in-flight for a given item.Key
		// at a time. If a duplicate comes in, the duplicate caller waits for the
		// original to complete and receives the same results.
		Once(item *Item) error
		// Delete deletes cached val with key.
		Delete(ctx context.Context, key string) error
		// DeleteFromLocalCache deletes local cached val with key.
		DeleteFromLocalCache(key string)
		// Exists reports whether val for the given key exists.
		Exists(ctx context.Context, key string) bool
		// Get gets the val for the given key and fills into val.
		Get(ctx context.Context, key string, val interface{}) error
		// GetSkippingLocal gets the val for the given key skipping local cache.
		GetSkippingLocal(ctx context.Context, key string, val interface{}) error
		// TaskSize returns Refresh task size.
		TaskSize() int
		// CacheType returns cache type
		CacheType() string
		// Close stop refresh tasks
		Close()
	}

	jetCache struct {
		sync.Mutex
		Options
		name           string
		group          singleflight.Group
		rand           *rand.Rand
		offset         time.Duration // 未命中key过期时间抖动因子
		refreshTaskMap sync.Map      // 异步刷新缓存任务集合<key, RefreshTask>
		stopChan       chan struct{} // 关闭信号
	}
)

func New(name string, opts ...Option) Cache {
	const maxOffset = 10 * time.Second
	o := newOptions(opts...)
	cache := &jetCache{
		name:     name,
		Options:  o,
		rand:     rand.New(rand.NewSource(time.Now().UnixNano())),
		stopChan: make(chan struct{}),
	}
	offset := cache.notFoundExpiry / 10
	if offset > maxOffset {
		offset = maxOffset
	}
	cache.offset = offset
	if cache.statsHandler == nil {
		cache.statsHandler = stats.NewHandles(cache.statsDisabled, stats.NewStatsLogger(cache.name))
	}
	if cache.refreshDuration > 0 {
		go util.WithRecover(func() {
			cache.tick()
		})
	}
	return cache
}

func (c *jetCache) Set(item *Item) error {
	_, _, err := c.set(item)
	return err
}

func (c *jetCache) set(item *Item) ([]byte, bool, error) {
	val, err := item.value()
	if item.Do != nil {
		c.statsHandler.IncrQuery()
	}

	if c.isNotFound(err) {
		if e := c.setNotFound(item.Context(), item.Key, item.SkipLocal); e != nil {
			logger.Error("setNotFound error(%v)", err)
		}
		return NotFoundPlaceholder, true, nil
	} else if err != nil {
		c.statsHandler.IncrQueryFail(err)
		return nil, false, err
	}

	b, err := c.Marshal(val)
	if err != nil {
		return nil, false, err
	}

	if c.local != nil && !item.SkipLocal {
		c.local.Set(item.Key, b)
	}

	if c.remote == nil {
		if c.local == nil {
			return b, true, ErrRemoteLocalBothNil
		}
		return b, true, nil
	}

	ttl := item.ttl()
	if ttl == 0 {
		return b, true, nil
	}

	if item.SetXX {
		_, err := c.remote.SetXX(item.Context(), item.Key, b, ttl)
		return b, true, err
	}
	if item.SetNX {
		_, err := c.remote.SetNX(item.Context(), item.Key, b, ttl)
		return b, true, err
	}
	return b, true, c.remote.SetEX(item.Context(), item.Key, b, ttl)
}

func (c *jetCache) Exists(ctx context.Context, key string) bool {
	_, err := c.getBytes(ctx, key, false)
	return err == nil
}

func (c *jetCache) Get(ctx context.Context, key string, val interface{}) error {
	return c.get(ctx, key, val, false)
}

func (c *jetCache) GetSkippingLocal(ctx context.Context, key string, val interface{}) error {
	return c.get(ctx, key, val, true)
}

func (c *jetCache) get(ctx context.Context, key string, val interface{}, skipLocal bool) error {
	b, err := c.getBytes(ctx, key, skipLocal)
	if err != nil {
		return err
	}

	return c.Unmarshal(b, val)
}

func (c *jetCache) getBytes(ctx context.Context, key string, skipLocal bool) ([]byte, error) {
	if !skipLocal && c.local != nil {
		b, ok := c.local.Get(key)
		if ok {
			c.statsHandler.IncrHit()
			c.statsHandler.IncrLocalHit()
			if bytes.Compare(b, NotFoundPlaceholder) == 0 {
				return nil, c.errNotFound
			}
			return b, nil
		}
		c.statsHandler.IncrLocalMiss()
	}

	if c.remote == nil {
		if c.local == nil {
			return nil, ErrRemoteLocalBothNil
		}
		c.statsHandler.IncrMiss()
		return nil, ErrCacheMiss
	}

	s, err := c.remote.Get(ctx, key)
	if err != nil {
		c.statsHandler.IncrMiss()
		c.statsHandler.IncrRemoteMiss()
		if err == c.remote.Nil() {
			return nil, ErrCacheMiss
		}
		return nil, err
	}

	c.statsHandler.IncrHit()
	c.statsHandler.IncrRemoteHit()

	b := util.Bytes(s)
	if bytes.Compare(b, NotFoundPlaceholder) == 0 {
		return nil, c.errNotFound
	}

	if !skipLocal && c.local != nil {
		c.local.Set(key, b)
	}

	return b, nil
}

func (c *jetCache) Once(item *Item) error {
	c.addOrUpdateRefreshTask(item)

	b, cached, err := c.getSetItemBytesOnce(item)
	if err != nil {
		return err
	}

	if bytes.Compare(b, NotFoundPlaceholder) == 0 {
		return c.errNotFound
	}

	if item.Value == nil || len(b) == 0 {
		return nil
	}

	if err := c.Unmarshal(b, item.Value); err != nil {
		if cached {
			_ = c.Delete(item.Context(), item.Key)
			return c.Once(item)
		}
		return err
	}

	return nil
}

func (c *jetCache) getSetItemBytesOnce(item *Item) (b []byte, cached bool, err error) {
	if c.local != nil {
		b, ok := c.local.Get(item.Key)
		if ok {
			c.statsHandler.IncrHit()
			c.statsHandler.IncrLocalHit()
			if bytes.Compare(b, NotFoundPlaceholder) == 0 {
				return nil, true, c.errNotFound
			}
			return b, true, nil
		}
	}

	v, err, _ := c.group.Do(item.Key, func() (interface{}, error) {
		b, err := c.getBytes(item.Context(), item.Key, item.SkipLocal)
		if err == nil {
			cached = true
			return b, nil
		} else if err == c.errNotFound {
			cached = true
			return nil, c.errNotFound
		}

		b, ok, err := c.set(item)
		if ok {
			return b, nil
		}
		return nil, err
	})

	if err != nil {
		return nil, false, err
	}

	return v.([]byte), cached, nil
}

func (c *jetCache) Delete(ctx context.Context, key string) error {
	if c.local != nil {
		c.local.Del(key)
	}

	if c.remote == nil {
		if c.local == nil {
			return ErrRemoteLocalBothNil
		}
		return nil
	}

	_, err := c.remote.Del(ctx, key)

	return err
}

func (c *jetCache) DeleteFromLocalCache(key string) {
	if c.local != nil {
		c.local.Del(key)
	}
}

func (c *jetCache) isNotFound(err error) bool {
	if err == nil {
		return false
	}

	return errors.Is(err, c.errNotFound)
}

func (c *jetCache) setNotFound(ctx context.Context, key string, skipLocal bool) error {
	if c.local != nil && !skipLocal {
		c.local.Set(key, NotFoundPlaceholder)
	}

	if c.remote == nil {
		if c.local == nil {
			return ErrRemoteLocalBothNil
		}
		return nil
	}

	ttl := c.notFoundExpiry + time.Duration(c.rand.Int63n(int64(c.offset)))

	return c.remote.SetEX(ctx, key, NotFoundPlaceholder, ttl)
}

func (c *jetCache) Marshal(val interface{}) ([]byte, error) {
	switch val := val.(type) {
	case nil:
		return nil, nil
	case []byte:
		return val, nil
	case string:
		return []byte(val), nil
	}

	return encoding.GetCodec(c.codec).Marshal(val)
}

func (c *jetCache) Unmarshal(b []byte, val interface{}) error {
	if len(b) == 0 {
		return nil
	}

	switch val := val.(type) {
	case nil:
		return nil
	case *[]byte:
		clone := make([]byte, len(b))
		copy(clone, b)
		*val = clone
		return nil
	case *string:
		*val = string(b)
		return nil
	}

	return encoding.GetCodec(c.codec).Unmarshal(b, val)
}

func (c *jetCache) Close() {
	c.stopRefresh()
	close(c.stopChan)
}

func (c *jetCache) TaskSize() (size int) {
	c.refreshTaskMap.Range(func(key, val interface{}) bool {
		size++
		return true
	})
	return
}

func (c *jetCache) CacheType() string {
	if c.local != nil && c.remote != nil {
		return TypeBoth
	} else if c.remote != nil {
		return TypeRemote
	}
	return TypeLocal
}

func (c *jetCache) addOrUpdateRefreshTask(item *Item) {
	if c.refreshDuration <= 0 || !item.Refresh {
		return
	}

	if ins, ok := c.refreshTaskMap.Load(item.Key); ok {
		ins.(*RefreshTask).LastAccessTime = time.Now()
	} else if ins, loaded := c.refreshTaskMap.LoadOrStore(item.Key, item.toRefreshTask()); loaded {
		ins.(*RefreshTask).LastAccessTime = time.Now()
	}
}

func (c *jetCache) cancel(key interface{}) {
	c.refreshTaskMap.Delete(key)
}

func (c *jetCache) stopRefresh() {
	c.refreshTaskMap.Range(func(key, val interface{}) bool {
		c.cancel(key)
		return true
	})
}

func (c *jetCache) tick() {
	var (
		ticker = time.NewTicker(c.refreshDuration)
		sem    = semaphore.NewWeighted(int64(c.refreshConcurrency))
	)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.Lock()
			// now is placed outside the Range to ensure that stopRefreshAfterLastAccess
			// does not time out under concurrent queuing.
			var now = time.Now()
			c.refreshTaskMap.Range(func(key, val interface{}) bool {
				task := val.(*RefreshTask)
				if c.stopRefreshAfterLastAccess > 0 {
					if task.LastAccessTime.Add(c.stopRefreshAfterLastAccess).Before(now) {
						logger.Debug("cancel refresh key: %s", key)
						c.cancel(key)
					} else {
						if err := sem.Acquire(context.Background(), 1); err != nil {
							logger.Error("tick#sem.Acquire error(%v)", err)
							return true
						}

						go util.WithRecover(func() {
							defer sem.Release(1)

							logger.Debug("start refresh key: %s", key)
							if c.remote != nil {
								c.externalLoad(context.Background(), task, now)
								return
							}
							c.load(context.Background(), task)
						})
					}
				}
				return true
			})
			c.Unlock()
		case <-c.stopChan:
			return
		}
	}
}

func (c *jetCache) externalLoad(ctx context.Context, task *RefreshTask, now time.Time) {
	var (
		lockKey    = fmt.Sprintf("%s%s", task.Key, lockKeySuffix)
		shouldLoad bool
	)
	_, err := c.remote.Get(ctx, lockKey)
	if err == c.remote.Nil() {
		shouldLoad = true
	} else if err != nil {
		logger.Error("externalLoad#c.remote.Get(%s) error(%v)", lockKey, err)
		return
	}

	if !shouldLoad {
		if c.local != nil {
			c.refreshLocal(ctx, task)
		}
		return
	}

	ok, err := c.remote.SetNX(ctx, lockKey, strconv.FormatInt(now.Unix(), 10), c.refreshDuration)
	if err != nil {
		logger.Error("externalLoad#c.remote.SetNX(%s) error(%v)", lockKey, err)
		return
	}
	if ok {
		if err = c.Set(task.toItem(ctx)); err != nil {
			logger.Error("externalLoad#c.Set(%s) error(%v)", task.Key, err)
			return
		}
	} else if c.local != nil {
		// If this coroutine fails to acquire the concurrent lock, it needs to wait briefly (delay) to trigger a refresh.
		// This way, it can directly fetch the origin result from Redis and refresh it locally.
		// The maximum concurrency here refers to the number of web machine instances, and the probability of
		// concurrent processing is actually not high. time.AfterFunc can be understood as a fallback mechanism to
		// reduce cache inconsistency time.
		time.AfterFunc(c.refreshDuration/5, func() {
			go util.WithRecover(func() {
				c.refreshLocal(context.Background(), task)
			})
		})
	}
}

func (c *jetCache) load(ctx context.Context, task *RefreshTask) {
	if err := c.Set(task.toItem(ctx)); err != nil {
		logger.Error("load#c.Set(%s) error(%v)", task.Key, err)
	}
}

func (c *jetCache) refreshLocal(ctx context.Context, task *RefreshTask) {
	val, err := c.remote.Get(ctx, task.Key)
	if err != nil {
		logger.Error("refreshLocal#c.remote.Get(%s) error(%v)", task.Key, err)
		return
	}
	c.local.Set(task.Key, util.Bytes(val))
}