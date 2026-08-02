# v2node Audit Nonblocking Admission Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove local bbolt transaction completion from the proxy data path while preserving bounded admission, single-writer batching, durable spool compatibility, and explicit audit-gap reporting.

**Architecture:** Keep the existing strict `Submit` API and add a `TrySubmit` admission API to `persistBatcher`. Ordinary access and FlowTraffic clients will use `TrySubmit`, returning as soon as an event enters the bounded queue; the existing writer goroutine remains responsible for batch persistence, success wakeups, and whole-batch failure callbacks.

**Tech Stack:** Go, generics, channels, `sync/atomic`, bbolt, standard `testing`, race detector, `go vet`.

---

## File map

- `common/accessaudit/persist_batcher.go`: define queue-full semantics and the nonblocking admission API; keep strict submission intact.
- `common/accessaudit/persist_batcher_test.go`: prove admission returns before disk completion, rejects a full queue immediately, and preserves batch failure callbacks.
- `common/accessaudit/client.go`: switch ordinary access events to nonblocking admission and retain explicit gap recording for unaccepted events.
- `common/accessaudit/flow_client.go`: switch FlowTraffic events to the same admission contract.
- `common/accessaudit/client_test.go`: cover FlowTraffic nonblocking behavior and update tests that previously relied on synchronous persistence.
- `common/accessaudit/access_client_test.go`: cover ordinary access nonblocking behavior and wait explicitly where upload tests require persisted events.
- `LESSONS_LEARNED.md`: record the final code fix and verification evidence without private host data.

### Task 1: Add a nonblocking batcher admission contract

**Files:**
- Modify: `common/accessaudit/persist_batcher_test.go`
- Modify: `common/accessaudit/persist_batcher.go`

- [ ] **Step 1: Extend the fake spool and write failing admission tests**

Add a one-shot `started` signal to `fakeBatchSpool`:

```go
type fakeBatchSpool[T any] struct {
	mu        sync.Mutex
	batches   [][]T
	block     chan struct{}
	started   chan struct{}
	startOnce sync.Once
	err       error
}

func (s *fakeBatchSpool[T]) EnqueueBatch(events []T) error {
	if s.started != nil {
		s.startOnce.Do(func() { close(s.started) })
	}
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, append([]T(nil), events...))
	return s.err
}
```

Add these tests:

```go
func TestPersistBatcherTrySubmitReturnsBeforeTransactionCompletes(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{})
	spool := &fakeBatchSpool[int]{block: block, started: started}
	batcher := newPersistBatcher(persistBatcherConfig[int]{
		Spool: spool, QueueSize: 1, BatchSize: 1,
		BatchWindow: time.Millisecond, SubmitTimeout: time.Hour,
	})
	defer func() {
		close(block)
		if err := batcher.Close(context.Background()); err != nil {
			t.Fatalf("close: %v", err)
		}
	}()

	if err := batcher.TrySubmit(1); err != nil {
		t.Fatalf("try submit: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("writer did not receive admitted event")
	}
}

func TestPersistBatcherTrySubmitRejectsFullQueueImmediately(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{})
	spool := &fakeBatchSpool[int]{block: block, started: started}
	batcher := newPersistBatcher(persistBatcherConfig[int]{
		Spool: spool, QueueSize: 1, BatchSize: 1,
		BatchWindow: time.Millisecond, SubmitTimeout: time.Hour,
	})
	defer func() {
		close(block)
		if err := batcher.Close(context.Background()); err != nil {
			t.Fatalf("close: %v", err)
		}
	}()

	if err := batcher.TrySubmit(1); err != nil {
		t.Fatalf("first admission: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("writer did not start")
	}
	if err := batcher.TrySubmit(2); err != nil {
		t.Fatalf("second admission: %v", err)
	}
	if err := batcher.TrySubmit(3); !errors.Is(err, ErrPersistenceQueueFull) {
		t.Fatalf("third admission error=%v", err)
	}
}
```

- [ ] **Step 2: Run the two tests and verify RED**

Run:

```text
go test ./common/accessaudit -run 'TestPersistBatcherTrySubmit' -count=1
```

Expected: compilation fails because `TrySubmit` and `ErrPersistenceQueueFull` do not exist.

- [ ] **Step 3: Implement minimal nonblocking admission**

Add the explicit error:

```go
ErrPersistenceQueueFull = errors.New("access audit persistence queue full")
```

Add the new method without changing `Submit`:

