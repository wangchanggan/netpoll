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

## LinkBuffer
nocopy_linkbuffer.go:814
### 零拷贝优化
* WriteString 和 WriteBinary 直接引用原始数据，避免拷贝
* ReadString 使用 unsafeSliceToString 实现零拷贝转换
* Peek 操作复用缓存，避免重复分配

### 性能优化
* 使用 BinaryInplaceThreshold 阈值（4KB）来决定是否使用拷贝
* 使用 atomic 操作保证并发安全

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