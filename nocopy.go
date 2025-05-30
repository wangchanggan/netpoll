// Copyright 2022 CloudWeGo Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package netpoll

import (
	"io"
	"reflect"
	"unsafe"

	"github.com/bytedance/gopkg/lang/dirtmake"
	"github.com/bytedance/gopkg/lang/mcache"
)

// Reader is a collection of operations for nocopy reads.
//
// For ease of use, it is recommended to implement Reader as a blocking interface,
// rather than simply fetching the buffer.
// For example, the return of calling Next(n) should be blocked if there are fewer than n bytes, unless timeout.
// The return value is guaranteed to meet the requirements or an error will be returned.
type Reader interface {
	// Next returns a slice containing the next n bytes from the buffer,
	// advancing the buffer as if the bytes had been returned by Read.
	//
	// If there are fewer than n bytes in the buffer, Next returns will be blocked
	// until data enough or an error occurs (such as a wait timeout).
	//
	// The slice p is only valid until the next call to the Release method.
	// Next is not globally optimal, and Skip, ReadString, ReadBinary methods
	// are recommended for specific scenarios.
	//
	// Return: len(p) must be n or 0, and p and error cannot be nil at the same time.
	// 读取并前进n个字节
	Next(n int) (p []byte, err error)

	// Peek returns the next n bytes without advancing the reader.
	// Other behavior is the same as Next.
	// 读取但不前进
	Peek(n int) (buf []byte, err error)

	// Skip the next n bytes and advance the reader, which is
	// a faster implementation of Next when the next data is not used.
	// 跳过n个字节
	Skip(n int) (err error)

	// Until reads until the first occurrence of delim in the input,
	// returning a slice stops with delim in the input buffer.
	// If Until encounters an error before finding a delimiter,
	// it returns all the data in the buffer and the error itself (often ErrEOF or ErrConnClosed).
	// Until returns err != nil only if line does not end in delim.
	// 读取直到遇到分隔符
	Until(delim byte) (line []byte, err error)

	// ReadString is a faster implementation of Next when a string needs to be returned.
	// It replaces:
	//
	//  var p, err = Next(n)
	//  return string(p), err
	//
	// 读取字符串
	ReadString(n int) (s string, err error)

	// ReadBinary is a faster implementation of Next when it needs to
	// return a copy of the slice that is not shared with the underlying layer.
	// It replaces:
	//
	//  var p, err = Next(n)
	//  var b = make([]byte, n)
	//  copy(b, p)
	//  return b, err
	//
	// 读取二进制数据
	ReadBinary(n int) (p []byte, err error)

	// ReadByte is a faster implementation of Next when a byte needs to be returned.
	// It replaces:
	//
	//  var p, err = Next(1)
	//  return p[0], err
	//
	// 读取单个字节
	ReadByte() (b byte, err error)

	// Slice returns a new Reader containing the Next n bytes from this Reader.
	//
	// If you want to make a new Reader using the []byte returned by Next, Slice already does that,
	// and the operation is zero-copy. Besides, Slice would also Release this Reader.
	// The logic pseudocode is similar:
	//
	//  var p, err = this.Next(n)
	//  var reader = new Reader(p) // pseudocode
	//  this.Release()
	//  return reader, err
	//
	// 创建新的Reader
	Slice(n int) (r Reader, err error)

	// Release the memory space occupied by all read slices. This method needs to be executed actively to
	// recycle the memory after confirming that the previously read data is no longer in use.
	// After invoking Release, the slices obtained by the method such as Next, Peek, Skip will
	// become an invalid address and cannot be used anymore.
	// 释放内存
	Release() (err error)

	// Len returns the total length of the readable data in the reader.
	// 获取可读数据长度
	Len() (length int)
}

