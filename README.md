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


## 高效的内存复用mcache

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