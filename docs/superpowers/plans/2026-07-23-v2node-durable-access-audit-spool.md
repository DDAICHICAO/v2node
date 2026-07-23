# v2node 访问审计持久队列修复 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在不关闭普通访问记录或 FlowTraffic 的前提下，把两类事件改为有界单写者批量持久化，消除 bbolt 全队列扫描和写锁等待导致的 goroutine、内存线性增长，并保留现有 `flow.db` 待上传数据。

**Architecture:** 新建一个包内泛型 bbolt FIFO 核心，持久化增量统计，并仅在首次打开旧库时扫描重建元数据；FlowTraffic 和普通访问记录分别用 `flow.db`、`access.db` 的薄封装。每类事件各有一个容量受 `MaxQueueSize` 限制的单写者批处理器，最多聚合 10 ms 或 `min(BatchSize, 1000)` 条，在一个事务中落盘；上传器只从磁盘队列读，远端失败不会丢弃。数据面最多等待本地持久化结果 1 秒：已接收但结果待定的事件继续由单写队列提交并只记延迟，未接收或事务真实失败才记录可查询的缺口指标和限频错误，业务流量继续放行。

**Tech Stack:** Go 1.26.1、`go.etcd.io/bbolt`、标准库 `net/http` / `sync` / `time`、现有 SNTP HMAC 协议、Go 单元测试、systemd、localhost-only pprof。

**Go command prerequisite:** 当前仓库直接使用 `encoding/json/v2`；在 PowerShell 执行本计划中的 `go test`、`go vet` 或 `go build` 前先设置 `$env:GOEXPERIMENT='jsonv2'`，命令结束后执行 `Remove-Item Env:GOEXPERIMENT`。缺少该开关时出现 `build constraints exclude all Go files` 属于环境错误，不是测试红灯。

---

## 文件与职责

- 新建 `common/accessaudit/spool_core.go`：泛型 bbolt FIFO、兼容迁移、增量统计、队首过期/容量裁剪。
- 新建 `common/accessaudit/persist_batcher.go`：有界单写者批处理器、1 秒提交上限、关闭排空、队列水位。
- 新建 `common/accessaudit/access_spool.go`：普通访问事件的磁盘队列封装。
- 新建 `common/accessaudit/spool_core_test.go`：旧库迁移、稳态不扫描、增量统计、重启一致性。
- 新建 `common/accessaudit/persist_batcher_test.go`：单事务批写、队列饱和、超时与关闭行为。
- 新建 `common/accessaudit/access_client_test.go`：普通访问日志持久化、失败保留、重启补传、400/413 拆分。
- 修改 `common/accessaudit/spool.go`：保留旧 Flow JSON 合同，改为共享核心的 Flow 薄封装。
- 修改 `common/accessaudit/flow_client.go`：Flow 写入改走单写批处理器，补齐持久化状态。
- 修改 `common/accessaudit/client.go`：普通访问日志先写 `access.db`，上传成功后确认。
- 修改 `common/accessaudit/client_test.go`：适配新构造参数，保留签名与 Flow 回归测试，删除“内存队列满即静默丢弃”的旧预期。
- 修改 `conf/access_audit.go`、`conf/access_audit_test.go`：普通访问 spool 配置和默认值。
- 修改 `api/v2board/update.go`、`node/access_audit_config_task.go`、`node/access_audit_config_task_test.go`：面板任务字段、配置写回和默认值。
- 修改 `api/v2board/status.go`、`node/user.go`：两类队列的运行状态上报。
- 修改 `script/install.sh`、`script/configure-access-audit.sh`：新安装和现场配置默认值。
- 修改 `docs/log-troubleshooting.md`：持久队列语义、容量、告警和排障命令。
- 修改 `LESSONS_LEARNED.md`：记录本次 O(N²) bbolt 写路径的症状、根因、修复与验证入口，不记录主机、IP、token 或用户标识。

### Task 1: 建立增量统计的 bbolt FIFO 核心

**Files:**
- Create: `common/accessaudit/spool_core.go`
- Create: `common/accessaudit/spool_core_test.go`
- Modify: `common/accessaudit/spool.go:17-377`

- [ ] **Step 1: 写旧库迁移和稳态不扫描的失败测试**

在 `common/accessaudit/spool_core_test.go` 写入以下测试；它先制造旧版 Flow 数据，再验证首次打开重建统计、第二次打开不扫描 pending、批量追加后计数和 FIFO 正确：

~~~go
package accessaudit

