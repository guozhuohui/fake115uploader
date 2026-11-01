package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// 计算文件指定范围内的 sha1 值
func hashFileRange(f *os.File, signCheck string) (rangeHash string, e error) {
	defer func() {
		if err := recover(); err != nil {
			e = fmt.Errorf("hashFileRange() error: %v", err)
		}
	}()

	var start, end int64
	_, err := fmt.Sscanf(signCheck, "%d-%d", &start, &end)
	checkErr(err)
	if start < 0 || end < 0 || end < start {
		return "", fmt.Errorf("sign_check范围错误：%s", signCheck)
	}

	_, err = f.Seek(start, io.SeekStart)
	checkErr(err)
	h := sha1.New()
	_, err = io.CopyN(h, f, end-start+1)
	checkErr(err)

	return strings.ToUpper(hex.EncodeToString(h.Sum(nil))), nil
}

// fileHashes 保存一次文件读取得到的哈希信息。
type fileHashes struct {
	BlockSHA1 string
	SHA1      string
	Size      int64
	ED2K      ed2kSums
}

// hashFile 只读取一次文件，同时计算 SHA1 和 ED2K。
//
// withED2K 为 false 时不会计算 MD4，行为和原来的 hashSHA1 一致。
func hashFile(f *os.File, withED2K bool) (r fileHashes, e error) {
	defer func() {
		if err := recover(); err != nil {
			e = fmt.Errorf("hashFile() error: %v", err)
		}
	}()

	// 计算文件最前面一个区块的 sha1 hash 值（保留原有行为）。
	// 空文件时 ReadFull 返回 io.EOF，不能当成错误。
	block := make([]byte, 128*1024)
	n, err := io.ReadFull(f, block)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return r, err
	}
	if n > 0 {
		data := sha1.Sum(block[:n])
		r.BlockSHA1 = strings.ToUpper(hex.EncodeToString(data[:]))
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return r, err
	}

	// 计算整个文件的 sha1 hash 值；需要时同时计算 ED2K。
	h := sha1.New()
	var w io.Writer = h
	var eh *ed2kHasher
	if withED2K {
		eh = newED2KHasher()
		w = io.MultiWriter(h, eh)
	}

	r.Size, err = io.Copy(w, f)
	if err != nil {
		return r, err
	}
	r.SHA1 = strings.ToUpper(hex.EncodeToString(h.Sum(nil)))
	if eh != nil {
		r.ED2K = eh.Sums()
	}

	return r, nil
}

// 计算文件的 sha1 值
func hashSHA1(f *os.File) (blockHash, totalHash string, e error) {
	h, err := hashFile(f, false)
	if err != nil {
		return "", "", err
	}

	return h.BlockSHA1, h.SHA1, nil
}
