package main

import (
	"encoding/hex"
	"hash"
	"strconv"
	"strings"

	"golang.org/x/crypto/md4"
)

// ED2K 使用 9,500 KiB（9,728,000 字节）作为分块大小。
const ed2kChunkSize = 9728000

// ed2kHasher 以流式方式计算 ED2K 哈希。
//
// 它只实现了 io.Writer，方便和 SHA1 一起放进 io.MultiWriter，
// 从而在读取文件一次的情况下同时得到 SHA1 和 ED2K。
type ed2kHasher struct {
	chunk   hash.Hash // 当前块的 MD4
	filled  int       // 当前块已经写入的字节数
	digests []byte    // 所有完整块的 MD4 拼接
	size    int64     // 总字节数
}

func newED2KHasher() *ed2kHasher {
	return &ed2kHasher{chunk: md4.New()}
}

// Write 实现 io.Writer。
func (h *ed2kHasher) Write(p []byte) (int, error) {
	n := len(p)
	h.size += int64(n)

	for len(p) > 0 {
		space := ed2kChunkSize - h.filled
		if space > len(p) {
			space = len(p)
		}

		_, _ = h.chunk.Write(p[:space])
		h.filled += space
		p = p[space:]

		if h.filled == ed2kChunkSize {
			h.digests = h.chunk.Sum(h.digests)
			h.chunk.Reset()
			h.filled = 0
		}
	}

	return n, nil
}

// ed2kSums 保存一次计算结果。
type ed2kSums struct {
	Size     int64
	Blue     string // 标准/修正实现
	Red      string // eMule 兼容实现
	Boundary bool   // 文件大小是否为 ed2kChunkSize 的正整数倍
}

// Sums 返回 Blue/Red 两种哈希。
//
// Blue 与 Red 只在文件大小恰好是 ed2kChunkSize 的整数倍时不同：
// Red 会在块哈希列表末尾追加一个空块的 MD4，再计算根哈希。
func (h *ed2kHasher) Sums() ed2kSums {
	digests := h.digests
	if h.filled > 0 || len(digests) == 0 {
		// 最后一块不满，或者文件为空：把当前块的 MD4 补进列表。
		digests = h.chunk.Sum(digests)
	}

	s := ed2kSums{
		Size:     h.size,
		Boundary: h.size > 0 && h.size%ed2kChunkSize == 0,
	}
	s.Blue = ed2kRoot(digests)
	s.Red = s.Blue

	if s.Boundary {
		empty := md4.New()
		all := make([]byte, 0, len(digests)+md4.Size)
		all = append(all, digests...)
		all = append(all, empty.Sum(nil)...)
		s.Red = ed2kRoot(all)
	}

	return s
}

// ed2kRoot 计算 ED2K 根哈希。
//
// 只有一个块时直接返回该块的 MD4；多个块时对拼接后的块哈希再做一次 MD4。
func ed2kRoot(digests []byte) string {
	if len(digests) == md4.Size {
		return strings.ToUpper(hex.EncodeToString(digests))
	}

	h := md4.New()
	_, _ = h.Write(digests)
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil)))
}

// ed2kLink 拼接 ED2K 链接。
//
// 文件名按原始 UTF-8 写入，不做 URL 编码。
// 注意：ED2K 用 | 分隔字段，文件名本身不能包含 |；Windows 文件名不允许 |，
// 其他平台遇到该字符时生成的链接会被客户端截断。
func ed2kLink(name string, size int64, hash string) string {
	return "ed2k://|file|" + name + "|" +
		strconv.FormatInt(size, 10) + "|" + strings.ToUpper(hash) + "|/"
}