import (
	"encoding/binary"
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestBoltFlowSpoolMigratesLegacyStatsOnceAndKeepsFIFO(t *testing.T) {
	now := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "flow.db")
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		pending, err := tx.CreateBucketIfNotExists([]byte("pending"))
		if err != nil {
			return err
		}
		if _, err = tx.CreateBucketIfNotExists([]byte("meta")); err != nil {
			return err
		}
		for sequence := uint32(1); sequence <= 2; sequence++ {
			record := storedFlowEvent{Event: flowEventForTest(sequence, now), EnqueuedAt: now}
			encoded, err := json.Marshal(record)
			if err != nil {
				return err
			}
			record.Size = uint64(len(encoded))
			encoded, err = json.Marshal(record)
			if err != nil {
				return err
			}
			key := make([]byte, 8)
			binary.BigEndian.PutUint64(key, uint64(sequence))
			if err := pending.Put(key, encoded); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	var migrationScans atomic.Uint64
	config := SpoolConfig{
		Path: path, MaxBytes: 1 << 20, MaxAge: time.Hour,
		Now: func() time.Time { return now },
		MigrationRecordObserver: func() { migrationScans.Add(1) },
	}
	spool, err := NewBoltFlowSpool(config)
	if err != nil {
		t.Fatalf("open legacy spool: %v", err)
	}
	stats, err := spool.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.PendingEvents != 2 || stats.PendingBytes == 0 || migrationScans.Load() != 2 {
		t.Fatalf("unexpected migrated state: stats=%#v scans=%d", stats, migrationScans.Load())
	}
	if err := spool.Close(); err != nil {
		t.Fatal(err)
	}

	migrationScans.Store(0)
	spool, err = NewBoltFlowSpool(config)
	if err != nil {
		t.Fatalf("reopen migrated spool: %v", err)
	}
	defer spool.Close()
	if err := spool.EnqueueBatch([]FlowEvent{
		flowEventForTest(3, now),
		flowEventForTest(4, now),
	}); err != nil {
		t.Fatalf("enqueue batch: %v", err)
	}
	stats, err = spool.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if migrationScans.Load() != 0 {
		t.Fatalf("steady-state open scanned %d records", migrationScans.Load())
	}
	if stats.PendingEvents != 4 || stats.PendingBytes == 0 {
		t.Fatalf("unexpected incremental stats: %#v", stats)
	}
	items, err := spool.Peek(4)
	if err != nil {
		t.Fatal(err)
	}
	for i, item := range items {
		if item.Event.Sequence != uint32(i+1) {
			t.Fatalf("FIFO changed at %d: %#v", i, items)
		}
	}
}
~~~

- [ ] **Step 2: 运行测试并确认旧实现失败**

Run: `go test ./common/accessaudit -run TestBoltFlowSpoolMigratesLegacyStatsOnceAndKeepsFIFO -count=1`

Expected: 编译失败，提示 `unknown field MigrationRecordObserver in struct literal of type SpoolConfig` 或 `spool.EnqueueBatch undefined`。

- [ ] **Step 3: 实现泛型队列、一次性迁移和增量统计**

在 `common/accessaudit/spool_core.go` 定义以下稳定类型：

~~~go
package accessaudit

import (
	"encoding/json"
	"time"

	bolt "go.etcd.io/bbolt"
)

const spoolSchemaVersion uint64 = 1

var (
	spoolPendingBucket    = []byte("pending")
	spoolMetaBucket       = []byte("meta")
	spoolStatsKey         = []byte("stats")
	spoolSchemaVersionKey = []byte("schema_version")
)

type spoolStats struct {
	PendingEvents           uint64 `json:"pending_events"`
	PendingBytes            uint64 `json:"pending_bytes"`
	DroppedEvents           uint64 `json:"dropped_events"`
	DroppedBytes            uint64 `json:"dropped_bytes"`
	RejectedEvents          uint64 `json:"rejected_events"`
	RejectedBytes           uint64 `json:"rejected_bytes"`
	PersistenceFailures     uint64 `json:"persistence_failures"`
	PersistenceFailureBytes uint64 `json:"persistence_failure_bytes"`
	DroppedEventFrom        int64  `json:"dropped_event_from"`
	DroppedEventTo          int64  `json:"dropped_event_to"`
	RejectedEventFrom       int64  `json:"rejected_event_from"`
	RejectedEventTo         int64  `json:"rejected_event_to"`
	PersistenceFailureFrom  int64  `json:"persistence_failure_from"`
	PersistenceFailureTo    int64  `json:"persistence_failure_to"`
	OldestEventAt           int64  `json:"oldest_event_at"`
	LastMigrationAt         int64  `json:"last_migration_at"`
	LastMigrationRecords    uint64 `json:"last_migration_records"`
	LastMigrationMillis     int64  `json:"last_migration_millis"`
}

type storedRecord[T any] struct {
	Event      T         `json:"event"`
	EnqueuedAt time.Time `json:"enqueued_at"`
	Size       uint64    `json:"size"`
}

type spoolCodec[T any] struct {
	Normalize func(*T, time.Time) error
	EventTime func(T) time.Time
}

type boltSpool[T any] struct {
	db                      *bolt.DB
	maxBytes                int64
	maxAge                  time.Duration
	now                     func() time.Time
	codec                   spoolCodec[T]
	migrationRecordObserver func()
}

type persistedItem[T any] struct {
	Key        uint64
	Event      T
	EnqueuedAt time.Time
}
~~~

实现以下方法：

~~~go
func openBoltSpool[T any](config SpoolConfig, codec spoolCodec[T]) (*boltSpool[T], error)
func (s *boltSpool[T]) enqueueBatch(events []T) error
func (s *boltSpool[T]) peek(limit int) ([]persistedItem[T], error)
func (s *boltSpool[T]) ack(keys []uint64) error
func (s *boltSpool[T]) reject(keys []uint64) error
func (s *boltSpool[T]) stats() (spoolStats, error)
func (s *boltSpool[T]) recordPersistenceFailure(eventAt time.Time, estimatedBytes uint64)
func (s *boltSpool[T]) close() error
~~~

`openBoltSpool` 创建 `pending` / `meta` bucket；若 `schema_version` 不是 1，则在一个事务内遍历旧记录一次，保留旧 `stats` 中的 dropped/rejected 值，重建 pending/oldest，写入版本和迁移耗时。版本为 1 时不得遍历 pending。

`enqueueBatch` 先复制并规范化事件，再在单个 `db.Update` 中连续 `NextSequence`、编码、`Put` 和累加 pending；编码沿用稳定 `Size` 的方式以保持旧 Flow JSON 可读。过期只从 cursor 头部删除到第一条未过期记录，容量只在 `PendingBytes > maxBytes` 时从头部删除。`ack`、`reject`、过期和容量删除都使用被删 record 的 `Size` 增量扣减，删除队首后最多解码新的第一条以刷新 `OldestEventAt`。`stats` 只读取 `meta/stats`。数据库写失败时的 gap 先留在进程内，下一次成功事务合并到 meta。

在 `common/accessaudit/spool.go` 保留现有公开类型名，改成薄封装：

~~~go
type SpoolConfig struct {
	Path                    string
	MaxBytes                int64
	MaxAge                  time.Duration
	Now                     func() time.Time
	MigrationRecordObserver func()
}

type BoltFlowSpool struct {
	core *boltSpool[FlowEvent]
}

func (s *BoltFlowSpool) Enqueue(event FlowEvent) error {
	return s.EnqueueBatch([]FlowEvent{event})
}

func (s *BoltFlowSpool) EnqueueBatch(events []FlowEvent) error {
	return s.core.enqueueBatch(events)
}
~~~

Flow codec 的 `Normalize` 调用 `FlowEvent.Normalize`，`EventTime` 返回 `event.EventTime`；`storedRecord[FlowEvent]` 的字段名必须保持 `event`、`enqueued_at`、`size`，不能改写现有 pending key。

- [ ] **Step 4: 运行迁移和原有 spool 回归测试**

Run: `go test ./common/accessaudit -run 'TestBoltFlowSpool(MigratesLegacyStatsOnceAndKeepsFIFO|PersistsFIFOAndCounters|DropsOldestAtCapacityAndExpiresOldItems)' -count=1`

Expected: PASS；首次迁移扫描 2 条，第二次打开扫描 0 条，旧 FIFO/裁剪测试继续通过。

- [ ] **Step 5: 提交核心队列**

~~~powershell
git add common/accessaudit/spool_core.go common/accessaudit/spool_core_test.go common/accessaudit/spool.go common/accessaudit/client_test.go
git commit -m "修复：持久队列改用增量统计" -m "兼容迁移旧版 flow.db，并移除稳态入队和状态读取中的全队列扫描。"
~~~

### Task 2: 增加有界单写者批处理器

**Files:**
- Create: `common/accessaudit/persist_batcher.go`
- Create: `common/accessaudit/persist_batcher_test.go`

- [ ] **Step 1: 写批量、饱和、超时和关闭测试**

在 `common/accessaudit/persist_batcher_test.go` 写入线程安全 fake spool，并验证 25 条事件以 `BatchSize=10` 写成 3 个批次，以及阻塞事务在 1 秒内返回超时：

~~~go
package accessaudit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeBatchSpool[T any] struct {
	mu      sync.Mutex
	batches [][]T
	block   chan struct{}
	err     error
}

func (s *fakeBatchSpool[T]) EnqueueBatch(events []T) error {
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, append([]T(nil), events...))
	return s.err
}

func TestPersistBatcherUsesBoundedSingleWriterBatches(t *testing.T) {
	spool := &fakeBatchSpool[int]{}
	batcher := newPersistBatcher(persistBatcherConfig[int]{
		Spool: spool, QueueSize: 32, BatchSize: 10,
		BatchWindow: 10 * time.Millisecond, SubmitTimeout: time.Second,
	})
	results := make([]chan error, 25)
	for value := 0; value < 25; value++ {
		results[value] = make(chan error, 1)
		batcher.requests <- persistRequest[int]{event: value, result: results[value]}
	}
	batcher.Start()
	defer batcher.Close(context.Background())
	for _, result := range results {
		err := <-result
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
	}
	spool.mu.Lock()
	defer spool.mu.Unlock()
	total := 0
	for _, batch := range spool.batches {
		if len(batch) > 10 {
			t.Fatalf("oversized batch: %d", len(batch))
		}
		total += len(batch)
	}
	if total != 25 || len(spool.batches) != 3 {
		t.Fatalf("batches=%d total=%d", len(spool.batches), total)
	}
}

func TestPersistBatcherTimesOutWithoutUnboundedWorkers(t *testing.T) {
	block := make(chan struct{})
	spool := &fakeBatchSpool[int]{block: block}
	batcher := newPersistBatcher(persistBatcherConfig[int]{
		Spool: spool, QueueSize: 1, BatchSize: 1,
		BatchWindow: time.Millisecond, SubmitTimeout: 30 * time.Millisecond,
	})
	batcher.Start()
	if err := batcher.Submit(1); !errors.Is(err, ErrPersistenceTimeout) {
		t.Fatalf("first submit error=%v", err)
	}
	if err := batcher.Submit(2); !errors.Is(err, ErrPersistenceTimeout) {
		t.Fatalf("second submit error=%v", err)
	}
	status := batcher.Status()
	if status.HighWatermark > 1 || status.Timeouts != 2 {
		t.Fatalf("unexpected status: %#v", status)
	}
	close(block)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := batcher.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
}
~~~

- [ ] **Step 2: 运行测试并确认类型尚不存在**

Run: `go test ./common/accessaudit -run TestPersistBatcher -count=1`

Expected: 编译失败，提示 `undefined: newPersistBatcher`、`undefined: persistBatcherConfig` 和 `undefined: ErrPersistenceTimeout`。

- [ ] **Step 3: 实现单写者批处理器**

在 `common/accessaudit/persist_batcher.go` 定义：

