package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/md4"
)

// ed2kOf 计算测试数据的 ED2K 哈希。
func ed2kOf(t *testing.T, data []byte) ed2kSums {
	t.Helper()

	h := newED2KHasher()
	n, err := h.Write(data)
	if err != nil {
		t.Fatalf("Write() error: %v", err)
	}
	if n != len(data) {
		t.Fatalf("Write() = %d, want %d", n, len(data))
	}

	return h.Sums()
}

func TestED2KHashVectors(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"empty", nil, "31D6CFE0D16AE931B73C59D7E0C089C0"},
		{"a", []byte("a"), "BDE52CB31DE33E46245E05FBDBD6FB24"},
		{"abc", []byte("abc"), "A448017AAF21D8525FC10AE87AA6729D"},
		{"hello world", []byte("hello world"), "AA010FBC1D14C795D86EF98C95479D17"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sums := ed2kOf(t, tc.data)
			if sums.Blue != tc.want {
				t.Fatalf("Blue = %s, want %s", sums.Blue, tc.want)
			}
			// 非整数倍时 Blue 和 Red 相同。
			if sums.Red != tc.want {
				t.Fatalf("Red = %s, want %s", sums.Red, tc.want)
			}
			if sums.Boundary {
				t.Fatalf("Boundary = true, want false")
			}
			if sums.Size != int64(len(tc.data)) {
				t.Fatalf("Size = %d, want %d", sums.Size, len(tc.data))
			}
		})
	}
}

func TestED2KHashBoundary(t *testing.T) {
	data := bytes.Repeat([]byte{0x55}, ed2kChunkSize)
	sums := ed2kOf(t, data)

	if !sums.Boundary {
		t.Fatal("Boundary = false, want true")
	}
	if sums.Blue != "4127A47867B6110F0F86F2D9845FB374" {
		t.Fatalf("Blue = %s", sums.Blue)
	}
	if sums.Red != "49E80F377B7E4E706DBD3ECC89F39306" {
		t.Fatalf("Red = %s", sums.Red)
	}
}

// referenceED2K 用独立的直读方式计算 ED2K，用于校验流式实现。
func referenceED2K(data []byte) (string, string) {
	size := len(data)

	var digests []byte
	for len(data) >= ed2kChunkSize {
		h := md4.New()
		_, _ = h.Write(data[:ed2kChunkSize])
		digests = append(digests, h.Sum(nil)...)
		data = data[ed2kChunkSize:]
	}
	if len(data) > 0 || len(digests) == 0 {
		h := md4.New()
		_, _ = h.Write(data)
		digests = append(digests, h.Sum(nil)...)
	}

	root := func(b []byte) string {
		if len(b) == md4.Size {
			return strings.ToUpper(hex.EncodeToString(b))
		}
		h := md4.New()
		_, _ = h.Write(b)
		return strings.ToUpper(hex.EncodeToString(h.Sum(nil)))
	}

	blue := root(digests)
	red := blue
	if size > 0 && size%ed2kChunkSize == 0 {
		empty := md4.New()
		all := make([]byte, 0, len(digests)+md4.Size)
		all = append(all, digests...)
		all = append(all, empty.Sum(nil)...)
		red = root(all)
	}

	return blue, red
}

func TestED2KHashMultiChunk(t *testing.T) {
	// 一块多一点，覆盖“最后一块不满”的路径。
	data := make([]byte, ed2kChunkSize+12345)
	for i := range data {
		data[i] = byte((i*31 + 7) & 0xff)
	}

	wantBlue, wantRed := referenceED2K(data)
	sums := ed2kOf(t, data)

	if sums.Blue != wantBlue {
		t.Fatalf("Blue = %s, want %s", sums.Blue, wantBlue)
	}
	if sums.Red != wantRed {
		t.Fatalf("Red = %s, want %s", sums.Red, wantRed)
	}
	if sums.Boundary {
		t.Fatal("Boundary = true, want false")
	}
}