```go
func (b *persistBatcher[T]) TrySubmit(event T) error {
	if b == nil || b.config.Spool == nil {
		return ErrPersistenceClosed
	}
	b.Start()
	request := persistRequest[T]{event: event}

	b.acceptMu.RLock()
	defer b.acceptMu.RUnlock()
	if b.closed.Load() {
		return ErrPersistenceClosed
	}
	select {
	case b.requests <- request:
		b.recordDepth()
		return nil
	default:
		return ErrPersistenceQueueFull
	}
}
```

Guard the result delivery in `persistBatch`:

```go
for _, request := range batch {
	if request.result != nil {
		request.result <- err
	}
}
```

- [ ] **Step 4: Run batcher tests and verify GREEN**

Run:

```text
go test ./common/accessaudit -run 'TestPersistBatcher' -count=1
```

Expected: all `TestPersistBatcher*` tests pass.

- [ ] **Step 5: Commit the isolated batcher change**

```text
git add common/accessaudit/persist_batcher.go common/accessaudit/persist_batcher_test.go
git commit -m "修复：审计批处理支持非阻塞接收" -m "保留同步提交接口，新增有界队列即时接收和队列满错误。后台单写入器继续统一回传事务结果与失败回调。"
```

### Task 2: Move both audit clients off transaction-result waiting

**Files:**
- Modify: `common/accessaudit/client_test.go`
- Modify: `common/accessaudit/access_client_test.go`
- Modify: `common/accessaudit/client.go`
- Modify: `common/accessaudit/flow_client.go`

- [ ] **Step 1: Write failing client-level nonblocking tests**

For ordinary access, construct a client around a blocked fake batcher and assert `Enqueue` returns before the transaction is released:

```go
func TestAccessClientEnqueueReturnsAfterAdmission(t *testing.T) {
	now := time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
	block := make(chan struct{})
	started := make(chan struct{})
	spool := &fakeBatchSpool[Event]{block: block, started: started}
	batcher := newPersistBatcher(persistBatcherConfig[Event]{
		Spool: spool, QueueSize: 1, BatchSize: 1,
		BatchWindow: time.Millisecond, SubmitTimeout: time.Hour,
	})
	client := &Client{
		config: Config{Enabled: true, Now: func() time.Time { return now }},
		persister: batcher,
		wakeCh: make(chan struct{}, 1),
	}
	returned := make(chan bool, 1)
	go func() { returned <- client.Enqueue(accessEventForTest("nonblocking", now)) }()
	select {
	case ok := <-returned:
		if !ok {
			t.Fatal("event was not admitted")
		}
	case <-time.After(100 * time.Millisecond):
		close(block)
		<-returned
		t.Fatal("enqueue waited for local transaction")
	}
	close(block)
	if err := batcher.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
}
```

For FlowTraffic, use `failingFlowSpool` with a blocked transaction and the same returned-before-unblock assertion:

```go
func TestFlowClientReportReturnsAfterAdmission(t *testing.T) {
	now := time.Date(2026, 8, 2, 8, 5, 0, 0, time.UTC)
	block := make(chan struct{})
	spool := &failingFlowSpool{block: block}
	client, err := NewFlowClient(FlowClientConfig{
		Enabled: true, Endpoint: "http://127.0.0.1:1", Token: "secret",
		BatchSize: 1, MaxQueueSize: 1, PersistTimeout: time.Hour,
		Now: func() time.Time { return now }, Spool: spool,
	})
	if err != nil {
		t.Fatal(err)
	}
	returned := make(chan error, 1)
	go func() { returned <- client.Report(flowEventForTest(1, now)) }()
	select {
	case reportErr := <-returned:
		if reportErr != nil {
			t.Fatalf("report: %v", reportErr)
		}
	case <-time.After(100 * time.Millisecond):
		close(block)
		<-returned
		t.Fatal("report waited for local transaction")
	}
	close(block)
	client.Close()
}
```

- [ ] **Step 2: Run the client tests and verify RED**

Run:

```text
go test ./common/accessaudit -run 'Test(AccessClientEnqueue|FlowClientReport)ReturnsAfterAdmission' -count=1
```

Expected: both tests fail with the message that the call waited for the local transaction.

- [ ] **Step 3: Switch ordinary access to `TrySubmit`**

Replace the persistence call in `Client.Enqueue`:

```go
if err := c.persister.TrySubmit(event); err != nil {
	if errors.Is(err, ErrPersistenceQueueFull) || errors.Is(err, ErrPersistenceClosed) {
		c.recordPersistenceFailures([]Event{event}, err)
	}
	return false
}
return true
```