~~~go
package accessaudit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrPersistenceTimeout = errors.New("access audit persistence timeout")
	ErrPersistenceClosed  = errors.New("access audit persistence is closed")
)

type batchSpool[T any] interface {
	EnqueueBatch([]T) error
}

type persistRequest[T any] struct {
	event  T
	result chan error
}

type persistBatcherConfig[T any] struct {
	Spool         batchSpool[T]
	QueueSize     int
	BatchSize     int
	BatchWindow   time.Duration
	SubmitTimeout time.Duration
}

type PersistBatcherStatus struct {
	QueueDepth    uint64
	HighWatermark uint64
	Timeouts      uint64
	Failures      uint64
}

type persistBatcher[T any] struct {
	config    persistBatcherConfig[T]
	requests  chan persistRequest[T]
	closeCh   chan struct{}
	doneCh    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
	closed    atomic.Bool
	highWater atomic.Uint64
	timeouts  atomic.Uint64
	failures  atomic.Uint64
}
~~~

`newPersistBatcher` 将 `BatchSize` 夹在 1..1000，`BatchWindow` 默认 10 ms，`SubmitTimeout` 默认 1 秒。`Submit` 使用同一个 1 秒 deadline 完成“进入有界 requests channel”和“等待 result”；result channel 容量为 1，超时后批处理器回写不会阻塞。进入 channel 前超时返回 `ErrPersistenceTimeout`，表示事件未被接收；进入 channel 后等待结果超时返回 `ErrPersistencePending`，事件仍由原单写循环提交。`loop` 只有一个 goroutine；收到第一条后收集到批次上限或窗口到期，再调用一次 `Spool.EnqueueBatch`，把同一个结果逐条回送。事务失败由 `OnFailure` 对整批回报一次，即使调用方已超时也能记录真实缺口；不得为单条事件启动 goroutine。`Close(ctx)` 停止接收、排空已接收请求并等待 `doneCh`，context 到期返回 `ErrPersistenceTimeout`。

- [ ] **Step 4: 运行批处理器测试和竞态测试**

Run: `go test -race ./common/accessaudit -run TestPersistBatcher -count=1`

Expected: PASS，且无 DATA RACE；批次数为 3，水位不超过 channel 容量。

- [ ] **Step 5: 提交批处理器**

~~~powershell
git add common/accessaudit/persist_batcher.go common/accessaudit/persist_batcher_test.go
git commit -m "优化：访问审计写入改为有界批处理" -m "为每类事件提供单写者批量事务，并限制等待时间、队列容量和关闭过程。"
~~~

### Task 3: FlowTraffic 接入批处理并记录持久化缺口

**Files:**
- Modify: `common/accessaudit/spool.go`
- Modify: `common/accessaudit/flow_client.go`
- Modify: `common/accessaudit/client.go:82-166`
- Modify: `common/accessaudit/client_test.go:17-310`

- [ ] **Step 1: 写 Flow 并发入队与失败状态测试**

在 `common/accessaudit/client_test.go` 增加：

~~~go
func TestFlowClientBatchesConcurrentReportsWithoutWriterWaiters(t *testing.T) {
	now := time.Date(2026, 7, 23, 2, 0, 0, 0, time.UTC)
	spool := openFlowSpoolForTest(t, now)
	client, err := NewFlowClient(FlowClientConfig{
		Enabled: true, Endpoint: "http://127.0.0.1:1", Token: "secret",
		BatchSize: 100, MaxQueueSize: 2000, PersistTimeout: time.Second,
		FlushInterval: time.Hour, Timeout: 20 * time.Millisecond,
		Now: func() time.Time { return now }, Spool: spool,
	})
	if err != nil {
		t.Fatal(err)
	}
	client.Start()
	defer client.Close()
	const events = 1000
	errs := make(chan error, events)
	var wg sync.WaitGroup
	for sequence := 1; sequence <= events; sequence++ {
		wg.Add(1)
		go func(sequence int) {
			defer wg.Done()
			errs <- client.Report(flowEventForTest(uint32(sequence), now))
		}(sequence)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("report: %v", err)
		}
	}
	status := client.Status()
	if status.PendingEvents != events || status.PersistQueueHighWatermark > 2000 {
		t.Fatalf("unexpected status: %#v", status)
	}
}

func TestFlowClientExposesPersistenceFailureGap(t *testing.T) {
	now := time.Date(2026, 7, 23, 2, 0, 0, 0, time.UTC)
	spool := &failingFlowSpool{err: errors.New("disk read-only")}
	client, err := NewFlowClient(FlowClientConfig{
		Enabled: true, Endpoint: "http://127.0.0.1:1", Token: "secret",
		BatchSize: 1, MaxQueueSize: 1, PersistTimeout: time.Second,
		Now: func() time.Time { return now }, Spool: spool,
	})
	if err != nil {
		t.Fatal(err)
	}
	client.Start()
	defer client.Close()
	if err := client.Report(flowEventForTest(1, now)); err == nil {
		t.Fatal("expected persistence error")
	}
	status := client.Status()
	if status.PersistenceFailures != 1 || status.PersistenceFailureFrom != now.Unix() {
		t.Fatalf("missing explicit gap: %#v", status)
	}
}
~~~

`failingFlowSpool` 实现扩展后的 `FlowSpool` 接口，并让 `EnqueueBatch` 返回注入错误。

- [ ] **Step 2: 运行测试并确认缺少批处理配置/状态**

Run: `go test ./common/accessaudit -run 'TestFlowClient(BatchesConcurrentReportsWithoutWriterWaiters|ExposesPersistenceFailureGap)' -count=1`

Expected: 编译失败，提示 `FlowClientConfig` 没有 `MaxQueueSize` / `PersistTimeout` 或 `FlowRuntimeStatus` 没有持久化字段。

- [ ] **Step 3: 将 FlowClient 的同步多写者改成单写批处理**

扩展 `FlowSpool`：

~~~go
type FlowSpool interface {
	Enqueue(FlowEvent) error
	EnqueueBatch([]FlowEvent) error
	Peek(limit int) ([]SpoolItem, error)
	Ack(keys []uint64) error
	Reject(keys []uint64) error
	Stats() (SpoolStats, error)
	RecordPersistenceFailure(eventAt time.Time, estimatedBytes uint64)
	Close() error
}
~~~

`FlowClientConfig` 同步增加并由 `NewFlowClient` 规范化：

~~~go
MaxQueueSize  int
PersistTimeout time.Duration
~~~

扩展 `SpoolStats` 和 `FlowRuntimeStatus`：

~~~go
PersistenceFailures      uint64
PersistenceFailureBytes  uint64
PersistenceFailureFrom   int64
PersistenceFailureTo     int64
PersistQueueDepth         uint64
PersistQueueHighWatermark uint64
PersistTimeouts           uint64
LastMigrationAt           int64
LastMigrationRecords      uint64
LastMigrationMillis       int64
~~~

`BoltFlowSpool.RecordPersistenceFailure` 把未能写盘的数量、估算 JSON 字节和事件时间先累计到进程内 pending-gap；下一次成功的 bbolt 事务把它合并进 meta。`Stats` 返回“已持久化 meta + 尚未刷入 meta 的 pending-gap”，磁盘恢复后不重复累加。

在 `FlowClient` 增加 `persister *persistBatcher[FlowEvent]`；`Start` 先启动 persister，再启动上传 loop；`Report` 先规范化并复制事件值，再调用 `persister.Submit`。批次提交成功后由 `OnSuccess` 唤醒上传器；已接收但结果待定时按排队成功返回并记录限频延迟 warning；未接收或事务真实失败才调用 `RecordPersistenceFailure`。事务失败由批处理器回调一次，避免调用方超时后漏记或同步返回时重复计数。日志不得包含 event、用户标识、IP、token。`Close` 先禁止新提交，用 2 秒 context 排空 persister，再停止上传器和关闭 spool。

在 `Configure` / `Shutdown` 中先在 `defaultMu` 下交换全局指针，再释放锁关闭旧 client，禁止持锁等待批处理排空：

~~~go
func swapClients(next *Client, nextFlow *FlowClient, flowConfig FlowConfig) (old *Client, oldFlow *FlowClient) {
	defaultMu.Lock()
	old, oldFlow = defaultClient, defaultFlowClient
	defaultClient, defaultFlowClient = next, nextFlow
	defaultFlowConfig = flowConfig
	flowConfigReported = true
	defaultMu.Unlock()
	return old, oldFlow
}
~~~

