# CloudWeGo-Netpoll 源码分析
Source Code From
https://github.com/cloudwego/netpoll/archive/refs/tags/v0.7.0.tar.gz

## 简介

[Netpoll][Netpoll] 是由 [字节跳动][ByteDance] 开发的高性能 NIO(Non-blocking I/O)
网络库，专注于 RPC 场景。

RPC 通常有较重的处理逻辑，因此无法串行处理 I/O。而 Go 的标准库 [net][net] 设计了 BIO(Blocking I/O) 模式的
API，使得 RPC 框架设计上只能为每个连接都分配一个 goroutine。 这在高并发下，会产生大量的
goroutine，大幅增加调度开销。此外，[net.Conn][net.Conn] 没有提供检查连接活性的 API，因此 RPC
框架很难设计出高效的连接池，池中的失效连接无法及时清理。

另一方面，开源社区目前缺少专注于 RPC 方案的 Go 网络库。类似的项目如：[evio][evio]
, [gnet][gnet] 等，均面向 [Redis][Redis], [HAProxy][HAProxy] 这样的场景。

因此 [Netpoll][Netpoll] 应运而生，它借鉴了 [evio][evio]
和 [netty][netty] 的优秀设计，具有出色的 [性能](#性能)，更适用于微服务架构。
同时，[Netpoll][Netpoll] 还提供了一些 [特性](#特性)，推荐在 RPC 设计中替代
[net][net] 。

基于 [Netpoll][Netpoll] 开发的 RPC 框架 [Kitex][Kitex] 和 HTTP 框架 [Hertz][Hertz]，性能均业界领先。

[范例][netpoll-examples] 展示了如何使用 [Netpoll][Netpoll]
构建 RPC Client 和 Server。

更多信息请参阅 [文档](#文档)。

## 流式读写nocopy API
### 核心接口
#### Reader
nocopy.go:32
#### Writer
nocopy.go:133
#### ReadWriter
nocopy.go:220

## Nocopy LinkBuffer
基于链表数组实现，将 []byte 数组抽象为 block，并以链表拼接的形式将 block 组合为 Nocopy Buffer，同时引入了引用计数、nocopy API 和对象池。

nocopy_linkbuffer.go:814

优势：
1. 读写并行无锁，支持 nocopy 地流式读写
   * 读写分别操作头尾指针，相互不干扰。
2. 高效扩缩容
   * 扩容阶段，直接在尾指针后添加新的 block 即可，无需 copy 原数组。
   * 缩容阶段，头指针会直接释放使用完毕的 block 节点，完成缩容。每个 block 都有独立的引用计数，当释放的 block 不再有引用时，主动回收 block 节点。
3. 灵活切片和拼接 buffer (链表特性)
   * 支持任意读取分段(nocopy)，上层代码可以 nocopy 地并行处理数据流分段，无需关心生命周期，通过引用计数 GC。
   * 支持任意拼接(nocopy)，写 buffer 支持通过 block 拼接到尾指针后的形式，无需 copy，保证数据只写一次。
4. Nocopy Buffer 池化，减少 GC
   * 将每个 []byte 数组视为 block 节点，构建对象池维护空闲 block，由此复用 block，减少内存占用和 GC。基于该 Nocopy Buffer，实现了 Nocopy Thrift，使得编解码过程内存零分配零拷贝。

### 优化
#### string / binary 零拷贝
直接用 []byte(string) 去转换一个 string 到 []byte 的话实际上是会发生一次拷贝的，原因是 Go 的设计中 string 是 immutable 的但是 []byte 是 mutable 的。

```
// zero-copy slice convert to string
func unsafeSliceToString(b []byte) string {
	return *(*string)(unsafe.Pointer(&b))
}

// zero-copy slice convert to string
func unsafeStringToSlice(s string) (b []byte) {
	p := unsafe.Pointer((*reflect.StringHeader)(unsafe.Pointer(&s)).Data)
	hdr := (*reflect.SliceHeader)(unsafe.Pointer(&b))
	hdr.Data = uintptr(p)
	hdr.Cap = len(s)
	hdr.Len = len(s)
	return b
}
```

先把 string 的地址拿到，再拼装上一个 slice byte 的 header。注意：这样生成的 []byte 不可写，否则行为未定义。


## 高效的内存复用[mcache][mcache]

mcache 是一个基于 sync.Pool 的内存池实现，用于提高内存分配性能。它通过预分配和复用内存块来减少频繁的内存分配和垃圾回收开销。

```
// 使用 46 个 sync.Pool 来管理不同大小的内存块
const maxSize = 46

// index contains []byte which cap is 1<<index
var caches [maxSize]sync.Pool

// 对应 Go 运行时中字节切片的内部表示，用于直接操作切片的内存布局。
type bytesHeader struct {
	Data *byte
	Len  int
	Cap  int
}

func init() {
	for i := 0; i < maxSize; i++ {
		// 内存块大小为 2^i，i 从 0 到 45
		// 每个 sync.Pool 管理一个固定大小的内存块
		size := 1 << i
		// 为每个池设置 New 函数，当池为空时创建新的内存块
		caches[i].New = func() interface{} {
			// 使用 dirtmake.Bytes 创建未清零的内存块（性能优化）
			buf := dirtmake.Bytes(0, size)
			h := (*bytesHeader)(unsafe.Pointer(&buf))
			// 只返回内存块的指针部分，节省存储空间
			return h.Data
		}
	}
}

//go:linkname mallocgc runtime.mallocgc
func mallocgc(size uintptr, typ unsafe.Pointer, needzero bool) unsafe.Pointer

// 字节分配字节片，但不清除其引用的内存。
// 如果CAP大于Runtime.MaxAlloc，则抛出致命错误，而不是恐慌。
// 注意：必须在读取之前设置任何字节元素。
func Bytes(len, cap int) (b []byte) {
    if len < 0 || len > cap {
        panic("dirtmake.Bytes: len out of range")
    }
    // 绕过 Go 的 make 函数，直接调用 Go 运行时的内存分配器
    p := mallocgc(uintptr(cap), nil, false)  // needzero = false，不初始化内存内容，避免不必要的内存清零操作
    
    // 构造切片结构
    sh := (*slice)(unsafe.Pointer(&b))
    sh.data = p
    sh.len = len
    sh.cap = cap
    return
}
```

```
// 自动计算合适的池索引
func calcIndex(size int) int {
	if size == 0 {
		return 0
	}
	// 对于 2 的幂次方
	if isPowerOfTwo(size) {
		return bsr(size) // 直接使用对应池
	}
	return bsr(size) + 1 // 向上取整到下一个池
}

// 计算 x 的二进制表示中最高位 1 的位置
func bsr(x int) int {
	return bits.Len(uint(x)) - 1
}

// 判断 x 是否为 2 的幂次方
func isPowerOfTwo(x int) bool {
	return (x != 0) && ((x & (-x)) == x)
}

// Malloc支持一个或两个整数参数。
// 大小指定返回片段的长度，这意味着 len(ret) == size。
// 可以提供第二个整数参数来指定最小容量，这意味着 cap(ret) >= cap。
func Malloc(size int, capacity ...int) []byte {
	if len(capacity) > 1 {
		panic("too many arguments to Malloc")
	}
	var c = size
	if len(capacity) > 0 && capacity[0] > size {
		c = capacity[0]
	}

	i := calcIndex(c)

	ret := []byte{}
	// 通过 unsafe 操作直接构造字节切片
	h := (*bytesHeader)(unsafe.Pointer(&ret))
	// 返回的切片长度等于请求的 size，容量为 2^i
	h.Len = size
	h.Cap = 1 << i
	h.Data = caches[i].Get().(*byte)
	return ret
}
```

```
// 当不再使用BUF时，应调用Free
func Free(buf []byte) {
	size := cap(buf)
	if !isPowerOfTwo(size) {
		// 非 2 的幂次方容量直接丢弃，避免内存污染
		return
	}
	// 只回收容量为 2 的幂次方的切片
	h := (*bytesHeader)(unsafe.Pointer(&buf))
	// 只回收内存指针，不回收整个切片结构
	caches[bsr(size)].Put(h.Data)
}
```


## 高性能goroutine池[gopool][gopool]
一个高性能的协程池实现，主要目标是重用协程并限制协程数量。它提供了以下特性：
1. 高性能
2. 自动恢复 panic
3. 限制协程数量
4. 重用协程栈

### 配置
```
const (
	// 默认的扩容阈值常量
	defaultScalaThreshold = 1
)

// Config 用于配置池
type Config struct {
	// 用于控制何时创建新的工作协程
	// 当任务队列长度超过这个阈值时，会触发创建新的工作协程
	ScaleThreshold int32
}

// NewConfig 创建一个默认的 Config
func NewConfig() *Config {
	c := &Config{
		ScaleThreshold: defaultScalaThreshold,
	}
	return c
}
```

### 主入口

```
// defaultPool 全局默认池
var defaultPool Pool

// poolMap 用于存储所有注册的池
var poolMap sync.Map

func init() {
	// 初始化全局默认池，使用最大协程数限制和默认配置
	defaultPool = NewPool("gopool.DefaultPool", math.MaxInt32, NewConfig())
}

// Go 是 Go 关键字的替代品，能够恢复 panic
func Go(f func()) {
	CtxGo(context.Background(), f)
}

// CtxGo 比 Go 更推荐使用，支持传入自定义 context
func CtxGo(ctx context.Context, f func()) {
	defaultPool.CtxGo(ctx, f)
}

// SetCap 不推荐使用，修改全局池的容量限制，会影响其他调用者
func SetCap(cap int32) {
	defaultPool.SetCap(cap)
}

// SetPanicHandler 设置全局池的 panic 处理函数
func SetPanicHandler(f func(context.Context, interface{})) {
	defaultPool.SetPanicHandler(f)
}

// WorkerCount 返回全局默认池的运行中协程数量
func WorkerCount() int32 {
	return defaultPool.WorkerCount()
}

// RegisterPool 注册一个新的池到全局池映射中
// GetPool 可以用来根据名称获取已注册的池
// 如果名称已存在，则返回错误
func RegisterPool(p Pool) error {
	_, loaded := poolMap.LoadOrStore(p.Name(), p)
	if loaded {
		return fmt.Errorf("name: %s already registered", p.Name())
	}
	return nil
}

// GetPool 根据名称获取已注册的池
// 如果未注册，则返回 nil
func GetPool(name string) Pool {
	p, ok := poolMap.Load(name)
	if !ok {
		return nil
	}
	return p.(Pool)
}
```

### 协程池的核心实现
```
type Pool interface {
	// 获取池的名称
	Name() string
	// 设置池的协程容量
	SetCap(cap int32)
	// 执行任务
	Go(f func())
	// 执行任务并传递上下文
	CtxGo(ctx context.Context, f func())
	// 设置 panic 处理函数
	SetPanicHandler(f func(context.Context, interface{}))
	// 获取运行中的协程数量
	WorkerCount() int32
}

// taskPool 使用 sync.Pool 复用 task 对象，减少 GC 压力
var taskPool sync.Pool

// init 初始化 taskPool
func init() {
	taskPool.New = newTask
}

// task 表示一个任务
type task struct {
	// 任务的上下文
	ctx context.Context
	// 任务的执行函数
	f func()
	// 下一个任务
	next *task
}

// zero 将任务对象重置为初始状态
func (t *task) zero() {
	t.ctx = nil
	t.f = nil
	t.next = nil
}

// Recycle 将任务对象放回池中
func (t *task) Recycle() {
	t.zero()
	taskPool.Put(t)
}

// newTask 创建一个新的任务对象
func newTask() interface{} {
	return &task{}
}

// taskList 用于存储任务队列
type taskList struct {
	sync.Mutex
	// 任务队列的头节点
	taskHead *task
	// 任务队列的尾节点
	taskTail *task
}

// pool 表示一个协程池
type pool struct {
	// 池的名称
	name string

	// 池的协程容量，即最大并发协程数
	cap int32
	// 池的配置信息
	config *Config
	// 任务队列
	taskHead  *task
	taskTail  *task
	taskLock  sync.Mutex
	taskCount int32

	// 记录运行中的协程数量
	workerCount int32

	// 当协程 panic 时调用的处理函数
	panicHandler func(context.Context, interface{})
}

// NewPool 创建一个新的池
func NewPool(name string, cap int32, config *Config) Pool {
	p := &pool{
		name:   name,
		cap:    cap,
		config: config,
	}
	return p
}

// Name 获取池的名称
func (p *pool) Name() string {
	return p.name
}

// SetCap 设置池的协程容量
func (p *pool) SetCap(cap int32) {
	atomic.StoreInt32(&p.cap, cap)
}

// Go 执行任务
func (p *pool) Go(f func()) {
	p.CtxGo(context.Background(), f)
}

// CtxGo 执行任务并传递上下文
func (p *pool) CtxGo(ctx context.Context, f func()) {
	// 从池中获取一个任务对象
	t := taskPool.Get().(*task)
	// 设置任务的上下文和执行函数
	t.ctx = ctx
	t.f = f

	p.taskLock.Lock()
	// 如果任务队列为空，则将任务设置为头节点和尾节点
	if p.taskHead == nil {
		p.taskHead = t
		p.taskTail = t
	} else {
		// 否则将任务添加到任务队列的末尾
		p.taskTail.next = t
		p.taskTail = t
	}
	// 增加任务计数
	atomic.AddInt32(&p.taskCount, 1)
	p.taskLock.Unlock()

	// 当以下两个条件满足时，会创建新的工作协程：
	// 1. 任务队列长度超过阈值，且当前运行中的协程数小于最大并发数限制
	// 2. 当前没有运行中的协程
	if (atomic.LoadInt32(&p.taskCount) >= p.config.ScaleThreshold && p.WorkerCount() < atomic.LoadInt32(&p.cap)) || p.WorkerCount() == 0 {
		p.incWorkerCount()
		w := workerPool.Get().(*worker)
		w.pool = p
		w.run()
	}
}

// SetPanicHandler 设置 panic 处理函数
func (p *pool) SetPanicHandler(f func(context.Context, interface{})) {
	p.panicHandler = f
}

// WorkerCount 获取运行中的协程数量
func (p *pool) WorkerCount() int32 {
	return atomic.LoadInt32(&p.workerCount)
}

// incWorkerCount 增加运行中的协程数量
func (p *pool) incWorkerCount() {
	atomic.AddInt32(&p.workerCount, 1)
}

// decWorkerCount 减少运行中的协程数量
func (p *pool) decWorkerCount() {
	atomic.AddInt32(&p.workerCount, -1)
}
```

### 工作协程的实现
```
// workerPool 使用 sync.Pool 复用 worker 对象，减少 GC 压力
var workerPool sync.Pool

// init 初始化 workerPool
func init() {
	workerPool.New = newWorker
}

// worker 表示一个工作协程
type worker struct {
	pool *pool
}

// newWorker 创建一个新的 worker 对象		
func newWorker() interface{} {
	return &worker{}
}

// run 启动工作协程
func (w *worker) run() {
	// 启动一个协程，用于执行任务
	go func() {
		// 循环执行任务
		for {
			// 从任务队列中获取一个任务
			var t *task
			// 加锁，防止并发访问任务队列
			w.pool.taskLock.Lock()
			// 如果任务队列不为空，则获取任务队列的头节点
			if w.pool.taskHead != nil {
				t = w.pool.taskHead
				// 将任务队列的头节点设置为下一个节点
				w.pool.taskHead = w.pool.taskHead.next
				// 减少任务计数
				atomic.AddInt32(&w.pool.taskCount, -1)
			}
			// 如果任务队列为空，则关闭工作协程
			if t == nil {
				// 关闭工作协程
				w.close()
				w.pool.taskLock.Unlock()
				w.Recycle()
				return
			}
			// 解锁，允许其他协程访问任务队列
			w.pool.taskLock.Unlock()
			// 执行任务
			func() {
				// 延迟执行，用于恢复 panic
				defer func() {
					if r := recover(); r != nil {
						// 如果 panic 处理函数不为空，则调用 panic 处理函数
						if w.pool.panicHandler != nil {
							w.pool.panicHandler(t.ctx, r)
						} else {
							// 如果 panic 处理函数为空，则打印错误信息
							msg := fmt.Sprintf("GOPOOL: panic in pool: %s: %v: %s", w.pool.name, r, debug.Stack())
							logger.CtxErrorf(t.ctx, msg)
						}
					}
				}()
				// 执行任务
				t.f()
			}()
			// 将任务对象放回池中
			t.Recycle()
		}
	}()
}

// close 关闭工作协程
func (w *worker) close() {
	w.pool.decWorkerCount()
}

// zero 重置 worker 对象
func (w *worker) zero() {
	w.pool = nil
}

// Recycle 将 worker 对象放回池中
func (w *worker) Recycle() {
	w.zero()
	workerPool.Put(w)
}
```


[Netpoll]: https://github.com/cloudwego/netpoll
[net]: https://github.com/golang/go/tree/master/src/net
[net.Conn]: https://github.com/golang/go/blob/master/src/net/net.go
[evio]: https://github.com/tidwall/evio
[gnet]: https://github.com/panjf2000/gnet
[netty]: https://github.com/netty/netty
[Kitex]: https://github.com/cloudwego/kitex
[Hertz]: https://github.com/cloudwego/hertz
[netpoll-examples]:https://github.com/cloudwego/netpoll-examples
[ByteDance]: https://www.bytedance.com
[Redis]: https://redis.io
[HAProxy]: http://www.haproxy.org

[gopool]: https://github.com/bytedance/gopkg/tree/develop/util/gopool
[mcache]: https://github.com/bytedance/gopkg/tree/develop/lang/mcache