Do not add a goroutine and do not change `logSntpUserAccess` or `handler.Dispatch` ordering; returning after admission is sufficient to unblock the existing dispatcher chain.

- [ ] **Step 4: Switch FlowTraffic to `TrySubmit`**

Replace the persistence call in `FlowClient.Report`:

```go
if err := c.persister.TrySubmit(event); err != nil {
	if errors.Is(err, ErrPersistenceQueueFull) || errors.Is(err, ErrPersistenceClosed) {
		c.recordPersistenceFailures([]FlowEvent{event}, err)
	}
	return err
}
return nil
```

- [ ] **Step 5: Update tests that require completed persistence**

Add shared polling helpers in `client_test.go`:

```go
func waitForPendingEvents(t *testing.T, stats func() (SpoolStats, error), want uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		got, err := stats()
		if err != nil {
			t.Fatal(err)
		}
		if got.PendingEvents == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending events=%d want=%d", got.PendingEvents, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForPersistenceFailures(t *testing.T, status func() uint64, want uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		got := status()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("persistence failures=%d want=%d", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}
```

Insert the pending wait after reports that are followed immediately by a direct flush:

```go
waitForPendingEvents(t, spool.Stats, 1)
```

For the two-event bisection cases use `want=2`; for ordinary access, pass `client.spool.Stats`. After the 1000 concurrent reports, wait for `client.spool.Stats` to reach 1000 before reading status.

Replace the synchronous disk-failure expectation with admission followed by asynchronous gap observation:

```go
if err := client.Report(flowEventForTest(1, now)); err != nil {
	t.Fatalf("accepted event: %v", err)
}
waitForPersistenceFailures(t, func() uint64 {
	return client.Status().PersistenceFailures
}, 1)
```

In the accepted-delay test, verify the call already returned, then require that the data path did not accumulate a result-wait timeout:

```go
status := client.Status()
if status.PersistTimeouts != 0 || status.PersistenceFailures != 0 {
	t.Fatalf("accepted event must not wait or create a gap: %#v", status)
}
```

- [ ] **Step 6: Run the package tests and verify GREEN**

Run:

```text
go test ./common/accessaudit -count=1
```

Expected: the new nonblocking tests and all existing access-audit tests pass.

- [ ] **Step 7: Commit the client migration**

```text
git add common/accessaudit/client.go common/accessaudit/flow_client.go common/accessaudit/client_test.go common/accessaudit/access_client_test.go
git commit -m "修复：代理请求不再等待审计落盘" -m "普通访问和 FlowTraffic 在事件进入有界队列后立即返回。队列满、关闭和后台事务失败继续记录明确审计缺口。"
```

### Task 3: Verify the affected chain and finalize the learning record

**Files:**
- Modify: `LESSONS_LEARNED.md`

- [ ] **Step 1: Format changed Go files**

```text
gofmt -w common/accessaudit/persist_batcher.go common/accessaudit/persist_batcher_test.go common/accessaudit/client.go common/accessaudit/flow_client.go common/accessaudit/client_test.go common/accessaudit/access_client_test.go
```

- [ ] **Step 2: Run targeted functional tests**

```text
go test ./common/accessaudit ./core/app/dispatcher -count=1
```

Expected: both packages pass with zero failures.

- [ ] **Step 3: Run concurrency and static checks**

```text
go test -race ./common/accessaudit -count=1
go vet ./common/accessaudit ./core/app/dispatcher
```

Expected: both commands exit zero with no race report or vet error.

- [ ] **Step 4: Update the existing 2026-08-02 lesson**

Append the actual implementation and verification outcome to the existing incident entry. Record the `TrySubmit` boundary, queue-full behavior, exact commands run, and any verification gap. Do not add the production host, user identifiers, tokens, certificates, or IP addresses.

- [ ] **Step 5: Review the diff and whitespace**

```text
git diff --check
git status --short
git diff -- common/accessaudit LESSONS_LEARNED.md
```

Expected: no whitespace errors; only the planned files are changed.

- [ ] **Step 6: Commit the verified lesson update**

```text
git add LESSONS_LEARNED.md
git commit -m "文档：记录审计非阻塞接收修复" -m "补充根因、代码边界、回归测试和上线观察项，便于后续排查固定首包延迟。"
```

- [ ] **Step 7: Report the deployment boundary**

State separately that the code is implemented and locally verified, while production remains unchanged. Provide the commit hashes and the rollout checks required before enabling the new binary on the affected node.