- [ ] **Step 4: 运行 Flow 全量测试和竞态测试**

Run: `go test -race ./common/accessaudit -run 'TestFlow|TestBoltFlow' -count=1`

Expected: PASS，无竞态；1000 条并发 Report 全部进入磁盘队列，持久化错误产生明确 gap。

- [ ] **Step 5: 提交 Flow 接入**

~~~powershell
git add common/accessaudit/spool.go common/accessaudit/flow_client.go common/accessaudit/client.go common/accessaudit/client_test.go
git commit -m "修复：消除流量审计写锁堆积" -m "FlowTraffic 通过有界单写者批量落盘，并上报持久化失败缺口与队列水位。"
~~~

### Task 4: 普通访问记录改为先落 access.db 再上传

**Files:**
- Create: `common/accessaudit/access_spool.go`
- Create: `common/accessaudit/access_client_test.go`
- Modify: `common/accessaudit/client.go`
- Modify: `common/accessaudit/client_test.go:425-529`

- [ ] **Step 1: 写上传失败保留、重启补传和坏事件隔离测试**

在 `common/accessaudit/access_client_test.go` 写入：

~~~go
package accessaudit

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func accessEventForTest(id string, now time.Time) Event {
	return Event{
		EventTime: now, NodeID: 9, NodeTag: "node",
		UID: 100, UUID: id, SourceIP: "192.0.2.10",
		TargetHost: "example.com", TargetPort: 443,
		Network: "tcp", InboundTag: "trojan", OutboundTag: "direct",
	}
}

func TestAccessClientRetainsFailureAndReplaysAfterRestart(t *testing.T) {
	now := time.Date(2026, 7, 23, 3, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "access.db")
	var fail atomic.Bool
	fail.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	config := Config{
		Enabled: true, Endpoint: server.URL, Token: "secret",
		BatchSize: 10, MaxQueueSize: 100, FlushInterval: time.Hour,
		Timeout: time.Second, Now: func() time.Time { return now },
		SpoolPath: path, MaxSpoolBytes: 1 << 20, MaxSpoolAge: 24 * time.Hour,
		HTTPClient: server.Client(),
	}
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	client.Start()
	if !client.Enqueue(accessEventForTest("event-a", now)) {
		t.Fatal("local persistence failed")
	}
	if err := client.flushOnce(); err == nil {
		t.Fatal("expected remote failure")
	}
	if got := client.Status().PendingEvents; got != 1 {
		t.Fatalf("pending=%d", got)
	}
	client.Close()

	fail.Store(false)
	client, err = NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	client.Start()
	defer client.Close()
	if err := client.flushOnce(); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := client.Status().PendingEvents; got != 0 {
		t.Fatalf("pending after replay=%d", got)
	}
}