func TestED2KHasherStreaming(t *testing.T) {
	data := make([]byte, ed2kChunkSize+12345)
	for i := range data {
		data[i] = byte(i)
	}

	wantBlue, wantRed := referenceED2K(data)

	chunked := newED2KHasher()
	for offset := 0; offset < len(data); {
		size := offset%4096 + 1
		if offset+size > len(data) {
			size = len(data) - offset
		}
		if _, err := chunked.Write(data[offset : offset+size]); err != nil {
			t.Fatalf("Write() error: %v", err)
		}
		offset += size
	}
	sums := chunked.Sums()

	if sums.Blue != wantBlue {
		t.Fatalf("Blue = %s, want %s", sums.Blue, wantBlue)
	}
	if sums.Red != wantRed {
		t.Fatalf("Red = %s, want %s", sums.Red, wantRed)
	}
	if sums.Size != int64(len(data)) {
		t.Fatalf("Size = %d, want %d", sums.Size, len(data))
	}
}

func TestED2KLinkRawName(t *testing.T) {
	hash := strings.Repeat("a", 32)
	cases := []string{
		"Tokyo-Hot e170 中田恵理 Eri Nakata [612P／281.8MB].zip",
		"hello world.zip",
		"中文 [x] ? 100%.zip",
	}

	for _, name := range cases {
		got := ed2kLink(name, 42, hash)
		want := "ed2k://|file|" + name + "|42|" + strings.Repeat("A", 32) + "|/"
		if got != want {
			t.Fatalf("ed2kLink(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.bin")
	data := []byte("hello world")

	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	got, err := hashFile(f, true)
	if err != nil {
		t.Fatalf("hashFile() error: %v", err)
	}

	wantSHA1 := strings.ToUpper(hex.EncodeToString(sha1Sum(data)))
	if got.SHA1 != wantSHA1 {
		t.Fatalf("SHA1 = %s, want %s", got.SHA1, wantSHA1)
	}
	if got.Size != int64(len(data)) {
		t.Fatalf("Size = %d, want %d", got.Size, len(data))
	}
	if got.ED2K.Blue != "AA010FBC1D14C795D86EF98C95479D17" {
		t.Fatalf("ED2K = %s", got.ED2K.Blue)
	}
	if got.BlockSHA1 == "" {
		t.Fatal("BlockSHA1 is empty")
	}
}

func TestHashFileEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.bin")

	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	got, err := hashFile(f, true)
	if err != nil {
		t.Fatalf("hashFile() error: %v", err)
	}

	wantSHA1 := strings.ToUpper(hex.EncodeToString(sha1Sum(nil)))
	if got.SHA1 != wantSHA1 {
		t.Fatalf("SHA1 = %s, want %s", got.SHA1, wantSHA1)
	}
	if got.Size != 0 {
		t.Fatalf("Size = %d, want 0", got.Size)
	}
	if got.ED2K.Blue != "31D6CFE0D16AE931B73C59D7E0C089C0" {
		t.Fatalf("ED2K = %s", got.ED2K.Blue)
	}
	if got.BlockSHA1 != "" {
		t.Fatalf("BlockSHA1 = %s, want empty", got.BlockSHA1)
	}
}

func TestAddED2KResult(t *testing.T) {
	oldConfig, oldResult := config, result
	defer func() {
		config, result = oldConfig, oldResult
	}()

	config.ED2K = true
	config.ED2KMode = "auto"

	blue := strings.Repeat("a", 32)
	red := strings.Repeat("b", 32)
	file := &fileInfo{
		Path:         "/tmp/x.bin",
		Name:         "x.bin",
		Size:         42,
		ED2KBlue:     blue,
		ED2KRed:      red,
		ED2KBoundary: true,
	}

	result.ED2K = nil
	file.addED2KResult()
	if len(result.ED2K) != 1 {
		t.Fatalf("len(result.ED2K) = %d, want 1", len(result.ED2K))
	}

	got := result.ED2K[0]
	if got.Variant != "both" {
		t.Fatalf("Variant = %s, want both", got.Variant)
	}
	if got.Link != got.LinkRed {
		t.Fatalf("Link = %s, want LinkRed %s", got.Link, got.LinkRed)
	}
	if got.LinkBlue != ed2kLink("x.bin", 42, blue) {
		t.Fatalf("LinkBlue = %s", got.LinkBlue)
	}

	file.ED2KBoundary = false
	result.ED2K = nil
	file.addED2KResult()
	if len(result.ED2K) != 1 {
		t.Fatalf("len(result.ED2K) = %d, want 1", len(result.ED2K))
	}
	if result.ED2K[0].Variant != "blue" {
		t.Fatalf("Variant = %s, want blue", result.ED2K[0].Variant)
	}
}

func sha1Sum(data []byte) []byte {
	sum := sha1.Sum(data)
	return sum[:]
}
