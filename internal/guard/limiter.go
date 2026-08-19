// Package guard 实现阶段 3 的行为收敛:并发门 + RPM、会话池、用量预算。
// 目标是把上游可见的聚合行为(并发数、活跃会话数、用量曲线)收敛到
// "单个真人 Claude Code 用户"的包线内。
package guard

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrQueueTimeout 表示排队超过上限仍未获得并发/RPM 配额。
var ErrQueueTimeout = errors.New("queue timeout")

// limEntry 是单个 key 的限流状态。
type limEntry struct {
	sem  chan struct{} // 并发槽(buffered,容量=MaxConcurrency;nil=不限)
	hits []time.Time   // RPM 滑动窗口内的请求时间戳
	mu   sync.Mutex
}

func (e *limEntry) rpmWait(limit int) (time.Duration, bool) {
	if limit <= 0 {
		return 0, true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	idx := 0
	for idx < len(e.hits) && now.Sub(e.hits[idx]) >= time.Minute {
		idx++
	}
	e.hits = e.hits[idx:]
	if len(e.hits) >= limit {
		return time.Minute - now.Sub(e.hits[0]), false
	}
	return 0, true
}

func (e *limEntry) recordHit() {
	e.mu.Lock()
	e.hits = append(e.hits, time.Now())
	e.mu.Unlock()
}

// rpmCountLocked 之外的重置读取辅助(供 stats)。
func (e *limEntry) rpmCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	idx := 0
	for idx < len(e.hits) && now.Sub(e.hits[idx]) >= time.Minute {
		idx++
	}
	return len(e.hits[idx:])
}

// Limiter 按 key 实施并发 + RPM 限制,超出时排队等待。
type Limiter struct {
	maxConc int
	rpm     int

	mu      sync.Mutex
	entries map[string]*limEntry
}

// NewLimiter 创建限流器。maxConc/rpm 为 0 表示不限制对应维度。
func NewLimiter(maxConc, rpm int) *Limiter {
	return &Limiter{maxConc: maxConc, rpm: rpm, entries: map[string]*limEntry{}}
}

func (l *Limiter) entry(key string) *limEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		e = &limEntry{}
		if l.maxConc > 0 {
			e.sem = make(chan struct{}, l.maxConc)
		}
		l.entries[key] = e
	}
	return e
}

// Acquire 获取该 key 的发送配额(RPM 窗口空位 + 并发槽)。
// 排队受 ctx 与 timeout 双重约束,超时返回 ErrQueueTimeout。
func (l *Limiter) Acquire(ctx context.Context, key string, timeout time.Duration) error {
	if l.maxConc <= 0 && l.rpm <= 0 {
		return nil
	}
	e := l.entry(key)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	for {
		// 1) RPM 窗口:满则小睡等待。
		if wait, ok := e.rpmWait(l.rpm); !ok {
			sleep := wait
			if sleep <= 0 || sleep > 200*time.Millisecond {
				sleep = 200 * time.Millisecond
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-deadline.C:
				return ErrQueueTimeout
			case <-time.After(sleep):
			}
			continue
		}

		// 2) 并发槽:先非阻塞尝试,避免 timer 已就绪时 select 随机选中超时分支的竞态。
		if e.sem == nil {
			e.recordHit()
			return nil
		}
		select {
		case e.sem <- struct{}{}:
			e.recordHit()
			return nil
		default:
		}
		select {
		case e.sem <- struct{}{}:
			e.recordHit()
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return ErrQueueTimeout
		}
	}
}

// Release 释放并发槽。
func (l *Limiter) Release(key string) {
	l.mu.Lock()
	e, ok := l.entries[key]
	l.mu.Unlock()
	if ok && e.sem != nil {
		select {
		case <-e.sem:
		default:
		}
	}
}

// RPMCount 返回该 key 最近一分钟的请求数(供 /stats)。
func (l *Limiter) RPMCount(key string) int {
	l.mu.Lock()
	e, ok := l.entries[key]
	l.mu.Unlock()
	if !ok {
		return 0
	}
	return e.rpmCount()
}

// InFlight 返回该 key 当前占用并发槽数(供 /stats)。
func (l *Limiter) InFlight(key string) int {
	l.mu.Lock()
	e, ok := l.entries[key]
	l.mu.Unlock()
	if !ok || e.sem == nil {
		return 0
	}
	return len(e.sem)
}

// EachKey 遍历已出现过的 key(供 /stats 聚合)。
func (l *Limiter) EachKey(fn func(key string)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k := range l.entries {
		fn(k)
	}
}