// Writer is a collection of operations for nocopy writes.
//
// The usage of the design is a two-step operation, first apply for a section of memory,
// fill it and then submit. E.g:
//
//	var buf, _ = Malloc(n)
//	buf = append(buf[:0], ...)
//	Flush()
//
// Note that it is not recommended to submit self-managed buffers to Writer.
// Since the writer is processed asynchronously, if the self-managed buffer is used and recycled after submission,
// it may cause inconsistent life cycle problems. Of course this is not within the scope of the design.
type Writer interface {
	// Malloc returns a slice containing the next n bytes from the buffer,
	// which will be written after submission(e.g. Flush).
	//
	// The slice p is only valid until the next submit(e.g. Flush).
	// Therefore, please make sure that all data has been written into the slice before submission.
	// 分配写入缓冲区
	Malloc(n int) (buf []byte, err error)

	// WriteString is a faster implementation of Malloc when a string needs to be written.
	// It replaces:
	//
	//  var buf, err = Malloc(len(s))
	//  n = copy(buf, s)
	//  return n, err
	//
	// The argument string s will be referenced based on the original address and will not be copied,
	// so make sure that the string s will not be changed.
	// 写入字符串
	WriteString(s string) (n int, err error)

	// WriteBinary is a faster implementation of Malloc when a slice needs to be written.
	// It replaces:
	//
	//  var buf, err = Malloc(len(b))
	//  n = copy(buf, b)
	//  return n, err
	//
	// The argument slice b will be referenced based on the original address and will not be copied,
	// so make sure that the slice b will not be changed.
	// 写入二进制数据
	WriteBinary(b []byte) (n int, err error)

	// WriteByte is a faster implementation of Malloc when a byte needs to be written.
	// It replaces:
	//
	//  var buf, _ = Malloc(1)
	//  buf[0] = b
	//
	// 写入单个字节
	WriteByte(b byte) (err error)

	// WriteDirect is used to insert an additional slice of data on the current write stream.
	// For example, if you plan to execute:
	//
	//  var bufA, _ = Malloc(nA)
	//  WriteBinary(b)
	//  var bufB, _ = Malloc(nB)
	//
	// It can be replaced by:
	//
	//  var buf, _ = Malloc(nA+nB)
	//  WriteDirect(b, nB)
	//
	// where buf[:nA] = bufA, buf[nA:nA+nB] = bufB.
	// 直接写入数据
	WriteDirect(p []byte, remainCap int) error

	// MallocAck will keep the first n malloc bytes and discard the rest.
	// The following behavior:
	//
	//  var buf, _ = Malloc(8)
	//  buf = buf[:5]
	//  MallocAck(5)
	//
	// equivalent as
	//  var buf, _ = Malloc(5)
	//
	// 确认分配
	MallocAck(n int) (err error)

	// Append the argument writer to the tail of this writer and set the argument writer to nil,
	// the operation is zero-copy, similar to p = append(p, w.p).
	// 追加Writer
	Append(w Writer) (err error)

	// Flush will submit all malloc data and must confirm that the allocated bytes have been correctly assigned.
	// Its behavior is equivalent to the io.Writer hat already has parameters(slice b).
	// 提交写入
	Flush() (err error)

	// MallocLen returns the total length of the writable data that has not yet been submitted in the writer.
	// 获取未提交的写入长度
	MallocLen() (length int)
}

// ReadWriter is a combination of Reader and Writer.
// 组合了 Reader 和 Writer 接口
type ReadWriter interface {
	Reader
	Writer
}

// NewReader convert io.Reader to nocopy Reader
func NewReader(r io.Reader) Reader {
	return newZCReader(r)
}

// NewWriter convert io.Writer to nocopy Writer
func NewWriter(w io.Writer) Writer {
	return newZCWriter(w)
}

// NewReadWriter convert io.ReadWriter to nocopy ReadWriter
func NewReadWriter(rw io.ReadWriter) ReadWriter {
	return &zcReadWriter{
		zcReader: newZCReader(rw),
		zcWriter: newZCWriter(rw),
	}
}

// NewIOReader convert Reader to io.Reader
func NewIOReader(r Reader) io.Reader {
	if reader, ok := r.(io.Reader); ok {
		return reader
	}
	return newIOReader(r)
}

// NewIOWriter convert Writer to io.Writer
func NewIOWriter(w Writer) io.Writer {
	if writer, ok := w.(io.Writer); ok {
		return writer
	}
	return newIOWriter(w)
}

// NewIOReadWriter convert ReadWriter to io.ReadWriter
func NewIOReadWriter(rw ReadWriter) io.ReadWriter {
	if rwer, ok := rw.(io.ReadWriter); ok {
		return rwer
	}
	return &ioReadWriter{
		ioReader: newIOReader(rw),
		ioWriter: newIOWriter(rw),
	}
}

const (
	block1k  = 1 * 1024
	block2k  = 2 * 1024
	block4k  = 4 * 1024
	block8k  = 8 * 1024
	block32k = 32 * 1024

	pagesize  = block8k
	mallocMax = block8k * block1k // mallocMax is 8MB

	minReuseBytes = 64 // only reuse bytes if n >= minReuseBytes

	defaultLinkBufferMode = 0
	// readonlyMask is used to set readonly mode,
	// which indicate that the buffer node memory is not controlled by itself,
	// so we cannot reuse the buffer or nocopy read it.
	readonlyMask uint8 = 1 << 0 // 0000 0001
	// readonlyMask is used to set nocopyRead mode,
	// which indicate that the buffer node has been no copy read and cannot reuse the buffer.
	nocopyReadMask uint8 = 1 << 1 // 0000 0010
)

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

// malloc limits the cap of the buffer from mcache.
func malloc(size, capacity int) []byte {
	if capacity > mallocMax {
		return dirtmake.Bytes(size, capacity)
	}
	return mcache.Malloc(size, capacity)
}

// free limits the cap of the buffer from mcache.
func free(buf []byte) {
	if cap(buf) > mallocMax {
		return
	}
	mcache.Free(buf)
}