func TestAccessClientBisects400AndRejectsOnlyBadSingleton(t *testing.T) {
	now := time.Date(2026, 7, 23, 3, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("event-bad")) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	client, err := NewClient(Config{
		Enabled: true, Endpoint: server.URL, Token: "secret",
		BatchSize: 10, MaxQueueSize: 100, FlushInterval: time.Hour,
		Timeout: time.Second, Now: func() time.Time { return now },
		SpoolPath: filepath.Join(t.TempDir(), "access.db"),
		MaxSpoolBytes: 1 << 20, MaxSpoolAge: time.Hour, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	client.Start()
	defer client.Close()
	if !client.Enqueue(accessEventForTest("event-bad", now)) ||
		!client.Enqueue(accessEventForTest("event-good", now)) {
		t.Fatal("persist access events")
	}
	if err := client.flushOnce(); err != nil {
		t.Fatalf("bisect: %v", err)
	}
	status := client.Status()
	if status.PendingEvents != 0 || status.RejectedEvents != 1 {
		t.Fatalf("unexpected status: %#v", status)
	}
}

func TestConfigureKeepsProxyAvailableWhenAccessSpoolCannotOpen(t *testing.T) {
	err := Configure(Config{
		Enabled: true, Endpoint: "https://logs.invalid/access", Token: "secret",
		SpoolPath: filepath.Join(t.TempDir(), "missing", "access.db"),
		MaxSpoolBytes: 1 << 20, MaxSpoolAge: time.Hour,
		MkdirAll: func(string, os.FileMode) error { return errors.New("disk read-only") },
	})
	if err != nil {
		t.Fatalf("local spool failure must not abort proxy startup: %v", err)
	}
	defer Shutdown()
	status := CurrentRuntimeStatus()
	if status.LastErrorCode != "spool_open" || status.PersistenceFailures != 1 {
		t.Fatalf("missing startup gap status: %#v", status)
	}
}
~~~

- [ ] **Step 2: 运行测试并确认普通 Client 仍是易失内存队列**

Run: `go test ./common/accessaudit -run 'TestAccessClient(RetainsFailureAndReplaysAfterRestart|Bisects400AndRejectsOnlyBadSingleton)' -count=1`

Expected: 编译失败，提示 `Config` 缺少 `SpoolPath` / `MaxSpoolBytes` / `MaxSpoolAge` / `MkdirAll`，`Client` 缺少 `flushOnce` / `Status`。

- [ ] **Step 3: 实现 AccessSpool 和磁盘驱动的普通 Client**

在 `common/accessaudit/access_spool.go` 建立 `BoltAccessSpool`，复用 `boltSpool[Event]`：

~~~go
type AccessSpoolItem struct {
	Key        uint64
	Event      Event
	EnqueuedAt time.Time
}

type AccessSpool interface {
	EnqueueBatch([]Event) error
	Peek(int) ([]AccessSpoolItem, error)
	Ack([]uint64) error
	Reject([]uint64) error
	Stats() (SpoolStats, error)
	RecordPersistenceFailure(time.Time, uint64)
	Close() error
}
~~~

Access codec 的 `Normalize` 调用 `Event.toWire` 做相同字段校验，然后把规范化后的字段写回紧凑 `Event` 值；`EventTime` 返回 `event.EventTime`。默认路径由上层配置提供，核心默认值为 1 GiB、7 天。

将 `Client` 的 `queue chan Event` / `dropped atomic.Uint64` 替换为：

~~~go
spool     AccessSpool
persister *persistBatcher[Event]
wakeCh    chan struct{}
statusMu  sync.RWMutex
status    AccessRuntimeStatus
retryAt   time.Time
~~~

`Config` 增加本地队列配置和测试注入点：

~~~go
SpoolPath      string
MaxSpoolBytes  int64
MaxSpoolAge    time.Duration
PersistTimeout time.Duration
MkdirAll       func(string, os.FileMode) error
~~~

普通访问运行状态类型固定为：

~~~go
type AccessRuntimeStatus struct {
	ConfigReported             bool
	Enabled                    bool
	LastSuccessAt              int64
	LastErrorAt                int64
	LastErrorCode              string
	RetryCount                 uint64
	PendingEvents              uint64
	PendingBytes               uint64
	OldestEventAt              int64
	DroppedEvents              uint64
	DroppedBytes               uint64
	DroppedEventFrom           int64
	DroppedEventTo             int64
	RejectedEvents             uint64
	RejectedBytes              uint64
	RejectedEventFrom          int64
	RejectedEventTo            int64
	PersistenceFailures        uint64
	PersistenceFailureBytes    uint64
	PersistenceFailureFrom     int64
	PersistenceFailureTo       int64
	PersistQueueDepth          uint64
	PersistQueueHighWatermark  uint64
	PersistTimeouts            uint64
	LastMigrationAt            int64
	LastMigrationRecords       uint64
	LastMigrationMillis        int64
}

func CurrentRuntimeStatus() AccessRuntimeStatus
~~~

`NewClient` 打开 `access.db` 并建立 persister；`Enqueue` 最多等待本地持久化结果 1 秒。提交成功或事件已接收但结果待定时返回 true，后者只累计延迟指标；队列未接收或事务真实失败时返回 false、累计 persistence gap、输出限频错误，但代理连接继续。批次真实成功后由回调唤醒上传器。`loop` 与 Flow 上传循环一致，只从 spool `Peek`；2xx 后 `Ack`，网络/超时/401/403/429/5xx 保留并指数退避，400/413 递归二分，单条坏事件 `Reject`。请求体仍由 `encodePayload` 生成，HMAC header 和 endpoint 不变。

`Config.MkdirAll` 只用于注入目录创建函数，默认 `os.MkdirAll`。`Configure` 遇到 access spool 打开失败或 Flow 旧库迁移失败时不得让 `core.Start` 失败：它保留代理服务，记录 `spool_open` / `migration` 状态和 persistence gap，并只启动成功初始化的那一类审计客户端。Flow 迁移失败不能覆盖、清空或改名旧 `flow.db`，也不能阻止普通 `access.db` 工作。

全局失败状态使用与 client 指针相同的锁保护：

~~~go
var (
	defaultAccessStatus AccessRuntimeStatus
	defaultFlowStatus   FlowRuntimeStatus
)
~~~

`CurrentRuntimeStatus` / `CurrentFlowRuntimeStatus` 在对应 client 不存在时返回这两个状态；下一次配置成功后清空旧的 startup error。`Configure` 只把远端 endpoint/token 等配置错误返回给 `core.Start`，本地目录、打开数据库和旧库迁移错误转成 startup gap。

删除旧测试 `TestClientDropsWhenQueueFull`，以“队列等待超时产生显式 persistence gap”的测试取代；保留 `TestClientFlushesSignedBatch` 并传入临时 `SpoolPath`。

- [ ] **Step 4: 运行普通访问日志测试和完整 accessaudit 测试**

Run: `go test -race ./common/accessaudit -count=1`

Expected: PASS，无竞态；远端 502 后事件仍在 `access.db`，重启后补传，400 只拒绝坏单条，签名回归不变。

- [ ] **Step 5: 提交普通访问日志持久化**

~~~powershell
git add common/accessaudit/access_spool.go common/accessaudit/access_client_test.go common/accessaudit/client.go common/accessaudit/client_test.go
git commit -m "功能：持久保存普通访问记录" -m "普通访问日志先写入 access.db，远端失败和进程重启后继续按 FIFO 补传。"
~~~

### Task 5: 贯通配置、面板任务与安装脚本

**Files:**
- Modify: `conf/access_audit.go`
- Modify: `conf/access_audit_test.go`
- Modify: `api/v2board/update.go`
- Modify: `node/access_audit_config_task.go`
- Modify: `node/access_audit_config_task_test.go`
- Modify: `script/install.sh`
- Modify: `script/configure-access-audit.sh`

- [ ] **Step 1: 写配置默认值和任务落盘的失败测试**

在 `conf/access_audit_test.go` 的默认值测试加入：

~~~go
if cfg.SpoolPath != "/var/lib/v2node/access-audit-spool/access.db" {
	t.Fatalf("SpoolPath=%q", cfg.SpoolPath)
}
if cfg.MaxSpoolBytes != 1073741824 || cfg.MaxSpoolAge != "168h" {
	t.Fatalf("unexpected access spool limits: bytes=%d age=%q", cfg.MaxSpoolBytes, cfg.MaxSpoolAge)
}
runtimeCfg, err := cfg.RuntimeConfig()
if err != nil {
	t.Fatal(err)
}
if runtimeCfg.SpoolPath != cfg.SpoolPath ||
	runtimeCfg.MaxSpoolBytes != cfg.MaxSpoolBytes ||
	runtimeCfg.MaxSpoolAge != 7*24*time.Hour {
	t.Fatalf("runtime access spool mismatch: %#v", runtimeCfg)
}
~~~

在 `node/access_audit_config_task_test.go` 的 merge 测试中给任务传入并断言：

~~~go
SpoolPath:     "/srv/v2node/access.db",
MaxSpoolBytes: 536870912,
MaxSpoolAge:   "72h",
~~~

反序列化保存结果后断言 `AccessAudit.SpoolPath`、`MaxSpoolBytes`、`MaxSpoolAge` 原样存在；缺省任务断言使用 `/var/lib/v2node/access-audit-spool/access.db`、`1073741824`、`168h`。

- [ ] **Step 2: 运行测试并确认字段缺失**

Run: `go test ./conf ./node -run 'TestAccessAudit|TestApplyAccessAudit' -count=1`

Expected: 编译失败，提示 `AccessAuditConfig`、`accessaudit.Config` 或 `panel.AccessAuditTask` 没有新 spool 字段。

- [ ] **Step 3: 增加配置字段和一致默认值**

在 `conf/access_audit.go` 增加：

~~~go
const (
	DefaultAccessAuditSpoolPath     = "/var/lib/v2node/access-audit-spool/access.db"
	DefaultAccessAuditMaxSpoolBytes = int64(1073741824)
	DefaultAccessAuditMaxSpoolAge   = "168h"
)

type AccessAuditConfig struct {
	Enabled       bool
	Endpoint      string
	Token         string
	BatchSize     int
	MaxQueueSize  int
	FlushInterval string
	Timeout       string
	SpoolPath     string `mapstructure:"SpoolPath"`
	MaxSpoolBytes int64  `mapstructure:"MaxSpoolBytes"`
	MaxSpoolAge   string `mapstructure:"MaxSpoolAge"`
	FlowTraffic   FlowTrafficConfig
}
~~~

`Normalize` trim 并补全三项默认值，启用时校验 `MaxSpoolAge`；`RuntimeConfig` 解析为 `time.Duration` 并传到 `accessaudit.Config`。

在 `api/v2board/update.go` 的 `AccessAuditTask` 增加相同 JSON 字段：

~~~go
SpoolPath     string `json:"spool_path"`
MaxSpoolBytes int64  `json:"max_spool_bytes"`
MaxSpoolAge   string `json:"max_spool_age"`
~~~

`normalizeAccessAuditTask` 和 `applyAccessAuditConfigTask` 使用同一组默认值/字段，不能覆盖已有 Flow 配置。

在两个脚本中增加环境变量：

~~~bash
ACCESS_AUDIT_SPOOL_PATH="/var/lib/v2node/access-audit-spool/access.db"
ACCESS_AUDIT_MAX_SPOOL_BYTES="1073741824"
ACCESS_AUDIT_MAX_SPOOL_AGE="168h"
~~~

生成 JSON 时把三项写在 `AccessAudit` 父级；不得改变现有 Flow 默认路径和 256 MiB/24h。

- [ ] **Step 4: 运行配置测试和脚本语法检查**

Run: `go test ./conf ./node -run 'TestAccessAudit|TestApplyAccessAudit' -count=1`

Expected: PASS。

Run: `bash -n script/install.sh && bash -n script/configure-access-audit.sh`

Expected: 无输出，退出码 0。若 Windows 当前没有 Bash，在后续 Linux canary 上传前运行同一命令，并在本地报告中明确记录该项未在 Windows 执行。

- [ ] **Step 5: 提交配置链路**

~~~powershell
git add conf/access_audit.go conf/access_audit_test.go api/v2board/update.go node/access_audit_config_task.go node/access_audit_config_task_test.go script/install.sh script/configure-access-audit.sh
git commit -m "配置：增加普通访问日志磁盘队列" -m "贯通本地配置、面板任务和安装脚本，默认保留 1 GiB 或 7 天。"
~~~

### Task 6: 上报两类队列状态并补齐运维文档

**Files:**
- Modify: `api/v2board/status.go`
- Modify: `node/user.go:192-222`
- Modify: `node/access_audit_config_task_test.go:178-end`
- Modify: `docs/log-troubleshooting.md:58-90`
- Modify: `LESSONS_LEARNED.md`

- [ ] **Step 1: 写运行状态映射的失败测试**

在 `node/access_audit_config_task_test.go` 的 `TestAppendAccessAuditRuntimeStatusReportsCurrentConfig` 中，用临时 access/flow spool 配置全局 client，分别积压一条事件，然后断言：

~~~go
if status.AccessAuditPendingEvents != 1 ||
	status.AccessAuditPendingBytes == 0 ||
	status.AccessAuditPersistQueueHighWatermark == 0 {
	t.Fatalf("missing access audit runtime status: %#v", status)
}
if status.FlowTrafficPendingEvents != 1 ||
	status.FlowTrafficPendingBytes == 0 ||
	status.FlowTrafficPersistQueueHighWatermark == 0 {
	t.Fatalf("missing flow runtime status: %#v", status)
}
if status.AccessAuditPersistenceFailures != 0 ||
	status.FlowTrafficPersistenceFailures != 0 {
	t.Fatalf("unexpected persistence gap: %#v", status)
}
~~~

- [ ] **Step 2: 运行状态测试并确认字段缺失**

Run: `go test ./node -run TestAppendAccessAuditRuntimeStatusReportsCurrentConfig -count=1`

Expected: 编译失败，提示 `NodeRuntimeStatus` 没有 `AccessAuditPendingEvents`、`AccessAuditPersistQueueHighWatermark` 或持久化失败字段。

- [ ] **Step 3: 增加状态结构与映射**

在 `api/v2board/status.go` 为普通访问队列增加：

~~~go
AccessAuditLastSuccessAt             int64  `json:"access_audit_last_success_at,omitempty"`
AccessAuditLastErrorAt               int64  `json:"access_audit_last_error_at,omitempty"`
AccessAuditLastErrorCode             string `json:"access_audit_last_error_code,omitempty"`
AccessAuditRetryCount                uint64 `json:"access_audit_retry_count"`
AccessAuditPendingEvents             uint64 `json:"access_audit_pending_events"`
AccessAuditPendingBytes              uint64 `json:"access_audit_pending_bytes"`
AccessAuditOldestEventAt             int64  `json:"access_audit_oldest_event_at,omitempty"`
AccessAuditDroppedEvents             uint64 `json:"access_audit_dropped_events"`
AccessAuditDroppedBytes              uint64 `json:"access_audit_dropped_bytes"`
AccessAuditDroppedEventFrom          int64  `json:"access_audit_dropped_event_from,omitempty"`
AccessAuditDroppedEventTo            int64  `json:"access_audit_dropped_event_to,omitempty"`
AccessAuditRejectedEvents            uint64 `json:"access_audit_rejected_events"`
AccessAuditRejectedBytes             uint64 `json:"access_audit_rejected_bytes"`
AccessAuditRejectedEventFrom         int64  `json:"access_audit_rejected_event_from,omitempty"`
AccessAuditRejectedEventTo           int64  `json:"access_audit_rejected_event_to,omitempty"`
AccessAuditPersistenceFailures       uint64 `json:"access_audit_persistence_failures"`
AccessAuditPersistenceFailureBytes   uint64 `json:"access_audit_persistence_failure_bytes"`
AccessAuditPersistenceFailureFrom    int64  `json:"access_audit_persistence_failure_from,omitempty"`
AccessAuditPersistenceFailureTo      int64  `json:"access_audit_persistence_failure_to,omitempty"`
AccessAuditPersistQueueDepth         uint64 `json:"access_audit_persist_queue_depth"`
AccessAuditPersistQueueHighWatermark uint64 `json:"access_audit_persist_queue_high_watermark"`
AccessAuditPersistTimeouts           uint64 `json:"access_audit_persist_timeouts"`
AccessAuditLastMigrationAt           int64  `json:"access_audit_last_migration_at,omitempty"`
AccessAuditLastMigrationRecords      uint64 `json:"access_audit_last_migration_records"`
AccessAuditLastMigrationMillis       int64  `json:"access_audit_last_migration_millis"`
~~~

为 Flow 增加以下精确字段：

~~~go
FlowTrafficPersistenceFailures       uint64 `json:"flow_traffic_persistence_failures"`
FlowTrafficPersistenceFailureBytes   uint64 `json:"flow_traffic_persistence_failure_bytes"`
FlowTrafficPersistenceFailureFrom    int64  `json:"flow_traffic_persistence_failure_from,omitempty"`
FlowTrafficPersistenceFailureTo      int64  `json:"flow_traffic_persistence_failure_to,omitempty"`
FlowTrafficPersistQueueDepth         uint64 `json:"flow_traffic_persist_queue_depth"`
FlowTrafficPersistQueueHighWatermark uint64 `json:"flow_traffic_persist_queue_high_watermark"`
FlowTrafficPersistTimeouts           uint64 `json:"flow_traffic_persist_timeouts"`
FlowTrafficLastMigrationAt           int64  `json:"flow_traffic_last_migration_at,omitempty"`
FlowTrafficLastMigrationRecords      uint64 `json:"flow_traffic_last_migration_records"`
FlowTrafficLastMigrationMillis       int64  `json:"flow_traffic_last_migration_millis"`
~~~

`node/user.go` 调用 `CurrentRuntimeStatus()` 和 `CurrentFlowRuntimeStatus()` 完整映射；读取状态不得扫描 pending bucket。

- [ ] **Step 4: 更新排障文档和经验记录**

`docs/log-troubleshooting.md` 将“远端异步内存队列，满了丢弃”改为：

- 普通访问日志先写 `access.db`，Flow 写 `flow.db`；
- `MaxQueueSize` 是等待本地批量事务的内存请求上限，不是远端积压上限；
- 普通日志默认 1 GiB/168h，Flow 默认 256 MiB/24h；
- 网络/ClickHouse 失败保留队列，400/413 单条无效才进入 rejected；
- 达到容量/期限进入 dropped gap；磁盘事务错误或队列未接收进入 persistence gap；已接收但 1 秒内未返回只增加 `persist_timeouts` 延迟指标；
- 给出 `systemctl show v2node -p MemoryCurrent -p NRestarts`、`du -h /var/lib/v2node/access-audit-spool/*.db`、`journalctl -u v2node --since '-15 min' --no-pager` 和 localhost pprof 检查命令。

`LESSONS_LEARNED.md` 新增一节，字段固定为“症状 / 链路 / 根因 / 修复 / 验证 / 下次先查 / 相关文件与命令 / 提交”；使用主机角色“高连接 Trojan 节点”和约数，不写 IP、主机名、token、完整用户数据。提交字段填写当前各实现 commit 的短 hash。

- [ ] **Step 5: 运行状态测试、格式检查并提交**

Run: `gofmt -w common/accessaudit/*.go conf/access_audit.go conf/access_audit_test.go api/v2board/update.go api/v2board/status.go node/access_audit_config_task.go node/access_audit_config_task_test.go node/user.go`

Expected: 退出码 0。

Run: `go test ./node -run TestAppendAccessAuditRuntimeStatusReportsCurrentConfig -count=1`

Expected: `./node` 测试 PASS。

Run: `go test ./api/v2board -run '^$' -count=1`

Expected: 包编译成功并报告 `[no tests to run]` 或 PASS。

Run: `git diff --check`

Expected: 无输出，退出码 0。

~~~powershell
git add api/v2board/status.go node/user.go node/access_audit_config_task_test.go docs/log-troubleshooting.md LESSONS_LEARNED.md
git commit -m "运维：上报访问审计持久队列状态" -m "补齐积压、缺口、重试和写入水位指标，并记录 bbolt 全量扫描故障的排查经验。"
~~~

### Task 7: 完整验证、独立复核并合并回 dev

**Files:**
- Review: `common/accessaudit/*.go`
- Review: `conf/access_audit.go`
- Review: `api/v2board/update.go`
- Review: `api/v2board/status.go`
- Review: `node/access_audit_config_task.go`
- Review: `node/user.go`
- Review: `script/install.sh`
- Review: `script/configure-access-audit.sh`
- Review: `docs/log-troubleshooting.md`
- Review: `LESSONS_LEARNED.md`

- [ ] **Step 1: 运行目标包测试**

Run: `go test ./common/accessaudit ./conf ./core/app/dispatcher ./node -count=1`

Expected: 四个包全部 PASS；没有挂起、panic 或超时。

- [ ] **Step 2: 运行竞态和静态检查**

Run: `go test -race ./common/accessaudit -count=1`

Expected: PASS，无 `DATA RACE`。

Run: `go vet ./common/accessaudit ./core/app/dispatcher ./node`

Expected: 无输出，退出码 0。

Run: `git diff --check`

Expected: 无输出，退出码 0。

- [ ] **Step 3: 按设计逐项复核**

确认以下事实都有测试证据：

1. 旧 `flow.db` 首次只扫描一次，第二次打开和每次 enqueue/stats 不全扫。
2. `EnqueueBatch` 每批只有一个 bbolt 写事务。
3. Flow 和普通访问各自只有一个本地写 goroutine，内存请求容量有硬上限。
4. 普通访问远端失败和正常重启后仍能补传。
5. HTTP JSON、`X-SNTP-Timestamp`、`X-SNTP-Signature`、Flow EventID 不变。
6. dropped、rejected、persistence failure 都有数量/字节/时间范围，状态读取为 O(1)。
7. `Configure` / `Shutdown` 不持有 `defaultMu` 等待关闭。
8. 日志不输出 token、完整 IP、UUID、事件正文。
9. 无新增管理页面、计费、在线统计、Trojan/Eclipse/Xray 路由改动。

- [ ] **Step 4: 进行只读独立代码复核**

使用 `superpowers:requesting-code-review` 检查正确性、旧库兼容、关闭竞态、重复/丢失窗口、日志隐私和测试缺口。若发现问题，在功能分支上按 TDD 修正，然后重新运行 `go test ./common/accessaudit ./conf ./core/app/dispatcher ./node -count=1`、`go test -race ./common/accessaudit -count=1`、`go vet ./common/accessaudit ./core/app/dispatcher ./node` 和 `git diff --check`。

- [ ] **Step 5: 确认工作树并合并回原 dev**

Run: `git status --short --branch`

Expected: 当前为 `codex/fix-v2node-access-audit-spool` 且工作树干净。

Run: `git switch dev`

Expected: 切换到 `dev`；若 dev 出现用户未提交改动，立即停止合并并保留现场。

Run: `git merge --no-ff codex/fix-v2node-access-audit-spool -m "合并：修复访问审计持久队列" -m "保留访问记录和 FlowTraffic，消除 bbolt 全队列扫描与写锁堆积，并加入分阶段上线观测。"`

Expected: merge 成功，无冲突，生成中文 merge commit。

- [ ] **Step 6: 合并后复验**

Run: `go test ./common/accessaudit ./conf ./core/app/dispatcher ./node -count=1`

Expected: 全部 PASS。

Run: `git status --short --branch`

Expected: `dev` 工作树干净，分支仅领先远端；不执行 push。

### Task 8: 构建 Linux canary 并先升级受影响节点

**Files:**
- Generate outside Git: Windows 临时目录下的 `v2node-canary-commit/v2node-linux-amd64`
- Production backup: `/usr/local/v2node/v2node.bak-$stamp`
- Production config backup: `/etc/v2node/config.json.bak-$stamp`

- [ ] **Step 1: 在 dev 上构建可追踪的 Linux amd64 二进制**

在 PowerShell 执行：

~~~powershell
$BuildVersion = "canary-" + (git rev-parse --short HEAD)
$ArtifactDir = Join-Path ([IO.Path]::GetTempPath()) "v2node-$BuildVersion"
$ArtifactPath = Join-Path $ArtifactDir "v2node-linux-amd64"
New-Item -ItemType Directory -Force $ArtifactDir | Out-Null
$env:GOOS = "linux"
$env:GOARCH = "amd64"
$env:CGO_ENABLED = "0"
$env:GOEXPERIMENT = "jsonv2"
go build -trimpath -ldflags "-X github.com/wyx2685/v2node/cmd.version=$BuildVersion -s -w -buildid=" -o $ArtifactPath .
Remove-Item Env:GOOS,Env:GOARCH,Env:CGO_ENABLED,Env:GOEXPERIMENT
Get-FileHash $ArtifactPath -Algorithm SHA256
~~~

Expected: build 退出码 0，在仓库外生成非空 Linux ELF 和 SHA256；`git status --short` 不包含构建产物。

- [ ] **Step 2: 记录 canary 基线**

先从受影响的高连接 Trojan 节点开始。用 `Read-Host` 输入主机，避免把生产地址写进仓库：

~~~powershell
$CanaryHost = Read-Host "Canary SSH host"
ssh "root@$CanaryHost" "systemctl is-active v2node; systemctl show v2node -p MemoryCurrent -p MemoryPeak -p NRestarts; /usr/local/v2node/v2node version; du -h /var/lib/v2node/access-audit-spool/*.db 2>/dev/null || true; journalctl -u v2node --since '-15 min' --no-pager | tail -n 80"
~~~

Expected: service 为 active；记录版本、MemoryCurrent/Peak、NRestarts、`access.db` / `flow.db` 大小和最近错误。确认生产仍是 `AccessAudit.Enabled=true`、`FlowTraffic.Enabled=false`。

- [ ] **Step 3: 备份并原子替换二进制**

~~~powershell
scp $ArtifactPath "root@$($CanaryHost):/tmp/v2node.canary"
ssh "root@$CanaryHost" 'set -eu; stamp=$(date -u +%Y%m%dT%H%M%SZ); cp -a /usr/local/v2node/v2node /usr/local/v2node/v2node.bak-$stamp; cp -a /etc/v2node/config.json /etc/v2node/config.json.bak-$stamp; chmod 0755 /tmp/v2node.canary; systemctl stop v2node; install -m 0755 /tmp/v2node.canary /usr/local/v2node/v2node; systemctl start v2node; systemctl is-active v2node; /usr/local/v2node/v2node version'
~~~

Expected: 新版本号等于 PowerShell 变量 `$BuildVersion`，service active，备份仍在本机；不删除 `flow.db`。

- [ ] **Step 4: 保持 FlowTraffic=false 验证普通访问日志**

观察普通访问业务至少 5 分钟，每次等待不超过 60 秒，重复采集：

~~~powershell
ssh "root@$CanaryHost" "systemctl show v2node -p MemoryCurrent -p NRestarts; du -h /var/lib/v2node/access-audit-spool/access.db; journalctl -u v2node --since '-2 min' --no-pager | grep -E 'access audit|persistence|panic|fatal|OOM' | tail -n 80"
~~~

Expected: `access.db` 已创建且普通访问持续上传；NRestarts 不变；没有 persistence gap、panic、fatal、OOM；MemoryCurrent 不呈每分钟单调快速增长。

- [ ] **Step 5: 在 canary 恢复 FlowTraffic 并观察 15 分钟**

先备份当前配置，再用 Python 只改一个布尔值：

~~~powershell
ssh "root@$CanaryHost" 'set -eu; stamp=$(date -u +%Y%m%dT%H%M%SZ); cp -a /etc/v2node/config.json /etc/v2node/config.json.bak-flow-on-$stamp; python3 -c "import json; p=\"/etc/v2node/config.json\"; d=json.load(open(p)); d[\"AccessAudit\"][\"FlowTraffic\"][\"Enabled\"]=True; open(p,\"w\").write(json.dumps(d,indent=4)+\"\n\")"; systemctl restart v2node; systemctl is-active v2node'
~~~

每分钟执行一次以下只读快照，共 15 次；单次等待不超过 60 秒：

~~~powershell
ssh "root@$CanaryHost" 'date -u; systemctl show v2node -p MemoryCurrent -p MemoryPeak -p NRestarts; pid=$(systemctl show -p MainPID --value v2node); grep -E "^(Rss|Pss|Pss_Anon):" /proc/$pid/smaps_rollup; ss -Htan state established | wc -l; du -h /var/lib/v2node/access-audit-spool/access.db /var/lib/v2node/access-audit-spool/flow.db; curl -fsS "http://127.0.0.1:6060/debug/pprof/goroutine?debug=1" | head -n 1; curl -fsS "http://127.0.0.1:6060/debug/pprof/goroutine?debug=2" | grep -F -c "go.etcd.io/bbolt.(*DB).BeginRWTx"; journalctl -u v2node --since "-70 sec" --no-pager | grep -E "persistence|access audit|FlowTraffic|reportUserTrafficTask|panic|fatal|OOM" | tail -n 60'
~~~

通过门槛：

- `bbolt.(*DB).BeginRWTx` 等待栈稳定在 0，瞬时不超过 5；
- 从第 5 分钟到第 15 分钟，连接量相近时 goroutine 不持续上升超过 20%；
- MemoryCurrent/Pss_Anon 连续三个样本不呈线性上升，且不接近节点内存上限；
- NRestarts 不增加，无 OOM/panic/fatal；
- 普通访问与 Flow 上传均有成功时间，persistence failure 为 0；
- 旧 `flow.db` pending 开始下降或在持续新流量下保持有界；
- `reportUserTrafficTask` 无超时。

- [ ] **Step 6: canary 失败时回滚**

若任一硬门槛失败，立即把 Flow 关闭并恢复上一二进制：

~~~powershell
ssh "root@$CanaryHost" 'set -eu; python3 -c "import json; p=\"/etc/v2node/config.json\"; d=json.load(open(p)); d[\"AccessAudit\"][\"FlowTraffic\"][\"Enabled\"]=False; open(p,\"w\").write(json.dumps(d,indent=4)+\"\n\")"; previous=$(ls -1t /usr/local/v2node/v2node.bak-* | head -n1); systemctl stop v2node; install -m 0755 "$previous" /usr/local/v2node/v2node; systemctl start v2node; systemctl is-active v2node'
~~~

Expected: service active，Flow 保持关闭；`access.db` 和 `flow.db` 原样保留，供修正版继续读取。

### Task 9: 扩大到少量后端，再经确认全量升级

**Files:**
- No repository changes.
- Per-host backups remain on each selected node.

- [ ] **Step 1: 选 2-3 台有代表性的第二批节点**

从真实后端清单中选择：

1. 一台高连接 Trojan 节点；
2. 一台主要跑 SNTP Eclipse 或低连接节点；
3. 一台当前有 Flow 积压但内存正常的节点。

排除正在维护、磁盘空间不足、MySQL/面板通信异常的节点。记录每台的匿名角色、当前版本、内存、连接数、NRestarts、Flow 开关和 spool 大小；主机地址只保留在当次终端变量中。

- [ ] **Step 2: 逐台执行“备份 → 升级 → Flow false 验证 → Flow true 观察”**

每次只输入一台第二批节点：

~~~powershell
$SecondWaveHost = Read-Host "Second-wave SSH host"
ssh "root@$SecondWaveHost" "systemctl is-active v2node; systemctl show v2node -p MemoryCurrent -p MemoryPeak -p NRestarts; /usr/local/v2node/v2node version; du -h /var/lib/v2node/access-audit-spool/*.db 2>/dev/null || true"
scp $ArtifactPath "root@$($SecondWaveHost):/tmp/v2node.canary"
ssh "root@$SecondWaveHost" 'set -eu; stamp=$(date -u +%Y%m%dT%H%M%SZ); cp -a /usr/local/v2node/v2node /usr/local/v2node/v2node.bak-$stamp; cp -a /etc/v2node/config.json /etc/v2node/config.json.bak-$stamp; python3 -c "import json; p=\"/etc/v2node/config.json\"; d=json.load(open(p)); d[\"AccessAudit\"][\"FlowTraffic\"][\"Enabled\"]=False; open(p,\"w\").write(json.dumps(d,indent=4)+\"\n\")"; systemctl stop v2node; install -m 0755 /tmp/v2node.canary /usr/local/v2node/v2node; systemctl start v2node; systemctl is-active v2node'
~~~

Flow 关闭阶段每分钟单独运行以下命令，共 5 个样本：

~~~powershell
ssh "root@$SecondWaveHost" "date -u; systemctl show v2node -p MemoryCurrent -p NRestarts; du -h /var/lib/v2node/access-audit-spool/access.db; journalctl -u v2node --since '-70 sec' --no-pager | grep -E 'persistence|access audit|panic|fatal|OOM' | tail -n 60"
~~~

五个样本通过后启用 Flow：

~~~powershell
ssh "root@$SecondWaveHost" 'set -eu; python3 -c "import json; p=\"/etc/v2node/config.json\"; d=json.load(open(p)); d[\"AccessAudit\"][\"FlowTraffic\"][\"Enabled\"]=True; open(p,\"w\").write(json.dumps(d,indent=4)+\"\n\")"; systemctl restart v2node; systemctl is-active v2node'
~~~

每分钟单独运行以下命令，共 15 个样本：

~~~powershell
ssh "root@$SecondWaveHost" 'date -u; systemctl show v2node -p MemoryCurrent -p MemoryPeak -p NRestarts; pid=$(systemctl show -p MainPID --value v2node); grep -E "^(Rss|Pss|Pss_Anon):" /proc/$pid/smaps_rollup; ss -Htan state established | wc -l; du -h /var/lib/v2node/access-audit-spool/access.db /var/lib/v2node/access-audit-spool/flow.db; curl -fsS "http://127.0.0.1:6060/debug/pprof/goroutine?debug=1" | head -n 1; curl -fsS "http://127.0.0.1:6060/debug/pprof/goroutine?debug=2" | grep -F -c "go.etcd.io/bbolt.(*DB).BeginRWTx"; journalctl -u v2node --since "-70 sec" --no-pager | grep -E "persistence|access audit|FlowTraffic|reportUserTrafficTask|panic|fatal|OOM" | tail -n 60'
~~~

Expected: 每台 bbolt 等待稳定为 0 且瞬时不超过 5；第 5 到第 15 分钟连接数相近时 goroutine 增幅不超过 20%；MemoryCurrent/Pss_Anon 不连续三个样本线性增长；NRestarts 不增加；没有 persistence gap、`reportUserTrafficTask` 超时、OOM、panic 或 fatal。任何一项失败就停止扩容，在该节点关闭 Flow，并从最新 `/usr/local/v2node/v2node.bak-*` 恢复二进制；不继续下一台。

- [ ] **Step 3: 汇总第二批证据并请求全量放行**

向用户报告每台匿名角色的：

- 新旧版本与二进制 SHA256；
- 观察起止时间；
- 连接数、goroutine、MemoryCurrent/Pss_Anon 趋势；
- bbolt BeginRWTx 等待峰值；
- 两类 pending/dropped/rejected/persistence failure；
- NRestarts、OOM/panic/fatal 和 `reportUserTrafficTask` 状态。

只有用户确认“全量升级”后才进入下一步。

- [ ] **Step 4: 分批全量升级**

用户放行后，每批通过交互输入不超过全量 20% 的主机：

~~~powershell
$RolloutHosts = (Read-Host "Comma-separated rollout SSH hosts").Split(",") | ForEach-Object { $_.Trim() } | Where-Object { $_ }
foreach ($RolloutHost in $RolloutHosts) {
    ssh "root@$RolloutHost" "systemctl is-active v2node; systemctl show v2node -p MemoryCurrent -p NRestarts; /usr/local/v2node/v2node version"
    scp $ArtifactPath "root@$($RolloutHost):/tmp/v2node.canary"
    ssh "root@$RolloutHost" 'set -eu; stamp=$(date -u +%Y%m%dT%H%M%SZ); cp -a /usr/local/v2node/v2node /usr/local/v2node/v2node.bak-$stamp; cp -a /etc/v2node/config.json /etc/v2node/config.json.bak-$stamp; systemctl stop v2node; install -m 0755 /tmp/v2node.canary /usr/local/v2node/v2node; systemctl start v2node; systemctl is-active v2node'
}
~~~

每批完成后，针对 `$RolloutHosts` 中每台主机每分钟单独采集以下命令，共 10 个样本：

~~~powershell
foreach ($RolloutHost in $RolloutHosts) {
    ssh "root@$RolloutHost" 'date -u; systemctl show v2node -p MemoryCurrent -p NRestarts; ss -Htan state established | wc -l; curl -fsS "http://127.0.0.1:6060/debug/pprof/goroutine?debug=2" | grep -F -c "go.etcd.io/bbolt.(*DB).BeginRWTx"; journalctl -u v2node --since "-70 sec" --no-pager | grep -E "persistence|reportUserTrafficTask|panic|fatal|OOM" | tail -n 40'
}
~~~

Expected: 每台 service active，NRestarts 不增加，bbolt 等待不超过 5，内存不呈线性上升，无 persistence gap/OOM/panic/fatal。任一批次失败立即停止后续批次，在失败节点关闭 Flow 并恢复最新 `v2node.bak-*`；两个 spool 数据库均保留。

- [ ] **Step 5: 全量完成后核对原始主机清单**

逐项对账：目标主机数、成功数、跳过数、失败/回滚数、仍运行旧版本数必须相加等于原始清单。最终报告真实的观测开始时间，不把上线前没有采集的数据描述成历史趋势。
